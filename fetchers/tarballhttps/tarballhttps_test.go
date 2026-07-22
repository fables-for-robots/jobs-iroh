package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

type tarEntry struct {
	name, body, link string
	typ              byte
	mode             int64
}

// gzTar builds a synthetic .tar.gz from the given entries.
func gzTar(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: e.mode, Linkname: e.link}
		if e.typ == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestExtractTarGzNodeLayout mirrors the musl Node tarball: a single versioned
// top dir, strip=1, and — the reason this fetcher exists — a relative bin/npm
// symlink whose leading "../" must be preserved (busybox tar strips it).
func TestExtractTarGzNodeLayout(t *testing.T) {
	data := gzTar(t, []tarEntry{
		{name: "node-x/", typ: tar.TypeDir, mode: 0o755},
		{name: "node-x/bin/", typ: tar.TypeDir, mode: 0o755},
		{name: "node-x/bin/node", body: "ELF", typ: tar.TypeReg, mode: 0o755},
		{name: "node-x/bin/npm", link: "../lib/node_modules/npm/bin/npm-cli.js", typ: tar.TypeSymlink, mode: 0o777},
		{name: "node-x/lib/node_modules/npm/bin/npm-cli.js", body: "#!/usr/bin/env node", typ: tar.TypeReg, mode: 0o755},
	})
	out := t.TempDir()
	if err := extractTarGz(bytes.NewReader(data), out, 1); err != nil {
		t.Fatalf("extractTarGz: %v", err)
	}
	target, err := os.Readlink(filepath.Join(out, "bin/npm"))
	if err != nil {
		t.Fatalf("readlink bin/npm: %v", err)
	}
	if target != "../lib/node_modules/npm/bin/npm-cli.js" {
		t.Fatalf("npm symlink target = %q, want it to keep the leading ../", target)
	}
	if _, err := os.Stat(filepath.Join(out, "bin/npm")); err != nil {
		t.Fatalf("bin/npm dangles: %v", err)
	}
}

// TestExtractTarGzRejectsTraversal: an entry escaping outDir (after strip) errors.
func TestExtractTarGzRejectsTraversal(t *testing.T) {
	data := gzTar(t, []tarEntry{{name: "x/../../evil", body: "x", typ: tar.TypeReg, mode: 0o644}})
	if err := extractTarGz(bytes.NewReader(data), t.TempDir(), 1); err == nil {
		t.Fatal("expected traversal error, got nil")
	}
}

// TestRunStreamsLargePayload proves the fetcher does not buffer the download in
// memory (large toolchain tarballs must not balloon the import's RSS): fetching
// a ~48MiB incompressible tarball must allocate far less than the payload.
// TotalAlloc is monotonic, so the bound is GC-independent. It also asserts no
// temp-file residue pollutes the output tree.
func TestRunStreamsLargePayload(t *testing.T) {
	big := make([]byte, 48<<20)
	rnd := rand.New(rand.NewSource(1))
	rnd.Read(big)
	body := gzTar(t, []tarEntry{{name: "d/blob", body: string(big), typ: tar.TypeReg, mode: 0o644}})
	sum := sha256.Sum256(body)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	outDir := t.TempDir()
	params, _ := json.Marshal(map[string]any{"url": srv.URL, "sha256": hex.EncodeToString(sum[:])})
	getenv := func(k string) string {
		switch k {
		case "JOBS_OUTPUT_DIR":
			return outDir
		case "JOBS_FETCH_PARAMS":
			return string(params)
		}
		return ""
	}

	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	code := run(getenv, os.Stderr)
	runtime.ReadMemStats(&m1)
	if code != exitOK {
		t.Fatalf("run = %d, want %d", code, exitOK)
	}
	if alloc := m1.TotalAlloc - m0.TotalAlloc; alloc > 16<<20 {
		t.Fatalf("run allocated %d MiB for a %d MiB payload — download is buffered in memory", alloc>>20, len(body)>>20)
	}

	got, err := os.ReadFile(filepath.Join(outDir, "d", "blob"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, big) {
		t.Fatal("extracted content differs")
	}
	ents, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != "d" {
		t.Fatalf("output dir polluted: %v", ents)
	}
}
