package serve

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/jobs-build/amber-store-iroh/protocol"
	"github.com/tmc/go-iroh/iroh"
	"github.com/tmc/go-iroh/netaddr"

	"github.com/jobs-build/jobs-iroh/natsiroh"
)

// startServer runs a full jobs-server on loopback and returns its handle plus
// a dial helper for one ALPN.
func startServer(t *testing.T, ctx context.Context) (*Server, func(alpn string) *iroh.Conn) {
	t.Helper()

	ready := make(chan *Server, 1)
	done := make(chan error, 1)
	runCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("server run: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("server did not shut down")
		}
	})
	go func() {
		done <- Run(runCtx, Options{
			DataDir:  t.TempDir(),
			BindAddr: netip.AddrPortFrom(netip.IPv6Loopback(), 0),
			Ready:    func(s *Server) { ready <- s },
		})
	}()

	var srv *Server
	select {
	case srv = <-ready:
	case <-time.After(30 * time.Second):
		t.Fatal("server not ready")
	}

	clientEP, err := iroh.Bind(ctx, iroh.WithBindAddr(netip.AddrPortFrom(netip.IPv6Loopback(), 0)))
	if err != nil {
		t.Fatalf("bind client endpoint: %v", err)
	}
	t.Cleanup(func() { clientEP.Shutdown(ctx) })

	dial := func(alpn string) *iroh.Conn {
		addr := netaddr.NewEndpointAddr(srv.Endpoint.ID()).WithIP(srv.Endpoint.LocalAddr())
		conn, err := clientEP.Connect(ctx, addr, alpn)
		if err != nil {
			t.Fatalf("connect %s: %v", alpn, err)
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	return srv, dial
}

func TestNATSTunnel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	_, dial := startServer(t, ctx)
	conn := dial(ALPNRunnerNATS)

	nc, err := natsiroh.Connect(natsiroh.StaticConn(conn))
	if err != nil {
		t.Fatalf("nats connect: %v", err)
	}
	defer nc.Close()

	sub, err := nc.SubscribeSync("jobs.test")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := nc.Publish("jobs.test", []byte("via server")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if _, err := sub.NextMsg(5 * time.Second); err != nil {
		t.Fatalf("next msg: %v", err)
	}
}

func TestAmberRefListOverAdminALPN(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	srv, dial := startServer(t, ctx)

	// Publish a ref server-side, then list it over the sync protocol.
	k, err := srv.Store.IngestFile(ctx, []byte("hello jobs-iroh"))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := srv.Store.PutRef(ctx, "smoke", k); err != nil {
		t.Fatalf("put ref: %v", err)
	}

	conn := dial(ALPNAmberAdmin)
	stream, err := conn.OpenStreamConn(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer stream.Close()

	if err := protocol.WriteMsg(stream, protocol.Msg{Type: protocol.TRefList}); err != nil {
		t.Fatalf("write ref-list request: %v", err)
	}
	msg, err := protocol.ReadMsg(stream)
	if err != nil {
		t.Fatalf("read ref-list reply: %v", err)
	}
	if msg.Type != protocol.TRefs {
		t.Fatalf("got frame type %d, want TRefs", msg.Type)
	}
	found := false
	for _, r := range msg.Refs {
		if r.Name == "smoke" {
			found = true
		}
	}
	if !found {
		t.Fatalf("ref %q not in listing (%d refs)", "smoke", len(msg.Refs))
	}
}
