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
