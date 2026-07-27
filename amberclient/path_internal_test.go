package amberclient

// Unit coverage for the path-in-use pick; the e2e side (a real loopback
// connection reporting "direct") lives in path_test.go.

import (
	"net/netip"
	"testing"
	"time"

	"github.com/tmc/go-iroh/iroh"
	"github.com/tmc/go-iroh/netaddr"
)

func ipPath(addr string, rtt time.Duration) iroh.PathInfo {
	return iroh.PathInfo{
		Validated: true,
		Addr:      netaddr.IPAddr{Addr: netip.MustParseAddrPort(addr)},
		HasAddr:   true,
		RTT:       rtt,
		HasRTT:    rtt > 0,
	}
}

func relayPath(rtt time.Duration) iroh.PathInfo {
	url, err := netaddr.ParseRelayURL("https://relay.example/")
	if err != nil {
		panic(err)
	}
	return iroh.PathInfo{
		Validated: true,
		Addr:      netaddr.RelayAddr{URL: url},
		HasAddr:   true,
		RTT:       rtt,
		HasRTT:    rtt > 0,
		Relayed:   true,
	}
}

func TestPathOf(t *testing.T) {
	selected := ipPath("10.0.0.9:1", 90*time.Millisecond)
	selected.Selected = true
	unvalidated := ipPath("10.0.0.8:1", time.Millisecond)
	unvalidated.Validated = false

	for _, tc := range []struct {
		name    string
		paths   []iroh.PathInfo
		ok      bool
		relayed bool
		addr    string
		rtt     time.Duration
	}{
		{name: "none"},
		{
			name:  "direct only",
			paths: []iroh.PathInfo{ipPath("192.168.1.5:4711", 2*time.Millisecond)},
			ok:    true, addr: "ip:192.168.1.5:4711", rtt: 2 * time.Millisecond,
		},
		{
			name:  "relay only",
			paths: []iroh.PathInfo{relayPath(40 * time.Millisecond)},
			ok:    true, relayed: true, addr: "relay:https://relay.example/", rtt: 40 * time.Millisecond,
		},
		{
			// Hole punching landed: the relay is still open as fallback but
			// the direct path is the one carrying data.
			name:  "direct beats relay",
			paths: []iroh.PathInfo{relayPath(40 * time.Millisecond), ipPath("192.168.1.5:4711", 2*time.Millisecond)},
			ok:    true, addr: "ip:192.168.1.5:4711", rtt: 2 * time.Millisecond,
		},
		{
			// The transport's own verdict outranks every heuristic, even a
			// faster direct path.
			name:  "selected wins",
			paths: []iroh.PathInfo{ipPath("192.168.1.5:4711", time.Millisecond), selected},
			ok:    true, addr: "ip:10.0.0.9:1", rtt: 90 * time.Millisecond,
		},
		{
			name:  "validated beats unvalidated",
			paths: []iroh.PathInfo{unvalidated, relayPath(40 * time.Millisecond)},
			ok:    true, relayed: true, addr: "relay:https://relay.example/", rtt: 40 * time.Millisecond,
		},
		{
			name:  "lower rtt wins",
			paths: []iroh.PathInfo{ipPath("10.0.0.1:1", 30*time.Millisecond), ipPath("10.0.0.2:1", 3*time.Millisecond)},
			ok:    true, addr: "ip:10.0.0.2:1", rtt: 3 * time.Millisecond,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, ok := pathOf(tc.paths)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			if p.Relayed != tc.relayed {
				t.Errorf("Relayed = %v, want %v", p.Relayed, tc.relayed)
			}
			if p.Addr != tc.addr {
				t.Errorf("Addr = %q, want %q", p.Addr, tc.addr)
			}
			if p.RTT != tc.rtt {
				t.Errorf("RTT = %v, want %v", p.RTT, tc.rtt)
			}
		})
	}
}

func TestPathKindAndLogAttrs(t *testing.T) {
	direct := Path{Addr: "ip:192.168.1.5:4711", RTT: 1500 * time.Microsecond}
	if got := direct.Kind(); got != "direct" {
		t.Errorf("Kind() = %q, want direct", got)
	}
	want := []any{"transport", "direct", "addr", "ip:192.168.1.5:4711", "rtt", "1.5ms"}
	got := direct.LogAttrs()
	if len(got) != len(want) {
		t.Fatalf("LogAttrs() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("LogAttrs() = %v, want %v", got, want)
		}
	}

	relayed := Path{Relayed: true}
	if got := relayed.Kind(); got != "relay" {
		t.Errorf("Kind() = %q, want relay", got)
	}
	// No address and no RTT observed: the attrs stay to the one fact known.
	if got := relayed.LogAttrs(); len(got) != 2 || got[1] != "relay" {
		t.Errorf("LogAttrs() = %v, want [transport relay]", got)
	}
}
