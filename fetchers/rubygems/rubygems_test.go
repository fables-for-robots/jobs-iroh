package main

import (
	"bytes"
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

func TestGemURLAndFilename(t *testing.T) {
	cases := []struct {
		name, version, platform, wantFile, wantURL string
	}{
		{"bcrypt", "3.1.20", "", "bcrypt-3.1.20.gem", "https://rubygems.org/gems/bcrypt-3.1.20.gem"},
		{"nokogiri", "1.18.0", "x86_64-linux-musl", "nokogiri-1.18.0-x86_64-linux-musl.gem", "https://rubygems.org/gems/nokogiri-1.18.0-x86_64-linux-musl.gem"},
	}
	for _, c := range cases {
		if got := gemFilename(c.name, c.version, c.platform); got != c.wantFile {
			t.Errorf("gemFilename(%q,%q,%q) = %q, want %q", c.name, c.version, c.platform, got, c.wantFile)
		}
		if got := gemURL(c.name, c.version, c.platform); got != c.wantURL {
			t.Errorf("gemURL(%q,%q,%q) = %q, want %q", c.name, c.version, c.platform, got, c.wantURL)
		}
	}
}

// TestRunStreamsLargePayload proves the fetcher does not buffer the gem in
// memory: fetching a ~32MiB gem must allocate far less than the payload.
// TotalAlloc is monotonic, so the bound is GC-independent.
func TestRunStreamsLargePayload(t *testing.T) {
	body := make([]byte, 32<<20)
	rnd := mrand.New(mrand.NewSource(1))
	rnd.Read(body)
	sum := sha256.Sum256(body)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()
	oldBase := gemBase
	gemBase = srv.URL + "/gems/"
	defer func() { gemBase = oldBase }()

	outDir := t.TempDir()
	params, _ := json.Marshal(map[string]string{
		"name": "big", "version": "1.0.0", "sha256": hex.EncodeToString(sum[:]),
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
	got, err := os.ReadFile(filepath.Join(outDir, "big-1.0.0.gem"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatal("written content differs")
	}
}

// TestRunRemovesFileOnMismatch: a sha256 mismatch must not leave a partial
// artifact in the output tree.
func TestRunRemovesFileOnMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("corrupt"))
	}))
	defer srv.Close()
	oldBase := gemBase
	gemBase = srv.URL + "/gems/"
	defer func() { gemBase = oldBase }()

	outDir := t.TempDir()
	params, _ := json.Marshal(map[string]string{
		"name": "big", "version": "1.0.0",
		"sha256": "0000000000000000000000000000000000000000000000000000000000000000",
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
