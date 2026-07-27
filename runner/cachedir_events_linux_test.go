//go:build linux

package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jobs-build/jobs-iroh/events"
)

// mkCacheDir writes one file ("hello", 5 bytes) into a fresh cache host dir.
func mkCacheDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestFinalizeCachesEmitsFinalizedEvents covers the three results
// (cache-observability design §4): updated (fresh content), unchanged
// (ingest key == seed key), empty (everything pruned; last state kept).
func TestFinalizeCachesEmitsFinalizedEvents(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const platform = "linux/amd64"
	past := time.Now().Add(-time.Minute) // cutoff before the files' atimes ⇒ kept

	// updated: fresh dir, zero SeedKey ⇒ ingest key differs.
	updatedDir := mkCacheDir(t)

	// unchanged: SeedKey = ingest of the same content.
	unchangedDir := mkCacheDir(t)
	seedKey, err := st.IngestDir(ctx, unchangedDir)
	if err != nil {
		t.Fatal(err)
	}

	ev, sink := capJob(t)
	refs, out := finalizeCaches(ctx, st, []CacheMount{
		{ID: "upd", Path: "/u", HostDir: updatedDir},
		{ID: "same", Path: "/s", HostDir: unchangedDir, SeedKey: seedKey},
	}, platform, past, ev)
	if out != nil {
		t.Fatalf("finalizeCaches outcome: %+v", out)
	}
	if len(refs) != 1 {
		t.Fatalf("want 1 ref (only upd), got %+v", refs)
	}
	evs := sink.events()
	if len(evs) != 2 {
		t.Fatalf("want 2 events, got %d: %+v", len(evs), evs)
	}
	upd, same := evs[0], evs[1]
	if upd.Type != events.TypeExecCache || upd.Data["cache"] != "upd" ||
		upd.Data["stage"] != "finalized" || upd.Data["result"] != "updated" {
		t.Fatalf("updated event wrong: %+v", upd)
	}
	if evInt(t, upd.Data["bytes"]) != 5 || evInt(t, upd.Data["files"]) != 1 {
		t.Fatalf("updated sizes wrong: %+v", upd.Data)
	}
	if same.Data["cache"] != "same" || same.Data["result"] != "unchanged" ||
		evInt(t, same.Data["bytes"]) != 5 {
		t.Fatalf("unchanged event wrong: %+v", same)
	}

	// empty: a future buildStart prunes everything ⇒ result empty, zeros, no ref.
	emptyDir := mkCacheDir(t)
	ev2, sink2 := capJob(t)
	refs2, out2 := finalizeCaches(ctx, st, []CacheMount{{ID: "gone", Path: "/g", HostDir: emptyDir}},
		platform, time.Now().Add(time.Hour), ev2)
	if out2 != nil || len(refs2) != 0 {
		t.Fatalf("empty case: refs=%v out=%+v", refs2, out2)
	}
	evs2 := sink2.events()
	if len(evs2) != 1 || evs2[0].Data["result"] != "empty" ||
		evInt(t, evs2[0].Data["bytes"]) != 0 || evInt(t, evs2[0].Data["files"]) != 0 {
		t.Fatalf("empty event wrong: %+v", evs2)
	}
}
