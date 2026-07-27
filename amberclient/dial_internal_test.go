package amberclient

import (
	"context"
	"net/netip"
	"slices"
	"testing"

	"github.com/jobs-build/jobs-iroh/hostaddr"

	"github.com/tmc/go-iroh/iroh"
)

// TestSeedLocalCandidates pins the reason a client is dialable at all: without
// the seed the endpoint's only IP candidate is the wildcard bind address, which
// go-iroh drops, leaving a peer nothing to reach us at but a relay.
func TestSeedLocalCandidates(t *testing.T) {
	ctx := context.Background()
	ep, err := iroh.Bind(ctx)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	defer ep.Shutdown(context.Background())

	want, err := hostaddr.LocalAddrPorts(ep.LocalAddr().Port())
	if err != nil {
		t.Skipf("no interface addresses on this host: %v", err)
	}
	if len(want) == 0 {
		t.Skip("host has no advertisable interface addresses")
	}

	// Before seeding, none of them are known.
	before := ep.Addr().IPAddrs()
	for _, ap := range want {
		if slices.Contains(before, ap) {
			t.Fatalf("endpoint already advertises %v before seeding", ap)
		}
	}

	seedLocalCandidates(ep)

	got := ep.Addr().IPAddrs()
	for _, ap := range want {
		if !slices.Contains(got, ap) {
			t.Errorf("interface address %v not advertised after seeding (got %v)", ap, got)
		}
	}
	// The wildcard bind address must not become a candidate.
	for _, ap := range got {
		if ap.Addr().IsUnspecified() && !isBound(ap, ep.LocalAddr()) {
			t.Errorf("wildcard address %v advertised as a candidate", ap)
		}
	}
}

// isBound reports whether ap is the endpoint's own bound address, which
// Endpoint.Addr includes unconditionally and the pkarr filter strips later.
func isBound(ap netip.AddrPort, local netip.AddrPort) bool { return ap == local }
