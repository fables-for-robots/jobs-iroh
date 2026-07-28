package amberclient

import (
	"io"
	"log/slog"
	"testing"
)

func TestTransferConnsGatesOnRelayedPath(t *testing.T) {
	c := &Client{conns: 4, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	c.pathFn = func() (Path, bool) { return Path{Relayed: true, Addr: "relay:https://euc1-1.relay.example./"}, true }
	if got := c.transferConns(); got != 1 {
		t.Fatalf("relayed control path: conns %d, want 1 (extras move no extra bytes through the relay)", got)
	}
	if c.demoted.Load() {
		t.Fatal("the gate must skip, never demote — the path can upgrade")
	}
	c.pathFn = func() (Path, bool) { return Path{Relayed: false}, true }
	if got := c.transferConns(); got != 4 {
		t.Fatalf("direct control path: conns %d, want 4", got)
	}
	c.pathFn = func() (Path, bool) { return Path{}, false }
	if got := c.transferConns(); got != 4 {
		t.Fatalf("no path snapshot: conns %d, want 4 (don't gate blind)", got)
	}
}
