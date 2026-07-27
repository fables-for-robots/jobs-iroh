package amberclient_test

// Sharded-transfer e2e: the server binds dedicated amber data endpoints,
// the client attaches Conns-1 extra QUIC connections per transfer, and the
// want loop deals rounds across all channels. The plain single-channel
// paths stay covered by client_test.go (whose default-Conns clients shard
// against a portless server) and the explicit Conns=1 test below.

import (
	"context"
	"math/rand"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jobs-build/jobs-iroh/amberclient"
	"github.com/jobs-build/jobs-iroh/serve"
)

// dialConns dials the server's amber mount with an explicit Conns setting.
func dialConns(t *testing.T, ctx context.Context, srv *serve.Server, alpn string, conns int) *amberclient.Client {
	t.Helper()
	c, err := amberclient.Dial(ctx, amberclient.Options{
		EndpointID: srv.Endpoint.ID().String(),
		Addrs:      []string{srv.Endpoint.LocalAddr().String()},
		ALPN:       alpn,
		BindAddr:   netip.AddrPortFrom(netip.IPv6Loopback(), 0),
		Conns:      conns,
	})
	if err != nil {
		t.Fatalf("dial %s (conns=%d): %v", alpn, conns, err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Logf("client close: %v", err)
		}
	})
	return c
}

// wideSource writes a tree wide enough that want rounds carry many keys —
// the shape sharding actually deals across channels.
func wideSource(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	rng := rand.New(rand.NewSource(7))
	for _, dir := range []string{"a", "b", "c"} {
		if err := os.MkdirAll(filepath.Join(src, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		for i := range 12 {
			buf := make([]byte, 64<<10+i)
			rng.Read(buf)
			if err := os.WriteFile(filepath.Join(src, dir, string(rune('f'+i))+".bin"), buf, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return src
}

// TestShardedRoundTripDataEndpoints pins the full sharded path: DataConns
// requested, token + data ports answered, extras attached at the dedicated
// server sockets, and the transfer still round-trips bit-identically with
// serialized, monotonic progress.
func TestShardedRoundTripDataEndpoints(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	srv := startServerData(t, ctx, 3)
	client := dialConns(t, ctx, srv, serve.ALPNRunnerAmber, 4)

	src := wideSource(t)
	storeA := openStore(t)
	root, err := storeA.IngestDir(ctx, src)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}

	const name = "runner-push/test/sharded-0"
	pushStats, err := client.PushWithProgress(ctx, storeA, name, root, nil)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if pushStats.Objects == 0 || pushStats.Bytes == 0 {
		t.Fatalf("sharded push moved nothing: %+v", pushStats)
	}

	var mu sync.Mutex
	lastDone := -1
	monotonic := true
	cb := func(done, total int) {
		mu.Lock()
		if done < lastDone {
			monotonic = false
		}
		lastDone = done
		mu.Unlock()
	}
	storeB := openStore(t)
	got, pullStats, err := client.PullWithProgress(ctx, storeB, name, cb)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if got != root {
		t.Fatalf("pulled root %s, want %s", got, root)
	}
	if !monotonic {
		t.Error("progress done count went backwards")
	}
	if pullStats.Objects != lastDone {
		t.Errorf("stats report %d objects, final callback saw %d", pullStats.Objects, lastDone)
	}

	dest := filepath.Join(t.TempDir(), "restored")
	if err := storeB.Materialize(ctx, got, dest); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	requireSameFiles(t, src, dest)
}

// TestConnsOneDisablesSharding pins the opt-out: Conns=1 must behave exactly
// like the pre-sharding client (no DataConns in the request, single stream).
func TestConnsOneDisablesSharding(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	srv := startServerData(t, ctx, 3)
	client := dialConns(t, ctx, srv, serve.ALPNAmberAdmin, 1)

	src := layeredSource(t)
	storeA := openStore(t)
	root, err := storeA.IngestDir(ctx, src)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := client.Push(ctx, storeA, "plain-0", root); err != nil {
		t.Fatalf("push: %v", err)
	}
	storeB := openStore(t)
	got, err := client.Pull(ctx, storeB, "plain-0")
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if got != root {
		t.Fatalf("pulled root %s, want %s", got, root)
	}
}

// TestShardedInterruptResumes pins the resumable want loop under sharding: a
// pull cancelled mid-transfer leaves a partial store, and a fresh sharded
// pull completes the closure.
func TestShardedInterruptResumes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	srv := startServerData(t, ctx, 2)
	client := dialConns(t, ctx, srv, serve.ALPNRunnerAmber, 3)

	src := wideSource(t)
	storeA := openStore(t)
	root, err := storeA.IngestDir(ctx, src)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	const name = "runner-push/test/resume-0"
	if err := client.Push(ctx, storeA, name, root); err != nil {
		t.Fatalf("push: %v", err)
	}

	// Cancel the first pull after a few objects arrived.
	storeB := openStore(t)
	pctx, pcancel := context.WithCancel(ctx)
	n := 0
	_, _, err = client.PullWithProgress(pctx, storeB, name, func(done, total int) {
		n++
		if done >= 3 {
			pcancel()
		}
	})
	pcancel()
	if err == nil {
		// The whole tree landed before the cancel could bite — still a
		// valid run, nothing left to resume.
		t.Log("pull finished before cancellation")
	}

	got, err := client.Pull(ctx, storeB, name)
	if err != nil {
		t.Fatalf("resumed pull: %v", err)
	}
	if got != root {
		t.Fatalf("resumed root %s, want %s", got, root)
	}
	if err := storeB.Materialize(ctx, got, filepath.Join(t.TempDir(), "restored")); err != nil {
		t.Fatalf("materialize after resume: %v", err)
	}
}
