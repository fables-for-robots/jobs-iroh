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

	"github.com/urfave/cli/v2"

	"github.com/fables-for-robots/jobs-iroh/sandbox"
	"github.com/fables-for-robots/jobs-iroh/serve"
	"github.com/fables-for-robots/jobs-iroh/version"
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
			opts := serve.Options{
				DataDir: c.String("data-dir"),
				Logger:  log,
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
