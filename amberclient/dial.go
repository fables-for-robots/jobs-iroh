package amberclient

// Dial plumbing lifted from amber-store-iroh cmd/amber/dial.go, minus the
// sharded-transfer machinery (extra endpoints, TAttach): v1 is
// single-connection, so a Client is exactly one endpoint + one connection.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"time"

	"github.com/tmc/go-iroh/iroh"
	"github.com/tmc/go-iroh/iroh/mdns"
	irohkey "github.com/tmc/go-iroh/key"
	"github.com/tmc/go-iroh/netaddr"
	"github.com/tmc/go-iroh/relay"
)

// bindAndResolve binds the client endpoint and produces the candidate
// addresses for the race. With direct addrs it parses/resolves them and
// binds a bare endpoint — no discovery, no relays. Without them it binds
// with the full resolver stack and resolves the endpoint ID.
func bindAndResolve(ctx context.Context, id irohkey.EndpointID, addrs []string, bindAddr netip.AddrPort) (*iroh.Endpoint, []netaddr.TransportAddr, error) {
	var bindOpts []iroh.Option
	if bindAddr.IsValid() {
		bindOpts = append(bindOpts, iroh.WithBindAddr(bindAddr))
	}

	if len(addrs) > 0 {
		cands, err := parseDirectAddrs(ctx, addrs)
		if err != nil {
			return nil, nil, err
		}
		ep, err := iroh.Bind(ctx, bindOpts...)
		if err != nil {
			return nil, nil, fmt.Errorf("amberclient: bind: %w", err)
		}
		return ep, cands, nil
	}

	sk, err := irohkey.GenerateSecretKey()
	if err != nil {
		return nil, nil, fmt.Errorf("amberclient: generate key: %w", err)
	}
	pkarrResolver, err := iroh.N0PkarrResolver(nil)
	if err != nil {
		return nil, nil, fmt.Errorf("amberclient: pkarr resolver: %w", err)
	}
	var services iroh.AddressLookupServices
	// mDNS first: on the server's LAN it yields direct addresses even when
	// the pkarr record is stale or unreachable. Passive (resolve only), and
	// a short lookup timeout so off-LAN dials fall through to pkarr/DNS
	// quickly. Start is the listen loop itself — it blocks until ctx ends,
	// so it runs on its own goroutine; it is only needed while resolving.
	disc := mdns.New(irohkey.EndpointID(sk.Public()), mdns.WithPassive(true), mdns.WithLookupTimeout(time.Second))
	go func() { _ = disc.Start(ctx) }()
	services.AddResolver(disc)
	services.AddResolver(pkarrResolver)
	services.AddResolver(iroh.N0DNSAddressLookup(nil))

	// Relays must be enabled explicitly: this go-iroh build defaults to
	// relay.ModeDisabled, under which the relay candidate in the server's
	// published record can never be dialed and the relay fallback below
	// would be dead weight.
	ep, err := iroh.Bind(ctx, append(bindOpts,
		iroh.WithSecretKey(sk),
		iroh.WithAddressLookup(&services),
		iroh.WithRelayMode(relay.ModeDefault()),
	)...)
	if err != nil {
		return nil, nil, fmt.Errorf("amberclient: bind: %w", err)
	}

	// Connect does no discovery on its own: it only dials addresses already
	// present in the EndpointAddr, so resolve first. One resolver's answer
	// can be a partial view — mDNS yields direct addresses with no relay,
	// and any single record may list candidates this network can't reach —
	// so union every resolver's candidates: the relay then always remains
	// available as fallback when a direct candidate turns out to be dead.
	addr := netaddr.NewEndpointAddr(id)
	resolved := false
	var lastErr error
	for item, err := range services.Resolve(ctx, id) {
		if err != nil {
			lastErr = err
			continue
		}
		addr = addr.WithAddrs(item.Addr().Addrs()...)
		resolved = true
	}
	if !resolved {
		_ = ep.Shutdown(context.WithoutCancel(ctx))
		if lastErr != nil {
			return nil, nil, fmt.Errorf("amberclient: no address found for endpoint %s (last resolver error: %v)", id, lastErr)
		}
		return nil, nil, fmt.Errorf("amberclient: no address found for endpoint %s", id)
	}
	return ep, addr.Addrs(), nil
}

// parseDirectAddrs turns direct address strings into dial candidates. Each
// value is host:port where host is an IP literal or a hostname; hostnames
// may resolve to several addresses and all of them become candidates.
func parseDirectAddrs(ctx context.Context, addrs []string) ([]netaddr.TransportAddr, error) {
	var out []netaddr.TransportAddr
	for _, s := range addrs {
		if ap, err := netip.ParseAddrPort(s); err == nil {
			out = append(out, netaddr.IPAddr{Addr: ap})
			continue
		}
		host, portStr, err := net.SplitHostPort(s)
		if err != nil {
			return nil, fmt.Errorf("amberclient: parse addr %q: %w", s, err)
		}
		port, err := strconv.ParseUint(portStr, 10, 16)
		if err != nil {
			return nil, fmt.Errorf("amberclient: parse addr %q: bad port: %w", s, err)
		}
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("amberclient: resolve addr host %q: %w", host, err)
		}
		for _, ip := range ips {
			out = append(out, netaddr.IPAddr{Addr: netip.AddrPortFrom(ip.Unmap(), uint16(port))})
		}
	}
	return out, nil
}

// raceConnect dials every candidate address concurrently and returns the
// first connection to complete. go-iroh's own multi-candidate connect walks
// a sorted candidate list with a per-candidate budget, so a few unreachable
// addresses (which sort low: container-bridge 10.x/172.x before LAN
// 192.168.x) exhaust the handshake window before a live one is tried; a live
// candidate answers in milliseconds when dialed directly. Losing attempts
// are canceled; late winners are closed.
func raceConnect(ctx context.Context, ep *iroh.Endpoint, id irohkey.EndpointID, alpn string, cands []netaddr.TransportAddr) (*iroh.Conn, error) {
	if len(cands) == 0 {
		return nil, fmt.Errorf("no candidate addresses for endpoint %s", id)
	}
	type result struct {
		conn *iroh.Conn
		err  error
	}
	results := make(chan result, len(cands))
	cancels := make([]context.CancelFunc, len(cands))
	for i, ta := range cands {
		actx, cancel := context.WithCancel(ctx)
		cancels[i] = cancel
		go func(ta netaddr.TransportAddr) {
			conn, err := ep.Connect(actx, netaddr.NewEndpointAddr(id, ta), alpn)
			results <- result{conn, err}
		}(ta)
	}
	var errs []error
	for range cands {
		r := <-results
		if r.err != nil {
			errs = append(errs, r.err)
			continue
		}
		// Winner: stop the losers and close any that already made it.
		for _, cancel := range cancels {
			cancel()
		}
		remaining := len(cands) - len(errs) - 1
		go func(n int) {
			for ; n > 0; n-- {
				if late := <-results; late.conn != nil {
					late.conn.Close()
				}
			}
		}(remaining)
		return r.conn, nil
	}
	for _, cancel := range cancels {
		cancel()
	}
	return nil, errors.Join(errs...)
}
