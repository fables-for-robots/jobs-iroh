// Package natsiroh tunnels the NATS client protocol over iroh QUIC streams.
//
// The server side embeds a NATS server with no TCP listener
// (server.Options{DontListen: true}); the only ways in are in-process
// (InProcessConn) and this tunnel. Each bidirectional iroh stream carries one
// NATS client connection: the dialer opens a stream per (re)connect attempt,
// the server proxies every accepted stream to a fresh InProcessConn.
//
// A QUIC stream does not exist for the peer until the opener sends data on
// it, but in the NATS protocol the server speaks first (INFO) — so the dialer
// writes a single preamble byte right after opening the stream, and the
// server consumes it before wiring the proxy.
package natsiroh

import (
	"context"
	"io"
	"net"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/tmc/go-iroh/iroh"
)

// streamPreamble is the byte the dialer writes right after opening a stream;
// without it neither side would ever see the other (see the package comment).
const streamPreamble = 0x00

// ConnSource yields the iroh connection to open NATS streams on. It is called
// once per NATS (re)connect attempt, so an implementation may transparently
// redial a broken iroh connection.
type ConnSource func(ctx context.Context) (*iroh.Conn, error)

// StaticConn is a ConnSource over an already-established connection.
func StaticConn(conn *iroh.Conn) ConnSource {
	return func(context.Context) (*iroh.Conn, error) { return conn, nil }
}

// Dialer implements nats.CustomDialer by opening a new bidirectional stream
// on an iroh connection for every (re)connect attempt of the NATS client.
type Dialer struct {
	Source ConnSource
	// Timeout bounds one dial attempt (conn acquisition + stream open).
	// Zero means 5s.
	Timeout time.Duration
}

func (d *Dialer) Dial(network, address string) (net.Conn, error) {
	timeout := d.Timeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	conn, err := d.Source(ctx)
	if err != nil {
		return nil, err
	}
	sc, err := conn.OpenStreamConn(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := sc.Write([]byte{streamPreamble}); err != nil {
		sc.Close()
		return nil, err
	}
	return sc, nil
}

// Connect establishes a NATS client connection tunneled over src. Extra
// options are appended after the tunnel wiring, so callers may override
// anything but the dialer.
func Connect(src ConnSource, opts ...nats.Option) (*nats.Conn, error) {
	base := []nats.Option{
		nats.SetCustomDialer(&Dialer{Source: src}),
		// The iroh connection outlives individual streams; retry forever and
		// let the ConnSource decide when the transport itself is gone.
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
	}
	return nats.Connect("nats://nats-over-iroh", append(base, opts...)...)
}

// ServeConn accepts bidirectional streams on one iroh connection and proxies
// each to a fresh in-process connection of ns. It returns when ctx is
// cancelled or the connection closes. Run it from the per-connection accept
// path (e.g. an iroh.Router ProtocolHandler for the tunnel ALPN).
func ServeConn(ctx context.Context, conn *iroh.Conn, ns *server.Server) error {
	for {
		sc, err := conn.AcceptStreamConn(ctx)
		if err != nil {
			// Connection closed or ctx cancelled — either way this
			// connection is done being served.
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go proxyStream(sc, ns)
	}
}

// Handler adapts ServeConn to an iroh.Router protocol handler.
func Handler(ns *server.Server) iroh.ProtocolHandler {
	return iroh.ProtocolHandlerFunc(func(ctx context.Context, conn *iroh.Conn) error {
		return ServeConn(ctx, conn, ns)
	})
}

func proxyStream(sc net.Conn, ns *server.Server) {
	defer sc.Close()

	var pre [1]byte
	if _, err := io.ReadFull(sc, pre[:]); err != nil {
		return
	}

	nconn, err := ns.InProcessConn()
	if err != nil {
		return
	}
	defer nconn.Close()

	done := make(chan struct{}, 2)
	go func() {
		io.Copy(nconn, sc)
		done <- struct{}{}
	}()
	go func() {
		io.Copy(sc, nconn)
		done <- struct{}{}
	}()
	<-done
	// Unblock whichever copy is still running.
	nconn.Close()
	sc.SetReadDeadline(time.Now())
	<-done
}
