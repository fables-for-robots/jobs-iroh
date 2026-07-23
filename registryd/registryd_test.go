package registryd_test

// Loopback e2e: a full jobs-server (serve.Run) holds a pushed build fixture;
// jobs-registry (registryd.Run) syncs it on demand and a real registry client
// (go-containerregistry's remote) pulls the image over HTTP. Everything stays
// on the local machine: direct addresses, no discovery, no relays.

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fables-for-robots/amber-store-core/key"
	"github.com/fxamacker/cbor/v2"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/validate"

	"github.com/fables-for-robots/jobs-iroh/amber"
	"github.com/fables-for-robots/jobs-iroh/amberclient"
	"github.com/fables-for-robots/jobs-iroh/builddef"
	"github.com/fables-for-robots/jobs-iroh/registryd"
	"github.com/fables-for-robots/jobs-iroh/serve"
)

// startServer runs a jobs-server on loopback and returns its handle plus a
// stop func that shuts it down and waits (for the serve-from-cache tests).
func startServer(t *testing.T, ctx context.Context) (*serve.Server, func()) {
	t.Helper()

	ready := make(chan *serve.Server, 1)
	done := make(chan error, 1)
	runCtx, cancel := context.WithCancel(ctx)
	var stopped bool
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("server run: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("server did not shut down")
		}
	}
	t.Cleanup(stop)
	go func() {
		done <- serve.Run(runCtx, serve.Options{
			DataDir:  t.TempDir(),
			BindAddr: netip.AddrPortFrom(netip.IPv6Loopback(), 0),
			Ready:    func(s *serve.Server) { ready <- s },
		})
	}()

	select {
	case srv := <-ready:
		return srv, stop
	case <-time.After(30 * time.Second):
		t.Fatal("server not ready")
		return nil, nil
	}
}

// startRegistry runs registryd against srv and returns its HTTP base host
// (host:port) and data dir.
func startRegistry(t *testing.T, ctx context.Context, srv *serve.Server, mod func(*registryd.Options)) (string, string) {
	t.Helper()

	dataDir := t.TempDir()
	ready := make(chan net.Addr, 1)
	o := registryd.Options{
		ServerID: srv.Endpoint.ID().String(),
		Addrs:    []string{srv.Endpoint.LocalAddr().String()},
		DataDir:  dataDir,
		Listen:   "127.0.0.1:0",
		BindAddr: netip.AddrPortFrom(netip.IPv6Loopback(), 0),
		Ready:    func(a net.Addr) { ready <- a },
	}
	if mod != nil {
		mod(&o)
	}

	done := make(chan error, 1)
	runCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("registry run: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("registry did not shut down")
		}
	})
	go func() { done <- registryd.Run(runCtx, o) }()

	select {
	case addr := <-ready:
		return addr.String(), dataDir
	case <-time.After(30 * time.Second):
		t.Fatal("registry not ready")
		return "", ""
	}
}

// fixtureStore opens a scratch local store used to push fixtures to the server.
func fixtureStore(t *testing.T) *amber.Store {
	t.Helper()
	st, err := amber.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func dialAmber(t *testing.T, ctx context.Context, srv *serve.Server) *amberclient.Client {
	t.Helper()
	c, err := amberclient.Dial(ctx, amberclient.Options{
		EndpointID: srv.Endpoint.ID().String(),
		Addrs:      []string{srv.Endpoint.LocalAddr().String()},
		ALPN:       "jobs-amber-admin/1.0",
		BindAddr:   netip.AddrPortFrom(netip.IPv6Loopback(), 0),
	})
	if err != nil {
		t.Fatalf("dial amber: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func ingestDir(t *testing.T, ctx context.Context, st *amber.Store, files map[string]string) key.Key {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	k, err := st.IngestDir(ctx, dir)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	return k
}

// pushServerBuild pushes a complete server-style build fixture: build:K (the
// definition, for the platform), build-from:K → F, build-output:F with
// c/{bin/app, JOBS.entrypoint}, and build-output-deps:F with one dep. dir
// varies the definition (and so K) between fixtures. Returns K and the dep
// BOK.
func pushServerBuild(t *testing.T, ctx context.Context, st *amber.Store, ac *amberclient.Client, dir string) (key.Key, key.Key) {
	t.Helper()

	def := builddef.Definition{
		Source:   builddef.Input{Kind: builddef.KindImport, Definition: cbor.RawMessage{0xf6}},
		Dir:      dir,
		Platform: "linux/arm64",
		Params:   cbor.RawMessage{0xf6},
	}
	canon, err := def.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	k, err := st.IngestFile(ctx, canon)
	if err != nil {
		t.Fatal(err)
	}
	if wantK, err := def.Key(); err != nil || wantK != k {
		t.Fatalf("ingested def key %s != def.Key() %v (%v)", k, wantK, err)
	}
	if err := ac.Push(ctx, st, "build:"+k.String(), k); err != nil {
		t.Fatal(err)
	}

	f := ingestDir(t, ctx, st, map[string]string{"platform": "linux/arm64"})
	if err := ac.Push(ctx, st, "build-from:"+k.String(), f); err != nil {
		t.Fatal(err)
	}

	out := ingestDir(t, ctx, st, map[string]string{
		"c/bin/app":         "#!/bin/sh\necho hi\n",
		"c/JOBS.entrypoint": `{"command":"bin/app","args":["x"],"env":{"LOG":"info"}}`,
	})
	if err := ac.Push(ctx, st, "build-output:"+f.String(), out); err != nil {
		t.Fatal(err)
	}

	depBOK := ingestDir(t, ctx, st, map[string]string{"lib/libfoo": "FOO"})
	depsTree, err := st.BuildStoreTree(ctx, []key.Key{depBOK})
	if err != nil {
		t.Fatal(err)
	}
	if err := ac.Push(ctx, st, "build-output-deps:"+f.String(), depsTree); err != nil {
		t.Fatal(err)
	}
	return k, depBOK
}

func pullImage(t *testing.T, ctx context.Context, host string, k key.Key) v1.Image {
	t.Helper()
	ref, err := name.ParseReference(fmt.Sprintf("%s/jobs:%s", host, k.String()), name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	img, err := remote.Image(ref, remote.WithContext(ctx))
	if err != nil {
		t.Fatalf("remote.Image: %v", err)
	}
	return img
}

type fsEntry struct {
	hdr     *tar.Header
	content string
}

// extractFS flattens the image's layers and returns path → entry.
func extractFS(t *testing.T, img v1.Image) map[string]fsEntry {
	t.Helper()
	rc := mutate.Extract(img)
	defer rc.Close()
	tr := tar.NewReader(rc)
	out := map[string]fsEntry{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read flattened fs: %v", err)
		}
		b, _ := io.ReadAll(tr)
		out[strings.TrimPrefix(h.Name, "./")] = fsEntry{hdr: h, content: string(b)}
	}
	return out
}

func TestRegistryServesServerBuild(t *testing.T) {
	ctx := context.Background()
	srv, stopServer := startServer(t, ctx)
	st := fixtureStore(t)
	ac := dialAmber(t, ctx, srv)
	k, depBOK := pushServerBuild(t, ctx, st, ac, "")
	host, dataDir := startRegistry(t, ctx, srv, nil)

	// Ping.
	resp, err := http.Get("http://" + host + "/v2/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/v2/ status = %d", resp.StatusCode)
	}

	// A real registry client pulls the image; validate digest coherence.
	img := pullImage(t, ctx, host, k)
	if err := validate.Image(img); err != nil {
		t.Fatalf("validate.Image: %v", err)
	}

	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	// Platform comes from the pushed definition, not the registry host.
	if cf.OS != "linux" || cf.Architecture != "arm64" {
		t.Errorf("os/arch = %q/%q, want linux/arm64 (from build:K definition)", cf.OS, cf.Architecture)
	}
	if got := cf.Config.Entrypoint; len(got) != 2 || got[0] != "/bin/app" || got[1] != "x" {
		t.Errorf("Entrypoint = %v, want [/bin/app x]", got)
	}

	layers, err := img.Layers()
	if err != nil {
		t.Fatal(err)
	}
	if len(layers) != 2 {
		t.Fatalf("layer count = %d, want 2 (deps + build)", len(layers))
	}

	fs := extractFS(t, img)
	if e, ok := fs["bin/app"]; !ok || e.content != "#!/bin/sh\necho hi\n" {
		t.Errorf("artifact not at image root: %+v", e)
	}
	if _, ok := fs["jobs/store/"+depBOK.String()+"/lib/libfoo"]; !ok {
		t.Errorf("dep not under /jobs/store")
	}

	// HEAD manifest by tag — how docker resolves a tag before pulling.
	manDigest, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	headResp := mustHead(t, "http://"+host+"/v2/jobs/manifests/"+k.String())
	if headResp.StatusCode != http.StatusOK {
		t.Errorf("HEAD manifest status = %d", headResp.StatusCode)
	}
	if got := headResp.Header.Get("Docker-Content-Digest"); got != manDigest.String() {
		t.Errorf("HEAD manifest Docker-Content-Digest = %q, want %q", got, manDigest.String())
	}
	if headResp.ContentLength <= 0 {
		t.Errorf("HEAD manifest Content-Length = %d, want > 0", headResp.ContentLength)
	}

	// GET manifest by digest — containerd's actual pull path.
	digResp, err := http.Get("http://" + host + "/v2/jobs/manifests/" + manDigest.String())
	if err != nil {
		t.Fatal(err)
	}
	digBody, _ := io.ReadAll(digResp.Body)
	digResp.Body.Close()
	if digResp.StatusCode != http.StatusOK {
		t.Errorf("GET manifest by digest status = %d", digResp.StatusCode)
	}
	if ct := digResp.Header.Get("Content-Type"); ct != "application/vnd.oci.image.manifest.v1+json" {
		t.Errorf("manifest by digest Content-Type = %q", ct)
	}
	if len(digBody) == 0 {
		t.Error("manifest by digest returned an empty body")
	}

	// A LAYER digest is not a manifest: the manifests route must 404 it
	// (it is a multi-GB stream for real images — the blobs route serves it).
	layerDigest, err := layers[0].Digest()
	if err != nil {
		t.Fatal(err)
	}
	assertOCIError(t, "http://"+host+"/v2/jobs/manifests/"+layerDigest.String(),
		http.StatusNotFound, "MANIFEST_UNKNOWN")

	// HEAD blob.
	blobHead := mustHead(t, "http://"+host+"/v2/jobs/blobs/"+layerDigest.String())
	if blobHead.StatusCode != http.StatusOK || blobHead.ContentLength <= 0 {
		t.Errorf("HEAD blob status = %d, length = %d", blobHead.StatusCode, blobHead.ContentLength)
	}

	// tags/list knows the build; the catalog is the one "jobs" repository.
	var tags struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	getJSON(t, "http://"+host+"/v2/jobs/tags/list", &tags)
	if tags.Name != "jobs" || !contains(tags.Tags, k.String()) {
		t.Errorf("tags/list = %+v, want name jobs and tag %s", tags, k.String())
	}
	var cat struct {
		Repositories []string `json:"repositories"`
	}
	getJSON(t, "http://"+host+"/v2/_catalog", &cat)
	if len(cat.Repositories) != 1 || cat.Repositories[0] != "jobs" {
		t.Errorf("catalog = %v, want [jobs]", cat.Repositories)
	}

	// The server can now go away: the image serves from the blob cache…
	stopServer()
	img2 := pullImage(t, ctx, host, k)
	if err := validate.Image(img2); err != nil {
		t.Fatalf("validate.Image from cache: %v", err)
	}

	// …and even with every cached blob deleted, the registry reassembles
	// offline from its own amber store (record keeps K→F).
	blobDir := filepath.Join(dataDir, "blobs", "sha256")
	ents, err := os.ReadDir(blobDir)
	if err != nil || len(ents) == 0 {
		t.Fatalf("expected cached blobs in %s (err=%v)", blobDir, err)
	}
	for _, e := range ents {
		if err := os.Remove(filepath.Join(blobDir, e.Name())); err != nil {
			t.Fatal(err)
		}
	}
	img3 := pullImage(t, ctx, host, k)
	if err := validate.Image(img3); err != nil {
		t.Fatalf("validate.Image after blob wipe: %v", err)
	}
	d1, _ := img.Digest()
	d3, _ := img3.Digest()
	if d1 != d3 {
		t.Errorf("reassembled image digest %s != original %s", d3, d1)
	}
}

func TestRegistryDirectOutputKey(t *testing.T) {
	ctx := context.Background()
	srv, _ := startServer(t, ctx)
	st := fixtureStore(t)
	ac := dialAmber(t, ctx, srv)

	// A local-build-style push: build-output:<F> directly, no bridge, no
	// definition, no JOBS.entrypoint, no deps ref.
	out := ingestDir(t, ctx, st, map[string]string{"c/bin/tool": "TOOL"})
	f := ingestDir(t, ctx, st, map[string]string{"seed": "s"})
	if err := ac.Push(ctx, st, "build-output:"+f.String(), out); err != nil {
		t.Fatal(err)
	}

	host, _ := startRegistry(t, ctx, srv, func(o *registryd.Options) {
		o.DefaultPlatform = "linux/riscv64"
	})

	img := pullImage(t, ctx, host, f)
	if err := validate.Image(img); err != nil {
		t.Fatalf("validate.Image: %v", err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if cf.OS != "linux" || cf.Architecture != "riscv64" {
		t.Errorf("os/arch = %q/%q, want the configured default linux/riscv64", cf.OS, cf.Architecture)
	}
	if len(cf.Config.Entrypoint) != 0 {
		t.Errorf("Entrypoint = %v, want none for an output without JOBS.entrypoint", cf.Config.Entrypoint)
	}
	fs := extractFS(t, img)
	if e, ok := fs["bin/tool"]; !ok || e.content != "TOOL" {
		t.Errorf("artifact not at image root: %+v", e)
	}
}

func TestRegistryErrors(t *testing.T) {
	ctx := context.Background()
	srv, _ := startServer(t, ctx)
	host, _ := startRegistry(t, ctx, srv, nil)
	st := fixtureStore(t)

	// A valid key the server has never seen.
	unknown := ingestDir(t, ctx, st, map[string]string{"x": "y"})
	assertOCIError(t, "http://"+host+"/v2/jobs/manifests/"+unknown.String(),
		http.StatusNotFound, "MANIFEST_UNKNOWN")

	// Only the "jobs" repository exists.
	assertOCIError(t, "http://"+host+"/v2/other/manifests/"+unknown.String(),
		http.StatusNotFound, "NAME_UNKNOWN")

	// Tags must be build keys.
	assertOCIError(t, "http://"+host+"/v2/jobs/manifests/latest",
		http.StatusNotFound, "MANIFEST_UNKNOWN")

	// Push is refused.
	req, _ := http.NewRequest(http.MethodPut, "http://"+host+"/v2/jobs/manifests/"+unknown.String(), strings.NewReader("{}"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("PUT manifest status = %d, want 405", resp.StatusCode)
	}
}

// countingHandler counts log records by message, preserving counting through
// With-derived loggers.
type countingHandler struct {
	slog.Handler
	mu     *sync.Mutex
	counts map[string]int
}

func (h countingHandler) Handle(ctx context.Context, rec slog.Record) error {
	h.mu.Lock()
	h.counts[rec.Message]++
	h.mu.Unlock()
	return h.Handler.Handle(ctx, rec)
}

func (h countingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return countingHandler{h.Handler.WithAttrs(attrs), h.mu, h.counts}
}

func (h countingHandler) WithGroup(name string) slog.Handler {
	return countingHandler{h.Handler.WithGroup(name), h.mu, h.counts}
}

// TestRegistryConcurrentPullsAssembleOnce: concurrent pulls of the same K
// must singleflight into exactly one assembly (and later pulls hit the
// record), never one per client.
func TestRegistryConcurrentPullsAssembleOnce(t *testing.T) {
	ctx := context.Background()
	srv, _ := startServer(t, ctx)
	st := fixtureStore(t)
	ac := dialAmber(t, ctx, srv)
	k, _ := pushServerBuild(t, ctx, st, ac, "concurrent")

	var mu sync.Mutex
	counts := map[string]int{}
	logger := slog.New(countingHandler{
		Handler: slog.NewTextHandler(io.Discard, nil),
		mu:      &mu,
		counts:  counts,
	})
	host, _ := startRegistry(t, ctx, srv, func(o *registryd.Options) { o.Logger = logger })

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			img := pullImage(t, ctx, host, k)
			if err := validate.Image(img); err != nil {
				t.Errorf("validate.Image: %v", err)
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	assembled := counts["image assembled"]
	mu.Unlock()
	if assembled != 1 {
		t.Errorf("assembled %d times for 4 concurrent pulls, want 1", assembled)
	}
}

// TestRegistryTagsPagination: n/last paging over the tag list, with the Link
// header pointing at the next page.
func TestRegistryTagsPagination(t *testing.T) {
	ctx := context.Background()
	srv, _ := startServer(t, ctx)
	st := fixtureStore(t)
	ac := dialAmber(t, ctx, srv)
	kA, _ := pushServerBuild(t, ctx, st, ac, "a")
	kB, _ := pushServerBuild(t, ctx, st, ac, "b")
	if kA == kB {
		t.Fatal("fixture keys must differ")
	}
	host, _ := startRegistry(t, ctx, srv, nil)

	var full struct {
		Tags []string `json:"tags"`
	}
	getJSON(t, "http://"+host+"/v2/jobs/tags/list", &full)
	if !contains(full.Tags, kA.String()) || !contains(full.Tags, kB.String()) {
		t.Fatalf("tags = %v, want both %s and %s", full.Tags, kA.String(), kB.String())
	}

	resp, err := http.Get("http://" + host + "/v2/jobs/tags/list?n=1")
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(page.Tags) != 1 || page.Tags[0] != full.Tags[0] {
		t.Errorf("page 1 = %v, want [%s]", page.Tags, full.Tags[0])
	}
	link := resp.Header.Get("Link")
	wantLink := "</v2/jobs/tags/list?n=1&last=" + full.Tags[0] + ">; rel=\"next\""
	if link != wantLink {
		t.Errorf("Link = %q, want %q", link, wantLink)
	}

	var page2 struct {
		Tags []string `json:"tags"`
	}
	getJSON(t, "http://"+host+"/v2/jobs/tags/list?n=1&last="+full.Tags[0], &page2)
	if len(page2.Tags) < 1 || page2.Tags[0] != full.Tags[1] {
		t.Errorf("page 2 = %v, want first element %s", page2.Tags, full.Tags[1])
	}
}

func mustHead(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.Head(url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}

func assertOCIError(t *testing.T, url string, wantStatus int, wantCode string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Errorf("GET %s status = %d, want %d", url, resp.StatusCode, wantStatus)
	}
	var body struct {
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body of %s: %v", url, err)
	}
	if len(body.Errors) != 1 || body.Errors[0].Code != wantCode {
		t.Errorf("GET %s error body = %+v, want code %s", url, body, wantCode)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
