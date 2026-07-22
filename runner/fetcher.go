package runner

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Platform is this runner's platform string (e.g. "linux/arm64").
func Platform() string { return runtime.GOOS + "/" + runtime.GOARCH }

// extractTar extracts a store tar to dest for EXECUTION/materialize use. It is
// deliberately LOSSY: no mtime restore, and directories get mode|0700 so the
// extracted tree can always be cleaned up with os.RemoveAll (store trees carry
// read-only 0555 directories). NEVER use it for cache seeding — amber hashes
// mtime, so the unchanged-skip compare there needs the lossless
// Store.Materialize path instead (see cachedir.go).
func extractTar(r io.Reader, dest string) error {
	cleanDest := filepath.Clean(dest)
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(cleanDest, filepath.Clean("/"+hdr.Name))
		if target != cleanDest && !strings.HasPrefix(target, cleanDest+string(os.PathSeparator)) {
			return fmt.Errorf("tar entry escapes destination: %s", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)|0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
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
		case tar.TypeSymlink:
			_ = os.Symlink(hdr.Linkname, target)
		}
	}
}
