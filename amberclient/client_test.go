package amberclient_test

// Loopback e2e: a full jobs-server (serve.Run — embedded NATS, embedded
// amber store, the real ALPN mounts) on one side, amberclient on the other.
// Everything stays on the local machine: direct addresses, no discovery, no
// relays.

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"math/rand"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/fables-for-robots/jobs-iroh/amber"
	"github.com/fables-for-robots/jobs-iroh/amberclient"
	"github.com/fables-for-robots/jobs-iroh/serve"
)

// startServer runs a jobs-server on loopback and returns its handle.
func startServer(t *testing.T, ctx context.Context) *serve.Server {
	t.Helper()

	ready := make(chan *serve.Server, 1)
	done := make(chan error, 1)
	runCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("server run: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("server did not shut down")
		}
	})
	go func() {
		done <- serve.Run(runCtx, serve.Options{
			DataDir:  t.TempDir(),
			BindAddr: netip.AddrPortFrom(netip.IPv6Loopback(), 0),
			Ready:    func(s *serve.Server) { ready <- s },
		})
	}()

	select {
	case srv := <-ready:
		return srv
	case <-time.After(30 * time.Second):
		t.Fatal("server not ready")
		return nil
	}
}

// dialClient dials the server's amber mount at alpn over its direct
// loopback address.
func dialClient(t *testing.T, ctx context.Context, srv *serve.Server, alpn string) *amberclient.Client {
	t.Helper()
	c, err := amberclient.Dial(ctx, amberclient.Options{
		EndpointID: srv.Endpoint.ID().String(),
		Addrs:      []string{srv.Endpoint.LocalAddr().String()},
		ALPN:       alpn,
		BindAddr:   netip.AddrPortFrom(netip.IPv6Loopback(), 0),
	})
	if err != nil {
		t.Fatalf("dial %s: %v", alpn, err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Logf("client close: %v", err)
		}
	})
	return c
}

func openStore(t *testing.T) *amber.Store {
	t.Helper()
	st, err := amber.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// layeredSource builds a tree deep enough for a multi-round want loop:
// round 1 asks for the root directory, round 2 for its children, round 3
// for sub's children.
func layeredSource(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for p, content := range map[string]string{
		"a.txt":     "alpha top level",
		"b.txt":     "bravo top level",
		"sub/c.txt": "charlie nested",
		"sub/d.txt": "delta nested",
	} {
		if err := os.WriteFile(filepath.Join(src, p), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return src
}

// requireSameFiles walks src and asserts every regular file exists in dest
// with identical bytes (materialized equality).
func requireSameFiles(t *testing.T, src, dest string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		want, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		got, err := os.ReadFile(filepath.Join(dest, rel))
		if err != nil {
			t.Errorf("materialized tree missing %s: %v", rel, err)
			return nil
		}
		if !bytes.Equal(want, got) {
			t.Errorf("materialized %s differs: %d bytes, want %d", rel, len(got), len(want))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPushPullRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	srv := startServer(t, ctx)
	client := dialClient(t, ctx, srv, serve.ALPNAmberAdmin)

	src := layeredSource(t)
	storeA := openStore(t)
	rootA, err := storeA.IngestDir(ctx, src)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}

	const name = "e2e/tree"
	if err := client.Push(ctx, storeA, name, rootA); err != nil {
		t.Fatalf("push: %v", err)
	}

	// The remote listing sees the ref at the pushed root.
	refs, err := client.Refs(ctx)
	if err != nil {
		t.Fatalf("refs: %v", err)
	}
	found := false
	for _, r := range refs {
		if r.Name == name {
			found = true
			if r.Key != rootA {
				t.Fatalf("remote ref at %s, want %s", r.Key, rootA)
			}
		}
	}
	if !found {
		t.Fatalf("ref %q not in remote listing (%d refs)", name, len(refs))
	}

	// Pull into a fresh store: same root, local plain ref written.
	storeB := openStore(t)
	got, err := client.Pull(ctx, storeB, name)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if got != rootA {
		t.Fatalf("pulled root %s, want %s", got, rootA)
	}
	k, ok, err := storeB.GetKey(ctx, name)
	if err != nil || !ok {
		t.Fatalf("local ref after pull: ok=%v err=%v", ok, err)
	}
	if k != rootA {
		t.Fatalf("local ref at %s, want %s", k, rootA)
	}

	// Materialized equality, and the restored tree re-ingests to the same
	// root — byte- and identity-level round-trip.
	dest := filepath.Join(t.TempDir(), "restored")
	if err := storeB.Materialize(ctx, got, dest); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	requireSameFiles(t, src, dest)
	reroot, err := storeB.IngestDir(ctx, dest)
	if err != nil {
		t.Fatalf("re-ingest: %v", err)
	}
	if reroot != rootA {
		t.Fatalf("restored tree re-ingests to %s, want %s", reroot, rootA)
	}

	// Force-mode second push under the same name: no CAS, the remote ref
	// just moves.
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("alpha changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	root2, err := storeA.IngestDir(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if root2 == rootA {
		t.Fatal("modified tree must have a new root")
	}
	if err := client.Push(ctx, storeA, name, root2); err != nil {
		t.Fatalf("force re-push: %v", err)
	}
	refs, err = client.Refs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range refs {
		if r.Name == name && r.Key != root2 {
			t.Fatalf("after re-push remote ref at %s, want %s", r.Key, root2)
		}
	}
}

func TestPullAbsentRef(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	srv := startServer(t, ctx)
	client := dialClient(t, ctx, srv, serve.ALPNAmberAdmin)
	storeB := openStore(t)

	_, err := client.Pull(ctx, storeB, "definitely/absent")
	if !errors.Is(err, amberclient.ErrRefNotFound) {
		t.Fatalf("pull of absent ref: want ErrRefNotFound, got %v", err)
	}
	// No local ref must appear for a failed pull.
	if _, ok, err := storeB.GetKey(ctx, "definitely/absent"); err != nil || ok {
		t.Fatalf("local ref after failed pull: ok=%v err=%v", ok, err)
	}
}

// TestBigTreeRoundTrip pushes a multi-megabyte, multi-chunk tree (files well
// past the 256 KiB max chunk size, so their item trees span many leaf
// chunks and the want loop runs several rounds) through the runner ALPN and
// pulls it back byte-identically.
func TestBigTreeRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	srv := startServer(t, ctx)
	client := dialClient(t, ctx, srv, serve.ALPNRunnerAmber)

	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "nested", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(1))
	writeRandom := func(rel string, n int) {
		buf := make([]byte, n)
		rng.Read(buf)
		if err := os.WriteFile(filepath.Join(src, rel), buf, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeRandom("big.bin", 3<<20)
	writeRandom("nested/mid.bin", 1<<20)
	if err := os.WriteFile(filepath.Join(src, "nested", "deep", "small.txt"), []byte("tiny"), 0o644); err != nil {
		t.Fatal(err)
	}

	storeA := openStore(t)
	root, err := storeA.IngestDir(ctx, src)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}

	const name = "runner-push/test/big-0"
	if err := client.Push(ctx, storeA, name, root); err != nil {
		t.Fatalf("push: %v", err)
	}

	storeB := openStore(t)
	got, err := client.Pull(ctx, storeB, name)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if got != root {
		t.Fatalf("pulled root %s, want %s", got, root)
	}

	dest := filepath.Join(t.TempDir(), "restored")
	if err := storeB.Materialize(ctx, got, dest); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	requireSameFiles(t, src, dest)
	reroot, err := storeB.IngestDir(ctx, dest)
	if err != nil {
		t.Fatalf("re-ingest: %v", err)
	}
	if reroot != root {
		t.Fatalf("restored tree re-ingests to %s, want %s", reroot, root)
	}
}

// TestPushMissingRoot pins the fail-fast: pushing a root the local store
// does not hold must error before any transfer, not mid-loop.
func TestPushMissingRoot(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	srv := startServer(t, ctx)
	client := dialClient(t, ctx, srv, serve.ALPNAmberAdmin)

	storeA := openStore(t)
	bogus, err := storeA.IngestFile(ctx, []byte("present"))
	if err != nil {
		t.Fatal(err)
	}
	storeEmpty := openStore(t)
	if err := client.Push(ctx, storeEmpty, "x", bogus); err == nil {
		t.Fatal("push of a locally missing root must fail")
	}
}

// TestPushPullProgress pins the observer seam: callbacks report monotonic
// done counts converging on the total, the returned stats match the final
// callback, and an already-synced transfer reports 0/0 without callbacks.
func TestPushPullProgress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	srv := startServer(t, ctx)
	client := dialClient(t, ctx, srv, serve.ALPNAmberAdmin)

	src := layeredSource(t)
	storeA := openStore(t)
	root, err := storeA.IngestDir(ctx, src)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}

	type sample struct{ done, total int }
	var mu sync.Mutex
	var pushSamples []sample
	record := func(dst *[]sample) amberclient.ProgressFunc {
		return func(done, total int) {
			mu.Lock()
			*dst = append(*dst, sample{done, total})
			mu.Unlock()
		}
	}

	const name = "e2e/progress"
	pushStats, err := client.PushWithProgress(ctx, storeA, name, root, record(&pushSamples))
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if pushStats.Objects == 0 || pushStats.Bytes == 0 {
		t.Fatalf("push stats empty: %+v", pushStats)
	}
	mu.Lock()
	if len(pushSamples) == 0 {
		mu.Unlock()
		t.Fatal("push progress callback never fired")
	}
	lastDone := -1
	for _, s := range pushSamples {
		if s.done < lastDone {
			t.Fatalf("done went backwards: %+v", pushSamples)
		}
		lastDone = s.done
	}
	final := pushSamples[len(pushSamples)-1]
	mu.Unlock()
	if final.done != pushStats.Objects || final.total != pushStats.Objects {
		t.Fatalf("final push sample %+v, want done=total=%d", final, pushStats.Objects)
	}

	// Pull into a fresh store: the same closure crosses back.
	var pullSamples []sample
	storeB := openStore(t)
	got, pullStats, err := client.PullWithProgress(ctx, storeB, name, record(&pullSamples))
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if got != root {
		t.Fatalf("pulled root %s, want %s", got, root)
	}
	if pullStats.Objects != pushStats.Objects || pullStats.Bytes != pushStats.Bytes {
		t.Fatalf("pull stats %+v, want the pushed closure %+v", pullStats, pushStats)
	}
	mu.Lock()
	if len(pullSamples) == 0 {
		t.Fatal("pull progress callback never fired")
	}
	mu.Unlock()

	// Re-pull into the same store: nothing is missing, so no objects move
	// and the callback stays silent (total 0 — no wants were exchanged).
	var againSamples []sample
	_, againStats, err := client.PullWithProgress(ctx, storeB, name, record(&againSamples))
	if err != nil {
		t.Fatalf("re-pull: %v", err)
	}
	if againStats.Objects != 0 || againStats.Bytes != 0 {
		t.Fatalf("re-pull moved objects: %+v", againStats)
	}
	mu.Lock()
	for _, s := range againSamples {
		if s.done != 0 {
			t.Fatalf("re-pull reported transfers: %+v", againSamples)
		}
	}
	mu.Unlock()
}

// TestManyOpsOneConnection regression-tests stream retirement: every
// operation must FULLY close its stream (send FIN + receive cancel on both
// peers), or the server stops granting MAX_STREAMS credit and the client
// blocks forever on the 101st open (the QUIC default bidi-stream limit) —
// exactly how a long-lived runner froze mid-build after its first hundred
// pushes/pulls. 120 mixed operations over ONE connection, each under its own
// short deadline, prove credit keeps flowing.
func TestManyOpsOneConnection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	srv := startServer(t, ctx)
	client := dialClient(t, ctx, srv, serve.ALPNRunnerAmber)
	st := openStore(t)

	root, err := st.IngestSourceDir(ctx, layeredSource(t))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := client.Push(ctx, st, "many/seed", root); err != nil {
		t.Fatalf("seed push: %v", err)
	}

	for i := 0; i < 120; i++ {
		opCtx, opDone := context.WithTimeout(ctx, 20*time.Second)
		var err error
		switch i % 3 {
		case 0:
			err = client.Push(opCtx, st, "many/seed", root)
		case 1:
			_, err = client.Pull(opCtx, st, "many/seed")
		default:
			_, err = client.Refs(opCtx)
		}
		opDone()
		if err != nil {
			t.Fatalf("op %d: %v (stream-credit leak: streams are not being fully closed)", i, err)
		}
	}
}
