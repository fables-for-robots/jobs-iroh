// Command jobs-runner is the jobs-iroh worker: it dials the server's iroh
// endpoint twice (NATS tunnel + amber sync), pulls work-queue jobs for every
// size class that fits its capacity, runs the ported stage drivers in the
// namespace sandbox against a private cache store, and reports results.
package main

import (
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/urfave/cli/v2"

	"github.com/jobs-build/jobs-iroh/runnerd"
	"github.com/jobs-build/jobs-iroh/sandbox"
	"github.com/jobs-build/jobs-iroh/version"
	"github.com/jobs-build/jobs-iroh/wire"
)

func main() {
	sandbox.Init()

	app := &cli.App{
		Name:    "jobs-runner",
		Version: version.Version,
		Usage:   "jobs-iroh build runner: pulls jobs from a jobs-server over iroh and executes them",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "server",
				Usage:    "endpoint ID of the jobs-server (printed in its startup log)",
				EnvVars:  []string{"JOBS_SERVER"},
				Required: true,
			},
			&cli.StringSliceFlag{
				Name:    "addr",
				Usage:   "direct server address host:port (repeatable; skips discovery and relays)",
				EnvVars: []string{"JOBS_SERVER_ADDR"},
			},
			&cli.StringFlag{
				Name:    "data-dir",
				Usage:   "directory for the private store, fetcher cache and runner identity",
				EnvVars: []string{"JOBS_DATA_DIR"},
				Value:   defaultDataDir(),
			},
			&cli.StringFlag{
				Name:    "size",
				Usage:   "cap capacity to a size-class ladder rung (e.g. c1-m2); empty uses the full detected machine capacity",
				EnvVars: []string{"JOBS_RUNNER_SIZE"},
			},
			&cli.IntFlag{
				Name:    "slots",
				Usage:   "cap on concurrently running jobs (0 = resource accounting only)",
				EnvVars: []string{"JOBS_RUNNER_SLOTS"},
			},
			&cli.StringFlag{
				Name:    "cpu",
				Usage:   "cap the advertised CPU capacity (e.g. 8 or 8000m); default: auto-detected (cgroup-aware)",
				EnvVars: []string{"JOBS_RUNNER_CPU"},
			},
			&cli.StringFlag{
				Name:    "memory",
				Usage:   "cap the advertised memory capacity (e.g. 28Gi); default: auto-detected (cgroup-aware)",
				EnvVars: []string{"JOBS_RUNNER_MEM"},
			},
			&cli.StringFlag{
				Name:    "name",
				Usage:   "display name announced to the server (default: hostname)",
				EnvVars: []string{"JOBS_RUNNER_NAME"},
			},
			&cli.BoolFlag{
				Name:    "skip-self-test",
				Usage:   "skip the boot self-test build that verifies this host can execute sandboxed builds",
				EnvVars: []string{"JOBS_RUNNER_SKIP_SELF_TEST"},
			},
			&cli.IntFlag{
				Name:    "sync-conns",
				Usage:   "parallel connections per store transfer (1-16; 1 disables sharding)",
				EnvVars: []string{"JOBS_SYNC_CONNS"},
				Value:   4,
			},
			&cli.StringFlag{
				Name:    "log-level",
				Usage:   "debug, info, warn or error",
				EnvVars: []string{"JOBS_LOG_LEVEL"},
				Value:   "info",
			},
		},
		Action: func(c *cli.Context) error {
			log, err := newLogger(c.String("log-level"))
			if err != nil {
				return err
			}
			if s := c.String("size"); s != "" {
				if _, err := wire.ParseClass(s); err != nil {
					return err
				}
			}
			ctx, stop := signal.NotifyContext(c.Context, syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			return runnerd.Run(ctx, runnerd.Options{
				ServerID:     c.String("server"),
				Addrs:        c.StringSlice("addr"),
				DataDir:      c.String("data-dir"),
				Size:         c.String("size"),
				Slots:        c.Int("slots"),
				CPU:          c.String("cpu"),
				Memory:       c.String("memory"),
				Name:         c.String("name"),
				SkipSelfTest: c.Bool("skip-self-test"),
				SyncConns:    max(1, c.Int("sync-conns")),
				Logger:       log,
			})
		},
	}

	if err := app.Run(os.Args); err != nil {
		slog.Error("jobs-runner failed", "error", err)
		os.Exit(1)
	}
}

func defaultDataDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return home + "/.local/share/jobs-iroh/runner"
	}
	return "./jobs-iroh-runner"
}

func newLogger(level string) (*slog.Logger, error) {
	var l slog.Level
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "info":
		l = slog.LevelInfo
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		return nil, errors.New("invalid --log-level (want debug, info, warn or error)")
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l})), nil
}
