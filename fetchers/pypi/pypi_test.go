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
	"strings"
	"testing"
)

// makeSdist builds an in-memory .tar.gz with a single top-level directory
// named root (e.g. "pkg-1.0"), containing the given files.
func makeSdist(t *testing.T, root string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	// write top-level directory entry
	if err := tw.WriteHeader(&tar.Header{
		Name:     root + "/",
		Mode:     0o755,
		Typeflag: tar.TypeDir,
	}); err != nil {
		t.Fatal(err)
	}

	for name, content := range files {
		full := root + "/" + name
		// write intermediate dir entries as needed
		if idx := strings.LastIndex(name, "/"); idx >= 0 {
			dir := root + "/" + name[:idx] + "/"
			if err := tw.WriteHeader(&tar.Header{
				Name:     dir,
				Mode:     0o755,
				Typeflag: tar.TypeDir,
			}); err != nil {
				t.Fatal(err)
			}
		}
		if err := tw.WriteHeader(&tar.Header{
			Name:     full,
			Mode:     0o644,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}); err != nil {
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

// deliverBytes stages data into a temp file (as run() does after a verified
// download) and calls deliver.
func deliverBytes(t *testing.T, data []byte, p params, outDir string) error {
	t.Helper()
	tmp, err := os.CreateTemp(outDir, ".fetch-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err := tmp.Write(data); err != nil {
		t.Fatal(err)
	}
	return deliver(tmp, p, outDir)
}

func TestDeliver_Wheel(t *testing.T) {
	data := []byte("PK\x03\x04 fake wheel zip bytes")
	sum := sha256.Sum256(data)
	out := t.TempDir()
	p := params{URL: "x", Filename: "click-8.1.7-py3-none-any.whl", Sha256: hex.EncodeToString(sum[:])}
	if err := deliverBytes(t, data, p, out); err != nil {
		t.Fatalf("verifyAndWrite: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(out, p.Filename))
	if err != nil {
		t.Fatalf("read wheel: %v", err)
	}
	if string(got) != string(data) {
		t.Error("wheel content mismatch")
	}
	// make sure the .whl file is written raw (not extracted)
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != p.Filename {
		t.Errorf("expected only %q in outDir, got %v", p.Filename, entries)
	}
}

func TestDeliver_Sdist(t *testing.T) {
	data := makeSdist(t, "pkg-1.0", map[string]string{
		"setup.py":        "from setuptools import setup\nsetup(name='pkg')\n",
		"pkg/__init__.py": "# pkg\n",
	})
	sum := sha256.Sum256(data)
	out := t.TempDir()
	p := params{
		URL:      "x",
		Filename: "pkg-1.0.tar.gz",
		Sha256:   hex.EncodeToString(sum[:]),
		Name:     "pkg",
		Version:  "1.0",
	}
	if err := deliverBytes(t, data, p, out); err != nil {
		t.Fatalf("verifyAndWrite: %v", err)
	}
	// top-level dir must NOT appear
	if _, err := os.Stat(filepath.Join(out, "pkg-1.0")); !os.IsNotExist(err) {
		t.Errorf("top-level dir pkg-1.0 should not exist (must be stripped)")
	}
	// setup.py at root
	if _, err := os.Stat(filepath.Join(out, "setup.py")); err != nil {
		t.Errorf("setup.py not at output root: %v", err)
	}
	// pkg/__init__.py under pkg/
	if _, err := os.Stat(filepath.Join(out, "pkg", "__init__.py")); err != nil {
		t.Errorf("pkg/__init__.py not extracted: %v", err)
	}
}

// TestRunRejectsMismatch: the sha256 check happens in run() before delivery;
// a corrupt download must fail hard and leave nothing (but the removed temp).
func TestRunRejectsMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("corrupt"))
	}))
	defer srv.Close()
	outDir := t.TempDir()
	pj, _ := json.Marshal(map[string]string{
		"url": srv.URL, "filename": "a.whl",
		"sha256": "0000000000000000000000000000000000000000000000000000000000000000",
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

func TestDeliver_UnsupportedExtension(t *testing.T) {
	data := []byte("fake zip")
	sum := sha256.Sum256(data)
	p := params{URL: "x", Filename: "pkg-1.0.zip", Sha256: hex.EncodeToString(sum[:])}
	if err := deliverBytes(t, data, p, t.TempDir()); err == nil {
		t.Fatal("expected error for unsupported .zip extension, got nil")
	}
}

func TestRunDownloads(t *testing.T) {
	data := []byte("wheel-bytes")
	sum := sha256.Sum256(data)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	out := t.TempDir()
	getenv := func(k string) string {
		switch k {
		case "JOBS_OUTPUT_DIR":
			return out
		case "JOBS_FETCH_PARAMS":
			return `{"url":"` + srv.URL + `","filename":"demo-1.0-py3-none-any.whl","sha256":"` + hex.EncodeToString(sum[:]) + `"}`
		}
		return ""
	}
	if code := run(getenv, os.Stderr); code != exitOK {
		t.Fatalf("run exit = %d, want %d", code, exitOK)
	}
	if _, err := os.Stat(filepath.Join(out, "demo-1.0-py3-none-any.whl")); err != nil {
		t.Errorf("wheel not written: %v", err)
	}
}

func TestRunRejectsUnsafeFilename(t *testing.T) {
	getenv := func(k string) string {
		switch k {
		case "JOBS_OUTPUT_DIR":
			return t.TempDir()
		case "JOBS_FETCH_PARAMS":
			return `{"url":"http://127.0.0.1:0/x","filename":"../evil.whl","sha256":"` + strings.Repeat("a", 64) + `"}`
		}
		return ""
	}
	if code := run(getenv, io.Discard); code != exitHard {
		t.Fatalf("run exit = %d, want %d (unsafe filename must be rejected before download)", code, exitHard)
	}
}

func TestRunRetryableOn5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable) // 503 → retryable
	}))
	defer srv.Close()
	getenv := func(k string) string {
		switch k {
		case "JOBS_OUTPUT_DIR":
			return t.TempDir()
		case "JOBS_FETCH_PARAMS":
			return `{"url":"` + srv.URL + `","filename":"demo-1.0-py3-none-any.whl","sha256":"` + strings.Repeat("a", 64) + `"}`
		}
		return ""
	}
	if code := run(getenv, io.Discard); code != exitRetryable {
		t.Fatalf("run exit = %d, want %d (5xx must be retryable)", code, exitRetryable)
	}
}

// TestRunStreamsLargePayload proves the fetcher does not buffer the wheel in
// memory: fetching a ~32MiB wheel must allocate far less than the payload.
// TotalAlloc is monotonic, so the bound is GC-independent. Also asserts no
// temp-file residue pollutes the output tree.
func TestRunStreamsLargePayload(t *testing.T) {
	body := make([]byte, 32<<20)
	rnd := mrand.New(mrand.NewSource(1))
	rnd.Read(body)
	sum := sha256.Sum256(body)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	outDir := t.TempDir()
	params, _ := json.Marshal(map[string]string{
		"url": srv.URL, "filename": "big-1.0-py3-none-any.whl", "sha256": hex.EncodeToString(sum[:]),
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
	got, err := os.ReadFile(filepath.Join(outDir, "big-1.0-py3-none-any.whl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatal("written content differs")
	}
	ents, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 {
		t.Fatalf("output dir polluted: %v", ents)
	}
}
