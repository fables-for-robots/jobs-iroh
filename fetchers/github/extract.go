package main

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// extractTarball gunzips and untars r into dest, stripping the single top-level
// directory GitHub wraps the archive in (owner-repo-sha/). Transfer-level failures
// (gunzip, tar read, body copy) are retryable; path-safety violations and local
// filesystem errors are hard. Hardlinks, devices, and other entry types are
// skipped.
func extractTarball(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return retryErr("gunzip: %v", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	wrote := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return retryErr("read tar: %v", err)
		}

		rel, ok := stripTop(hdr.Name)
		if !ok {
			continue // the bare wrapper directory entry
		}
		target, err := safeJoin(dest, rel)
		if err != nil {
			return hardErr("unsafe path %q: %v", hdr.Name, err)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return hardErr("mkdir %s: %v", target, err)
			}
			wrote = true
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return hardErr("mkdir parent of %s: %v", target, err)
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&os.ModePerm)
			if err != nil {
				return hardErr("create %s: %v", target, err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return retryErr("write %s: %v", target, err)
			}
			if err := f.Close(); err != nil {
				return hardErr("close %s: %v", target, err)
			}
			wrote = true
		case tar.TypeSymlink:
			if filepath.IsAbs(hdr.Linkname) {
				return hardErr("absolute symlink %q -> %q", hdr.Name, hdr.Linkname)
			}
			resolved := filepath.Join(filepath.Dir(target), hdr.Linkname)
			if !within(dest, resolved) {
				return hardErr("symlink %q escapes root", hdr.Name)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return hardErr("mkdir parent of %s: %v", target, err)
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return hardErr("symlink %s: %v", target, err)
			}
			wrote = true
		default:
			// skip hardlinks, devices, fifos, etc.
		}
	}
	if !wrote {
		return hardErr("tarball had no repository content")
	}
	return nil
}

// stripTop drops the first path component. ok is false for the bare wrapper entry
// (no nested path remains).
func stripTop(name string) (string, bool) {
	name = strings.TrimPrefix(name, "./")
	name = strings.TrimLeft(name, "/")
	i := strings.IndexByte(name, '/')
	if i < 0 {
		return "", false
	}
	rest := name[i+1:]
	if rest == "" {
		return "", false
	}
	return rest, true
}

// safeJoin joins rel onto base after neutralizing "..", and verifies the result
// stays within base.
func safeJoin(base, rel string) (string, error) {
	// Join rel onto base, then clean, and verify result is within base
	target := filepath.Join(base, rel)
	target = filepath.Clean(target)
	if !within(base, target) {
		return "", errPathEscape
	}
	return target, nil
}

var errPathEscape = hardErr("path escapes root")

// within reports whether target is base itself or nested under it.
func within(base, target string) bool {
	base = filepath.Clean(base)
	target = filepath.Clean(target)
	if target == base {
		return true
	}
	return strings.HasPrefix(target, base+string(os.PathSeparator))
}
