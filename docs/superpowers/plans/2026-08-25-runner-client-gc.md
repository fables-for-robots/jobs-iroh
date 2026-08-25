# Runner- and Client-Side GC Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Extend v0.30.0's server GC to the runner's and client's local stores by extracting the sweep machinery into a shared `gcsweep` package, and add a trees sweep that collects `cache/trees/<key>` materializations orphaned by store GC.

**Architecture:** `serve/gc.go`'s `gcRunner` core moves verbatim into `gcsweep.Sweeper` (host-agnostic: tracker + collector + sweep pipeline + loop); `serve` keeps a thin adapter mapping `gcsweep.Stats` → `api.GCStats` so the admin wire shape is byte-identical. The runner constructs a Sweeper after its boot self-test and runs the hourly loop; the client constructs one per command open (observer must be live for touches), sweeps opportunistically behind a 24h stamp plus an explicit `jobs-client gc`. The trees sweep deletes `trees/<k>` dirs whose key the store no longer holds, plus stale `fetcher-*` work dirs.

**Tech Stack:** Go, amber-store-core `gc` package, existing `reftrack`, urfave/cli.

**Design spec:** `docs/design/2026-08-25-runner-client-gc.md` — read it first.

## Global Constraints

- Build/test ONLY via the Nix devShell: `nix develop -c go test ./...`, `nix develop -c go build ./...`.
- macOS cross-vet after any task touching `gcsweep`/`serve`: `nix develop -c env CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go vet ./...`.
- Task 1 is **behavior-frozen**: every existing test in `serve/` (GC e2e, admin frames) must pass UNCHANGED — they are the refactor gate. `api.GCStats` stays byte-identical on the wire.
- No new wire or API surface anywhere in this arc.
- GC disabled = zero-value retention: no tracker, no collector, no observer — existing runnerd/clientcli tests run in that mode and must be unaffected.
- Retention default 720h everywhere; env vars `JOBS_GC_RETENTION`/`JOBS_GC_INTERVAL`/`JOBS_GC_RATE`/`JOBS_GC_MIN_FREE`.
- `nix develop -c gofmt -l .` may list ONLY the pre-existing `runner/capacity_linux_test.go` and `runner/develop_linux.go` — do not format them, do not add to the list.
- Every commit message body ends with:
  `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`
  `Claude-Session: https://claude.ai/code/session_01BD4piG8AjhmpJ8ZeCTMauu`

## File Structure

| File | Responsibility |
|---|---|
| `gcsweep/gcsweep.go` (new) | Host-agnostic Sweeper: options, construction (hooks install), sweep pipeline, loop, close, stats, pin/touch accessors |
| `gcsweep/trees.go` (new, Task 2) | Trees + fetcher-* sweep step |
| `gcsweep/trees_test.go` (new, Task 2) | Trees sweep unit tests |
| `serve/gc.go` | Shrinks to adapter: gcRunner wraps Sweeper, api.GCStats mapping, test capture |
| `serve/apihandler.go` | `dirSize` moves to gcsweep (exported `DirSize`); TStats calls it |
| `runnerd/runnerd.go`, `runnerd/gc.go` (new) | Options fields + Sweeper wiring after self-test |
| `runnerd/gc_test.go` (new) | Runner sweeper construction/sweep test |
| `cmd/jobs-runner/main.go` | Four GC flags |
| `clientcli/clientstore.go`, `clientcli/gc.go` (new) | Sweeper per store open, stamp-gated `maybeGC`, `jobs-client gc` command |
| `clientcli/gc_test.go` (new) | Stamp/auto-sweep + command tests |
| `clientcli/local.go`, `clientcli/image.go`, `clientcli/remote.go`, `clientcli/app.go` | `MaybeGC` call sites + command registration |
| `CLAUDE.md`, `CHANGELOG.md` | Docs |

---

### Task 1: Extract `gcsweep` (behavior-frozen)

**Files:**
- Create: `gcsweep/gcsweep.go`
- Modify: `serve/gc.go` (shrink to adapter), `serve/serve.go` (wiring calls), `serve/apihandler.go` (dirSize → `gcsweep.DirSize`)

**Interfaces:**
- Consumes: `reftrack` package, `amber.Store` (`SetObserver`/`SetRefGuard`/`ListRefs`/`DeleteRef`/`GetRef`), amber-store-core `gc` package — all as used by `serve/gc.go` today.
- Produces (later tasks and serve rely on these EXACT signatures):
  - `gcsweep.Options{StoreDir, SnapshotPath, CacheDir string; Retention, Interval time.Duration; Rate int64; MinFree uint64}` (`CacheDir` unused until Task 2; document it as "reserved: enables the trees sweep").
  - `gcsweep.New(log *slog.Logger, store *amber.Store, opts Options) (*Sweeper, error)` — loads the snapshot (corrupt → warn + empty), opens the collector at `<StoreDir>/closures`, installs `store.SetObserver(tracker.Touch)` and `store.SetRefGuard(coll)`.
  - `(*Sweeper).Sweep(ctx context.Context, garbage float64, force bool) (Stats, error)`
  - `(*Sweeper).Start(ctx context.Context)` — the ticker loop at `Options.Interval`.
  - `(*Sweeper).Close()` — stop loop → hold sweep mutex → collector close → tracker flush.
  - `(*Sweeper).StatsSnapshot() Stats`
  - `(*Sweeper).Entry(name string) (reftrack.Entry, bool)`
  - `(*Sweeper).Touch(name string)`, `(*Sweeper).TouchAll(names []string)`, `(*Sweeper).MarkPinned(name string)` — tracker pass-throughs for host hooks (serve's amberiroh/sched wiring).
  - `(*Sweeper).Guard() amber.RefGuard` — the collector (interface-to-interface assignment to `amberiroh.RefGuard` is structural and legal).
  - `(*Sweeper).Pin(ctx context.Context, name string) (amber.RefInfo, reftrack.Entry, error)` — err = `amber.ErrRefNotFound`-wrapped when absent; flushes.
  - `(*Sweeper).Unpin(ctx context.Context, name string) (amber.RefInfo, reftrack.Entry, bool)` — bool = ref exists; flushes; never errors.
  - `gcsweep.Stats{RetentionNs, LastSweepNs int64; ExpiredLast, ExpiredTotal, Pinned, RefCount int; DiskBytes, LiveBytes, GarbageBytes int64; LastCycleNs int64; LastCycleReaped int; LastCycleFreed, LastCycleWallNs int64; LastError string; TreesRemoved, FetcherDirsRemoved int}` (last two zero until Task 2).
  - `gcsweep.DirSize(dir string) int64` (moved from `serve/apihandler.go:253`).

- [ ] **Step 1: Create the package as a verbatim move**

Create `gcsweep/gcsweep.go` by MOVING code from `serve/gc.go` (read it first — it is the source of truth; ~337 lines) with only these mechanical changes:

1. Package clause + doc:

```go
// Package gcsweep is the host-agnostic GC engine shared by jobs-server,
// jobs-runner and jobs-client: a reftrack access tracker + an
// amber-store-core mark-sweep collector, driven by one sweep pipeline
// (reconcile → expire → advisory status → conditional cycle → flush).
// Hosts construct a Sweeper over their open amber store (which installs
// the store's read observer and PutRef guard), then either Start the
// periodic loop (server, runner) or call Sweep directly (client).
// Extracted verbatim from serve/gc.go (v0.30.0); the sweep semantics —
// protected classes, build-output family ordering and pin mirroring,
// failed-output sibling skip, pin-race re-check — are unchanged.
package gcsweep
```

2. Renames: type `gcRunner` → `Sweeper` (receiver `g` stays); `newGCRunner` → `New` with the signature above (`Options` replaces the `dataDir, storeDir string, opts serve.Options` params: `snapPath := opts.SnapshotPath`, closures dir `filepath.Join(opts.StoreDir, "closures")`, `Rate: opts.Rate`, `MinFree: opts.MinFree`, retention/interval from `opts`); `start` → exported `Start`; `api.GCStats` → `Stats` (declare it with the field list above — the field names match `api.GCStats` exactly, plus the two Trees fields); drop the `api` and remove the now-unused imports.
3. Move `depsOutputName` + the two prefix consts, `loop`, `Close`, `Sweep`, `failSweep`, `StatsSnapshot`, `Entry`, `freeBelow` (with its upstream-mirror comment) verbatim.
4. `Pin`/`Unpin` change shape (they currently build `api.RefInfo` via `refRow`): 

```go
// Pin marks an existing ref kept-forever and returns its record and entry.
func (g *Sweeper) Pin(ctx context.Context, name string) (amber.RefInfo, reftrack.Entry, error) {
	ri, err := g.store.GetRef(ctx, name)
	if err != nil {
		return amber.RefInfo{}, reftrack.Entry{}, err
	}
	g.tracker.Pin(name)
	if err := g.tracker.Flush(g.snapPath); err != nil {
		g.log.Warn("gc: tracker flush after pin", "error", err)
	}
	e, _ := g.tracker.Get(name)
	return ri, e, nil
}

// Unpin clears the flag (always succeeds; the ref may already be gone).
// The returned bool reports whether the ref record still exists.
func (g *Sweeper) Unpin(ctx context.Context, name string) (amber.RefInfo, reftrack.Entry, bool) {
	g.tracker.Unpin(name)
	if err := g.tracker.Flush(g.snapPath); err != nil {
		g.log.Warn("gc: tracker flush after unpin", "error", err)
	}
	e, _ := g.tracker.Get(name)
	ri, err := g.store.GetRef(ctx, name)
	if err != nil {
		return amber.RefInfo{Name: name}, e, false
	}
	return ri, e, true
}
```

(Note: `Unpin`'s `GetRef` fires the observer exactly as today — behavior preserved.)

5. Add the pass-throughs:

```go
// Touch, TouchAll and MarkPinned expose the tracker to host-side hooks
// (the server wires amberiroh pull/pin callbacks and the scheduler's
// runner-report forwarding through these).
func (g *Sweeper) Touch(name string)      { g.tracker.Touch(name) }
func (g *Sweeper) TouchAll(names []string) { g.tracker.TouchAll(names) }
func (g *Sweeper) MarkPinned(name string)  { g.tracker.Pin(name) }

// Guard is the collector as a reference write barrier, for hosts that
// write refs outside amber.PutRef (the server's amberiroh push path).
func (g *Sweeper) Guard() amber.RefGuard { return g.coll }
```

6. Move `dirSize` from `serve/apihandler.go:253-265` here, exported:

```go
// DirSize sums the regular-file bytes under dir, best-effort: stats belong
// to observation, so a racing compaction or permission hiccup yields a
// smaller number, never an error.
func DirSize(dir string) int64 { … verbatim body … }
```

and change the two internal uses (`Sweep`'s step 4) to `DirSize`.
7. `Stats.RetentionNs` is set in `New` exactly as today (`g.stats.RetentionNs = int64(opts.Retention)`).
8. In `Sweep`, immediately before "4. Persist & report", insert the Task 2 hook as a no-op call so Task 2 is additive: `treesRemoved, fetcherRemoved := g.sweepTrees()` with, for now:

```go
// sweepTrees is the cache/trees + fetcher-* sweep; enabled by
// Options.CacheDir in a follow-up task. No-op when unset.
func (g *Sweeper) sweepTrees() (trees, fetchers int) { return 0, 0 }
```

and record `g.stats.TreesRemoved`, `g.stats.FetcherDirsRemoved` in the stats block; append `"trees", treesRemoved` to the log args only when `g.cacheDir != ""` (field added now, set from `opts.CacheDir`).

- [ ] **Step 2: Shrink `serve/gc.go` to the adapter**

Replace the moved code; what remains (complete file contents in spirit — write exactly this shape):

```go
package serve

import (
	"context"
	"log/slog"
	"path/filepath"

	"github.com/jobs-build/jobs-iroh/amber"
	"github.com/jobs-build/jobs-iroh/api"
	"github.com/jobs-build/jobs-iroh/gcsweep"
	"github.com/jobs-build/jobs-iroh/reftrack"
)

// gcTestCapture, when set (tests only), receives the gcRunner Run wires.
// (keep the existing comment about the buffered(1)/single-server constraint)
var gcTestCapture func(*gcRunner)

// gcRunner adapts the shared gcsweep.Sweeper to the admin API surface:
// api.GCStats/api.RefInfo mapping and the serve-side hook wiring points.
// One per server; nil when GC is disabled (Options.GCRetention == 0).
type gcRunner struct {
	sweeper *gcsweep.Sweeper
}

func newGCRunner(log *slog.Logger, store *amber.Store, dataDir, storeDir string, opts Options) (*gcRunner, error) {
	sw, err := gcsweep.New(log, store, gcsweep.Options{
		StoreDir:     storeDir,
		SnapshotPath: filepath.Join(dataDir, "refaccess.cbor"),
		Retention:    opts.GCRetention,
		Interval:     opts.GCInterval,
		Rate:         opts.GCRate,
		MinFree:      opts.GCMinFree,
	})
	if err != nil {
		return nil, err
	}
	return &gcRunner{sweeper: sw}, nil
}

func (g *gcRunner) start(ctx context.Context) { g.sweeper.Start(ctx) }
func (g *gcRunner) Close()                    { g.sweeper.Close() }

func (g *gcRunner) Sweep(ctx context.Context, garbage float64, force bool) (api.GCStats, error) {
	st, err := g.sweeper.Sweep(ctx, garbage, force)
	return toAPIGCStats(st), err
}

func (g *gcRunner) StatsSnapshot() api.GCStats { return toAPIGCStats(g.sweeper.StatsSnapshot()) }

func (g *gcRunner) Entry(name string) (reftrack.Entry, bool) { return g.sweeper.Entry(name) }

func (g *gcRunner) Pin(ctx context.Context, name string) (api.RefInfo, error) {
	ri, e, err := g.sweeper.Pin(ctx, name)
	if err != nil {
		return api.RefInfo{}, err
	}
	return refRow(ri, e), nil
}

func (g *gcRunner) Unpin(ctx context.Context, name string) (api.RefInfo, error) {
	ri, e, ok := g.sweeper.Unpin(ctx, name)
	if !ok {
		return api.RefInfo{Name: name}, nil
	}
	return refRow(ri, e), nil
}

func refRow(ri amber.RefInfo, e reftrack.Entry) api.RefInfo {
	return api.RefInfo{
		Name: ri.Name, Key: ri.Key[:], CreatedNs: ri.CreatedAt.UnixNano(),
		LastAccessNs: e.LastAccess.UnixNano(), Pinned: e.Pinned,
	}
}

// toAPIGCStats maps the shared stats onto the frozen admin wire shape.
func toAPIGCStats(s gcsweep.Stats) api.GCStats {
	return api.GCStats{
		RetentionNs: s.RetentionNs, LastSweepNs: s.LastSweepNs,
		ExpiredLast: s.ExpiredLast, ExpiredTotal: s.ExpiredTotal,
		Pinned: s.Pinned, RefCount: s.RefCount, DiskBytes: s.DiskBytes,
		LiveBytes: s.LiveBytes, GarbageBytes: s.GarbageBytes,
		LastCycleNs: s.LastCycleNs, LastCycleReaped: s.LastCycleReaped,
		LastCycleFreed: s.LastCycleFreed, LastCycleWallNs: s.LastCycleWallNs,
		LastError: s.LastError,
	}
}
```

Behavior-preservation caveats the implementer must check against the old code:
- Old `Unpin` on a vanished ref returned `api.RefInfo{Name: name}` with zero LastAccess/Pinned — the adapter above preserves that (the `!ok` branch).
- Old `Pin` returned the row with LastAccess = the pin's touch — preserved (`Pin` re-Gets the entry after pinning).
- `refRow`'s old nil-entry case (`if e, ok := …; ok`) only mattered for untracked names; after `Pin`/`Unpin` the entry always exists (Pin touches; Unpin on a tracked name keeps it, on an untracked name `Get` returns zero Entry — zero LastAccessNs, false Pinned — matching the old behavior of the `ok=false` branch). Note this reasoning in a comment if it helps, but add no logic.

- [ ] **Step 3: Update serve wiring**

In `serve/serve.go`, the GC block and hook wiring change only in what they reference:
- `if gcTestCapture != nil { gcTestCapture(gcr) }` — unchanged.
- `schedOpts.Touch = gcr.tracker.TouchAll` → `schedOpts.Touch = gcr.sweeper.TouchAll`
- `amberSrv.SetOnAccess(gcr.tracker.Touch)` → `amberSrv.SetOnAccess(gcr.sweeper.Touch)`
- `amberSrv.SetOnPin(gcr.tracker.Pin)` → `amberSrv.SetOnPin(gcr.sweeper.MarkPinned)`
- `amberSrv.SetRefGuard(gcr.coll)` → `amberSrv.SetRefGuard(gcr.sweeper.Guard())`
- `gcr.start(ctx)` — unchanged.

In `serve/apihandler.go`: delete `dirSize`, change its TStats use to `gcsweep.DirSize(svc.storeDir)`, add the import.

In `serve/gc_test.go`: the tests reference `gcr.Sweep`, `gcr.Entry` — signatures unchanged, so the file should need NO edits. If it referenced any now-moved unexported symbol (grep for `depsOutputName`, `freeBelow`, `.tracker`, `.coll` in `serve/*_test.go` first), adjust minimally via the new accessors and report the adjustment.

- [ ] **Step 4: Verify — the whole point of this task**

```bash
nix develop -c go build ./...
nix develop -c go test ./serve/ ./gcsweep/ ./reftrack/ -count=1
nix develop -c go test ./...
nix develop -c env CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go vet ./...
nix develop -c gofmt -l .    # only the two pre-existing runner/ files
```
Expected: everything green with zero test-file changes outside what Step 3's grep forced.

- [ ] **Step 5: Commit**

```bash
git add gcsweep/ serve/
git commit -m "gcsweep: extract the host-agnostic GC engine from serve (behavior-frozen)"
```

---

### Task 2: Trees sweep

**Files:**
- Create: `gcsweep/trees.go`, `gcsweep/trees_test.go`
- Modify: `gcsweep/gcsweep.go` (replace the no-op `sweepTrees`)

**Interfaces:**
- Consumes: `Sweeper.cacheDir` (set from `Options.CacheDir` in Task 1), `g.store.Has(k)`.
- Produces: real `sweepTrees` filling `Stats.TreesRemoved`/`FetcherDirsRemoved`; behavior per design §3.

- [ ] **Step 1: Confirm the trees naming and key parsing**

Read `runner/importexec_linux.go:346-370` (`stagedTree`): confirm tree dirs are created at `<cacheDir>/trees/<k.String()>`. Then find the string-parse function in `amber-store-core/key` (grep `func Parse` in the module cache) — `key.Parse` takes bytes; there is a string form (check for `ParseString`, `FromString`, or whether `stagedTree` round-trips via `k.String()` only). If the key package has NO string-parse, parse via hex/`key.Parse` according to what `String()` emits — read `key.Key.String` to decide, and record the choice in the code comment. A name that fails to parse is junk → delete.

- [ ] **Step 2: Write the failing tests**

Create `gcsweep/trees_test.go`:

```go
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
		filepath.Join(trees, deadName), // valid-shaped key, absent from the store
		filepath.Join(trees, "not-a-key"),
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
	if exists(filepath.Join(trees, deadName)) {
		t.Error("dead-key tree kept")
	}
	if exists(filepath.Join(trees, "not-a-key")) {
		t.Error("junk-name tree kept")
	}
	if exists(filepath.Join(cacheDir, "fetcher-old")) {
		t.Error("stale fetcher dir kept")
	}
	if !exists(filepath.Join(cacheDir, "fetcher-new")) {
		t.Error("fresh fetcher dir deleted")
	}
	if stats.TreesRemoved != 2 || stats.FetcherDirsRemoved != 1 {
		t.Errorf("stats = trees %d, fetchers %d; want 2, 1", stats.TreesRemoved, stats.FetcherDirsRemoved)
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
```

Adjust `deadName` to whatever shape `key.Key.String()` actually emits (Step 1's finding) so it parses as a valid key that the store does not hold; if `String()` includes a type prefix, mimic it.

- [ ] **Step 3: Run to verify failure**

```bash
nix develop -c go test ./gcsweep/ -run TestSweepTrees
```
Expected: FAIL — the no-op `sweepTrees` deletes nothing.

- [ ] **Step 4: Implement `gcsweep/trees.go`**

```go
package gcsweep

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// fetcherStaleAfter is how old an abandoned fetcher-* work dir must be
// before the sweep collects it: live ones are torn down by their own
// cleanup within a job's lifetime, so anything past a day is a crash
// leftover.
const fetcherStaleAfter = 24 * time.Hour

// sweepTrees collects the cache dir's derived state (design §3): staged
// tree materializations whose store object is gone (the store is the
// truth — a tree in use belongs to refs the retention window shields, so
// its objects survive the cycle and Has holds), and fetcher work dirs
// abandoned by a crash. Best-effort: failures log and retry next sweep.
func (g *Sweeper) sweepTrees() (trees, fetchers int) {
	if g.cacheDir == "" {
		return 0, 0
	}
	treesDir := filepath.Join(g.cacheDir, "trees")
	if entries, err := os.ReadDir(treesDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			k, perr := parseTreeKey(e.Name())
			if perr == nil {
				has, herr := g.store.Has(k)
				if herr != nil || has {
					continue // present, or unknown — keep (conservative)
				}
			}
			// Dead key or unparseable name: the materialization is orphaned.
			p := filepath.Join(treesDir, e.Name())
			if err := os.RemoveAll(p); err != nil {
				g.log.Warn("gc: remove orphaned tree", "dir", p, "error", err)
				continue
			}
			trees++
			g.log.Debug("gc: orphaned tree removed", "dir", p)
		}
	}
	cutoff := time.Now().Add(-fetcherStaleAfter)
	if entries, err := os.ReadDir(g.cacheDir); err == nil {
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), "fetcher-") {
				continue
			}
			info, ierr := e.Info()
			if ierr != nil || info.ModTime().After(cutoff) {
				continue
			}
			p := filepath.Join(g.cacheDir, e.Name())
			if err := os.RemoveAll(p); err != nil {
				g.log.Warn("gc: remove stale fetcher dir", "dir", p, "error", err)
				continue
			}
			fetchers++
		}
	}
	return trees, fetchers
}
```

plus `parseTreeKey(name string) (key.Key, error)` implemented per Step 1's finding (in this file, with the finding documented). Delete the Task 1 no-op stub from `gcsweep.go`.

- [ ] **Step 5: Run tests**

```bash
nix develop -c go test ./gcsweep/ ./serve/ -count=1
```
Expected: PASS (serve confirms the server path — no CacheDir — is untouched).

- [ ] **Step 6: Commit**

```bash
git add gcsweep/
git commit -m "gcsweep: sweep orphaned tree materializations and stale fetcher dirs"
```

---

### Task 3: Runner GC

**Files:**
- Modify: `runnerd/runnerd.go` (Options + wiring), `cmd/jobs-runner/main.go` (flags)
- Create: `runnerd/gc_test.go`

**Interfaces:**
- Consumes: `gcsweep.New`/`Start`/`Close`/`Sweep` (Task 1), trees sweep via `Options.CacheDir` (Task 2).
- Produces: `runnerd.Options` gains `GCRetention, GCInterval time.Duration; GCRate int64; GCMinFree uint64` (zero retention = disabled — every existing runnerd test passes zero and must be unaffected).

- [ ] **Step 1: Options + wiring**

`runnerd/runnerd.go` — Options additions (after `SyncConns`):

```go
	// GCRetention enables local-store GC: refs unread for this long are
	// deleted and the store mark-sweeps; everything here is a cache of the
	// server, so a wrong expiry costs one re-pull. 0 disables GC.
	GCRetention time.Duration
	// GCInterval is the sweep period (default 1h when GC is enabled).
	GCInterval time.Duration
	// GCRate caps the GC copier in bytes/s (0 = unlimited). Compaction
	// holds local ref publication for its duration.
	GCRate int64
	// GCMinFree is the free-space floor in bytes under which the collector
	// reaps more aggressively (0 = 5% of the filesystem).
	GCMinFree uint64
```

Wiring in `Run`, immediately AFTER the boot self-test block (the `if o.SkipSelfTest … else if err := bootSelfTest(…)` block ending ~`runnerd.go:198`) — the self-test churns temporary refs and must not race the first sweep:

```go
	if o.GCRetention > 0 {
		if o.GCInterval <= 0 {
			o.GCInterval = time.Hour
		}
		gcw, err := gcsweep.New(log.With("component", "gc"), st, gcsweep.Options{
			StoreDir:     filepath.Join(o.DataDir, "store"),
			SnapshotPath: filepath.Join(o.DataDir, "refaccess.cbor"),
			CacheDir:     cacheDir,
			Retention:    o.GCRetention,
			Interval:     o.GCInterval,
			Rate:         o.GCRate,
			MinFree:      o.GCMinFree,
		})
		if err != nil {
			return fmt.Errorf("open gc: %w", err)
		}
		defer gcw.Close()
		gcw.Start(ctx)
	}
```

(`defer gcw.Close()` lands after `defer st.Close()` in source order → runs before it, LIFO — the required close order. State this in a one-line comment.)

`cmd/jobs-runner/main.go` — add the four flags (mirroring `cmd/jobs-server`'s, same names/envs/defaults: `gc-retention` 720h `JOBS_GC_RETENTION`, `gc-interval` 1h `JOBS_GC_INTERVAL`, `gc-rate` `JOBS_GC_RATE` with the compaction-stall sentence in Usage, `gc-min-free` `JOBS_GC_MIN_FREE`) and thread them into the `runnerd.Options` literal (`GCRetention: c.Duration("gc-retention")`, etc.). Add `"time"` to imports if absent.

- [ ] **Step 2: Write the test**

Create `runnerd/gc_test.go` — the runner's GC is gcsweep over the runner's store layout; the runner-specific claims to verify are the layout paths and that protected classes + fresh refs survive while cold ones expire:

```go
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
```

- [ ] **Step 3: Run**

```bash
nix develop -c go test ./runnerd/ -count=1
nix develop -c go build ./...
```
Expected: new test PASS, whole existing runnerd suite untouched (zero-retention Options in every existing test = GC never constructed).

- [ ] **Step 4: Commit**

```bash
git add runnerd/ cmd/jobs-runner/
git commit -m "runner: local store GC (gcsweep over the private cache, on by default)"
```

---

### Task 4: Client GC

**Files:**
- Modify: `clientcli/clientstore.go` (Sweeper + maybeGC + stamp), `clientcli/app.go` (register `gcCmd`), `clientcli/local.go`, `clientcli/image.go`, `clientcli/remote.go` (MaybeGC call sites)
- Create: `clientcli/gc.go` (the command + retention env helper), `clientcli/gc_test.go`

**Interfaces:**
- Consumes: `gcsweep` (Tasks 1–2).
- Produces: `(*clientStore).MaybeGC(ctx context.Context)` (stamp-gated auto sweep, silent unless something reclaimed); `jobs-client gc [--garbage F] [--retention D] [--data-dir …]`.

- [ ] **Step 1: clientstore changes**

In `clientcli/clientstore.go`:

1. `clientStore` gains `gc *gcsweep.Sweeper` (nil = disabled).
2. In `openClientStore`, after `amber.Open` succeeds and before the return, construct it (skip entirely on the `testStore` path — the bypass must stay behavior-identical for the whole existing suite):

```go
	cs := &clientStore{Store: st, CacheDir: cacheDir, dataDir: dataDir,
		release: release, closeFn: st.Close}
	if ret := clientGCRetention(); ret > 0 {
		sw, err := gcsweep.New(slog.Default(), st, gcsweep.Options{
			StoreDir:     filepath.Join(dataDir, "store"),
			SnapshotPath: filepath.Join(dataDir, "refaccess.cbor"),
			CacheDir:     cacheDir,
			Retention:    ret,
		})
		if err != nil {
			// GC must never block a build: warn and run without it.
			fmt.Fprintf(os.Stderr, "gc disabled for this run: %v\n", err)
		} else {
			cs.gc = sw
		}
	}
	return cs, nil
```

(The Sweeper's observer is what records touches during the command — constructing it per open is required for correctness, not just for sweeping; its Close flushes the tracker snapshot.)
3. `Close` closes the sweeper first: `if cs.gc != nil { cs.gc.Close() }` before `cs.closeFn()`.
4. Add:

```go
// gcCheckEvery is how often the auto sweep is allowed to run; the stamp
// file's mtime is the record.
const gcCheckEvery = 24 * time.Hour

// MaybeGC runs the opportunistic sweep: at most once per gcCheckEvery
// (stamp-gated), after a command's main work, while the flock is still
// held. Silent unless something was reclaimed; never fails the command.
func (cs *clientStore) MaybeGC(ctx context.Context) {
	if cs.gc == nil {
		return
	}
	stamp := filepath.Join(cs.dataDir, "gc.stamp")
	if info, err := os.Stat(stamp); err == nil && time.Since(info.ModTime()) < gcCheckEvery {
		return
	}
	stats, err := cs.gc.Sweep(ctx, -1, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gc sweep failed (will retry within %s): %v\n", gcCheckEvery, err)
		return
	}
	_ = os.WriteFile(stamp, nil, 0o644) // mtime is the record; content unused
	if stats.ExpiredLast > 0 || stats.LastCycleFreed > 0 || stats.TreesRemoved > 0 {
		fmt.Fprintf(os.Stderr, "gc: expired %d refs, freed %d bytes, removed %d orphaned trees\n",
			stats.ExpiredLast, stats.LastCycleFreed, stats.TreesRemoved)
	}
}
```

- [ ] **Step 2: `clientcli/gc.go` — env helper + command**

```go
package clientcli

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/urfave/cli/v2"

	"github.com/jobs-build/jobs-iroh/gcsweep"
)

// clientGCRetention is the auto path's retention: JOBS_GC_RETENTION
// (Go duration; "0" disables), default 30 days. An unparsable value
// disables with a warning rather than failing a build command.
func clientGCRetention() time.Duration {
	v := os.Getenv("JOBS_GC_RETENTION")
	if v == "" {
		return 720 * time.Hour
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		fmt.Fprintf(os.Stderr, "ignoring invalid JOBS_GC_RETENTION %q; gc disabled\n", v)
		return 0
	}
	return d
}

func gcCmd() *cli.Command {
	var dataDir string
	return &cli.Command{
		Name:  "gc",
		Usage: "sweep the local store now: expire unused refs, mark-sweep the objects, collect orphaned trees",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "data-dir", EnvVars: []string{"JOBS_DATA_DIR"}, Value: defaultDataDir(), Usage: "client data directory (embedded store + cache)", Destination: &dataDir},
			&cli.Float64Flag{Name: "garbage", Value: -1, Usage: "force the pack selection line 0..1 (default: policy)"},
			&cli.DurationFlag{Name: "retention", Usage: "override the expiry window for this run (default: JOBS_GC_RETENTION or 720h)"},
		},
		Action: func(c *cli.Context) error {
			ctx, stop := signalCtx(c.Context)
			defer stop()
			ret := clientGCRetention()
			if c.IsSet("retention") {
				ret = c.Duration("retention")
			}
			if ret <= 0 {
				return cli.Exit("gc is disabled (retention 0)", 1)
			}
			cs, err := openClientStore(dataDir, lockExclusive)
			if err != nil {
				return err
			}
			defer cs.Close()
			sw := cs.gc
			if sw == nil || c.IsSet("retention") {
				// The store-open sweeper used the env retention; a --retention
				// override needs its own. Two collectors must never coexist on
				// one store (each sweeps <store>/closures and installs the
				// PutRef guard), so close the open one before constructing
				// the override.
				if cs.gc != nil {
					cs.gc.Close()
					cs.gc = nil
				}
				sw, err = gcsweep.New(slog.Default(), cs.Store, gcsweep.Options{
					StoreDir:     filepath.Join(cs.dataDir, "store"),
					SnapshotPath: filepath.Join(cs.dataDir, "refaccess.cbor"),
					CacheDir:     cs.CacheDir,
					Retention:    ret,
				})
				if err != nil {
					return err
				}
				defer sw.Close()
			}
			stats, err := sw.Sweep(ctx, c.Float64("garbage"), true)
			if err != nil {
				return err
			}
			w := c.App.Writer
			pct := 0.0
			if tot := stats.LiveBytes + stats.GarbageBytes; tot > 0 {
				pct = 100 * float64(stats.GarbageBytes) / float64(tot)
			}
			fmt.Fprintf(w, "disk:      %d bytes\n", stats.DiskBytes)
			fmt.Fprintf(w, "refs:      %d (%d expired this sweep)\n", stats.RefCount, stats.ExpiredLast)
			fmt.Fprintf(w, "store:     live %d, garbage %d (%.1f%%)\n", stats.LiveBytes, stats.GarbageBytes, pct)
			fmt.Fprintf(w, "trees:     %d orphaned removed, %d stale fetcher dirs\n", stats.TreesRemoved, stats.FetcherDirsRemoved)
			if stats.LastCycleNs != 0 {
				fmt.Fprintf(w, "cycle:     reaped %d packs, freed %d bytes in %s\n",
					stats.LastCycleReaped, stats.LastCycleFreed, time.Duration(stats.LastCycleWallNs).Round(time.Millisecond))
			}
			if stats.LastError != "" {
				return cli.Exit("gc cycle error: "+stats.LastError, 1)
			}
			return nil
		},
	}
}
```

IMPORTANT correctness note baked into the code above (verify, don't skip): two `gc.Collector`s cannot coexist on one store (`gc.Open` sweeps `<store>/closures` and both would install guards) — when `--retention` handling needs a fresh sweeper, CLOSE the store-open one first, exactly as written.

Register in `clientcli/app.go`'s command list after `adminCmd()`: `gcCmd(),`.

- [ ] **Step 3: MaybeGC call sites**

Add `cs.MaybeGC(ctx)` as the last act before the successful return of each store-touching command action, AFTER its main work (find the exact spot by reading each action; the clientStore variable name varies):
- `clientcli/local.go`: the `build` action, the `run` action (after the entrypoint exec result is determined but before returning its exit-code error — sweep even on a failed entrypoint: the BUILD ran; place it before the exit-code translation), and `develop` (after the PTY session ends).
- `clientcli/image.go`: the `image` action after the tar is written.
- `clientcli/remote.go`: the `remote-build` action after pull-home completes.
Do NOT add to logs/watch/diagnose/status/admin/tui — they never open the client store. Skip any action where the store handle isn't in scope at the return; report which sites you instrumented.

- [ ] **Step 4: Write the tests**

Create `clientcli/gc_test.go`:

```go
package clientcli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
	if _, ok, _ := cs.Store.GetKey(ctx, "build-output:cold"); !ok {
		t.Fatal("swept despite fresh stamp")
	}

	// Backdated stamp: the next MaybeGC sweeps and expires the cold ref.
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stamp, old, old); err != nil {
		t.Fatal(err)
	}
	cs.MaybeGC(ctx)
	if _, ok, _ := cs.Store.GetKey(ctx, "build-output:cold"); ok {
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
```

Check first whether the existing clientcli suite sets `testStore` globally in a TestMain — if so, these two tests must run with `testStore` nil (save/restore around them) since they need the real data-dir path; note what you did.

- [ ] **Step 5: Run**

```bash
nix develop -c go test ./clientcli/ -count=1
nix develop -c go build ./...
```
Expected: new tests PASS; existing suite untouched (testStore path skips GC construction).

- [ ] **Step 6: Commit**

```bash
git add clientcli/
git commit -m "jobs-client: local store GC — stamp-gated auto sweep and a gc command"
```

---

### Task 5: Docs + full verification

**Files:**
- Modify: `CLAUDE.md`, `CHANGELOG.md`

- [ ] **Step 1: CLAUDE.md**

- Package map: add `gcsweep/` row — "Host-agnostic GC engine (extracted from serve): reftrack + collector + sweep pipeline + trees/fetcher-dir sweep; embedded by server, runner (hourly loop) and client (stamp-gated per-command sweep + `gc` command)." Trim the serve row's GC clause to reference it.
- `jobs-runner` binary docs: add the four GC flags with a one-liner ("local cache GC, on by default; 0 disables").
- `jobs-client` docs: add `gc` to the command list and one sentence on the auto sweep (24h stamp, `JOBS_GC_RETENTION`).

- [ ] **Step 2: CHANGELOG.md** — new `## Unreleased` section on top: runner + client GC via the shared gcsweep extraction, trees/fetcher-dir sweep, `jobs-client gc`, no wire changes.

- [ ] **Step 3: Full verification**

```bash
nix develop -c gofmt -l .        # only the two pre-existing runner/ files
nix develop -c go build ./...
nix develop -c go test ./...
nix develop -c env CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go vet ./...
nix develop -c go run ./cmd/jobs-client build   # self-build; rerun must print (cached)
```

Note: the self-build now runs with client GC enabled by default — its second (cached) run doubles as a smoke test that the auto path never breaks a build. If the first `jobs-client build` prints a gc line, that is expected behavior, not a failure.

- [ ] **Step 4: Commit**

```bash
git add CLAUDE.md CHANGELOG.md
git commit -m "docs: runner/client GC — CLAUDE.md and changelog"
```

---

## Plan Self-Review Notes

- **Spec coverage:** design §2 → Task 1; §3 → Task 2; §4 → Task 3; §5 → Task 4; §6 log line rides the extraction, client lines in Task 4; §7 no-wire-change is a global constraint; §8 tests distributed per task; §9 order matches.
- **Known judgment points for implementers, stated in-task:** key string-parse function (Task 2 Step 1); serve test-file grep (Task 1 Step 3); testStore TestMain interaction (Task 4 Step 4); MaybeGC call-site anchoring (Task 4 Step 3).
- **Type consistency:** `gcsweep.Options`/`Stats`/`Sweeper` methods appear identically in Tasks 1–4; serve adapter signatures match what `serve/apihandler.go` already calls.
