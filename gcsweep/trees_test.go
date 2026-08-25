package gcsweep

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jobs-build/jobs-iroh/amber"
)

func TestSweepTrees(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := amber.Open(filepath.Join(dir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	liveKey, err := st.IngestFile(ctx, []byte("live tree content"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutRef(ctx, "keep", liveKey); err != nil { // keeps it live through the cycle
		t.Fatal(err)
	}

	cacheDir := filepath.Join(dir, "cache")
	trees := filepath.Join(cacheDir, "trees")
	deadName := "00deadbeef00deadbeef00deadbeef00deadbeef00deadbeef00deadbeef0000"
	for _, d := range []string{
		filepath.Join(trees, liveKey.String()),
		filepath.Join(trees, liveKey.String()+".bin"), // stagedBinDir companion, owned by liveKey
		filepath.Join(trees, deadName),                // valid-shaped key, absent from the store
		filepath.Join(trees, deadName+".bin"),         // companion of a dead key
		filepath.Join(trees, "not-a-key"),
		filepath.Join(trees, "staging-live123"), // in-flight stagedTree materialization, young
		filepath.Join(trees, "staging-crashed"), // abandoned staging dir, backdated below
		filepath.Join(cacheDir, "fetcher-old"),
		filepath.Join(cacheDir, "fetcher-new"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(filepath.Join(cacheDir, "fetcher-old"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(trees, "staging-crashed"), old, old); err != nil {
		t.Fatal(err)
	}

	sw, err := New(slog.Default(), st, Options{
		StoreDir:     filepath.Join(dir, "store"),
		SnapshotPath: filepath.Join(dir, "refaccess.cbor"),
		Retention:    time.Hour,
		CacheDir:     cacheDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sw.Close()

	stats, err := sw.Sweep(ctx, -1, true)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	if !exists(filepath.Join(trees, liveKey.String())) {
		t.Error("live tree deleted")
	}
	if !exists(filepath.Join(trees, liveKey.String()+".bin")) {
		t.Error("live tree's .bin companion deleted")
	}
	if exists(filepath.Join(trees, deadName)) {
		t.Error("dead-key tree kept")
	}
	if exists(filepath.Join(trees, deadName+".bin")) {
		t.Error("dead-key tree's .bin companion kept")
	}
	if exists(filepath.Join(trees, "not-a-key")) {
		t.Error("junk-name tree kept")
	}
	if !exists(filepath.Join(trees, "staging-live123")) {
		t.Error("young staging dir deleted")
	}
	if exists(filepath.Join(trees, "staging-crashed")) {
		t.Error("stale staging dir kept")
	}
	if exists(filepath.Join(cacheDir, "fetcher-old")) {
		t.Error("stale fetcher dir kept")
	}
	if !exists(filepath.Join(cacheDir, "fetcher-new")) {
		t.Error("fresh fetcher dir deleted")
	}
	if stats.TreesRemoved != 4 || stats.FetcherDirsRemoved != 1 {
		t.Errorf("stats = trees %d, fetchers %d; want 4, 1", stats.TreesRemoved, stats.FetcherDirsRemoved)
	}
}

func TestSweepTreesDisabled(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := amber.Open(filepath.Join(dir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sw, err := New(slog.Default(), st, Options{
		StoreDir:     filepath.Join(dir, "store"),
		SnapshotPath: filepath.Join(dir, "refaccess.cbor"),
		Retention:    time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sw.Close()
	if stats, err := sw.Sweep(ctx, -1, false); err != nil || stats.TreesRemoved != 0 {
		t.Fatalf("disabled trees sweep: stats %+v err %v", stats, err)
	}
}
