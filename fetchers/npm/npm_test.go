package main

import (
	"bytes"
	"crypto/sha512"
	"encoding/base64"
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

func TestVerifyIntegritySha512(t *testing.T) {
	data := []byte("tarball-bytes")
	sum := sha512.Sum512(data)
	good := "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
	if err := verifyIntegrity(sumsOf(data), good); err != nil {
		t.Errorf("matching sha512 integrity should pass: %v", err)
	}
	if err := verifyIntegrity(sumsOf(data), "sha512-"+base64.StdEncoding.EncodeToString(make([]byte, 64))); err == nil {
		t.Error("non-matching sha512 integrity should fail")
	}
	// multiple space-separated digests: any match is ok
	multi := "sha1-AAAA " + good
	if err := verifyIntegrity(sumsOf(data), multi); err != nil {
		t.Errorf("integrity with one matching digest should pass: %v", err)
	}
}

func TestMirrorName(t *testing.T) {
	cases := []struct{ name, version, want string }{
		{"esbuild", "0.25.12", "esbuild-0.25.12.tgz"},
		{"@esbuild/linux-x64", "0.25.12", "@esbuild-linux-x64-0.25.12.tgz"},
		{"turbo", "2.0.0", "turbo-2.0.0.tgz"},
	}
	for _, c := range cases {
		if got := mirrorName(c.name, c.version); got != c.want {
			t.Errorf("mirrorName(%q,%q) = %q, want %q", c.name, c.version, got, c.want)
		}
	}
}

// TestRunStreamsLargePayload proves the fetcher does not buffer the tarball in
// memory: fetching a ~32MiB package must allocate far less than the payload.
// TotalAlloc is monotonic, so the bound is GC-independent.
func TestRunStreamsLargePayload(t *testing.T) {
	body := make([]byte, 32<<20)
	rnd := mrand.New(mrand.NewSource(1))
	rnd.Read(body)
	sum := sha512.Sum512(body)
	integrity := "sha512-" + base64.StdEncoding.EncodeToString(sum[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	outDir := t.TempDir()
	params, _ := json.Marshal(map[string]string{
		"name": "big", "version": "1.0.0", "url": srv.URL, "integrity": integrity,
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
	got, err := os.ReadFile(filepath.Join(outDir, "big-1.0.0.tgz"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatal("written content differs")
	}
}

// TestRunRemovesFileOnIntegrityMismatch: a failed download must not leave a
// partial artifact in the output tree.
func TestRunRemovesFileOnIntegrityMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("corrupt"))
	}))
	defer srv.Close()
	outDir := t.TempDir()
	params, _ := json.Marshal(map[string]string{
		"name": "big", "version": "1.0.0", "url": srv.URL,
		"integrity": "sha512-" + base64.StdEncoding.EncodeToString(make([]byte, 64)),
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
