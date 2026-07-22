//go:build linux

package runner

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func writeFileAt(t *testing.T, p, content string, atime, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, atime, mtime); err != nil {
		t.Fatal(err)
	}
}

func atimeOf(t *testing.T, p string) time.Time {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Lstat(p, &st); err != nil {
		t.Fatal(err)
	}
	return time.Unix(st.Atim.Sec, st.Atim.Nsec)
}

// preAgeAtimes: regular files get atime=epoch with mtime preserved; symlinks
// are left alone.
func TestPreAgeAtimes(t *testing.T) {
	dir := t.TempDir()
	mtime := time.Date(2026, 1, 2, 3, 4, 5, 600700800, time.UTC)
	writeFileAt(t, filepath.Join(dir, "sub", "f"), "x", time.Now(), mtime)
	if err := os.Symlink("f", filepath.Join(dir, "sub", "ln")); err != nil {
		t.Fatal(err)
	}

	if err := preAgeAtimes(dir); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Lstat(filepath.Join(dir, "sub", "f"))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(mtime) {
		t.Fatalf("mtime changed: %v != %v", fi.ModTime(), mtime)
	}
	if at := atimeOf(t, filepath.Join(dir, "sub", "f")); !at.Before(time.Unix(2, 0)) {
		t.Fatalf("atime not pre-aged: %v", at)
	}
}

// pruneCache: cold regular files deleted, warm kept, now-empty dirs removed
// bottom-up, symlinks kept while their dir is non-empty.
func TestPruneCache(t *testing.T) {
	dir := t.TempDir()
	cutoff := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	old := cutoff.Add(-time.Hour)
	fresh := cutoff.Add(time.Minute)

	writeFileAt(t, filepath.Join(dir, "keep"), "k", fresh, old)
	writeFileAt(t, filepath.Join(dir, "cold"), "c", old, old)
	writeFileAt(t, filepath.Join(dir, "deaddir", "inner", "cold2"), "c", old, old)
	writeFileAt(t, filepath.Join(dir, "livedir", "warm"), "w", fresh, old)
	if err := os.Symlink("keep", filepath.Join(dir, "livedir", "ln")); err != nil {
		t.Fatal(err)
	}

	if err := pruneCache(dir, cutoff); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"keep", "livedir/warm", "livedir/ln"} {
		if _, err := os.Lstat(filepath.Join(dir, want)); err != nil {
			t.Errorf("%s should survive: %v", want, err)
		}
	}
	for _, gone := range []string{"cold", "deaddir"} {
		if _, err := os.Lstat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
			t.Errorf("%s should be pruned (err=%v)", gone, err)
		}
	}
	// The cache ROOT itself must survive even when everything is pruned.
	if err := pruneCache(dir, fresh.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("cache root removed: %v", err)
	}
}
