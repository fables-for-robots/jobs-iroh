// Command jobs-registry is a read-only OCI registry that serves jobs build
// outputs as pullable container images named jobs:<build-K>. Image content is
// synced on demand from a jobs-server's store over iroh and assembled into
// two-layer images (runtime closure + artifact) whose blobs are cached on
// disk with last-read expiry.
package main

import (
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/urfave/cli/v2"

	"github.com/fables-for-robots/jobs-iroh/registryd"
	"github.com/fables-for-robots/jobs-iroh/sandbox"
	"github.com/fables-for-robots/jobs-iroh/version"
)

func main() {
	sandbox.Init()

	app := &cli.App{
		Name:    "jobs-registry",
		Version: version.Version,
		Usage:   "OCI registry serving jobs build outputs as pullable images (docker pull <registry>/jobs:<build-K>)",
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
				Usage:   "directory for the private store, blob cache and image records",
				EnvVars: []string{"JOBS_DATA_DIR"},
				Value:   defaultDataDir(),
			},
			&cli.StringFlag{
				Name:    "listen",
				Usage:   "HTTP listen address for the registry API",
				EnvVars: []string{"JOBS_REGISTRY_LISTEN"},
				Value:   ":5000",
			},
			&cli.DurationFlag{
				Name:    "cache-ttl",
				Usage:   "delete cached blobs not pulled for this long",
				EnvVars: []string{"JOBS_REGISTRY_CACHE_TTL"},
				Value:   24 * time.Hour,
			},
			&cli.StringFlag{
				Name:    "default-platform",
				Usage:   "os/arch stamped on image configs when a build's definition is unavailable (default: host platform)",
				EnvVars: []string{"JOBS_DEFAULT_PLATFORM"},
			},
			&cli.BoolFlag{
				Name:    "no-shell",
				Usage:   "do not bake the platform shell (/bin/sh, /jobs/shell) into images; script entrypoints then need no shell",
				EnvVars: []string{"JOBS_REGISTRY_NO_SHELL"},
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
			ctx, stop := signal.NotifyContext(c.Context, syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			return registryd.Run(ctx, registryd.Options{
				ServerID:        c.String("server"),
				Addrs:           c.StringSlice("addr"),
				DataDir:         c.String("data-dir"),
				Listen:          c.String("listen"),
				CacheTTL:        c.Duration("cache-ttl"),
				DefaultPlatform: c.String("default-platform"),
				NoShell:         c.Bool("no-shell"),
				Logger:          log,
			})
		},
	}

	if err := app.Run(os.Args); err != nil {
		slog.Error("jobs-registry failed", "error", err)
		os.Exit(1)
	}
}

func defaultDataDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return home + "/.local/share/jobs-iroh/registry"
	}
	return "./jobs-iroh-registry"
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
