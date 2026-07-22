// Command fetch is the JOBS crates.io fetcher: it downloads one crate's .crate
// archive from static.crates.io, verifies its sha256 against the Cargo.lock
// checksum, unpacks it into JOBS_OUTPUT_DIR, and writes a .cargo-checksum.json so
// cargo's directory-source replacement accepts it. Conforms to the fetcher
// contract (import.md §3.3): JOBS_FETCH_PARAMS in, JOBS_OUTPUT_DIR out, exit
// 0=success / 75=retryable / other=hard. Statically linked (CGO disabled), so it
// runs as a plain host subprocess (runner/executor.go) with network access.
package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	exitOK        = 0
	exitHard      = 1
	exitRetryable = 75
)

// params is the JOBS_FETCH_PARAMS JSON payload.
type params struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Checksum string `json:"checksum"`
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
	if p.Name == "" || p.Version == "" || p.Checksum == "" {
		fmt.Fprintln(stderr, "params: name, version and checksum are required")
		return exitHard
	}
	// Stream the download to a temp file (hashing inline) instead of buffering
	// it in memory — the import may run under a cgroup memory cap. outDir is
	// the one dir the fetcher contract guarantees writable; the temp file is
	// removed before exit.
	tmp, err := os.CreateTemp(outDir, ".fetch-*.tmp")
	if err != nil {
		fmt.Fprintf(stderr, "temp file: %v\n", err)
		return exitHard
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	got, retryable, err := download(p.Name, p.Version, tmp)
	if err != nil {
		fmt.Fprintln(stderr, err)
		if retryable {
			return exitRetryable
		}
		return exitHard
	}
	if got != p.Checksum {
		fmt.Fprintf(stderr, "sha256 mismatch for %s-%s: got %s want %s\n", p.Name, p.Version, got, p.Checksum)
		return exitHard
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		fmt.Fprintf(stderr, "seek: %v\n", err)
		return exitHard
	}
	if err := extractCrate(bufio.NewReader(tmp), p, outDir); err != nil {
		fmt.Fprintln(stderr, err)
		return exitHard // bad pinned content / unsafe archive — not transient
	}
	return exitOK
}

// cratesBase is the static.crates.io download URL base; a var so tests can
// point it at a local server.
var cratesBase = "https://static.crates.io/crates"

// download streams the .crate archive into w, returning the hex sha256 of the
// bytes. The bool reports whether a failure is retryable (network error, 5xx,
// or 429).
func download(name, version string, w io.Writer) (string, bool, error) {
	url := fmt.Sprintf("%s/%s/%s-%s.crate", cratesBase, name, name, version)
	client := &http.Client{Timeout: 120 * time.Second}
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

// extractCrate unpacks the checksum-verified gzip-tar read from r into outDir
// (entries are rooted at <name>-<version>/), and writes
// <name>-<version>/.cargo-checksum.json with the full per-file sha256 map +
// the package checksum (what cargo's directory source requires).
func extractCrate(r io.Reader, p params, outDir string) error {
	root := fmt.Sprintf("%s-%s", p.Name, p.Version)
	rootDir := filepath.Join(outDir, root)
	files := map[string]string{}

	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("gunzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}
		clean := filepath.Clean(hdr.Name)
		if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			return fmt.Errorf("unsafe path in archive: %q", hdr.Name)
		}
		dst := filepath.Join(outDir, clean)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			h := sha256.New()
			if _, err := io.Copy(io.MultiWriter(f, h), tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
			if rel, err := filepath.Rel(rootDir, dst); err == nil && !strings.HasPrefix(rel, "..") {
				files[filepath.ToSlash(rel)] = hex.EncodeToString(h.Sum(nil))
			}
		}
	}

	cc := struct {
		Files   map[string]string `json:"files"`
		Package string            `json:"package"`
	}{Files: files, Package: p.Checksum}
	b, err := json.Marshal(cc) // json sorts map keys → deterministic
	if err != nil {
		return err
	}
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(rootDir, ".cargo-checksum.json"), b, 0o644)
}
