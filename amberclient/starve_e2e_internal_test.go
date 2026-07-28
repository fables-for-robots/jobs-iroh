package amberclient

// Reproducer for shard-conn stream starvation: cache-hot sharded pulls
// attach one stream per pooled conn per pull; if those streams never
// retire server-side, quic-go's 100-stream credit runs dry after ~100
// pulls and OpenStreamSync hangs — the field failure's exact shape.

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

func TestManySharedPullsDoNotStarveShardStreams(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
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
	if err := c.Push(ctx, push, "starve/ref", root); err != nil {
		t.Fatalf("push: %v", err)
	}

	// The pull store already holds the closure after pull 1, so pulls 2..N
	// are cache-hot: one empty want round, one attach stream per pooled
	// conn, done in milliseconds — unless stream credit runs dry.
	dst, err := amber.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	for i := range 130 {
		pctx, pcancel := context.WithTimeout(ctx, 15*time.Second)
		_, err := c.Pull(pctx, dst, "starve/ref")
		pcancel()
		if err != nil {
			t.Fatalf("pull %d: %v", i, err)
		}
		if c.demoted.Load() {
			t.Fatalf("demoted after pull %d — shard streams are starving", i)
		}
	}
}
