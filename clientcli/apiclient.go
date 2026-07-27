package clientcli

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/tmc/go-iroh/iroh"

	"github.com/jobs-build/jobs-iroh/amberclient"
	"github.com/jobs-build/jobs-iroh/api"
)

// The client-facing server ALPNs (frozen contract — serve/ mounts these).
// Local constants, like runnerd's, so the CLI binary does not import the
// server composition (embedded NATS et al.) just for the names.
const (
	alpnBuild      = "jobs-build/1.0"
	alpnAdmin      = "jobs-admin/1.0"
	alpnAmberAdmin = "jobs-amber-admin/1.0"
)

// apiConn is one framed-API connection (build or admin ALPN) on its own
// ephemeral client endpoint. Streams are one-request-each (api contract);
// concurrent streams on one connection are safe.
type apiConn struct {
	conn *iroh.Conn
	ep   *iroh.Endpoint
}

// dialAPI connects to the server's build or admin ALPN: direct addresses
// when given, discovery otherwise (amberclient's dial plumbing).
func dialAPI(ctx context.Context, server string, addrs []string, alpn string) (*apiConn, error) {
	conn, ep, err := amberclient.DialConn(ctx, amberclient.Options{
		EndpointID: server,
		Addrs:      addrs,
		ALPN:       alpn,
	})
	if err != nil {
		return nil, err
	}
	return &apiConn{conn: conn, ep: ep}, nil
}

// Close tears down the connection and its endpoint.
func (a *apiConn) Close() {
	_ = a.conn.Close()
	_ = a.ep.Shutdown(context.Background())
}

// openRequest opens one stream and sends its single request frame. The
// returned stop arms ctx cancellation (deadline-expiry, like amberclient's
// streams) and must be deferred alongside the stream close.
func (a *apiConn) openRequest(ctx context.Context, t string, body any) (net.Conn, func(), error) {
	stream, err := a.conn.OpenStreamConn(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s stream: %w", t, err)
	}
	stop := context.AfterFunc(ctx, func() { _ = stream.SetDeadline(time.Now()) })
	if err := api.WriteFrame(stream, t, body); err != nil {
		stop()
		amberclient.CloseStream(stream)
		return nil, nil, fmt.Errorf("send %s: %w", t, err)
	}
	return stream, func() { stop() }, nil
}

// call is the one-shot request/reply exchange: send t, read exactly one
// frame, decode it into reply (nil for bodyless replies like ok).
func (a *apiConn) call(ctx context.Context, t string, body any, wantType string, reply any) error {
	stream, stop, err := a.openRequest(ctx, t, body)
	if err != nil {
		return err
	}
	defer stop()
	// Full termination (send FIN + receive cancel): one-shot request streams
	// are never read to EOF by either side, and only fully closed streams
	// hand their MAX_STREAMS credit back — long-lived admin/TUI sessions
	// would otherwise stall on the 101st call.
	defer amberclient.CloseStream(stream)
	rt, rb, err := api.ReadFrame(stream)
	if err != nil {
		return fmt.Errorf("%s: read reply: %w", t, err)
	}
	return decodeReply(rt, rb, wantType, reply)
}

// decodeReply maps a reply frame onto the expected type, surfacing server
// api.Error frames as Go errors.
func decodeReply(rt string, body cbor.RawMessage, wantType string, reply any) error {
	if rt == api.TError {
		var e api.Error
		if err := api.DecodeBody(body, &e); err != nil {
			return fmt.Errorf("undecodable server error frame: %w", err)
		}
		return fmt.Errorf("server: %s: %s", e.Code, e.Text)
	}
	if rt != wantType {
		return fmt.Errorf("unexpected reply frame %q (want %q)", rt, wantType)
	}
	if reply == nil {
		return nil
	}
	return api.DecodeBody(body, reply)
}
