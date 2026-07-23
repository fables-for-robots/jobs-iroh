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
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync/atomic"
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
	// Announce makes the endpoint discoverable (amber-serve's export
	// stack): connect to a relay (best-effort, bounded — an offline host
	// still starts), advertise direct addresses on every interface,
	// publish them over mDNS on the local link and via pkarr over the
	// internet. The jobs-server main sets it; loopback tests stay offline
	// with direct addresses only.
	Announce bool
	// AdvertiseAddrs are direct addresses to advertise, ip or ip:port
	// (bare IPs get the bound port). Non-empty replaces interface
	// auto-detection. Only meaningful with Announce.
	AdvertiseAddrs []string
	// RelayURL pins the relay fallback path; empty probes the built-in
	// relay map for the nearest one. Only meaningful with Announce.
	RelayURL string
	// DataEndpoints is how many extra UDP endpoints the amber mounts bind
	// for sharded transfers (0 = none, max 15): one socket's recv loop
	// caps well below a fast link, so shards get dedicated server sockets.
	// The ports are advertised in-band to sharding clients only —
	// direct-path, never published; a client that cannot reach them falls
	// back to the control connection's addresses.
	DataEndpoints int
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
	if opts.Announce {
		relayMode, err := serverRelayMode(ctx, opts.RelayURL, log)
		if err != nil {
			return err
		}
		bindOpts = append(bindOpts, iroh.WithRelayMode(relayMode))
	}
	ep, err := iroh.Bind(ctx, bindOpts...)
	if err != nil {
		return fmt.Errorf("bind endpoint: %w", err)
	}
	defer ep.Shutdown(context.WithoutCancel(ctx))

	if opts.Announce {
		pub, err := announce(ctx, ep, sk, opts.AdvertiseAddrs, log)
		if err != nil {
			return err
		}
		defer pub.Close()
	}

	amberSrv := ambserver.New(log.With("component", "amber"), store.Objects(), store.RefStore())

	// Extra data endpoints for sharded transfers, bound with the SAME
	// secret key so shard dials authenticate the same server identity.
	// Each carries only the two amber ALPNs; TAttach reaches the one
	// amberSrv (and its transfer tokens) from any of them.
	var dataRouters []*iroh.Router
	if n := min(opts.DataEndpoints, 15); n > 0 {
		// Bind every endpoint and publish the ports BEFORE any router
		// accepts: SetDataPorts is an unsynchronized write the handlers
		// read (ambserver documents "call before Serve").
		var deps []*iroh.Endpoint
		var ports []uint16
		for range n {
			depOpts := []iroh.Option{iroh.WithSecretKey(sk)}
			if opts.BindAddr.IsValid() {
				depOpts = append(depOpts, iroh.WithBindAddr(netip.AddrPortFrom(opts.BindAddr.Addr(), 0)))
			}
			dep, err := iroh.Bind(ctx, depOpts...)
			if err != nil {
				return fmt.Errorf("bind data endpoint: %w", err)
			}
			defer dep.Shutdown(context.WithoutCancel(ctx))
			deps = append(deps, dep)
			ports = append(ports, dep.LocalAddr().Port())
		}
		amberSrv.SetDataPorts(ports)
		amberOnly := map[string]iroh.ProtocolHandler{
			ALPNRunnerAmber: amberConnHandler(amberSrv),
			ALPNAmberAdmin:  amberConnHandler(amberSrv),
		}
		for _, dep := range deps {
			dr, err := iroh.NewRouter(dep, amberOnly, &iroh.RouterConfig{Logger: log})
			if err != nil {
				return fmt.Errorf("data endpoint router: %w", err)
			}
			dataRouters = append(dataRouters, dr)
		}
		log.Info("amber data endpoints", "ports", ports)
	}

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
	for _, dr := range dataRouters {
		if err := dr.Shutdown(shCtx); err != nil {
			log.Warn("data router shutdown", "error", err)
		}
	}
	return nil
}

// closeTracked wraps a stream so the handler can tell, after HandleStream
// returns, whether the operation finished with the stream (closed) or handed
// it to an in-progress sharded transfer (still open — the transfer owns it).
type closeTracked struct {
	net.Conn
	closed atomic.Bool
}

func (c *closeTracked) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
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
				tracked := &closeTracked{Conn: stream}
				srv.HandleStream(conn.RemoteID().String(), tracked)
				// HandleStream closes only the send side; the sync protocol
				// never reads the client's FIN after the final frame. Cancel
				// the receive side so the stream fully terminates — QUIC
				// returns MAX_STREAMS credit to the client only for fully
				// closed streams, and without this every operation leaks one
				// stream until the client's 101st open blocks forever.
				//
				// EXCEPT when HandleStream returned without closing: a
				// TAttach stream changed hands to an in-progress sharded
				// transfer, which owns and closes it — canceling here would
				// sever a live data channel mid-transfer. (Attach streams
				// leak no credit either way: the client tears down their
				// whole per-shard connection when the transfer ends.)
				if tracked.closed.Load() {
					if cr, ok := stream.(interface{ CancelRead(code uint64) }); ok {
						cr.CancelRead(0)
					}
				}
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
