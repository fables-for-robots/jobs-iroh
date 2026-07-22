// Command fetch is the JOBS tarball+xz fetcher: it downloads a .tar.xz from a
// URL, verifies its sha256, and extracts it into JOBS_OUTPUT_DIR with an optional
// leading-component strip. Go has no stdlib xz, so it uses the pure-Go
// github.com/ulikunitz/xz decompressor (keeping the binary static / CGO-free).
// Needed for the self-contained ziglang.org zig toolchain (shipped only as
// .tar.xz), which the rails-build example uses as its hermetic C compiler.
// Conforms to the fetcher contract (import.md §3.3): JOBS_FETCH_PARAMS in,
// JOBS_OUTPUT_DIR out, exit 0=success / 75=retryable / other=hard.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/fables-for-robots/jobs-iroh/fetchers/tarextract"
	"github.com/ulikunitz/xz"
)

const (
	exitOK        = 0
	exitHard      = 1
	exitRetryable = 75
)

// params is the JOBS_FETCH_PARAMS JSON payload.
type params struct {
	URL    string `json:"url"`
	Sha256 string `json:"sha256"`
	Strip  int    `json:"strip"` // leading path components to drop (default 0)
}

func main() { os.Exit(run(os.Getenv, os.Stderr)) }

// run is the testable entrypoint.
func run(getenv func(string) string, stderr io.Writer) int {
	outDir := getenv("JOBS_OUTPUT_DIR")
	if outDir == "" {
		fmt.Fprintln(stderr, "JOBS_OUTPUT_DIR not set")
		return exitHard
	}
	var p params
	if err := json.Unmarshal([]byte(getenv("JOBS_FETCH_PARAMS")), &p); err != nil {
		fmt.Fprintf(stderr, "params: %v\n", err)
		return exitHard
	}
	if p.URL == "" || p.Sha256 == "" {
		fmt.Fprintln(stderr, "params: url and sha256 are required")
		return exitHard
	}
	// Stream the download to a temp file (hashing inline) instead of buffering
	// it in memory — rust toolchain tarballs run to hundreds of MB, and the
	// import may run under a cgroup memory cap. outDir is the one dir the
	// fetcher contract guarantees writable; the temp file is removed before exit.
	tmp, err := os.CreateTemp(outDir, ".fetch-*.tmp")
	if err != nil {
		fmt.Fprintf(stderr, "temp file: %v\n", err)
		return exitHard
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	got, retryable, err := download(p.URL, tmp)
	if err != nil {
		fmt.Fprintln(stderr, err)
		if retryable {
			return exitRetryable
		}
		return exitHard
	}
	if got != p.Sha256 {
		fmt.Fprintf(stderr, "sha256 mismatch for %s: got %s want %s\n", p.URL, got, p.Sha256)
		return exitHard
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		fmt.Fprintf(stderr, "seek: %v\n", err)
		return exitHard
	}
	if err := extractTarXz(tmp, outDir, p.Strip); err != nil {
		fmt.Fprintln(stderr, err)
		return exitHard
	}
	return exitOK
}

// download streams the tarball into w, returning the hex sha256 of the bytes.
// The bool reports whether a failure is retryable.
func download(url string, w io.Writer) (string, bool, error) {
	client := &http.Client{Timeout: 300 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", true, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return "", retryable, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(w, h), resp.Body); err != nil {
		return "", true, fmt.Errorf("read body: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), false, nil
}

// extractTarXz xz-decompresses r and untars it into outDir via the shared
// symlink-preserving extractor, dropping the first `strip` leading path
// components from each entry.
func extractTarXz(r io.Reader, outDir string, strip int) error {
	xr, err := xz.NewReader(bufio.NewReader(r))
	if err != nil {
		return fmt.Errorf("xz: %w", err)
	}
	return tarextract.Extract(xr, outDir, strip)
}
