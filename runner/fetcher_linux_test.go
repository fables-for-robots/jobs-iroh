//go:build linux

package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// sameDevice reports whether path and its parent are on the same filesystem —
// i.e. path is NOT a separate mountpoint. A materialized directory shares its
// parent's device (there is no FUSE in jobs-iroh, and this locks that in).
func sameDevice(t *testing.T, path string) bool {
	t.Helper()
	var st, pst unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if err := unix.Stat(filepath.Dir(path), &pst); err != nil {
		t.Fatalf("stat %s: %v", filepath.Dir(path), err)
	}
	return st.Dev == pst.Dev
}

// TestResolveFetcher_Materializes asserts ResolveFetcher resolves the
// fetcher:<name>:<platform> ref (plain local read) and materializes the
// artifact to a real on-disk directory holding an executable ./fetch, and
// that cleanup removes it.
func TestResolveFetcher_Materializes(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	// A fetcher artifact: a dir whose root holds an executable ./fetch.
	art := t.TempDir()
	if err := os.WriteFile(filepath.Join(art, "fetch"), []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	k, err := st.IngestDir(ctx, art)
	if err != nil {
		t.Fatal(err)
	}
	platform := Platform()
	if err := st.PutRef(ctx, "fetcher:testfetch:"+platform, k); err != nil {
		t.Fatal(err)
	}

	cacheDir := t.TempDir()
	dir, cleanup, found, err := ResolveFetcher(ctx, st, cacheDir, "testfetch", platform)
	if err != nil {
		t.Fatalf("ResolveFetcher: %v", err)
	}
	if !found {
		t.Fatal("fetcher not found")
	}

	fi, err := os.Stat(filepath.Join(dir, "fetch"))
	if err != nil {
		t.Fatalf("stat ./fetch: %v", err)
	}
	if fi.Mode()&0o111 == 0 {
		t.Error("./fetch is not executable")
	}
	if !sameDevice(t, dir) {
		t.Error("ResolveFetcher returned a separate mountpoint; want a materialized directory")
	}

	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("cleanup must remove the materialized dir (err=%v)", err)
	}
}

// TestResolveFetcher_MissingRefDeclines: an absent fetcher ref is found=false,
// not an error (→ Decline upstream).
func TestResolveFetcher_MissingRefDeclines(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	_, _, found, err := ResolveFetcher(ctx, st, t.TempDir(), "nope", Platform())
	if err != nil {
		t.Fatalf("ResolveFetcher: %v", err)
	}
	if found {
		t.Fatal("missing ref must report found=false")
	}
}
