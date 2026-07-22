// Command fetch is the JOBS RubyGems fetcher: it downloads one .gem from
// rubygems.org, verifies its sha256 (the value RubyGems publishes — identical to
// the Gemfile.lock CHECKSUMS entry, so the content address has no trust gap), and
// writes the raw .gem into JOBS_OUTPUT_DIR. The build stages these into
// vendor/cache for an offline `bundle install --local`.
// Conforms to the fetcher contract (import.md §3.3): JOBS_FETCH_PARAMS in,
// JOBS_OUTPUT_DIR out, exit 0=success / 75=retryable / other=hard. Statically
// linked (CGO disabled), so it runs as a network-capable host subprocess.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
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
	Platform string `json:"platform"` // "" for the pure-ruby gem; e.g. "x86_64-linux-musl"
	Sha256   string `json:"sha256"`
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
	if p.Name == "" || p.Version == "" || p.Sha256 == "" {
		fmt.Fprintln(stderr, "params: name, version and sha256 are required")
		return exitHard
	}
	filename := gemFilename(p.Name, p.Version, p.Platform)
	if filepath.Base(filename) != filename {
		fmt.Fprintf(stderr, "unsafe filename %q\n", filename)
		return exitHard
	}
	// Stream the download straight into the .gem file (hashing inline) instead
	// of buffering it in memory — the import may run under a cgroup memory
	// cap. The file is removed again if the download or the sha256 check
	// fails, so a bad fetch leaves no partial artifact.
	dest := filepath.Join(outDir, filename)
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		fmt.Fprintf(stderr, "create %s: %v\n", dest, err)
		return exitHard
	}
	got, retryable, err := download(gemURL(p.Name, p.Version, p.Platform), f)
	if cerr := f.Close(); cerr != nil && err == nil {
		err, retryable = cerr, true
	}
	if err != nil {
		os.Remove(dest)
		fmt.Fprintln(stderr, err)
		if retryable {
			return exitRetryable
		}
		return exitHard
	}
	if got != p.Sha256 {
		os.Remove(dest)
		fmt.Fprintf(stderr, "sha256 mismatch for %s: got %s want %s\n", filename, got, p.Sha256)
		return exitHard
	}
	return exitOK
}

// gemFilename is the canonical .gem file name: NAME-VERSION[-PLATFORM].gem.
func gemFilename(name, version, platform string) string {
	if platform == "" {
		return fmt.Sprintf("%s-%s.gem", name, version)
	}
	return fmt.Sprintf("%s-%s-%s.gem", name, version, platform)
}

// gemBase is the rubygems.org download URL prefix; a var so tests can point it
// at a local server.
var gemBase = "https://rubygems.org/gems/"

// gemURL is the rubygems.org download URL for a gem.
func gemURL(name, version, platform string) string {
	return gemBase + gemFilename(name, version, platform)
}

// download streams the gem into w, returning the hex sha256 of the bytes. The
// bool reports whether a failure is retryable.
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
