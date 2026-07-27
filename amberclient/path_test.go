package amberclient_test

// E2e: a real loopback connection classifies itself as direct, and WatchPath
// delivers that verdict synchronously to its callback.

import (
	"context"
	"testing"
	"time"

	"github.com/jobs-build/jobs-iroh/amberclient"
	"github.com/jobs-build/jobs-iroh/serve"
)

func TestPathDirectOnLoopback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	srv := startServer(t, ctx)
	c := dialClient(t, ctx, srv, serve.ALPNRunnerAmber)

	p, ok := c.Path()
	if !ok {
		t.Fatal("Path() reported no open path on a live connection")
	}
	if p.Relayed {
		t.Errorf("loopback connection reported as relayed: %+v", p)
	}
	if p.Addr == "" {
		t.Errorf("Path() reported no address: %+v", p)
	}

	got := make(chan amberclient.Path, 4)
	c.WatchPath(ctx, func(p amberclient.Path) { got <- p })
	select {
	case first := <-got:
		if first.Relayed {
			t.Errorf("WatchPath reported a relayed path on loopback: %+v", first)
		}
	default:
		t.Fatal("WatchPath did not report the current path synchronously")
	}
}
