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
