package amberclient

// Sharded transfers, ported from amber-store-iroh cmd/amber (dial.go's
// attachExtras + push.go's runSenders): a transfer with Conns > 1 asks the
// server for extra data channels, attaches one stream per extra QUIC
// connection — each on its own endpoint, because an endpoint's single UDP
// socket loop caps throughput — and the want loop deals every round across
// all of them. A server without sharding support (no token in its reply)
// degrades to the single control stream, as do attach failures.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/jobs-build/jobs-iroh/amberiroh"
	"github.com/tmc/go-iroh/iroh"
	"github.com/tmc/go-iroh/netaddr"
	"github.com/tmc/go-iroh/relay"

	"github.com/jobs-build/jobs-iroh/amber"
)

// Attach timing. The server serves whatever shards arrived within its gather
// window (ambserver's attachWait, 10s; 5s on pre-DataEndpoints servers) —
// but a TAttach landing LATER is
// silently parked on a channel nothing ever reads. Handing such a dead
// channel to the want loop deadlocks a pull and fails a push whose ref the
// server already committed. So a shard's whole attach — dial, stream, TAttach
// frame — runs under a budget safely below that window, and a shard that
// misses it is closed, never used. (Residual risk needs one-way latency
// beyond seconds; links that slow rarely complete the dial inside the budget
// at all.)
const (
	// attachBudget bounds one shard's full attach.
	attachBudget = 3 * time.Second
	// dedicatedDialTimeout bounds the dial to an advertised data port. The
	// ports are kernel-assigned (rarely firewall-open) and a filtered UDP
	// port fails only by timeout, so the sub-budget keeps room for the
	// control-candidate fallback.
	dedicatedDialTimeout = 1500 * time.Millisecond
	// punchAttachBudget replaces attachBudget when the server advertised
	// DataEndpoints (which is also the signal that it gathers for 10s): a
	// punching attach pays a relay connect before its TAttach, and the
	// budget must still land safely inside the server's window.
	punchAttachBudget = 8 * time.Second
)

// extraConn opens one more connection for shard i. A server that advertised
// DataEndpoints records gets a record-authenticated dial: the dedicated
// port (on the address the control connection reached) races the record's
// own candidates. On a discovery-dialed client the shard endpoint binds
// with the control dial's full stack — relay mode, net report, seeded local
// candidates — which is what feeds QNT punch coordination, so a relay-won
// shard upgrades to direct mid-transfer exactly like the control connection
// did. A direct-addr client (Options.Addrs) stays bare, mirroring the
// control dial's own direct branch. Without records, the legacy path runs:
// dedicated ports under the server identity, then the control candidates.
func (c *Client) extraConn(ctx context.Context, i int, ports []uint16, eps []amberiroh.DataEndpointRec) (*iroh.Conn, *iroh.Endpoint, error) {
	ctrlIP, haveCtrlIP := c.remoteIP()
	id, cands, ok, terr := shardTarget(i, ctrlIP, haveCtrlIP, ports, eps, !c.punchDials)
	if terr != nil {
		return nil, nil, terr
	}
	if ok {
		ep, err := c.bindShardEndpoint(ctx)
		if err != nil {
			return nil, nil, err
		}
		conn, err := raceConnect(ctx, ep, id, c.alpn, cands)
		if err != nil {
			_ = ep.Shutdown(context.WithoutCancel(ctx))
			return nil, nil, err
		}
		return conn, ep, nil
	}

	var bindOpts []iroh.Option
	if c.bindAddr.IsValid() {
		// Same host as the pinned bind, port 0: every shard needs its own
		// socket.
		bindOpts = append(bindOpts, iroh.WithBindAddr(netip.AddrPortFrom(c.bindAddr.Addr(), 0)))
	}
	ep, err := iroh.Bind(ctx, bindOpts...)
	if err != nil {
		return nil, nil, err
	}
	if len(ports) > 0 && haveCtrlIP {
		dctx, cancel := context.WithTimeout(ctx, dedicatedDialTimeout)
		cand := []netaddr.TransportAddr{netaddr.IPAddr{Addr: netip.AddrPortFrom(ctrlIP, ports[i%len(ports)])}}
		conn, derr := raceConnect(dctx, ep, c.id, c.alpn, cand)
		cancel()
		if derr == nil {
			return conn, ep, nil
		}
	}
	conn, err := raceConnect(ctx, ep, c.id, c.alpn, c.cands)
	if err != nil {
		_ = ep.Shutdown(context.WithoutCancel(ctx))
		return nil, nil, err
	}
	return conn, ep, nil
}

// bindShardEndpoint binds one record-authenticated shard's endpoint.
func (c *Client) bindShardEndpoint(ctx context.Context) (*iroh.Endpoint, error) {
	var opts []iroh.Option
	if c.bindAddr.IsValid() {
		opts = append(opts, iroh.WithBindAddr(netip.AddrPortFrom(c.bindAddr.Addr(), 0)))
	}
	if c.punchDials {
		// See bindAndResolve's discovery branch for why each piece exists;
		// mDNS/pkarr are omitted — the candidates are already provided.
		opts = append(opts, iroh.WithRelayMode(relay.ModeDefault()), iroh.WithNetReport())
	}
	ep, err := iroh.Bind(ctx, opts...)
	if err != nil {
		return nil, err
	}
	if c.punchDials {
		seedLocalCandidates(ep)
	}
	return ep, nil
}

// remoteIP is the address the control connection actually reached the
// server at.
func (c *Client) remoteIP() (netip.Addr, bool) {
	ua, ok := c.conn.RemoteAddr().(*net.UDPAddr)
	if !ok {
		return netip.Addr{}, false
	}
	ip, ok := netip.AddrFromSlice(ua.IP)
	if !ok {
		return netip.Addr{}, false
	}
	return ip.Unmap(), true
}

// attachExtras opens up to n extra connections concurrently, attaches each
// to the transfer token, and returns the attached streams with a closer.
// Failures (or attaches that would miss the server's gather window — see
// attachBudget) reduce parallelism instead of failing the transfer. Every
// returned stream is armed with ctx cancellation exactly like the control
// stream, so a cancelled transfer unblocks all channels. When not a single
// shard attaches, the client demotes itself: this connection's topology
// evidently cannot shard (relay-only path, filtered ports), and paying the
// attach stall on every subsequent transfer would be pure waste.
func (c *Client) attachExtras(ctx context.Context, token []byte, ports []uint16, eps []amberiroh.DataEndpointRec, n int) ([]net.Conn, func()) {
	budget := attachBudget
	if len(eps) > 0 && c.punchDials {
		budget = punchAttachBudget
	}
	type extra struct {
		stream net.Conn
		close  func()
	}
	slots := make([]*extra, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			actx, cancel := context.WithTimeout(ctx, budget)
			defer cancel()

			conn, ep, err := c.extraConn(actx, i, ports, eps)
			if err != nil {
				c.log.Debug("amberclient: shard attach failed", "shard", i, "error", err)
				return
			}
			teardown := func() {
				_ = conn.Close()
				_ = ep.Shutdown(context.WithoutCancel(ctx))
			}
			stream, err := conn.OpenStreamConn(actx)
			if err != nil {
				teardown()
				return
			}
			// The TAttach frame must land inside the budget too; once it is
			// out, the stream belongs to the whole transfer, so the write
			// deadline is lifted and the transfer ctx takes over.
			if dl, ok := actx.Deadline(); ok {
				_ = stream.SetWriteDeadline(dl)
			}
			err = amberiroh.WriteMsg(stream, amberiroh.Msg{Type: amberiroh.TAttach, Token: token})
			if err != nil || actx.Err() != nil {
				CloseStream(stream)
				teardown()
				return
			}
			_ = stream.SetWriteDeadline(time.Time{})
			stop := context.AfterFunc(ctx, func() { _ = stream.SetDeadline(time.Now()) })
			slots[i] = &extra{stream: stream, close: func() {
				stop()
				CloseStream(stream)
				teardown()
			}}
		}(i)
	}
	wg.Wait()
	var streams []net.Conn
	var closers []func()
	for _, e := range slots {
		if e != nil {
			streams = append(streams, e.stream)
			closers = append(closers, e.close)
		}
	}
	// A cancelled transfer proves nothing about the path (mirrors
	// retrySingle's guard) — only a live context's zero-attach does.
	if len(streams) == 0 && n > 0 && ctx.Err() == nil {
		c.demote("no shard attached within budget")
	}
	return streams, func() {
		for _, cl := range closers {
			cl()
		}
	}
}

// shardReadIdle bounds how long one Read on a pull's shard channel may sit
// without data. On pull the server answers each channel's TWants shard
// independently and promptly, so a healthy channel never idles long
// mid-round — but a shard whose TAttach landed just past the server's
// gather window (a transient delivery stall the local attach budget cannot
// see) is parked: nothing ever reads or answers it, and without a bound the
// want loop deadlocks. The watchdog turns that into a failed round, which
// retrySingle heals unsharded.
const shardReadIdle = 30 * time.Second

// watchdogConn arms a rolling read deadline on a pull shard channel.
type watchdogConn struct {
	net.Conn
	ctx context.Context
}

func (w *watchdogConn) Read(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	// Re-armed per read. This can overwrite the cancellation AfterFunc's
	// immediate deadline in a narrow race, delaying that one read's
	// unblock by at most shardReadIdle — bounded, and only on shards.
	_ = w.Conn.SetReadDeadline(time.Now().Add(shardReadIdle))
	return w.Conn.Read(p)
}

// runSenders drives a push's sending side. With one connection it is exactly
// the plain Send loop. With more, the first server frame decides: TAccept
// means a sharding-aware server — attach the extra connections and run one
// Send loop per channel; a TWants means a server that ignored the request's
// DataConns — replay the consumed frame in front of the stream and fall back
// to a single channel; a TErr surfaces as the usual remote error.
func (c *Client) runSenders(ctx context.Context, ctrl io.ReadWriter, st *amber.Store, prog amberiroh.Progress, conns int) error {
	if conns <= 1 {
		return amberiroh.Send(ctrl, st.Objects(), prog)
	}
	first, err := amberiroh.ReadMsg(ctrl)
	if err != nil {
		return err
	}
	switch first.Type {
	case amberiroh.TErr:
		return amberiroh.RemoteFromMsg(first)
	case amberiroh.TWants:
		var replay bytes.Buffer
		if err := amberiroh.WriteMsg(&replay, first); err != nil {
			return err
		}
		fallback := struct {
			io.Reader
			io.Writer
		}{io.MultiReader(&replay, ctrl), ctrl}
		return amberiroh.Send(fallback, st.Objects(), prog)
	case amberiroh.TAccept:
	default:
		return fmt.Errorf("%w: type %d, want TAccept or TWants", amberiroh.ErrProtocol, first.Type)
	}

	extras, closeExtras := c.attachExtras(ctx, first.Token, first.DataPorts, first.DataEndpoints, conns-1)
	defer closeExtras()
	// No read watchdog here, unlike pull: a push channel legitimately sits
	// in a read for as long as the server takes to verify and store a whole
	// round before its next TWants, which is unbounded. A parked channel's
	// Send unblocks at transfer end (the server closes ungathered streams)
	// and is tolerated below.
	channels := []io.ReadWriter{ctrl}
	for _, ex := range extras {
		channels = append(channels, ex)
	}
	errs := make([]error, len(channels))
	var wg sync.WaitGroup
	for i, ch := range channels {
		wg.Add(1)
		go func(i int, ch io.ReadWriter) {
			defer wg.Done()
			errs[i] = amberiroh.Send(ch, st.Objects(), prog)
		}(i, ch)
	}
	wg.Wait()
	if errs[0] != nil {
		return errors.Join(errs...)
	}
	if err := errors.Join(errs[1:]...); err != nil {
		// The control loop completed, so the server's commit verdict (read
		// next by the caller) is authoritative. A failed shard channel only
		// reduced parallelism: the server never depends on a channel it did
		// not gather, and a gathered channel that died fails the server's
		// OWN Receive — which surfaces as TErr on the control stream, not
		// here. (This is also what a server closing never-gathered late
		// shards at transfer end looks like: EOF after commit.)
		c.log.Debug("amberclient: shard channels errored after control completed", "error", err)
	}
	return nil
}
