// Package tui is the jobs-client admin TUI over the jobs-admin/1.0 ALPN:
// live build requests (with per-build snapshot watch, node logs, cancel and
// delete), the runner fleet, server stats, and a prefix-filterable ref
// browser. One bubbletea program: a root router model over one model per
// view. Every network exchange runs inside a tea.Cmd goroutine — Update
// never blocks — and a lost server degrades to a status-line error with the
// 2s poll tick doubling as the retry loop.
package tui

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/tmc/go-iroh/iroh"

	"github.com/jobs-build/jobs-iroh/amberclient"
	"github.com/jobs-build/jobs-iroh/api"
)

// alpnAdmin is the server's admin ALPN (frozen contract — serve/ mounts it).
// A local constant like clientcli's, so the TUI does not import the server
// composition for the name. The admin ALPN serves the whole build surface
// too (watch/logs/cancel), so one connection carries every view.
const alpnAdmin = "jobs-admin/1.0"

// client is the TUI's one connection to the server's admin ALPN: lazy dial,
// one stream per request (the api contract; concurrent streams are safe),
// and drop-on-transport-error so the next call redials. It is clientcli's
// apiConn plumbing with reconnect folded in.
type client struct {
	server string
	addrs  []string

	// mu also serializes dialing: concurrent cmds needing the connection
	// queue behind the first dialer instead of racing N dials.
	mu   sync.Mutex
	conn *iroh.Conn
	ep   *iroh.Endpoint
}

func newClient(server string, addrs []string) *client {
	return &client{server: server, addrs: addrs}
}

// connect returns the live connection, dialing if there is none.
func (c *client) connect(ctx context.Context) (*iroh.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		return c.conn, nil
	}
	conn, ep, err := amberclient.DialConn(ctx, amberclient.Options{
		EndpointID: c.server,
		Addrs:      c.addrs,
		ALPN:       alpnAdmin,
	})
	if err != nil {
		return nil, err
	}
	c.conn, c.ep = conn, ep
	return conn, nil
}

// invalidate drops conn if it is still the current one, so the next call
// redials. Transport errors only — a decoded api.Error frame means the
// server answered and the connection is healthy.
func (c *client) invalidate(conn *iroh.Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != conn {
		return
	}
	_ = c.conn.Close()
	_ = c.ep.Shutdown(context.Background())
	c.conn, c.ep = nil, nil
}

// close tears down the connection and its endpoint (program exit).
func (c *client) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return
	}
	_ = c.conn.Close()
	_ = c.ep.Shutdown(context.Background())
	c.conn, c.ep = nil, nil
}

// openStream opens one request stream and writes its single request frame.
// ctx cancellation (and only that) unblocks reads via the deadline trick;
// the returned closer disarms it and closes the stream.
func (c *client) openStream(ctx context.Context, t string, body any) (net.Conn, func(), error) {
	conn, err := c.connect(ctx)
	if err != nil {
		return nil, nil, err
	}
	stream, err := conn.OpenStreamConn(ctx)
	if err != nil {
		c.invalidate(conn)
		return nil, nil, fmt.Errorf("open %s stream: %w", t, err)
	}
	stop := context.AfterFunc(ctx, func() { _ = stream.SetDeadline(time.Now()) })
	if err := api.WriteFrame(stream, t, body); err != nil {
		stop()
		amberclient.CloseStream(stream)
		c.invalidate(conn)
		return nil, nil, fmt.Errorf("send %s: %w", t, err)
	}
	closer := func() {
		stop()
		// Full termination (send FIN + receive cancel): the TUI polls over
		// one long-lived connection, and only fully closed streams return
		// MAX_STREAMS credit — Close alone stalls the 101st request.
		amberclient.CloseStream(stream)
	}
	return stream, closer, nil
}

// call is the one-shot request/reply exchange: send t, read exactly one
// frame, decode into reply (nil for bodyless replies like ok).
func (c *client) call(ctx context.Context, t string, body any, wantType string, reply any) error {
	conn, err := c.connect(ctx)
	if err != nil {
		return err
	}
	stream, closer, err := c.openStream(ctx, t, body)
	if err != nil {
		return err
	}
	defer closer()
	rt, rb, err := api.ReadFrame(stream)
	if err != nil {
		c.invalidate(conn)
		return fmt.Errorf("%s: read reply: %w", t, err)
	}
	return decodeReply(rt, rb, wantType, reply)
}

// serverError is a decoded api.Error frame: the server answered, so the
// transport stays up — these must never invalidate the connection.
type serverError struct{ code, text string }

func (e *serverError) Error() string { return "server: " + e.code + ": " + e.text }

// isServerError reports whether err is an answered api.Error (as opposed to
// a transport failure).
func isServerError(err error) bool {
	var se *serverError
	return errors.As(err, &se)
}

// decodeReply maps a reply frame onto the expected type, surfacing server
// api.Error frames as *serverError.
func decodeReply(rt string, body cbor.RawMessage, wantType string, reply any) error {
	if rt == api.TError {
		var e api.Error
		if err := api.DecodeBody(body, &e); err != nil {
			return fmt.Errorf("undecodable server error frame: %w", err)
		}
		return &serverError{code: e.Code, text: e.Text}
	}
	if rt != wantType {
		return fmt.Errorf("unexpected reply frame %q (want %q)", rt, wantType)
	}
	if reply == nil {
		return nil
	}
	return api.DecodeBody(body, reply)
}
