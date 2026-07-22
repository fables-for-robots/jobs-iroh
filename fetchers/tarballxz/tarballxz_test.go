package main

import (
	"archive/tar"
	"bytes"
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

	"github.com/ulikunitz/xz"
)

type tarEntry struct {
	name, body, link string
	typ              byte
	mode             int64
}

// xzTar builds a synthetic .tar.xz from the given entries.
func xzTar(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	xw, err := xz.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(xw)
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
	if err := xw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestExtractTarXzStripsTopDir mirrors the zig tarball layout (a single versioned
// top dir) and asserts strip=1 lands the contents at outDir with modes preserved.
func TestExtractTarXzStripsTopDir(t *testing.T) {
	data := xzTar(t, []tarEntry{
		{name: "zig-x/", typ: tar.TypeDir, mode: 0o755},
		{name: "zig-x/zig", body: "ELF", typ: tar.TypeReg, mode: 0o755},
		{name: "zig-x/lib/", typ: tar.TypeDir, mode: 0o755},
		{name: "zig-x/lib/std.zig", body: "pub", typ: tar.TypeReg, mode: 0o644},
	})
	out := t.TempDir()
	if err := extractTarXz(bytes.NewReader(data), out, 1); err != nil {
		t.Fatalf("extractTarXz: %v", err)
	}
	fi, err := os.Stat(filepath.Join(out, "zig"))
	if err != nil {
		t.Fatalf("zig: %v", err)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("zig not executable: %v", fi.Mode())
	}
	if b, _ := os.ReadFile(filepath.Join(out, "lib/std.zig")); string(b) != "pub" {
		t.Errorf("lib/std.zig = %q, want pub", b)
	}
}

// TestExtractTarXzRejectsTraversal: an entry escaping outDir (after strip) errors.
func TestExtractTarXzRejectsTraversal(t *testing.T) {
	data := xzTar(t, []tarEntry{{name: "x/../../evil", body: "x", typ: tar.TypeReg, mode: 0o644}})
	if err := extractTarXz(bytes.NewReader(data), t.TempDir(), 1); err == nil {
		t.Fatal("expected traversal error, got nil")
	}
}

// TestRunStreamsLargePayload proves the fetcher does not buffer the download in
// memory (rust toolchain tarballs run to hundreds of MB): fetching a ~24MiB
// incompressible tarball must allocate far less than the payload. TotalAlloc is
// monotonic, so the bound is GC-independent. Also asserts no temp-file residue.
func TestRunStreamsLargePayload(t *testing.T) {
	big := make([]byte, 24<<20)
	rnd := rand.New(rand.NewSource(1))
	rnd.Read(big)
	body := xzTar(t, []tarEntry{{name: "d/blob", body: string(big), typ: tar.TypeReg, mode: 0o644}})
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
	// The xz decoder allocates a fixed ~20MiB (dictionary + lzma2 buffers)
	// regardless of payload size; buffering the download measured 72MiB here.
	// The bound sits between the two: it trips on any payload-proportional
	// buffering while tolerating the decoder's fixed cost.
	if alloc := m1.TotalAlloc - m0.TotalAlloc; alloc > 32<<20 {
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
