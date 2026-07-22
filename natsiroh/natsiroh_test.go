package natsiroh

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/tmc/go-iroh/iroh"
	"github.com/tmc/go-iroh/netaddr"
)

const testALPN = "jobs-runner-nats/1.0"

// startTunnel builds the full loopback stack: an embedded NATS server with no
// TCP listener, an iroh server endpoint proxying the tunnel ALPN into it, and
// a client endpoint connected over loopback. It returns a ConnSource for the
// client side.
func startTunnel(t *testing.T, ctx context.Context, opts *server.Options) (*server.Server, ConnSource) {
	t.Helper()

	if opts == nil {
		opts = &server.Options{}
	}
	opts.DontListen = true
	ns, err := server.NewServer(opts)
	if err != nil {
		t.Fatalf("new nats server: %v", err)
	}
	go ns.Start()
	t.Cleanup(ns.Shutdown)
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats server not ready")
	}

	serverEP, err := iroh.Bind(ctx,
		iroh.WithALPNs(testALPN),
		iroh.WithBindAddr(netip.AddrPortFrom(netip.IPv6Loopback(), 0)),
	)
	if err != nil {
		t.Fatalf("bind server endpoint: %v", err)
	}
	t.Cleanup(func() { serverEP.Shutdown(ctx) })

	go func() {
		for {
			conn, err := serverEP.Accept(ctx)
			if err != nil {
				return
			}
			go ServeConn(ctx, conn, ns)
		}
	}()

	clientEP, err := iroh.Bind(ctx,
		iroh.WithBindAddr(netip.AddrPortFrom(netip.IPv6Loopback(), 0)),
	)
	if err != nil {
		t.Fatalf("bind client endpoint: %v", err)
	}
	t.Cleanup(func() { clientEP.Shutdown(ctx) })

	addr := netaddr.NewEndpointAddr(serverEP.ID()).WithIP(serverEP.LocalAddr())
	qconn, err := clientEP.Connect(ctx, addr, testALPN)
	if err != nil {
		t.Fatalf("iroh connect: %v", err)
	}
	t.Cleanup(func() { qconn.Close() })

	return ns, StaticConn(qconn)
}

func TestPubSubOverIroh(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, src := startTunnel(t, ctx, nil)

	nc, err := Connect(src)
	if err != nil {
		t.Fatalf("nats connect: %v", err)
	}
	defer nc.Close()

	sub, err := nc.SubscribeSync("greet")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := nc.Publish("greet", []byte("hello over iroh")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	msg, err := sub.NextMsg(5 * time.Second)
	if err != nil {
		t.Fatalf("next msg: %v", err)
	}
	if got, want := string(msg.Data), "hello over iroh"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestJetStreamOverIroh(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, src := startTunnel(t, ctx, &server.Options{
		JetStream: true,
		StoreDir:  t.TempDir(),
	})

	nc, err := Connect(src)
	if err != nil {
		t.Fatalf("nats connect: %v", err)
	}
	defer nc.Close()

	js, err := nc.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if _, err := js.AddStream(&nats.StreamConfig{
		Name:      "RESULTS",
		Subjects:  []string{"results.>"},
		Retention: nats.LimitsPolicy,
	}); err != nil {
		t.Fatalf("add stream: %v", err)
	}
	if _, err := js.Publish("results.q.job-1", []byte(`{"exit":0}`),
		nats.MsgId("result-job-1")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	// Duplicate publish with the same Nats-Msg-Id must dedup.
	if _, err := js.Publish("results.q.job-1", []byte(`{"exit":0}`),
		nats.MsgId("result-job-1")); err != nil {
		t.Fatalf("duplicate publish: %v", err)
	}

	sub, err := js.SubscribeSync("results.q.job-1", nats.DeliverAll())
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if _, err := sub.NextMsg(5 * time.Second); err != nil {
		t.Fatalf("first msg: %v", err)
	}
	if msg, err := sub.NextMsg(500 * time.Millisecond); err == nil {
		t.Fatalf("expected dedup, got second message %q", msg.Data)
	}
}

func TestClientReconnectOpensNewStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ns, src := startTunnel(t, ctx, nil)

	reconnected := make(chan struct{}, 1)
	nc, err := Connect(src,
		nats.ReconnectWait(50*time.Millisecond),
		nats.ReconnectHandler(func(*nats.Conn) {
			select {
			case reconnected <- struct{}{}:
			default:
			}
		}),
	)
	if err != nil {
		t.Fatalf("nats connect: %v", err)
	}
	defer nc.Close()

	// Sever the current tunnel stream underneath the client by kicking it
	// server-side; the client must dial a fresh stream on the same iroh
	// connection.
	cid, err := nc.GetClientID()
	if err != nil {
		t.Fatalf("client id: %v", err)
	}
	if err := ns.DisconnectClientByID(cid); err != nil {
		t.Fatalf("disconnect client: %v", err)
	}

	select {
	case <-reconnected:
	case <-time.After(10 * time.Second):
		t.Fatal("client did not reconnect over a new stream")
	}

	// The reconnected client must still work.
	sub, err := nc.SubscribeSync("after.reconnect")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := nc.Publish("after.reconnect", []byte("ok")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := nc.FlushTimeout(5 * time.Second); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if _, err := sub.NextMsg(5 * time.Second); err != nil {
		t.Fatalf("next msg after reconnect: %v", err)
	}
}
