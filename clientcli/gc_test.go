package clientcli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Note: package clientcli's TestMain (main_test.go) does not set testStore —
// no test in this suite does, today — so openClientStore already takes the
// real data-dir path here without any save/restore dance. These tests still
// require it explicitly (testStore nil), since the GC construction they
// exercise is skipped entirely on the testStore bypass (clientstore.go).

// hasRef checks ref presence via ListRefs rather than GetKey: GetKey (via
// GetRef) fires the store's read observer — amber/store.go's "called with
// the name of every reference read" — which is exactly the tracker.Touch
// hook gcsweep installs. Using it here to verify "did the sweep delete
// this ref" would itself count as an access and reset the ref's clock,
// making a test assertion silently defeat the next sweep's expiry. Sweep's
// own reconcile step avoids this the same way (ListRefs, not GetRef).
func hasRef(t *testing.T, cs *clientStore, name string) bool {
	t.Helper()
	refs, err := cs.Store.ListRefs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range refs {
		if r.Name == name {
			return true
		}
	}
	return false
}

// Auto-GC: first MaybeGC seeds and stamps; a backdated stamp + aged
// tracker expires a cold ref on the next MaybeGC; a fresh stamp suppresses
// sweeping entirely.
func TestClientMaybeGC(t *testing.T) {
	t.Setenv("JOBS_GC_RETENTION", "300ms")
	ctx := context.Background()
	dataDir := t.TempDir()

	cs, err := openClientStore(dataDir, lockExclusive)
	if err != nil {
		t.Fatal(err)
	}
	if cs.gc == nil {
		t.Fatal("gc sweeper not constructed")
	}
	k, err := cs.Store.IngestFile(ctx, []byte("client gc fixture"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.Store.PutRef(ctx, "build-output:cold", k); err != nil {
		t.Fatal(err)
	}

	cs.MaybeGC(ctx) // seeds the tracker, writes the stamp
	stamp := filepath.Join(dataDir, "gc.stamp")
	if _, err := os.Stat(stamp); err != nil {
		t.Fatalf("stamp not written: %v", err)
	}

	// Fresh stamp: MaybeGC must not sweep (the cold ref survives even aged).
	time.Sleep(400 * time.Millisecond)
	cs.MaybeGC(ctx)
	if !hasRef(t, cs, "build-output:cold") {
		t.Fatal("swept despite fresh stamp")
	}

	// Backdated stamp: the next MaybeGC sweeps and expires the cold ref.
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stamp, old, old); err != nil {
		t.Fatal(err)
	}
	cs.MaybeGC(ctx)
	if hasRef(t, cs, "build-output:cold") {
		t.Fatal("cold ref survived a due sweep")
	}
	cs.Close()
}

func TestClientGCDisabledByEnv(t *testing.T) {
	t.Setenv("JOBS_GC_RETENTION", "0")
	cs, err := openClientStore(t.TempDir(), lockExclusive)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if cs.gc != nil {
		t.Fatal("gc constructed despite retention 0")
	}
	cs.MaybeGC(context.Background()) // must be a no-op, not a panic
}
