package bootstrap

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/jobs-build/jobs-iroh/amber"
)

// openStore opens a fresh embedded store under a test temp dir (the jobs-iroh
// counterpart of jobs' ambertest daemon; no signer — refs are unsigned).
func openStore(t *testing.T) *amber.Store {
	t.Helper()
	st, err := amber.Open(t.TempDir())
	if err != nil {
		t.Fatalf("amber.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store close: %v", err)
		}
	})
	return st
}

func TestSeededPlatforms(t *testing.T) {
	got := SeededPlatforms()
	if len(got) == 0 {
		t.Fatal("no embedded seed platforms")
	}
	want := map[string]bool{"linux/amd64": false, "linux/arm64": false}
	for _, p := range got {
		if _, ok := want[p]; ok {
			want[p] = true
		}
	}
	for p, seen := range want {
		if !seen {
			t.Errorf("expected embedded seed platform %q", p)
		}
	}
}

func TestSeed_PublishesAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	platforms := SeededPlatforms()
	if err := Seed(ctx, st, platforms, log); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	for _, p := range platforms {
		for _, ref := range []string{"fetcher:tarball+https:" + p, "shell:" + p, "fetcher:hostmusl:" + p, "fetcher:github:" + p} {
			_, ok, err := st.GetKey(ctx, ref)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Errorf("ref %s not published", ref)
			}
		}
	}

	// Idempotent: re-seeding an already-populated store is a no-op (the
	// skip-if-ref-exists guard, not key-stability — amber hashes mtime/uid/gid,
	// so re-ingesting the same bytes yields a different key; see the fleet
	// seed-race note in architecture/bootstrap.md §8).
	if err := Seed(ctx, st, platforms, log); err != nil {
		t.Fatalf("re-Seed: %v", err)
	}
}

// TestSeed_RefreshesStaleRef: a fetcher ref that exists but was not seeded
// from the currently-embedded blob (an older binary's seed, or a manual
// registration) must be re-published from the embedded artifact — otherwise a
// fixed seed fetcher never rolls out (refs used to be skip-if-present
// forever). A second Seed over the refreshed store must then be a true no-op
// (no re-ingest churn — amber keys are metadata-sensitive, so a re-ingest
// would visibly change the key).
func TestSeed_RefreshesStaleRef(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	platforms := SeededPlatforms()
	if len(platforms) == 0 {
		t.Skip("no embedded seed")
	}
	ref := "fetcher:tarball+https:" + platforms[0]

	// Simulate the pre-refresh state: the ref exists (pointing at some other
	// artifact) with no seed marker recording which blob produced it.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fetch"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	stale, err := st.IngestDir(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutRef(ctx, ref, stale); err != nil {
		t.Fatal(err)
	}

	if err := Seed(ctx, st, platforms, log); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	got, ok, err := st.GetKey(ctx, ref)
	if err != nil || !ok {
		t.Fatalf("ref %s missing after Seed (ok=%v err=%v)", ref, ok, err)
	}
	if got == stale {
		t.Fatalf("ref %s still points at the stale artifact — seed did not refresh", ref)
	}

	// Re-seeding must now be a no-op: the key stays byte-identical.
	if err := Seed(ctx, st, platforms, log); err != nil {
		t.Fatalf("re-Seed: %v", err)
	}
	again, ok, err := st.GetKey(ctx, ref)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if again != got {
		t.Fatalf("re-Seed churned the ref key (%s → %s) — marker fast-path broken", got, again)
	}
}
