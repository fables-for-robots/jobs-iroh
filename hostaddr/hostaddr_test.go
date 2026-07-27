package hostaddr

import (
	"net"
	"net/netip"
	"slices"
	"testing"
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
	in := []IfaceAddrs{
		{Name: "en0", Up: true, Addrs: []net.Addr{
			ipNet("192.168.1.50/24"),   // LAN IPv4: keep
			ipNet("fe80::1/64"),        // link-local: drop
			ipNet("192.168.1.50/24"),   // duplicate: dedupe
			ipNet("2001:db8::1234/64"), // global IPv6: keep
		}},
		{Name: "lo0", Up: true, Addrs: []net.Addr{
			ipNet("127.0.0.1/8"), // loopback: drop
			ipNet("::1/128"),     // loopback: drop
		}},
		{Name: "en5", Up: false, Addrs: []net.Addr{
			ipNet("10.9.9.9/8"), // down interface: drop
		}},
		{Name: "utun3", Up: true, Addrs: []net.Addr{
			ipNet("100.100.1.2/32"), // VPN/tailscale: keep
		}},
		{Name: "misc", Up: true, Addrs: []net.Addr{
			ipNet("0.0.0.0/0"), // unspecified: drop
			&net.TCPAddr{IP: net.ParseIP("192.168.1.60")}, // not *net.IPNet: drop
		}},
	}
	got := AdvertisedAddrPorts(in, 4242)
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
	in := []IfaceAddrs{
		{Name: "eth0", Up: true, Addrs: []net.Addr{ipNet("192.168.1.61/24")}},
		{Name: "cni0", Up: true, Addrs: []net.Addr{ipNet("10.42.1.1/24")}},
		{Name: "flannel.1", Up: true, Addrs: []net.Addr{ipNet("10.42.1.0/32")}},
		{Name: "docker0", Up: true, Addrs: []net.Addr{ipNet("172.17.0.1/16")}},
		{Name: "br-9f2a", Up: true, Addrs: []net.Addr{ipNet("172.20.0.1/16")}},
		{Name: "veth12ab", Up: true, Addrs: []net.Addr{ipNet("172.21.0.1/16")}},
		{Name: "virbr0", Up: true, Addrs: []net.Addr{ipNet("192.168.122.1/24")}},
		{Name: "lxcbr0", Up: true, Addrs: []net.Addr{ipNet("10.0.3.1/24")}},
	}
	got := AdvertisedAddrPorts(in, 39192)
	want := []netip.AddrPort{netip.MustParseAddrPort("192.168.1.61:39192")}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAdvertisedAddrPortsUnmapsIPv4InIPv6(t *testing.T) {
	in := []IfaceAddrs{{Name: "en0", Up: true, Addrs: []net.Addr{ipNet("::ffff:192.168.1.9/96")}}}
	got := AdvertisedAddrPorts(in, 1)
	want := []netip.AddrPort{netip.MustParseAddrPort("192.168.1.9:1")}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
