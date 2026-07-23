package serve

// Address advertising, ported from amber-store-iroh cmd/amber-serve/addrs.go:
// which of the machine's addresses are worth publishing, and the pkarr
// publisher's address filter.

import (
	"fmt"
	"net"
	"net/netip"
	"strings"

	"github.com/tmc/go-iroh/netaddr"
)

// ifaceAddrs is one network interface's name, state, and addresses —
// the seam that lets the advertising filter be tested without real
// interfaces.
type ifaceAddrs struct {
	name  string
	up    bool
	addrs []net.Addr
}

// bridgePrefixes match container and virtualization plumbing whose
// addresses are unreachable from other machines; advertising them makes
// peers burn connect budget on black holes (go-iroh walks candidates with
// a per-candidate timeout rather than racing them).
var bridgePrefixes = []string{"docker", "br-", "cni", "flannel", "veth", "virbr", "lxc"}

func isBridgeName(name string) bool {
	for _, p := range bridgePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// advertisedAddrPorts pairs the machine's unicast interface addresses
// with the endpoint's bound UDP port, yielding the direct addresses worth
// advertising. The default wildcard bind address is not a usable dial
// candidate and is dropped from published records, which would leave
// peers with only a relay path; these are the candidates that let them
// dial direct. Down interfaces, container bridges, loopback, link-local,
// and unspecified addresses are excluded, duplicates removed, order
// preserved.
func advertisedAddrPorts(ifaces []ifaceAddrs, port uint16) []netip.AddrPort {
	var out []netip.AddrPort
	seen := make(map[netip.Addr]bool)
	for _, ifc := range ifaces {
		if !ifc.up || isBridgeName(ifc.name) {
			continue
		}
		for _, a := range ifc.addrs {
			n, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(n.IP)
			if !ok {
				continue
			}
			ip = ip.Unmap()
			if seen[ip] || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || !ip.IsValid() || ip.IsUnspecified() {
				continue
			}
			seen[ip] = true
			out = append(out, netip.AddrPortFrom(ip, port))
		}
	}
	return out
}

// localIfaceAddrs snapshots the machine's interfaces for
// advertisedAddrPorts.
func localIfaceAddrs() ([]ifaceAddrs, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]ifaceAddrs, 0, len(ifaces))
	for _, ifc := range ifaces {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		out = append(out, ifaceAddrs{name: ifc.Name, up: ifc.Flags&net.FlagUp != 0, addrs: addrs})
	}
	return out, nil
}

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
