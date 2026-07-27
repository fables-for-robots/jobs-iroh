// Package hostaddr picks which of the machine's own addresses are worth
// telling a peer about.
//
// Both ends of a connection need this. The server publishes them in its
// discovery record so peers have something to dial besides a relay; a client
// seeds them as QNT NAT traversal candidates so a server behind NAT can open a
// direct path back. Neither can rely on the endpoint's bound address, which is
// the wildcard, nor on QUIC address discovery, which reports nothing until a
// relay observes the host.
//
// The walk is deliberately conservative: an address that is not reachable from
// another machine costs a peer real connect budget (go-iroh walks candidates
// with a per-candidate timeout rather than racing them), so container bridges,
// loopback, link-local and down interfaces are all dropped.
package hostaddr

import (
	"net"
	"net/netip"
	"strings"
)

// IfaceAddrs is one network interface's name, state, and addresses — the seam
// that lets the filter be tested without real interfaces.
type IfaceAddrs struct {
	Name  string
	Up    bool
	Addrs []net.Addr
}

// bridgePrefixes match container and virtualization plumbing whose addresses
// are unreachable from other machines; advertising them makes peers burn
// connect budget on black holes.
var bridgePrefixes = []string{"docker", "br-", "cni", "flannel", "veth", "virbr", "lxc"}

// IsBridgeName reports whether name looks like container or virtualization
// plumbing.
func IsBridgeName(name string) bool {
	for _, p := range bridgePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// AdvertisedAddrPorts pairs the machine's unicast interface addresses with the
// endpoint's bound UDP port, yielding the addresses worth telling a peer about.
// The wildcard bind address is not a usable dial candidate and is dropped from
// published records, which would leave peers with only a relay path; these are
// the candidates that let them go direct. Down interfaces, container bridges,
// loopback, link-local, and unspecified addresses are excluded, duplicates
// removed, order preserved.
func AdvertisedAddrPorts(ifaces []IfaceAddrs, port uint16) []netip.AddrPort {
	var out []netip.AddrPort
	seen := make(map[netip.Addr]bool)
	for _, ifc := range ifaces {
		if !ifc.Up || IsBridgeName(ifc.Name) {
			continue
		}
		for _, a := range ifc.Addrs {
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

// Local snapshots the machine's interfaces for [AdvertisedAddrPorts].
func Local() ([]IfaceAddrs, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]IfaceAddrs, 0, len(ifaces))
	for _, ifc := range ifaces {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		out = append(out, IfaceAddrs{Name: ifc.Name, Up: ifc.Flags&net.FlagUp != 0, Addrs: addrs})
	}
	return out, nil
}

// LocalAddrPorts is [Local] followed by [AdvertisedAddrPorts], for callers with
// nothing to inject.
func LocalAddrPorts(port uint16) ([]netip.AddrPort, error) {
	ifaces, err := Local()
	if err != nil {
		return nil, err
	}
	return AdvertisedAddrPorts(ifaces, port), nil
}
