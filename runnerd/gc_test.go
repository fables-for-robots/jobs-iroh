package runnerd

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/jobs-build/jobs-iroh/amber"
	"github.com/jobs-build/jobs-iroh/gcsweep"
)

// The runner wires gcsweep over <data-dir>/store with the fetcher cache
// dir; this exercises that layout end to end: a cold ref expires, a fresh
// one and a protected one survive.
func TestRunnerGCSweep(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	st, err := amber.Open(filepath.Join(dataDir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	k, err := st.IngestFile(ctx, []byte("runner gc fixture"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"build-output:cold", "build-output:warm", "shell:linux/amd64"} {
		if err := st.PutRef(ctx, name, k); err != nil {
			t.Fatal(err)
		}
	}

	sw, err := gcsweep.New(slog.Default(), st, gcsweep.Options{
		StoreDir:     filepath.Join(dataDir, "store"),
		SnapshotPath: filepath.Join(dataDir, "refaccess.cbor"),
		CacheDir:     filepath.Join(dataDir, "cache"),
		Retention:    300 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sw.Close()

	if _, err := sw.Sweep(ctx, -1, false); err != nil { // seeds the tracker
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond) // age everything past retention
	sw.Touch("build-output:warm")      // a job read it (ensureRef → observer)

	stats, err := sw.Sweep(ctx, -1, true)
	if err != nil {
		t.Fatalf("sweep: %v (stats %+v)", err, stats)
	}
	assertRef := func(name string, want bool) {
		t.Helper()
		_, ok, err := st.GetKey(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if ok != want {
			t.Errorf("%s present=%v want %v", name, ok, want)
		}
	}
	assertRef("build-output:cold", false)
	assertRef("build-output:warm", true)
	assertRef("shell:linux/amd64", true)
}
