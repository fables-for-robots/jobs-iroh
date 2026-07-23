// Package serve wires the jobs-server process: one iroh endpoint whose ALPNs
// multiplex every service — the client build API, the runner NATS tunnel, the
// runner and admin CAS sync, and the admin API — over a single embedded
// amber store and a single embedded NATS server.
//
// The server is the only shared authority in jobs-iroh: runners and clients
// each own private stores and reach this one exclusively through the iroh
// protocols served here.
package serve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	ambserver "github.com/fables-for-robots/amber-store-iroh/server"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/tmc/go-iroh/iroh"
	"golang.org/x/sync/errgroup"

	"github.com/fables-for-robots/jobs-iroh/amber"
	"github.com/fables-for-robots/jobs-iroh/bootstrap"
	"github.com/fables-for-robots/jobs-iroh/natsiroh"
	"github.com/fables-for-robots/jobs-iroh/sched"
)

// The five service ALPNs of a jobs-server endpoint.
const (
	ALPNBuild       = "jobs-build/1.0"        // client: submit builds, watch progress
	ALPNRunnerNATS  = "jobs-runner-nats/1.0"  // runner: NATS client tunnel
	ALPNRunnerAmber = "jobs-runner-amber/1.0" // runner: CAS object/ref sync
	ALPNAdmin       = "jobs-admin/1.0"        // admin TUI: observe builds, stats
	ALPNAmberAdmin  = "jobs-amber-admin/1.0"  // client: CAS ref sync (source up, outputs home)
)

// shutdownGrace bounds how long in-flight protocol handlers may run after
// shutdown begins.
const shutdownGrace = 10 * time.Second

// Options configures Run.
type Options struct {
	// DataDir holds everything the server owns: store/ (embedded amber
	// store), nats/ (JetStream state), server.key (endpoint identity).
	DataDir string
	// BindAddr optionally pins the UDP bind address (e.g. loopback in
	// tests). Zero value binds the default wildcard.
	BindAddr netip.AddrPort
	// Logger receives server logs; nil means slog.Default().
	Logger *slog.Logger
	// Ready, when non-nil, is closed once the endpoint accepts connections.
	// The Server value is valid from that moment on.
	Ready func(*Server)
}

// Server exposes the running server's identity and internals to tests and to
// the admin surface.
type Server struct {
	Endpoint *iroh.Endpoint
	Store    *amber.Store
	NATS     *natsserver.Server
	Sched    *sched.Sched
}

// Run starts the server and blocks until ctx is cancelled, then shuts down
// gracefully: stop accepting, wait (bounded) for in-flight handlers, stop
// NATS, close the store.
func Run(ctx context.Context, opts Options) error {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	if opts.DataDir == "" {
		return errors.New("serve: DataDir is required")
	}
	// Absolutize so logged paths and JetStream/store state never depend on
	// the launch cwd (mirrors runnerd/clientcli).
	if abs, err := filepath.Abs(opts.DataDir); err == nil {
		opts.DataDir = abs
	}
	if err := os.MkdirAll(opts.DataDir, 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}

	sk, err := loadOrCreateSecretKey(filepath.Join(opts.DataDir, "server.key"))
	if err != nil {
		return err
	}

	store, err := amber.Open(filepath.Join(opts.DataDir, "store"))
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	// Self-seed the bootstrap floor (shell:<p>, fetcher:*:<p>) for every
	// embedded platform: runners own no seeds — they pull fetchers and the
	// shell from this store, so a fresh server must publish them before the
	// first job lands.
	if err := bootstrap.Seed(ctx, store, bootstrap.SeededPlatforms(), log.With("component", "bootstrap")); err != nil {
		return fmt.Errorf("seed bootstrap artifacts: %w", err)
	}

	ns, err := natsserver.NewServer(&natsserver.Options{
		ServerName: "jobs-server",
		DontListen: true,
		JetStream:  true,
		StoreDir:   filepath.Join(opts.DataDir, "nats"),
	})
	if err != nil {
		return fmt.Errorf("new nats server: %w", err)
	}
	ns.SetLoggerV2(newNATSLogger(log.With("component", "nats")), false, false, false)
	go ns.Start()
	defer ns.Shutdown()
	if !ns.ReadyForConnections(10 * time.Second) {
		return errors.New("embedded nats server not ready")
	}

	// The scheduler rides an in-process NATS connection — no TCP listener
	// exists; runners reach the same server through the iroh tunnel.
	nc, err := nats.Connect("", nats.InProcessServer(ns))
	if err != nil {
		return fmt.Errorf("in-process nats connect: %w", err)
	}
	defer nc.Close()

	sd, err := sched.New(ctx, sched.Options{
		Store: store,
		NC:    nc,
		Log:   log.With("component", "sched"),
	})
	if err != nil {
		return fmt.Errorf("start scheduler: %w", err)
	}
	defer sd.Close()

	bindOpts := []iroh.Option{iroh.WithSecretKey(sk)}
	if opts.BindAddr.IsValid() {
		bindOpts = append(bindOpts, iroh.WithBindAddr(opts.BindAddr))
	}
	ep, err := iroh.Bind(ctx, bindOpts...)
	if err != nil {
		return fmt.Errorf("bind endpoint: %w", err)
	}
	defer ep.Shutdown(context.WithoutCancel(ctx))

	amberSrv := ambserver.New(log.With("component", "amber"), store.Objects(), store.RefStore())

	storeDir := filepath.Join(opts.DataDir, "store")
	buildSvc := &apiService{log: log.With("service", "build"), sd: sd, store: store, storeDir: storeDir}
	adminSvc := &apiService{log: log.With("service", "admin"), sd: sd, store: store, storeDir: storeDir, admin: true}

	handlers := map[string]iroh.ProtocolHandler{
		ALPNRunnerNATS:  logConns(log, "runner-nats", natsiroh.Handler(ns)),
		ALPNRunnerAmber: logConns(log, "runner-amber", amberConnHandler(amberSrv)),
		ALPNAmberAdmin:  logConns(log, "amber-admin", amberConnHandler(amberSrv)),
		ALPNBuild:       logConns(log, "build", buildSvc.handler()),
		ALPNAdmin:       logConns(log, "admin", adminSvc.handler()),
	}
	router, err := iroh.NewRouter(ep, handlers, &iroh.RouterConfig{Logger: log})
	if err != nil {
		return fmt.Errorf("new router: %w", err)
	}

	log.Info("jobs-server listening",
		"endpoint", ep.ID().String(),
		"addr", ep.LocalAddr(),
		"data-dir", opts.DataDir,
	)
	if opts.Ready != nil {
		opts.Ready(&Server{Endpoint: ep, Store: store, NATS: ns, Sched: sd})
	}

	<-ctx.Done()
	log.Info("shutting down")
	shCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()
	if err := router.Shutdown(shCtx); err != nil {
		log.Warn("router shutdown", "error", err)
	}
	return nil
}

// amberConnHandler serves the amber-store-iroh sync protocol on every stream
// of a connection — the per-connection half of amber-store-iroh's own Serve
// loop, adapted to router dispatch.
func amberConnHandler(srv *ambserver.Server) iroh.ProtocolHandler {
	return iroh.ProtocolHandlerFunc(func(ctx context.Context, conn *iroh.Conn) error {
		g, ctx := errgroup.WithContext(ctx)
		// Closing the connection on ctx cancel unblocks in-flight handlers.
		stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
		defer stop()
		for {
			stream, err := conn.AcceptStreamConn(ctx)
			if err != nil {
				break // peer closed the connection, or ctx cancelled
			}
			g.Go(func() error {
				srv.HandleStream(conn.RemoteID().String(), stream)
				return nil
			})
		}
		return g.Wait()
	})
}

// logConns wraps a handler with connect/disconnect logging.
func logConns(log *slog.Logger, service string, h iroh.ProtocolHandler) iroh.ProtocolHandler {
	return iroh.ProtocolHandlerFunc(func(ctx context.Context, conn *iroh.Conn) error {
		l := log.With("service", service, "remote", conn.RemoteID().String())
		l.Info("connected")
		err := h.Accept(ctx, conn)
		l.Info("disconnected", "error", err)
		return err
	})
}
