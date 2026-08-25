package serve

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/jobs-build/amber-store-core/gc"

	"github.com/jobs-build/jobs-iroh/amber"
	"github.com/jobs-build/jobs-iroh/api"
	"github.com/jobs-build/jobs-iroh/reftrack"
)

// gcTestCapture, when set (tests only), receives the gcRunner Run wires.
var gcTestCapture func(*gcRunner)

// gcRunner owns the GC feature: the access tracker, the mark-sweep
// collector, and the periodic sweep. One per server; nil when GC is
// disabled (Options.GCRetention == 0).
type gcRunner struct {
	log       *slog.Logger
	store     *amber.Store
	storeDir  string
	snapPath  string // <data-dir>/refaccess.cbor
	tracker   *reftrack.Tracker
	coll      *gc.Collector
	retention time.Duration
	interval  time.Duration
	minFree   uint64

	sweepMu sync.Mutex // serializes sweeps (hourly loop vs admin gc)

	mu    sync.Mutex // guards stats
	stats api.GCStats

	stop context.CancelFunc
	done chan struct{}
}

// newGCRunner opens the collector next to the already-open store, loads the
// tracker snapshot, and installs the store hooks. The caller wires the
// amberiroh and sched hooks and starts the loop.
func newGCRunner(log *slog.Logger, store *amber.Store, dataDir, storeDir string, opts Options) (*gcRunner, error) {
	tracker := reftrack.New()
	snapPath := filepath.Join(dataDir, "refaccess.cbor")
	if err := tracker.Load(snapPath); err != nil {
		// Corrupt snapshot: start empty (refs re-seed at the next sweep),
		// never fatal — access data is approximately reconstructible.
		log.Warn("gc: tracker snapshot unreadable; starting empty", "error", err)
	}
	coll, err := gc.Open(filepath.Join(storeDir, "closures"), store.Objects(), store.RefStore(), gc.Options{
		Rate:    opts.GCRate,
		MinFree: opts.GCMinFree,
	})
	if err != nil {
		return nil, fmt.Errorf("open gc collector: %w", err)
	}
	g := &gcRunner{
		log:       log,
		store:     store,
		storeDir:  storeDir,
		snapPath:  snapPath,
		tracker:   tracker,
		coll:      coll,
		retention: opts.GCRetention,
		interval:  opts.GCInterval,
		minFree:   opts.GCMinFree,
	}
	g.stats.RetentionNs = int64(opts.GCRetention)
	store.SetObserver(tracker.Touch)
	store.SetRefGuard(coll)
	return g, nil
}

// start launches the periodic sweep loop.
func (g *gcRunner) start(ctx context.Context) {
	loopCtx, cancel := context.WithCancel(ctx)
	g.stop = cancel
	g.done = make(chan struct{})
	go g.loop(loopCtx)
}

func (g *gcRunner) loop(ctx context.Context) {
	defer close(g.done)
	t := time.NewTicker(g.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := g.Sweep(ctx, -1, false); err != nil && ctx.Err() == nil {
				g.log.Warn("gc sweep failed; next tick retries", "error", err)
			}
		}
	}
}

// Close stops the loop, closes the collector (waiting out a running cycle)
// and flushes the tracker. Runs before the store closes.
func (g *gcRunner) Close() {
	if g.stop != nil {
		g.stop()
		<-g.done
	}
	if err := g.coll.Close(); err != nil {
		g.log.Warn("gc collector close", "error", err)
	}
	if err := g.tracker.Flush(g.snapPath); err != nil {
		g.log.Warn("gc tracker flush", "error", err)
	}
}

// Sweep is one full tick: reconcile the tracker with the ref listing,
// expire cold refs, score the store (advisory mark), run a cycle when
// worth it (or force=true), flush the tracker, log and return the stats.
// garbage >= 0 forces that selection line on the cycle; -1 = policy.
func (g *gcRunner) Sweep(ctx context.Context, garbage float64, force bool) (api.GCStats, error) {
	g.sweepMu.Lock()
	defer g.sweepMu.Unlock()
	now := time.Now()

	// 1. Seed & prune.
	refs, err := g.store.ListRefs(ctx)
	if err != nil {
		return g.failSweep(fmt.Errorf("list refs: %w", err))
	}
	names := make([]string, len(refs))
	for i, r := range refs {
		names[i] = r.Name
	}
	g.tracker.Reconcile(names, now)

	// 2. Expire (output before deps — Expired orders them).
	expired := g.tracker.Expired(g.retention, now)
	deleted := 0
	for _, name := range expired {
		e, _ := g.tracker.Get(name)
		if err := g.store.DeleteRef(ctx, name); err != nil {
			g.log.Warn("gc: delete expired ref", "ref", name, "error", err)
			continue
		}
		g.tracker.Forget(name)
		deleted++
		g.log.Debug("gc: expired ref deleted", "ref", name,
			"idle", now.Sub(e.LastAccess).Round(time.Minute))
	}

	// 3. Score, then cycle when worth it. The advisory mark walk is the
	// price of the garbage-percentage stat; ingests ride the write barrier.
	st, err := g.coll.Status(ctx)
	if err != nil {
		return g.failSweep(fmt.Errorf("gc status: %w", err))
	}
	frac := 0.0
	if tot := st.LiveBytes + st.GarbageBytes; tot > 0 {
		frac = float64(st.GarbageBytes) / float64(tot)
	}
	run := force || deleted > 0 || frac >= gc.DefaultGarbage || freeBelow(g.storeDir, g.minFree)
	var cycle gc.CycleStats
	var cycleErr error
	if run {
		cycle, cycleErr = g.coll.Run(ctx, garbage)
	}

	// 4. Persist & report.
	if err := g.tracker.Flush(g.snapPath); err != nil {
		g.log.Warn("gc: tracker flush", "error", err)
	}
	total, pinned := g.tracker.Counts()

	g.mu.Lock()
	g.stats.LastSweepNs = now.UnixNano()
	g.stats.ExpiredLast = deleted
	g.stats.ExpiredTotal += deleted
	g.stats.Pinned = pinned
	g.stats.RefCount = total
	g.stats.DiskBytes = dirSize(g.storeDir)
	g.stats.LiveBytes = st.LiveBytes
	g.stats.GarbageBytes = st.GarbageBytes
	g.stats.LastError = ""
	if cycleErr != nil {
		g.stats.LastError = cycleErr.Error()
	}
	if run && cycleErr == nil {
		g.stats.LastCycleNs = cycle.Start.UnixNano()
		g.stats.LastCycleReaped = len(cycle.Reaped)
		g.stats.LastCycleFreed = cycle.FreedBytes
		g.stats.LastCycleWallNs = int64(cycle.Duration)
	}
	out := g.stats
	g.mu.Unlock()

	args := []any{
		"disk", out.DiskBytes, "refs", total, "pinned", pinned,
		"expired", deleted, "live", st.LiveBytes, "garbage", st.GarbageBytes,
		"garbagePct", fmt.Sprintf("%.1f%%", frac*100),
	}
	if run {
		args = append(args, "reaped", len(cycle.Reaped), "freed", cycle.FreedBytes,
			"cycle", cycle.Duration.Round(time.Millisecond))
	}
	if cycleErr != nil {
		args = append(args, "cycleError", cycleErr)
	}
	g.log.Info("gc sweep", args...)
	return out, cycleErr
}

// failSweep records a sweep-level failure in the stats and returns it.
func (g *gcRunner) failSweep(err error) (api.GCStats, error) {
	g.mu.Lock()
	g.stats.LastError = err.Error()
	out := g.stats
	g.mu.Unlock()
	return out, err
}

// StatsSnapshot returns the last-known stats — no walk, no lock beyond the
// stats mutex (the numbers are "as of the last sweep").
func (g *gcRunner) StatsSnapshot() api.GCStats {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stats
}

// Entry exposes one tracker entry for the refs listing.
func (g *gcRunner) Entry(name string) (reftrack.Entry, bool) {
	return g.tracker.Get(name)
}

// Pin marks an existing ref kept-forever and returns its row.
func (g *gcRunner) Pin(ctx context.Context, name string) (api.RefInfo, error) {
	ri, err := g.store.GetRef(ctx, name)
	if err != nil {
		return api.RefInfo{}, err
	}
	g.tracker.Pin(name)
	if err := g.tracker.Flush(g.snapPath); err != nil {
		g.log.Warn("gc: tracker flush after pin", "error", err)
	}
	return g.refRow(ri), nil
}

// Unpin clears the flag (always succeeds; the ref may already be gone).
func (g *gcRunner) Unpin(ctx context.Context, name string) (api.RefInfo, error) {
	g.tracker.Unpin(name)
	if err := g.tracker.Flush(g.snapPath); err != nil {
		g.log.Warn("gc: tracker flush after unpin", "error", err)
	}
	ri, err := g.store.GetRef(ctx, name)
	if err != nil {
		return api.RefInfo{Name: name}, nil
	}
	return g.refRow(ri), nil
}

func (g *gcRunner) refRow(ri amber.RefInfo) api.RefInfo {
	row := api.RefInfo{Name: ri.Name, Key: ri.Key[:], CreatedNs: ri.CreatedAt.UnixNano()}
	if e, ok := g.tracker.Get(ri.Name); ok {
		row.LastAccessNs = e.LastAccess.UnixNano()
		row.Pinned = e.Pinned
	}
	return row
}

// freeBelow reports whether the filesystem holding path has less than min
// bytes free; min 0 means 5% of the filesystem (the collector's own
// pressure line). Portable across linux and darwin.
func freeBelow(path string, min uint64) bool {
	var st unix.Statfs_t
	if unix.Statfs(path, &st) != nil {
		return false
	}
	if min == 0 {
		min = uint64(st.Blocks) * uint64(st.Bsize) / 20
	}
	return uint64(st.Bavail)*uint64(st.Bsize) < min
}
