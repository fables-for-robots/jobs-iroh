package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	mrand "math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func makeCrate(t *testing.T, root string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		full := root + "/" + name
		if err := tw.WriteHeader(&tar.Header{Name: full, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractCrate(t *testing.T) {
	root := "demo-1.0.0"
	data := makeCrate(t, root, map[string]string{
		"Cargo.toml": "[package]\nname = \"demo\"\n",
		"src/lib.rs": "// hi\n",
	})
	sum := sha256.Sum256(data)
	checksum := hex.EncodeToString(sum[:])

	out := t.TempDir()
	p := params{Name: "demo", Version: "1.0.0", Checksum: checksum}
	if err := extractCrate(bytes.NewReader(data), p, out); err != nil {
		t.Fatalf("extractCrate: %v", err)
	}

	if _, err := os.Stat(filepath.Join(out, root, "src/lib.rs")); err != nil {
		t.Errorf("src/lib.rs not extracted: %v", err)
	}

	b, err := os.ReadFile(filepath.Join(out, root, ".cargo-checksum.json"))
	if err != nil {
		t.Fatalf("read .cargo-checksum.json: %v", err)
	}
	var cc struct {
		Files   map[string]string `json:"files"`
		Package string            `json:"package"`
	}
	if err := json.Unmarshal(b, &cc); err != nil {
		t.Fatal(err)
	}
	if cc.Package != checksum {
		t.Errorf("package = %q, want %q", cc.Package, checksum)
	}
	wantLib := sha256.Sum256([]byte("// hi\n"))
	if cc.Files["src/lib.rs"] != hex.EncodeToString(wantLib[:]) {
		t.Errorf("files[src/lib.rs] = %q, want %q", cc.Files["src/lib.rs"], hex.EncodeToString(wantLib[:]))
	}
}

// TestRunRejectsChecksumMismatch: the sha256 check happens in run() before
// extraction; a corrupt download must fail hard and leave nothing behind.
func TestRunRejectsChecksumMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("corrupt"))
	}))
	defer srv.Close()
	oldBase := cratesBase
	cratesBase = srv.URL
	defer func() { cratesBase = oldBase }()

	outDir := t.TempDir()
	pj, _ := json.Marshal(map[string]string{
		"name": "demo", "version": "1.0.0",
		"checksum": "0000000000000000000000000000000000000000000000000000000000000000",
	})
	getenv := func(k string) string {
		switch k {
		case "JOBS_OUTPUT_DIR":
			return outDir
		case "JOBS_FETCH_PARAMS":
			return string(pj)
		}
		return ""
	}
	if code := run(getenv, io.Discard); code != exitHard {
		t.Fatalf("run = %d, want %d", code, exitHard)
	}
	ents, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Fatalf("output dir polluted after mismatch: %v", ents)
	}
}

// TestRunStreamsLargePayload proves the fetcher does not buffer the .crate in
// memory: fetching a ~32MiB crate must allocate far less than the payload.
// TotalAlloc is monotonic, so the bound is GC-independent. Also asserts no
// temp-file residue pollutes the output tree.
func TestRunStreamsLargePayload(t *testing.T) {
	big := make([]byte, 32<<20)
	rnd := mrand.New(mrand.NewSource(1))
	rnd.Read(big)
	body := makeCrate(t, "big-1.0.0", map[string]string{"src/blob.rs": string(big)})
	sum := sha256.Sum256(body)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()
	oldBase := cratesBase
	cratesBase = srv.URL
	defer func() { cratesBase = oldBase }()

	outDir := t.TempDir()
	params, _ := json.Marshal(map[string]string{
		"name": "big", "version": "1.0.0", "checksum": hex.EncodeToString(sum[:]),
	})
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

	got, err := os.ReadFile(filepath.Join(outDir, "big-1.0.0", "src", "blob.rs"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, big) {
		t.Fatal("extracted content differs")
	}
	if _, err := os.Stat(filepath.Join(outDir, "big-1.0.0", ".cargo-checksum.json")); err != nil {
		t.Fatalf(".cargo-checksum.json missing: %v", err)
	}
	ents, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != "big-1.0.0" {
		t.Fatalf("output dir polluted: %v", ents)
	}
}
