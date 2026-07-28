package amberclient

import (
	"io"
	"log/slog"
	"testing"
	"time"
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

func TestTransferConnsWaitsForYoungPunch(t *testing.T) {
	c := &Client{conns: 4, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		dialedAt: time.Now(), gatePoll: time.Millisecond, gateWait: time.Second}
	var calls int
	c.pathFn = func() (Path, bool) {
		calls++
		return Path{Relayed: calls < 3}, true // direct on the third poll
	}
	if got := c.transferConns(); got != 4 {
		t.Fatalf("young relayed conn that punches mid-wait: conns %d, want 4", got)
	}
}

func TestTransferConnsWaitBoundedOnYoungConn(t *testing.T) {
	c := &Client{conns: 4, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		dialedAt: time.Now(), gatePoll: time.Millisecond, gateWait: 10 * time.Millisecond}
	c.pathFn = func() (Path, bool) { return Path{Relayed: true}, true }
	start := time.Now()
	if got := c.transferConns(); got != 1 {
		t.Fatalf("punch never lands: conns %d, want 1 after bounded wait", got)
	}
	if time.Since(start) > time.Second {
		t.Fatal("wait not bounded")
	}
}

func TestTransferConnsSkipsSettledRelayImmediately(t *testing.T) {
	c := &Client{conns: 4, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		dialedAt: time.Now().Add(-2 * gateSettle), gatePoll: time.Millisecond, gateWait: time.Second}
	var calls int
	c.pathFn = func() (Path, bool) { calls++; return Path{Relayed: true}, true }
	start := time.Now()
	if got := c.transferConns(); got != 1 {
		t.Fatalf("settled relay: conns %d, want 1", got)
	}
	if calls != 1 || time.Since(start) > 100*time.Millisecond {
		t.Fatalf("settled relay must not wait (polled %d times in %s)", calls, time.Since(start))
	}
}
