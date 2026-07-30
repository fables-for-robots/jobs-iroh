package amberclient

// planTransfer decides how many channels the next transfer promises the
// server. The promise must be truthful: every shard the request announces
// in DataConns is one the server will WAIT for (its gather window), so a
// shard that will never attach — parked on a relay — must not be promised.

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

func testReserveClient(d *stubDialer) *Client {
	c := &Client{
		conns: 4,
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		pool:  testPool(d, 3, 11, time.Hour),
	}
	c.pathFn = func() (Path, bool) { return Path{Relayed: false}, true }
	return c
}

func TestPlanTransferOptimisticWithoutEndpoints(t *testing.T) {
	c := testReserveClient(&stubDialer{})
	conns, rsv := c.planTransfer(context.Background())
	if conns != 4 || rsv != nil {
		t.Fatalf("conns %d rsv %v, want 4/nil (endpoints unknown before the first sharded reply)", conns, rsv)
	}
}

func TestPlanTransferReservesDirectShards(t *testing.T) {
	d := &stubDialer{}
	c := testReserveClient(d)
	recs := testRecs(t, 3)
	c.cacheDataEndpoints(nil, recs)

	conns, rsv := c.planTransfer(context.Background())
	if conns != 4 {
		t.Fatalf("conns %d, want 4", conns)
	}
	if rsv == nil || len(rsv.entries) != 3 {
		t.Fatalf("reservation %+v, want 3 reserved entries", rsv)
	}
	rsv.release()
}

func TestPlanTransferSingleChannelWhenAllParked(t *testing.T) {
	d := &stubDialer{}
	c := testReserveClient(d)
	recs := testRecs(t, 3)
	c.cacheDataEndpoints(nil, recs)

	// Warm the pool, then strand every entry on a relay.
	_, _, _, rel := c.pool.acquire(context.Background(), 3, true, nil, recs)
	rel()
	c.pool.mu.Lock()
	for _, e := range c.pool.entries {
		e.path = func() (Path, bool) { return Path{Relayed: true}, true }
	}
	c.pool.mu.Unlock()

	conns, rsv := c.planTransfer(context.Background())
	if conns != 1 || rsv != nil {
		t.Fatalf("conns %d rsv %v, want 1/nil (all shards parked)", conns, rsv)
	}
	if c.demoted.Load() {
		t.Fatal("parked shards are a pending punch, not a topology verdict; must not demote")
	}
}

func TestPlanTransferColdRelayedDialsGoSingleChannel(t *testing.T) {
	d := &stubDialer{relayedNew: true}
	c := testReserveClient(d)
	c.cacheDataEndpoints(nil, testRecs(t, 3))

	conns, rsv := c.planTransfer(context.Background())
	if conns != 1 || rsv != nil {
		t.Fatalf("conns %d rsv %v, want 1/nil (fresh dials all landed relayed)", conns, rsv)
	}
	if c.demoted.Load() {
		t.Fatal("relayed fresh dials must park, not demote — the punch may still land")
	}
}

func TestPlanTransferSkipsReserveWhenDemoted(t *testing.T) {
	c := testReserveClient(&stubDialer{})
	c.cacheDataEndpoints(nil, testRecs(t, 3))
	c.demote("test")
	conns, rsv := c.planTransfer(context.Background())
	if conns != 1 || rsv != nil {
		t.Fatalf("conns %d rsv %v, want 1/nil (demoted)", conns, rsv)
	}
}
