package serve

// Address advertising, ported from amber-store-iroh cmd/amber-serve/addrs.go:
// the operator's --advertise-addr parsing and the pkarr publisher's address
// filter. Which of the machine's own addresses are worth telling a peer about
// lives in package hostaddr, because the client needs the same answer.

import (
	"fmt"
	"net/netip"

	"github.com/tmc/go-iroh/netaddr"
)

// parseAdvertiseAddrs turns --advertise-addr values (ip or ip:port) into
// socket addresses; a bare IP gets the endpoint's bound port. Explicit
// values replace interface auto-detection entirely, for hosts where the
// heuristics advertise unreachable addresses (or miss NAT mappings).
func parseAdvertiseAddrs(vals []string, defaultPort uint16) ([]netip.AddrPort, error) {
	out := make([]netip.AddrPort, 0, len(vals))
	for _, v := range vals {
		if ap, err := netip.ParseAddrPort(v); err == nil {
			out = append(out, ap)
			continue
		}
		ip, err := netip.ParseAddr(v)
		if err != nil {
			return nil, fmt.Errorf("parse --advertise-addr %q: want ip or ip:port: %w", v, err)
		}
		out = append(out, netip.AddrPortFrom(ip.Unmap(), defaultPort))
	}
	return out, nil
}

// publishableAddrs is the pkarr publisher's address filter: everything
// except unusable IP candidates. The publisher's default (RelayOnlyFilter)
// would strip the direct addresses we advertise on purpose; publishing
// unfiltered would leak the raw wildcard bind address that rides along in
// the endpoint's address set and must not be advertised.
func publishableAddrs(addrs []netaddr.TransportAddr) []netaddr.TransportAddr {
	out := make([]netaddr.TransportAddr, 0, len(addrs))
	for _, a := range addrs {
		if ip, ok := a.(netaddr.IPAddr); ok && (!ip.Addr.IsValid() || ip.Addr.Addr().IsUnspecified()) {
			continue
		}
		out = append(out, a)
	}
	return out
}
