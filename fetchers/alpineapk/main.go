// Command fetch is the JOBS Alpine-apk fetcher: it downloads one Alpine Linux
// package (.apk) from the Alpine CDN, verifies its sha256, and extracts the
// package's file tree into JOBS_OUTPUT_DIR. An .apk is several concatenated
// gzip-tar segments (signature, control, data); only the data files are
// extracted — the control members (root-level dotfiles like .PKGINFO/.SIGN.*)
// are skipped. Used to assemble the relocatable musl Ruby toolchain slice (and
// Node's libstdc++) for the Rails example.
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
	"syscall"
	"time"
)

const (
	exitOK        = 0
	exitHard      = 1
	exitRetryable = 75
)

// params is the JOBS_FETCH_PARAMS JSON payload.
type params struct {
	Branch  string `json:"branch"`  // e.g. "v3.22"
	Repo    string `json:"repo"`    // e.g. "main" or "community"
	Arch    string `json:"arch"`    // e.g. "x86_64"
	Name    string `json:"name"`    // package name, e.g. "ruby-dev"
	Version string `json:"version"` // full version incl. release, e.g. "3.4.4-r0"
	Sha256  string `json:"sha256"`  // hex digest of the .apk
}

// mirror is the Alpine CDN base URL; a var so tests can point it at a local
// server.
var mirror = "https://dl-cdn.alpinelinux.org/alpine"

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
	if p.Branch == "" || p.Repo == "" || p.Arch == "" || p.Name == "" || p.Version == "" || p.Sha256 == "" {
		fmt.Fprintln(stderr, "params: branch, repo, arch, name, version and sha256 are required")
		return exitHard
	}
	url := fmt.Sprintf("%s/%s/%s/%s/%s-%s.apk", mirror, p.Branch, p.Repo, p.Arch, p.Name, p.Version)
	// Stream the download to a temp file (hashing inline) instead of buffering
	// it in memory — big apks (gcc, libstdc++) shouldn't balloon the import's
	// RSS, and the import may run under a cgroup memory cap. outDir is the one
	// dir the fetcher contract guarantees writable; the temp file is removed
	// before exit.
	tmp, err := os.CreateTemp(outDir, ".fetch-*.tmp")
	if err != nil {
		fmt.Fprintf(stderr, "temp file: %v\n", err)
		return exitHard
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	got, retryable, err := download(url, tmp)
	if err != nil {
		fmt.Fprintln(stderr, err)
		if retryable {
			return exitRetryable
		}
		return exitHard
	}
	if got != p.Sha256 {
		fmt.Fprintf(stderr, "sha256 mismatch for %s: got %s want %s\n", url, got, p.Sha256)
		return exitHard
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		fmt.Fprintf(stderr, "seek: %v\n", err)
		return exitHard
	}
	if err := extractAPK(bufio.NewReader(tmp), outDir); err != nil {
		fmt.Fprintln(stderr, err)
		return exitHard
	}
	return exitOK
}

// download streams the .apk into w, returning the hex sha256 of the bytes. The
// bool reports whether a failure is retryable (network error, 5xx, or 429).
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

// extractAPK unpacks the data files of an .apk read from r into outDir. An
// .apk is several concatenated gzip members (signature / control / data), each
// an independent tar archive, so we walk the gzip members one at a time
// (Multistream(false) + Reset) and run a fresh tar reader over each. Control
// members (root-level dotfiles: .PKGINFO, .SIGN.*, .pre/post-install,
// .trigger, …) are skipped; everything else is the package's file tree.
func extractAPK(r io.Reader, outDir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("gunzip: %w", err)
	}
	defer gz.Close()
	for {
		gz.Multistream(false)
		tr := tar.NewReader(gz)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("tar: %w", err)
			}
			if err := extractEntry(hdr, tr, outDir); err != nil {
				return err
			}
		}
		// tar.Reader stops at the EOF marker, leaving the gzip member's trailing
		// blocking-factor padding unread; drain it so the gzip reader consumes
		// the whole member and positions the underlying reader at the next one.
		if _, err := io.Copy(io.Discard, gz); err != nil {
			return fmt.Errorf("drain gzip member: %w", err)
		}
		// Advance to the next gzip member; io.EOF means there are no more.
		if err := gz.Reset(r); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("gzip member: %w", err)
		}
	}
}

// isControlMember reports whether name is an apk control/signature member — a
// root-level dotfile (no path separator, leading "."). Real package files live
// under usr/, lib/, etc., so a data dotfile like usr/share/.foo is NOT matched.
func isControlMember(name string) bool {
	return !strings.Contains(name, "/") && strings.HasPrefix(name, ".")
}

// extractEntry writes one tar entry into outDir, preserving file modes and
// symlinks and rejecting paths that escape outDir.
func extractEntry(hdr *tar.Header, tr io.Reader, outDir string) error {
	if isControlMember(hdr.Name) {
		return nil
	}
	clean := filepath.Clean(hdr.Name)
	if clean == "." {
		return nil
	}
	if escapes(clean) {
		return fmt.Errorf("unsafe path in apk: %q", hdr.Name)
	}
	dst := filepath.Join(outDir, clean)
	switch hdr.Typeflag {
	case tar.TypeDir:
		return os.MkdirAll(dst, 0o755)
	case tar.TypeReg:
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		perm := os.FileMode(hdr.Mode).Perm()
		// O_NOFOLLOW: never write through a symlink at the final path component
		// (defence against a symlink planted earlier in the same archive).
		f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, perm)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		// OpenFile perms are masked by umask; restore the recorded mode so
		// executables (ruby, .so files) keep their bits.
		return os.Chmod(dst, perm)
	case tar.TypeSymlink:
		// Reject any symlink whose target would resolve outside outDir
		// (Zip-Slip via symlink). Most Alpine symlinks are same-dir relative; an
		// absolute target (e.g. gcc's "//usr/libexec/gcc/…/liblto_plugin.so")
		// means "relative to the install root", so reinterpret it under outDir
		// and rewrite it as a relative link — that resolves identically whether
		// the tree is staged into / or mounted at a store path.
		target := hdr.Linkname
		if filepath.IsAbs(target) {
			rooted := filepath.Join(outDir, filepath.Clean(target))
			rel, err := filepath.Rel(filepath.Dir(dst), rooted)
			if err != nil {
				return fmt.Errorf("unsafe symlink in apk: %q -> %q", hdr.Name, target)
			}
			target = rel
		}
		if !within(outDir, filepath.Clean(filepath.Join(filepath.Dir(dst), target))) {
			return fmt.Errorf("unsafe symlink in apk: %q -> %q", hdr.Name, target)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		_ = os.Remove(dst)
		return os.Symlink(target, dst)
	case tar.TypeLink:
		// Hardlink source must also stay within outDir.
		link := filepath.Clean(hdr.Linkname)
		if escapes(link) {
			return fmt.Errorf("unsafe hardlink in apk: %q -> %q", hdr.Name, hdr.Linkname)
		}
		src := filepath.Join(outDir, link)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		_ = os.Remove(dst)
		return os.Link(src, dst)
	default:
		// char/block/fifo and other special types: not present in the packages
		// we consume; skip rather than fail.
		return nil
	}
}

// escapes reports whether a cleaned relative path leaves its root (starts with
// ".." or is absolute).
func escapes(clean string) bool {
	return clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.IsAbs(clean)
}

// within reports whether path is inside root (root itself counts as inside).
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || !escapes(rel)
}
