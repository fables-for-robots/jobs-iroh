// Package tarextract untars a (already-decompressed) tar stream into a
// directory with an optional leading-component strip, preserving file modes and
// — critically — symlink targets EXACTLY.
//
// It exists because busybox tar (the ambient tar on a minimal runner image)
// applies a path-traversal guard that strips a leading "../" from relative
// symlink targets ("removing leading '../' from member names"). That silently
// corrupts legitimate relative links such as a Node distribution's
// `bin/npm -> ../lib/node_modules/npm/bin/npm-cli.js` into `bin/npm -> lib/...`,
// a dangling link that makes `npm` resolve to nothing ("command not found").
// Go's archive/tar has no such guard, so a fetcher extracting through this
// package is portable across GNU tar and busybox hosts alike.
//
// Paths (and symlink targets) that escape outDir are rejected; writes use
// O_NOFOLLOW so a malicious archive cannot redirect a write through a symlink.
package tarextract

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Extract untars r into outDir, dropping the first `strip` leading path
// components from each entry.
func Extract(r io.Reader, outDir string, strip int) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}
		rel, ok := stripComponents(hdr.Name, strip)
		if !ok {
			continue // entry fully consumed by the strip (e.g. the top dir itself)
		}
		clean := filepath.Clean(rel)
		if clean == "." {
			continue
		}
		if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.IsAbs(clean) {
			return fmt.Errorf("unsafe path in tarball: %q", hdr.Name)
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
			perm := os.FileMode(hdr.Mode).Perm()
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
			if err := os.Chmod(dst, perm); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// hdr.Linkname is used VERBATIM — this is the whole point of the
			// package (see the doc comment): busybox tar would strip a leading
			// "../" here and break relative links.
			target := hdr.Linkname
			resolved := target
			if !filepath.IsAbs(resolved) {
				resolved = filepath.Join(filepath.Dir(dst), resolved)
			}
			if r, err := filepath.Rel(outDir, filepath.Clean(resolved)); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
				return fmt.Errorf("unsafe symlink in tarball: %q -> %q", hdr.Name, target)
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			_ = os.Remove(dst)
			if err := os.Symlink(target, dst); err != nil {
				return err
			}
		}
	}
	return nil
}

// stripComponents drops the first n slash-separated components of name. It
// returns ok=false when nothing remains (e.g. the stripped top-level dir entry).
func stripComponents(name string, n int) (string, bool) {
	if n <= 0 {
		return name, true
	}
	parts := strings.Split(filepath.ToSlash(strings.TrimRight(name, "/")), "/")
	if len(parts) <= n {
		return "", false
	}
	return strings.Join(parts[n:], "/"), true
}
