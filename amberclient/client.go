// Package amberclient is the importable client half of the amber sync
// protocol — the dial/push/pull/refs orchestration that upstream lives only
// in amber-store-iroh's cmd/amber mains, lifted into a library so jobs-iroh
// components can sync stores over any of the server's amber ALPNs
// (jobs-runner-amber/1.0, jobs-amber-admin/1.0). The wire protocol itself is
// amber-store-iroh's protocol + wantsync packages, unchanged.
//
// v1 is deliberately single-connection: one QUIC connection, one stream per
// operation, DataConns always 0 — the sharded TAttach transfer path of the
// upstream CLI is dropped (perf follow-up). What is kept is everything that
// matters for correctness: the resumable want-loop (an interrupted transfer
// resumes where it stopped, because wantsync prunes only CheckComplete
// subtrees), verified writes on pull (the peer is untrusted), and pack
// draining through TDataEnd so streams stay frame-aligned between rounds.
//
// Pushes are force-mode (no CAS, no remote-tracking refs): jobs-iroh pushes
// scratch/output refs it owns, so last-write-wins is the intended semantic
// and the upstream CLI's tracking-ref bookkeeping would be dead weight.
package amberclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"time"

	"github.com/fables-for-robots/amber-store-core/key"
	"github.com/fables-for-robots/amber-store-core/reference"
	"github.com/fables-for-robots/amber-store-iroh/protocol"
	"github.com/fables-for-robots/amber-store-iroh/wantsync"
	"github.com/tmc/go-iroh/iroh"
	irohkey "github.com/tmc/go-iroh/key"

	"github.com/fables-for-robots/jobs-iroh/amber"
)

// ErrRefNotFound is returned (errors.Is-able) by Pull when the server does
// not have the requested ref.
var ErrRefNotFound = errors.New("amberclient: remote ref not found")

// Options configures Dial.
type Options struct {
	// EndpointID is the server's iroh endpoint ID (required).
	EndpointID string
	// Addrs are direct server addresses, "ip:port" or "host:port"
	// (hostnames may resolve to several candidates). When non-empty the
	// dial goes straight at them — no discovery, no relays — which is also
	// how loopback tests connect. When empty the endpoint ID is resolved
	// via mDNS (passive), pkarr and DNS, and every resolver's candidates
	// are raced.
	Addrs []string
	// ALPN selects the server's amber mount (e.g. serve.ALPNRunnerAmber or
	// serve.ALPNAmberAdmin). Empty means amber-store-iroh's own protocol
	// ALPN, for talking to a stock amber-serve.
	ALPN string
	// BindAddr optionally pins the client's UDP bind address (e.g.
	// loopback in tests). Zero value binds the default wildcard.
	BindAddr netip.AddrPort
	// Logger receives client logs; nil means slog.Default().
	Logger *slog.Logger
}

// RefInfo is one reference in a Refs listing.
type RefInfo struct {
	Name      string
	Key       key.Key
	CreatedAt time.Time
	User      string
}

// Client is a connected amber sync client: one QUIC connection on its own
// ephemeral endpoint, one stream per operation. Methods are safe for
// concurrent use — each opens its own stream and the protocol is
// stream-scoped. Close releases the connection and the endpoint.
type Client struct {
	conn *iroh.Conn
	ep   *iroh.Endpoint
	log  *slog.Logger
}

// Dial connects to the amber server described by o: direct addresses when
// given, discovery otherwise, always with an ephemeral client identity
// (access is open at this trust level; transport identity is the server's
// endpoint key). All candidate addresses are dialed concurrently and the
// first connection wins.
func Dial(ctx context.Context, o Options) (*Client, error) {
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	alpn := o.ALPN
	if alpn == "" {
		alpn = protocol.ALPN
	}
	id, err := irohkey.ParseEndpointID(o.EndpointID)
	if err != nil {
		return nil, fmt.Errorf("amberclient: parse endpoint id: %w", err)
	}

	ep, cands, err := bindAndResolve(ctx, id, o.Addrs, o.BindAddr)
	if err != nil {
		return nil, err
	}
	conn, err := raceConnect(ctx, ep, id, alpn, cands)
	if err != nil {
		_ = ep.Shutdown(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("amberclient: connect %s: %w", id, err)
	}
	log.Debug("amberclient connected", "endpoint", id.String(), "alpn", alpn)
	return &Client{conn: conn, ep: ep, log: log}, nil
}

// Close tears down the connection and shuts down the client endpoint.
func (c *Client) Close() error {
	err := c.conn.Close()
	if serr := c.ep.Shutdown(context.Background()); serr != nil {
		err = errors.Join(err, serr)
	}
	return err
}

// Push uploads the tree rooted at root from st and points the remote ref
// name at it. Force-mode: no CAS precondition, the remote ref is overwritten
// unconditionally (last-write-wins — jobs-iroh pushes refs it owns). Only
// objects the server is missing cross the wire; the server verifies
// completeness before committing the ref, so a reported success means the
// remote ref exists and its whole closure is on the server.
func (c *Client) Push(ctx context.Context, st *amber.Store, name string, root key.Key) error {
	_, err := c.PushWithProgress(ctx, st, name, root, nil)
	return err
}

// PushWithProgress is Push with a transfer observer: prog (nil is fine)
// receives objects done/total as want rounds are answered, and the returned
// stats report what actually moved — 0/0 when the server already had the
// whole closure.
func (c *Client) PushWithProgress(ctx context.Context, st *amber.Store, name string, root key.Key, prog ProgressFunc) (XferStats, error) {
	if err := reference.ValidateName(name); err != nil {
		return XferStats{}, fmt.Errorf("amberclient: push: %w", err)
	}
	// Fail with a clear message before any traffic: a missing local root
	// would otherwise surface mid-transfer as a get error inside Send.
	ok, err := st.Has(root)
	if err != nil {
		return XferStats{}, fmt.Errorf("amberclient: push %q: %w", name, err)
	}
	if !ok {
		return XferStats{}, fmt.Errorf("amberclient: push %q: root %s not in local store", name, root)
	}

	stream, stop, err := c.openStream(ctx)
	if err != nil {
		return XferStats{}, fmt.Errorf("amberclient: push %q: %w", name, err)
	}
	defer stop()
	defer CloseStream(stream)

	// A QUIC stream is invisible to the server until data flows, so the
	// request goes out before any read. DataConns is omitted (0): the
	// server answers with plain TWants rounds, no TAccept/token detour.
	req := protocol.Msg{Type: protocol.TPush, Name: name, Root: root[:]}
	if err := protocol.WriteMsg(stream, req); err != nil {
		return XferStats{}, fmt.Errorf("amberclient: push %q: %w", name, err)
	}
	mtr := &meter{cb: prog}
	if err := wantsync.Send(stream, st.Objects(), mtr); err != nil {
		return mtr.stats(), fmt.Errorf("amberclient: push %q: %w", name, err)
	}
	m, err := protocol.ReadMsg(stream)
	if err != nil {
		return mtr.stats(), fmt.Errorf("amberclient: push %q: read commit: %w", name, err)
	}
	switch m.Type {
	case protocol.TOK:
	case protocol.TErr:
		return mtr.stats(), fmt.Errorf("amberclient: push %q: %w", name, protocol.RemoteFromMsg(m))
	default:
		return mtr.stats(), fmt.Errorf("amberclient: push %q: %w: type %d, want TOK", name, protocol.ErrProtocol, m.Type)
	}
	c.log.Debug("amberclient push committed", "ref", name, "root", root.String())
	return mtr.stats(), nil
}

// Pull fetches the remote ref name and every object below it that st is
// missing, then writes the plain local ref name → root via st.PutRef so
// local consumers (runner drivers' ensure* short-circuits) resolve the ref
// without touching the network again. Received objects are verified against
// their keys before being stored — the peer is untrusted — and the want loop
// only terminates once the whole closure is complete locally, so the local
// ref is written strictly after its objects (the objects-before-ref
// invariant). An absent remote ref returns an error satisfying
// errors.Is(err, ErrRefNotFound).
func (c *Client) Pull(ctx context.Context, st *amber.Store, name string) (key.Key, error) {
	root, _, err := c.PullWithProgress(ctx, st, name, nil)
	return root, err
}

// PullWithProgress is Pull with a transfer observer: prog (nil is fine)
// receives objects done/total as want rounds are exchanged, and the returned
// stats report what actually moved — 0/0 when the closure was already local.
func (c *Client) PullWithProgress(ctx context.Context, st *amber.Store, name string, prog ProgressFunc) (key.Key, XferStats, error) {
	if err := reference.ValidateName(name); err != nil {
		return key.Key{}, XferStats{}, fmt.Errorf("amberclient: pull: %w", err)
	}
	stream, stop, err := c.openStream(ctx)
	if err != nil {
		return key.Key{}, XferStats{}, fmt.Errorf("amberclient: pull %q: %w", name, err)
	}
	defer stop()
	defer CloseStream(stream)

	if err := protocol.WriteMsg(stream, protocol.Msg{Type: protocol.TPull, Name: name}); err != nil {
		return key.Key{}, XferStats{}, fmt.Errorf("amberclient: pull %q: %w", name, err)
	}
	m, err := protocol.ReadMsg(stream)
	if err != nil {
		return key.Key{}, XferStats{}, fmt.Errorf("amberclient: pull %q: read ref: %w", name, err)
	}
	switch m.Type {
	case protocol.TRef:
	case protocol.TErr:
		if m.Code == protocol.CodeUnknownRef {
			return key.Key{}, XferStats{}, fmt.Errorf("amberclient: pull %q: %w", name, ErrRefNotFound)
		}
		return key.Key{}, XferStats{}, fmt.Errorf("amberclient: pull %q: %w", name, protocol.RemoteFromMsg(m))
	default:
		return key.Key{}, XferStats{}, fmt.Errorf("amberclient: pull %q: %w: type %d, want TRef", name, protocol.ErrProtocol, m.Type)
	}
	rec, err := reference.Decode(m.Record)
	if err != nil {
		return key.Key{}, XferStats{}, fmt.Errorf("amberclient: pull %q: server ref record: %w", name, err)
	}
	root, err := key.Parse(rec.Key)
	if err != nil {
		return key.Key{}, XferStats{}, fmt.Errorf("amberclient: pull %q: server ref record key: %w", name, err)
	}

	// Receive verifies every object against its key (WriteParallel with
	// Verify) and re-walks the frontier until CheckComplete prunes
	// everything — returning nil IS the completeness proof for root's
	// whole closure.
	mtr := &meter{cb: prog}
	if _, err := wantsync.Receive([]io.ReadWriter{stream}, st.Objects(), root, 0, mtr); err != nil {
		return key.Key{}, mtr.stats(), fmt.Errorf("amberclient: pull %q: %w", name, err)
	}
	if err := st.PutRef(ctx, name, root); err != nil {
		return key.Key{}, mtr.stats(), fmt.Errorf("amberclient: pull %q completed but writing local ref failed: %w", name, err)
	}
	c.log.Debug("amberclient pull complete", "ref", name, "root", root.String())
	return root, mtr.stats(), nil
}

// Refs lists every reference on the server.
func (c *Client) Refs(ctx context.Context) ([]RefInfo, error) {
	stream, stop, err := c.openStream(ctx)
	if err != nil {
		return nil, fmt.Errorf("amberclient: refs: %w", err)
	}
	defer stop()
	defer CloseStream(stream)

	if err := protocol.WriteMsg(stream, protocol.Msg{Type: protocol.TRefList}); err != nil {
		return nil, fmt.Errorf("amberclient: refs: %w", err)
	}
	m, err := protocol.ReadMsg(stream)
	if err != nil {
		return nil, fmt.Errorf("amberclient: refs: %w", err)
	}
	switch m.Type {
	case protocol.TRefs:
	case protocol.TErr:
		return nil, fmt.Errorf("amberclient: refs: %w", protocol.RemoteFromMsg(m))
	default:
		return nil, fmt.Errorf("amberclient: refs: %w: type %d, want TRefs", protocol.ErrProtocol, m.Type)
	}
	out := make([]RefInfo, 0, len(m.Refs))
	for _, r := range m.Refs {
		k, err := key.Parse(r.Key)
		if err != nil {
			return nil, fmt.Errorf("amberclient: refs: ref %q: key: %w", r.Name, err)
		}
		out = append(out, RefInfo{
			Name:      r.Name,
			Key:       k,
			CreatedAt: time.Unix(0, r.CreatedAt),
			User:      r.User,
		})
	}
	return out, nil
}

// openStream opens one operation stream and arms ctx cancellation: the
// protocol and wantsync loops speak plain io.ReadWriter, so cancellation is
// delivered by expiring the stream's deadline, which unblocks any pending
// read or write with an error. The returned stop must be called (deferred)
// to release the AfterFunc.
func (c *Client) openStream(ctx context.Context) (net.Conn, func(), error) {
	stream, err := c.conn.OpenStreamConn(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("open stream: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = stream.SetDeadline(time.Now()) })
	return stream, func() { stop() }, nil
}

// CloseStream fully terminates a request/response stream: Close finishes the
// send side (FIN), and CancelRead terminates the receive side, which these
// protocols never read to EOF — after the final expected frame both peers
// just stop reading. A QUIC stream is only retired (and its MAX_STREAMS
// credit returned by the peer) once BOTH directions terminate, so without
// the cancel every operation leaks a half-open stream and the peer's stream
// credit eventually runs dry — the 101st OpenStreamSync then blocks forever.
// Shared by every jobs-iroh client that speaks one-request-per-stream.
func CloseStream(stream net.Conn) {
	_ = stream.Close()
	if cr, ok := stream.(interface{ CancelRead(code uint64) }); ok {
		cr.CancelRead(0)
	}
}
