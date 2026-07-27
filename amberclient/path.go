package amberclient

// Transport path reporting: whether a connection reaches the peer directly
// (LAN or hole-punched UDP) or through a relay. It matters most for store
// transfers — a relayed path carries every CAS byte through a third party,
// which caps throughput and is one of the usual reasons a connection demotes
// itself out of sharded transfers (see Client.demote).

import (
	"context"
	"time"

	"github.com/tmc/go-iroh/iroh"
)

// Path summarises how a connection currently reaches its peer.
type Path struct {
	// Relayed reports that the path in use runs through a relay server.
	Relayed bool
	// Addr is that path's transport address ("ip:host:port", "relay:url"),
	// empty when the transport reported none.
	Addr string
	// RTT is that path's smoothed round-trip time; 0 when unobserved.
	RTT time.Duration
}

// Kind is the one-word transport classification, for logs.
func (p Path) Kind() string {
	if p.Relayed {
		return "relay"
	}
	return "direct"
}

// LogAttrs renders p as slog key/value pairs.
func (p Path) LogAttrs() []any {
	attrs := []any{"transport", p.Kind()}
	if p.Addr != "" {
		attrs = append(attrs, "addr", p.Addr)
	}
	if p.RTT > 0 {
		attrs = append(attrs, "rtt", p.RTT.Round(time.Microsecond).String())
	}
	return attrs
}

// ConnPath summarises the path conn is currently using. It reports false
// when the transport exposes no open path — a connection that has not
// finished handshaking, or one already closed.
func ConnPath(conn *iroh.Conn) (Path, bool) { return pathOf(conn.Paths()) }

// Path summarises how the control connection reaches the server. Shard
// connections for a transfer are dialed at the same candidates, so this is
// representative of the whole transfer.
func (c *Client) Path() (Path, bool) { return ConnPath(c.conn) }

// pathOf picks the path in use out of a snapshot and summarises it.
func pathOf(paths []iroh.PathInfo) (Path, bool) {
	best := -1
	for i := range paths {
		if best < 0 || preferred(paths[i], paths[best]) {
			best = i
		}
	}
	if best < 0 {
		return Path{}, false
	}
	p := paths[best]
	out := Path{Relayed: p.Relayed}
	if p.HasAddr {
		out.Addr = p.Addr.String()
	}
	if p.HasRTT {
		out.RTT = p.RTT
	}
	return out, true
}

// preferred reports whether a is the likelier path in use than b: the path
// the transport says it selected wins outright. The rest is a tie-break for
// snapshots that carry no selection — a validated path over an unvalidated
// one, a direct path over a relayed one, then the lower RTT.
func preferred(a, b iroh.PathInfo) bool {
	if a.Selected != b.Selected {
		return a.Selected
	}
	if a.Validated != b.Validated {
		return a.Validated
	}
	if a.Relayed != b.Relayed {
		return b.Relayed
	}
	if a.HasRTT != b.HasRTT {
		return a.HasRTT
	}
	return a.HasRTT && a.RTT < b.RTT
}

// WatchPath reports conn's path to fn: once synchronously with the current
// snapshot, then from a goroutine on every later change, until ctx ends or
// the connection closes. The follow-ups are the point — a connection that
// starts out relayed commonly upgrades to direct a moment after the dial,
// once hole punching lands, and can fall back to the relay later.
//
// Nothing is reported while the transport exposes no path at all.
func WatchPath(ctx context.Context, conn *iroh.Conn, fn func(Path)) {
	last, had := ConnPath(conn)
	if had {
		fn(last)
	}
	events, err := conn.WatchPaths(ctx)
	if err != nil {
		// No path observation on this connection: the one snapshot above is
		// all this connection will ever report.
		return
	}
	go func() {
		for range events {
			p, ok := ConnPath(conn)
			if !ok || (had && p == last) {
				continue
			}
			last, had = p, true
			fn(p)
		}
	}()
}

// WatchPath reports the control connection's path, as [WatchPath].
func (c *Client) WatchPath(ctx context.Context, fn func(Path)) {
	WatchPath(ctx, c.conn, fn)
}
