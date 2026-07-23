package serve

import (
	"net"
	"net/netip"
	"slices"
	"testing"

	"github.com/tmc/go-iroh/netaddr"
)

func ipNet(s string) *net.IPNet {
	ip, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	n.IP = ip
	return n
}

func TestAdvertisedAddrPorts(t *testing.T) {
	in := []ifaceAddrs{
		{name: "en0", up: true, addrs: []net.Addr{
			ipNet("192.168.1.50/24"),   // LAN IPv4: keep
			ipNet("fe80::1/64"),        // link-local: drop
			ipNet("192.168.1.50/24"),   // duplicate: dedupe
			ipNet("2001:db8::1234/64"), // global IPv6: keep
		}},
		{name: "lo0", up: true, addrs: []net.Addr{
			ipNet("127.0.0.1/8"), // loopback: drop
			ipNet("::1/128"),     // loopback: drop
		}},
		{name: "en5", up: false, addrs: []net.Addr{
			ipNet("10.9.9.9/8"), // down interface: drop
		}},
		{name: "utun3", up: true, addrs: []net.Addr{
			ipNet("100.100.1.2/32"), // VPN/tailscale: keep
		}},
		{name: "misc", up: true, addrs: []net.Addr{
			ipNet("0.0.0.0/0"), // unspecified: drop
			&net.TCPAddr{IP: net.ParseIP("192.168.1.60")}, // not *net.IPNet: drop
		}},
	}
	got := advertisedAddrPorts(in, 4242)
	want := []netip.AddrPort{
		netip.MustParseAddrPort("192.168.1.50:4242"),
		netip.MustParseAddrPort("[2001:db8::1234]:4242"),
		netip.MustParseAddrPort("100.100.1.2:4242"),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestAdvertisedAddrPortsSkipsContainerBridges pins the gx10 failure:
// k3s (cni0/flannel) and docker bridge addresses are unreachable from
// other machines, and every advertised dead candidate costs connecting
// peers real handshake budget.
func TestAdvertisedAddrPortsSkipsContainerBridges(t *testing.T) {
	in := []ifaceAddrs{
		{name: "eth0", up: true, addrs: []net.Addr{ipNet("192.168.1.61/24")}},
		{name: "cni0", up: true, addrs: []net.Addr{ipNet("10.42.1.1/24")}},
		{name: "flannel.1", up: true, addrs: []net.Addr{ipNet("10.42.1.0/32")}},
		{name: "docker0", up: true, addrs: []net.Addr{ipNet("172.17.0.1/16")}},
		{name: "br-9f2a", up: true, addrs: []net.Addr{ipNet("172.20.0.1/16")}},
		{name: "veth12ab", up: true, addrs: []net.Addr{ipNet("172.21.0.1/16")}},
		{name: "virbr0", up: true, addrs: []net.Addr{ipNet("192.168.122.1/24")}},
		{name: "lxcbr0", up: true, addrs: []net.Addr{ipNet("10.0.3.1/24")}},
	}
	got := advertisedAddrPorts(in, 39192)
	want := []netip.AddrPort{netip.MustParseAddrPort("192.168.1.61:39192")}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAdvertisedAddrPortsUnmapsIPv4InIPv6(t *testing.T) {
	in := []ifaceAddrs{{name: "en0", up: true, addrs: []net.Addr{ipNet("::ffff:192.168.1.9/96")}}}
	got := advertisedAddrPorts(in, 1)
	want := []netip.AddrPort{netip.MustParseAddrPort("192.168.1.9:1")}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestParseAdvertiseAddrs(t *testing.T) {
	got, err := parseAdvertiseAddrs([]string{"192.168.1.61", "203.0.113.9:5000", "2001:db8::7"}, 39192)
	if err != nil {
		t.Fatal(err)
	}
	want := []netip.AddrPort{
		netip.MustParseAddrPort("192.168.1.61:39192"),
		netip.MustParseAddrPort("203.0.113.9:5000"),
		netip.MustParseAddrPort("[2001:db8::7]:39192"),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if _, err := parseAdvertiseAddrs([]string{"gx10.local"}, 1); err == nil {
		t.Fatal("hostnames must be rejected with a clear error")
	}
}

func TestPublishableAddrsDropsWildcard(t *testing.T) {
	relay := netaddr.RelayAddr{}
	keep := netaddr.IPAddr{Addr: netip.MustParseAddrPort("192.168.1.9:1")}
	in := []netaddr.TransportAddr{
		relay,
		keep,
		netaddr.IPAddr{Addr: netip.MustParseAddrPort("[::]:1")},    // wildcard: drop
		netaddr.IPAddr{Addr: netip.MustParseAddrPort("0.0.0.0:1")}, // wildcard: drop
		netaddr.IPAddr{}, // invalid: drop
	}
	got := publishableAddrs(in)
	if len(got) != 2 || got[0] != relay || got[1] != keep {
		t.Fatalf("got %v", got)
	}
}
