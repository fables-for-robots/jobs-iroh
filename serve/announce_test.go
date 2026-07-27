package serve

import (
	"iter"
	"net/netip"
	"testing"

	irohkey "github.com/tmc/go-iroh/key"
	"github.com/tmc/go-iroh/netaddr"
)

func ipAddr(s string) netaddr.IPAddr {
	return netaddr.IPAddr{Addr: netip.MustParseAddrPort(s)}
}

func addrSet(addrs ...netaddr.TransportAddr) netaddr.EndpointAddr {
	return netaddr.NewEndpointAddr(irohkey.EndpointID{}, addrs...)
}

// TestPinnedAddrKeepsDiscoveredAndPinned covers the whole point of pinning:
// go-iroh's net report REPLACES the endpoint's external candidate set when it
// lands, dropping everything announce contributed via AddExternalAddr. The
// live set then carries only the discovered public mapping; merging the pinned
// list back in is what keeps LAN peers resolvable.
func TestPinnedAddrKeepsDiscoveredAndPinned(t *testing.T) {
	// What ep.Addr() looks like after a net report replaced the candidates:
	// wildcard bind + relay + the QAD-discovered public mapping.
	live := addrSet(
		ipAddr("[::]:40817"),
		ipAddr("203.0.113.7:40817"), // discovered public mapping
	)
	pinned := []netip.AddrPort{
		netip.MustParseAddrPort("192.168.1.61:40817"),
		netip.MustParseAddrPort("192.168.1.157:40817"),
	}

	got := pinnedAddr(live, pinned)

	want := []string{
		"ip:192.168.1.61:40817",
		"ip:192.168.1.157:40817",
		"ip:203.0.113.7:40817",
		"ip:[::]:40817",
	}
	assertAddrs(t, got, want)
}

func TestPinnedAddrDedupes(t *testing.T) {
	live := addrSet(ipAddr("192.168.1.61:40817"))
	pinned := []netip.AddrPort{netip.MustParseAddrPort("192.168.1.61:40817")}

	assertAddrs(t, pinnedAddr(live, pinned), []string{"ip:192.168.1.61:40817"})
}

// TestPublishAddrChangesRepublishesOnNewMapping is the regression this whole
// change exists for: the public mapping is discovered AFTER the first publish,
// and must reach the record.
func TestPublishAddrChangesRepublishesOnNewMapping(t *testing.T) {
	pinned := []netip.AddrPort{netip.MustParseAddrPort("192.168.1.61:40817")}
	seq := func(yield func(netaddr.EndpointAddr) bool) {
		// t=0: no net report yet — wildcard only.
		if !yield(addrSet(ipAddr("[::]:40817"))) {
			return
		}
		// t=1s: QAD lands, the public mapping appears.
		yield(addrSet(ipAddr("[::]:40817"), ipAddr("203.0.113.7:40817")))
	}

	var published []netaddr.EndpointAddr
	publishAddrChanges(seq, pinned, func(a netaddr.EndpointAddr) {
		published = append(published, a)
	})

	if len(published) != 2 {
		t.Fatalf("want 2 publishes (initial + discovered mapping), got %d: %v", len(published), published)
	}
	assertAddrs(t, published[0], []string{"ip:192.168.1.61:40817", "ip:[::]:40817"})
	assertAddrs(t, published[1], []string{
		"ip:192.168.1.61:40817",
		"ip:203.0.113.7:40817",
		"ip:[::]:40817",
	})
}

// TestPublishAddrChangesSkipsUnchanged keeps the pkarr publisher quiet when the
// endpoint reports a change that the pinned merge flattens away.
func TestPublishAddrChangesSkipsUnchanged(t *testing.T) {
	pinned := []netip.AddrPort{netip.MustParseAddrPort("192.168.1.61:40817")}
	seq := func(yield func(netaddr.EndpointAddr) bool) {
		if !yield(addrSet(ipAddr("192.168.1.61:40817"))) {
			return
		}
		// The net report dropped the LAN candidate; pinning puts it straight
		// back, so the merged record is identical and must not be republished.
		if !yield(addrSet()) {
			return
		}
		yield(addrSet(ipAddr("192.168.1.61:40817")))
	}

	var n int
	publishAddrChanges(seq, pinned, func(netaddr.EndpointAddr) { n++ })

	if n != 1 {
		t.Fatalf("want 1 publish for an unchanged merged record, got %d", n)
	}
}

var _ iter.Seq[netaddr.EndpointAddr] = (func(func(netaddr.EndpointAddr) bool))(nil)

func assertAddrs(t *testing.T, got netaddr.EndpointAddr, want []string) {
	t.Helper()
	addrs := got.Addrs()
	if len(addrs) != len(want) {
		t.Fatalf("got %d addrs %v, want %d %v", len(addrs), got, len(want), want)
	}
	for i, a := range addrs {
		if a.String() != want[i] {
			t.Fatalf("addr %d: got %q, want %q (full: %v)", i, a.String(), want[i], got)
		}
	}
}
