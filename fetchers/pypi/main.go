// Command fetch is the JOBS PyPI fetcher: it downloads one wheel or sdist from
// its uv.lock URL, verifies its sha256, and either writes the raw .whl into
// JOBS_OUTPUT_DIR (a one-entry wheelhouse the recipe merges via --find-links)
// or extracts a .tar.gz sdist with the top-level directory stripped so the
// package root lands directly at JOBS_OUTPUT_DIR.
// Conforms to the fetcher contract (import.md §3.3): JOBS_FETCH_PARAMS in,
// JOBS_OUTPUT_DIR out, exit 0=success / 75=retryable / other=hard. Statically
// linked (CGO disabled), so it runs as a network-capable host subprocess.
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
	URL      string `json:"url"`
	Filename string `json:"filename"`
	Sha256   string `json:"sha256"`
	Name     string `json:"name"`
	Version  string `json:"version"`
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
	if p.URL == "" || p.Filename == "" || p.Sha256 == "" {
		fmt.Fprintln(stderr, "params: url, filename and sha256 are required")
		return exitHard
	}
	if filepath.Base(p.Filename) != p.Filename {
		fmt.Fprintf(stderr, "unsafe filename %q\n", p.Filename)
		return exitHard
	}
	// Stream the download to a temp file (hashing inline) instead of buffering
	// it in memory — wheels run to hundreds of MB, and the import may run
	// under a cgroup memory cap. outDir is the one dir the fetcher contract
	// guarantees writable; the temp file is renamed into place (wheel) or
	// extracted (sdist) after the checksum verifies, and removed otherwise.
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
		fmt.Fprintf(stderr, "sha256 mismatch for %s: got %s want %s\n", p.Filename, got, p.Sha256)
		return exitHard
	}
	if err := deliver(tmp, p, outDir); err != nil {
		fmt.Fprintln(stderr, err)
		return exitHard // bad pinned content / unsupported type — not transient
	}
	return exitOK
}

// download streams the artifact into w, returning the hex sha256 of the bytes.
// The bool reports whether a failure is retryable (network error, 5xx, or 429).
func download(url string, w io.Writer) (string, bool, error) {
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

// deliver dispatches the checksum-verified temp file on the artifact type
// indicated by p.Filename:
//   - .whl         → rename into outDir (wheelhouse entry).
//   - .tar.gz/.tgz → extract the gzip-tar into outDir, stripping the single
//     top-level <name>-<version>/ directory so the package root lands at outDir.
//   - anything else → hard error (unsupported, out of scope for sdist builds).
func deliver(tmp *os.File, p params, outDir string) error {
	switch {
	case strings.HasSuffix(p.Filename, ".whl"):
		// filename is pre-validated by run() (no path separators)
		dest := filepath.Join(outDir, p.Filename)
		if err := tmp.Close(); err != nil {
			return err
		}
		if err := os.Rename(tmp.Name(), dest); err != nil {
			return err
		}
		// CreateTemp makes the file 0600; the wheelhouse entry is world-readable.
		return os.Chmod(dest, 0o644)
	case strings.HasSuffix(p.Filename, ".tar.gz") || strings.HasSuffix(p.Filename, ".tgz"):
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return err
		}
		return extractSdist(bufio.NewReader(tmp), outDir)
	default:
		return fmt.Errorf("unsupported artifact type %q (only .whl and .tar.gz sdists are supported)", p.Filename)
	}
}

// extractSdist unpacks a gzip-tar sdist into outDir, stripping the single
// top-level directory (e.g. "pkg-1.0/") from every entry so that
// pkg-1.0/setup.py → outDir/setup.py.
func extractSdist(r io.Reader, outDir string) error {
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

		// Safety check on the raw path before any manipulation.
		clean := filepath.Clean(hdr.Name)
		if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			return fmt.Errorf("unsafe path in archive: %q", hdr.Name)
		}

		// Strip the leading top-level directory component.
		// e.g. "pkg-1.0/setup.py" → "setup.py"; "pkg-1.0/" → "" (skip).
		parts := strings.SplitN(filepath.ToSlash(clean), "/", 2)
		if len(parts) < 2 || parts[1] == "" {
			// bare top-level dir entry — skip
			continue
		}
		rel := parts[1]

		// Re-validate the stripped path.
		relClean := filepath.Clean(rel)
		if strings.HasPrefix(relClean, "..") || filepath.IsAbs(relClean) {
			return fmt.Errorf("unsafe stripped path in archive: %q", hdr.Name)
		}
		dst := filepath.Join(outDir, relClean)

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
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}
	return nil
}
