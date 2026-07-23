package registryd

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// blobCache is the on-disk store of assembled registry blobs (layer tars,
// image configs, manifests), one file per digest under <dir>/sha256/<hex>.
// A file's mtime is its LAST-READ time — bumped on every serve — which is
// what the TTL sweep expires on, so it survives restarts without a database.
type blobCache struct {
	dir string
	log *slog.Logger
}

func (b *blobCache) path(d v1.Hash) string {
	return filepath.Join(b.dir, d.Algorithm, d.Hex)
}

// has reports whether the blob exists without touching it.
func (b *blobCache) has(d v1.Hash) bool {
	_, err := os.Stat(b.path(d))
	return err == nil
}

// touch bumps the blob's last-read time. Best-effort: a lost touch only makes
// the sweep a little eager.
func (b *blobCache) touch(d v1.Hash) {
	now := time.Now()
	_ = os.Chtimes(b.path(d), now, now)
}

// open returns the blob file for serving (the caller closes it) and its size.
// Deleting a blob while a serve holds the fd is safe (POSIX unlink).
func (b *blobCache) open(d v1.Hash) (*os.File, int64, error) {
	f, err := os.Open(b.path(d))
	if err != nil {
		return nil, 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, fi.Size(), nil
}

// get reads a whole (small) blob.
func (b *blobCache) get(d v1.Hash) ([]byte, error) {
	return os.ReadFile(b.path(d))
}

// put streams r into the cache as digest d: written to a temp file in the
// same dir, hashed on the way, verified against d and renamed into place —
// a crash never leaves a partial blob under a digest name. An existing blob
// is only touched (content under a digest is immutable).
func (b *blobCache) put(d v1.Hash, r io.Reader) error {
	if d.Algorithm != "sha256" {
		return fmt.Errorf("unsupported digest algorithm %q", d.Algorithm)
	}
	if b.has(d) {
		b.touch(d)
		return nil
	}
	dir := filepath.Join(b.dir, d.Algorithm)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), r); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != d.Hex {
		return fmt.Errorf("blob hash mismatch: wrote sha256:%s, want %s", got, d.String())
	}
	// Sync before rename: a digest-named file must never hold truncated
	// bytes after a crash — touch-on-read would keep serving it forever.
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), b.path(d))
}

// sweep deletes blobs whose last-read time is older than ttl, plus temp files
// abandoned by a crash. Returns what it removed.
func (b *blobCache) sweep(ttl time.Duration, now time.Time) (removed int, bytes int64) {
	cutoff := now.Add(-ttl)
	entries, err := os.ReadDir(filepath.Join(b.dir, "sha256"))
	if err != nil {
		return 0, 0
	}
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil || fi.IsDir() {
			continue
		}
		stale := fi.ModTime().Before(cutoff)
		// Abandoned temp files expire on the same clock but under any TTL
		// longer than an assembly could take.
		if strings.HasPrefix(e.Name(), "tmp-") {
			stale = fi.ModTime().Before(now.Add(-time.Hour)) || stale
		}
		if !stale {
			continue
		}
		p := filepath.Join(b.dir, "sha256", e.Name())
		if err := os.Remove(p); err != nil {
			b.log.Warn("sweep remove", "blob", e.Name(), "error", err)
			continue
		}
		removed++
		bytes += fi.Size()
	}
	return removed, bytes
}
