package amberclient

// End-to-end pool reuse against a real server: two sequential pulls on one
// Client must ride the same shard connections — the second transfer opens
// only streams, no connections. Internal package so the test can inspect
// pool entries; the harness mirrors client_test.go's startServerData.

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jobs-build/jobs-iroh/amber"
	"github.com/jobs-build/jobs-iroh/serve"
)

func TestPoolReuseAcrossPullsE2E(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	ready := make(chan *serve.Server, 1)
	done := make(chan error, 1)
	runCtx, runCancel := context.WithCancel(ctx)
	defer func() {
		runCancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("server run: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("server did not shut down")
		}
	}()
	go func() {
		done <- serve.Run(runCtx, serve.Options{
			DataDir:       t.TempDir(),
			BindAddr:      netip.AddrPortFrom(netip.IPv6Loopback(), 0),
			DataEndpoints: 2,
			Ready:         func(s *serve.Server) { ready <- s },
		})
	}()
	var srv *serve.Server
	select {
	case srv = <-ready:
	case <-time.After(30 * time.Second):
		t.Fatal("server not ready")
	}

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	push, err := amber.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer push.Close()
	root, err := push.IngestDir(ctx, src)
	if err != nil {
		t.Fatal(err)
	}

	c, err := Dial(ctx, Options{
		EndpointID: srv.Endpoint.ID().String(),
		Addrs:      []string{srv.Endpoint.LocalAddr().String()},
		ALPN:       serve.ALPNAmberAdmin,
		BindAddr:   netip.AddrPortFrom(netip.IPv6Loopback(), 0),
		Conns:      4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Push(ctx, push, "pool/reuse", root); err != nil {
		t.Fatalf("push: %v", err)
	}

	poolConns := func() map[shardConn]bool {
		c.pool.mu.Lock()
		defer c.pool.mu.Unlock()
		out := map[shardConn]bool{}
		for _, e := range c.pool.entries {
			out[e.conn] = true
		}
		return out
	}
	afterPush := poolConns()
	if len(afterPush) != 3 {
		t.Fatalf("pool holds %d conns after push, want 3", len(afterPush))
	}

	for i := range 2 {
		dst, err := amber.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Pull(ctx, dst, "pool/reuse"); err != nil {
			t.Fatalf("pull %d: %v", i, err)
		}
		dst.Close()
		got := poolConns()
		if len(got) != 3 {
			t.Fatalf("pull %d: pool holds %d conns, want 3", i, len(got))
		}
		for conn := range got {
			if !afterPush[conn] {
				t.Fatalf("pull %d dialed a fresh shard connection; want reuse", i)
			}
		}
	}

	if err := c.Close(); err != nil {
		t.Logf("close: %v", err)
	}
	c.pool.mu.Lock()
	n := len(c.pool.entries)
	c.pool.mu.Unlock()
	if n != 0 {
		t.Fatalf("pool holds %d conns after Close, want 0", n)
	}
}

// TestParkedShardsPullSingleChannelE2E pins the reserve-first flow against
// a real server: with every pooled shard parked on a (simulated) relay, a
// pull must run single-channel — the request promises no shards, so the
// server starts its want rounds immediately instead of sitting out its
// gather window, and the parked entries are never handed to the transfer.
func TestParkedShardsPullSingleChannelE2E(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	ready := make(chan *serve.Server, 1)
	done := make(chan error, 1)
	runCtx, runCancel := context.WithCancel(ctx)
	defer func() {
		runCancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("server run: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("server did not shut down")
		}
	}()
	go func() {
		done <- serve.Run(runCtx, serve.Options{
			DataDir:       t.TempDir(),
			BindAddr:      netip.AddrPortFrom(netip.IPv6Loopback(), 0),
			DataEndpoints: 2,
			Ready:         func(s *serve.Server) { ready <- s },
		})
	}()
	var srv *serve.Server
	select {
	case srv = <-ready:
	case <-time.After(30 * time.Second):
		t.Fatal("server not ready")
	}

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	push, err := amber.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer push.Close()
	root, err := push.IngestDir(ctx, src)
	if err != nil {
		t.Fatal(err)
	}

	c, err := Dial(ctx, Options{
		EndpointID: srv.Endpoint.ID().String(),
		Addrs:      []string{srv.Endpoint.LocalAddr().String()},
		ALPN:       serve.ALPNAmberAdmin,
		BindAddr:   netip.AddrPortFrom(netip.IPv6Loopback(), 0),
		Conns:      4,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Push(ctx, push, "pool/parked", root); err != nil {
		t.Fatalf("push: %v", err)
	}

	// Strand every pooled entry on a pretend relay and record its uses: the
	// reserve-first pull below must neither hand them to the transfer nor
	// bump their use counts.
	usesBefore := map[*poolEntry]int{}
	c.pool.mu.Lock()
	for _, e := range c.pool.entries {
		e.path = func() (Path, bool) { return Path{Relayed: true}, true }
		usesBefore[e] = e.uses
	}
	c.pool.mu.Unlock()

	dst, err := amber.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	start := time.Now()
	if _, err := c.Pull(ctx, dst, "pool/parked"); err != nil {
		t.Fatalf("pull: %v", err)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("single-channel pull took %s; a gather-window stall means DataConns lied", el)
	}
	c.pool.mu.Lock()
	for e, before := range usesBefore {
		if e.uses != before {
			c.pool.mu.Unlock()
			t.Fatal("parked entry was handed to the transfer")
		}
	}
	c.pool.mu.Unlock()
	if c.demoted.Load() {
		t.Fatal("parked shards must not demote the connection")
	}
}
