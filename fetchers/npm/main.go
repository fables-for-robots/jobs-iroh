// Command fetch is the JOBS npm fetcher: it downloads one package tarball from
// its resolved registry URL (as recorded in yarn.lock / package-lock.json),
// verifies the Subresource-Integrity digest (sha512-… or sha1-…), and writes the
// raw .tgz into JOBS_OUTPUT_DIR. The build stages these into a Yarn offline
// mirror for an offline `yarn install`.
// Conforms to the fetcher contract (import.md §3.3): JOBS_FETCH_PARAMS in,
// JOBS_OUTPUT_DIR out, exit 0=success / 75=retryable / other=hard. Statically
// linked (CGO disabled), so it runs as a network-capable host subprocess.
package main

import (
	"crypto/sha1"
	"crypto/sha512"
	"encoding/base64"
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
	Name      string `json:"name"`
	Version   string `json:"version"`
	URL       string `json:"url"`       // the resolved tarball URL (registry.npmjs.org/...)
	Integrity string `json:"integrity"` // SRI: "sha512-<base64>" or "sha1-<base64>"
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
	if p.Name == "" || p.Version == "" || p.URL == "" || p.Integrity == "" {
		fmt.Fprintln(stderr, "params: name, version, url and integrity are required")
		return exitHard
	}
	// Stream the download straight into the mirror file (hashing inline)
	// instead of buffering it in memory — the import may run under a cgroup
	// memory cap. The file is removed again if the download or the integrity
	// check fails, so a bad fetch leaves no partial artifact.
	dest := filepath.Join(outDir, mirrorName(p.Name, p.Version))
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		fmt.Fprintf(stderr, "create %s: %v\n", dest, err)
		return exitHard
	}
	sums, retryable, err := download(p.URL, f)
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
	if err := verifyIntegrity(sums, p.Integrity); err != nil {
		os.Remove(dest)
		fmt.Fprintln(stderr, err)
		return exitHard
	}
	return exitOK
}

// mirrorName is the .tgz filename Yarn's offline mirror expects:
// NAME-VERSION.tgz, with the scope "/" replaced by "-" (e.g.
// "@esbuild/linux-x64" 0.25.12 -> "@esbuild-linux-x64-0.25.12.tgz").
func mirrorName(name, version string) string {
	return strings.ReplaceAll(name, "/", "-") + "-" + version + ".tgz"
}

// sums carries the digests computed inline while the download streams — both
// SRI algorithms npm lockfiles use, so verification never needs the bytes.
type sums struct {
	sha512 []byte
	sha1   []byte
}

// sumsOf digests a byte slice (test helper / small-payload path).
func sumsOf(data []byte) sums {
	s512 := sha512.Sum512(data)
	s1 := sha1.Sum(data)
	return sums{sha512: s512[:], sha1: s1[:]}
}

// download streams the tarball into w, returning the digests of the bytes.
func download(url string, w io.Writer) (sums, bool, error) {
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return sums{}, true, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return sums{}, retryable, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	h512, h1 := sha512.New(), sha1.New()
	if _, err := io.Copy(io.MultiWriter(w, h512, h1), resp.Body); err != nil {
		return sums{}, true, fmt.Errorf("read body: %w", err)
	}
	return sums{sha512: h512.Sum(nil), sha1: h1.Sum(nil)}, false, nil
}

// verifyIntegrity checks an npm Subresource-Integrity string against the
// download's digests.
func verifyIntegrity(s sums, integrity string) error {
	// An integrity field may list multiple space-separated digests; any match is ok.
	for _, tok := range strings.Fields(integrity) {
		if rest, ok := strings.CutPrefix(tok, "sha512-"); ok {
			if base64.StdEncoding.EncodeToString(s.sha512) == rest {
				return nil
			}
		} else if rest, ok := strings.CutPrefix(tok, "sha1-"); ok {
			if base64.StdEncoding.EncodeToString(s.sha1) == rest {
				return nil
			}
		}
	}
	return fmt.Errorf("integrity mismatch (no digest in %q matched the download)", integrity)
}
