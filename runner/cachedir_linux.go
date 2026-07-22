//go:build linux

package runner

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/fables-for-robots/jobs-iroh/amber"
	"github.com/fables-for-robots/jobs-iroh/builddef"
	"github.com/fables-for-robots/jobs-iroh/events"
)

// finalizeCaches runs after a successful build script (design §5.3): prune
// each cache to what the build actually accessed, ingest, and return the
// build-cache:<id>:<platform> refs to publish. Skips (no ref) when the pruned
// tree is empty (keep the last published state) or unchanged from the seed
// (no ref churn — the common no-op rebuild).
//
// Emits one exec.cache/finalized event per cache with result
// updated|unchanged|empty (cache-observability design §4); ev is nil-safe.
func finalizeCaches(ctx context.Context, st *amber.Store, caches []CacheMount, platform string, buildStart time.Time, ev *events.Job) ([]Ref, *Outcome) {
	// Margin absorbs coarse atime granularity: an access in the build's first
	// second could floor below buildStart on a seconds-resolution filesystem,
	// while pre-aged (untouched) files sit at the epoch — decades away.
	cutoff := buildStart.Add(-2 * time.Second)
	var refs []Ref
	for _, cm := range caches {
		start := time.Now()
		if err := pruneCache(cm.HostDir, cutoff); err != nil {
			o := retryable("pruning cache", err)
			return nil, &o
		}
		ents, err := os.ReadDir(cm.HostDir)
		if err != nil {
			o := retryable("pruning cache", err)
			return nil, &o
		}
		if len(ents) == 0 {
			// pruned to empty: keep the last published state (§5.3)
			ev.CacheFinalized(cm.ID, time.Since(start).Milliseconds(), 0, 0, "empty")
			continue
		}
		files, bytes, err := dirStats(cm.HostDir)
		if err != nil {
			o := retryable("pruning cache", err)
			return nil, &o
		}
		c, err := st.IngestDir(ctx, cm.HostDir)
		if err != nil {
			o := retryable("ingesting cache", err)
			return nil, &o
		}
		ms := time.Since(start).Milliseconds()
		if c == cm.SeedKey {
			ev.CacheFinalized(cm.ID, ms, bytes, files, "unchanged")
			continue // unchanged: no ref churn (§5.3)
		}
		ev.CacheFinalized(cm.ID, ms, bytes, files, "updated")
		refs = append(refs, Ref{Name: builddef.CacheRefName(cm.ID, platform), Key: c})
	}
	return refs, nil
}

// pruneCache deletes regular files whose atime predates cutoff, then removes
// now-empty directories bottom-up. Symlinks are kept (readlink does not
// reliably update atime); they are swept only when file pruning empties their
// directory — a dir holding only symlinks stays.
func pruneCache(dir string, cutoff time.Time) error {
	if err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		var st unix.Stat_t
		if err := unix.Lstat(p, &st); err != nil {
			return err
		}
		if time.Unix(st.Atim.Sec, st.Atim.Nsec).Before(cutoff) {
			return os.Remove(p)
		}
		return nil
	}); err != nil {
		return err
	}
	_, err := removeEmptyDirs(dir, true)
	return err
}

// removeEmptyDirs removes empty directories under dir depth-first; keepRoot
// spares dir itself. Reports whether dir ended up empty.
func removeEmptyDirs(dir string, keepRoot bool) (bool, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	empty := true
	for _, e := range ents {
		if !e.IsDir() {
			empty = false
			continue
		}
		sub := filepath.Join(dir, e.Name())
		subEmpty, err := removeEmptyDirs(sub, false)
		if err != nil {
			return false, err
		}
		if subEmpty {
			if err := os.Remove(sub); err != nil {
				return false, err
			}
		} else {
			empty = false
		}
	}
	return empty, nil
}
