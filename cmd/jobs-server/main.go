// Command jobs-server is the single shared authority of a jobs-iroh
// deployment: one iroh endpoint serving the client build API, the runner
// NATS tunnel, CAS sync, and the admin API over an embedded amber store and
// an embedded NATS server.
package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/urfave/cli/v2"

	"github.com/jobs-build/jobs-iroh/sandbox"
	"github.com/jobs-build/jobs-iroh/serve"
	"github.com/jobs-build/jobs-iroh/version"
)

func main() {
	sandbox.Init()

	app := &cli.App{
		Name:    "jobs-server",
		Version: version.Version,
		Usage:   "jobs-iroh build server: iroh endpoint + embedded NATS + embedded amber store",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "data-dir",
				Usage:   "directory for the store, NATS state and endpoint key",
				EnvVars: []string{"JOBS_DATA_DIR"},
				Value:   defaultDataDir(),
			},
			&cli.StringFlag{
				Name:    "bind",
				Usage:   "UDP bind address (host:port); empty binds the default wildcard",
				EnvVars: []string{"JOBS_BIND"},
			},
			&cli.StringFlag{
				Name:    "relay",
				Usage:   "relay server URL to use as the fallback path (default: nearest of the built-in relays)",
				EnvVars: []string{"JOBS_RELAY"},
			},
			&cli.StringSliceFlag{
				Name:  "advertise-addr",
				Usage: "direct address to advertise, ip or ip:port (repeatable; replaces interface auto-detection)",
			},
			&cli.BoolFlag{
				Name:  "no-announce",
				Usage: "skip discovery export entirely (no relay, no mDNS, no pkarr; clients must dial --addr)",
			},
			&cli.IntFlag{
				Name:    "data-endpoints",
				Usage:   "extra UDP endpoints for sharded store transfers (0 disables; one socket caps well below fast links)",
				EnvVars: []string{"JOBS_DATA_ENDPOINTS"},
				Value:   3,
			},
			&cli.StringFlag{
				Name:    "log-level",
				Usage:   "debug, info, warn or error",
				EnvVars: []string{"JOBS_LOG_LEVEL"},
				Value:   "info",
			},
			&cli.DurationFlag{
				Name:    "gc-retention",
				Usage:   "delete refs unread for this long and run store GC (0 disables)",
				EnvVars: []string{"JOBS_GC_RETENTION"},
				Value:   720 * time.Hour, // 30 days
			},
			&cli.DurationFlag{
				Name:    "gc-interval",
				Usage:   "period of the GC sweep job",
				EnvVars: []string{"JOBS_GC_INTERVAL"},
				Value:   time.Hour,
			},
			&cli.Int64Flag{
				Name:    "gc-rate",
				Usage:   "GC copier bandwidth cap in bytes/s (0 = unlimited)",
				EnvVars: []string{"JOBS_GC_RATE"},
			},
			&cli.Uint64Flag{
				Name:    "gc-min-free",
				Usage:   "free-space floor in bytes for aggressive GC (0 = 5% of the filesystem)",
				EnvVars: []string{"JOBS_GC_MIN_FREE"},
			},
		},
		Action: func(c *cli.Context) error {
			log, err := newLogger(c.String("log-level"))
			if err != nil {
				return err
			}
			opts := serve.Options{
				DataDir:        c.String("data-dir"),
				Announce:       !c.Bool("no-announce"),
				AdvertiseAddrs: c.StringSlice("advertise-addr"),
				RelayURL:       c.String("relay"),
				DataEndpoints:  c.Int("data-endpoints"),
				GCRetention:    c.Duration("gc-retention"),
				GCInterval:     c.Duration("gc-interval"),
				GCRate:         c.Int64("gc-rate"),
				GCMinFree:      c.Uint64("gc-min-free"),
				Logger:         log,
			}
			if bind := c.String("bind"); bind != "" {
				addr, err := netip.ParseAddrPort(bind)
				if err != nil {
					return fmt.Errorf("parse --bind: %w", err)
				}
				opts.BindAddr = addr
			}
			ctx, stop := signal.NotifyContext(c.Context, syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			return serve.Run(ctx, opts)
		},
	}

	if err := app.Run(os.Args); err != nil {
		slog.Error("jobs-server failed", "error", err)
		os.Exit(1)
	}
}

func defaultDataDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return home + "/.local/share/jobs-iroh/server"
	}
	return "./jobs-iroh-server"
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
