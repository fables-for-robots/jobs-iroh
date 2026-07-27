package serve

import (
	"net/netip"
	"slices"
	"testing"

	"github.com/tmc/go-iroh/netaddr"
)

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
