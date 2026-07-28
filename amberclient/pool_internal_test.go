package amberclient

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/jobs-build/jobs-iroh/amberiroh"
	irohkey "github.com/tmc/go-iroh/key"
)

// stubConn is a fake shardConn whose liveness is its context; failOpen
// simulates a connection whose path died between transfers.
type stubConn struct {
	ctx      context.Context
	cancel   context.CancelFunc
	failOpen bool
}

func newStubConn() *stubConn {
	ctx, cancel := context.WithCancel(context.Background())
	return &stubConn{ctx: ctx, cancel: cancel}
}

func (s *stubConn) Context() context.Context { return s.ctx }
func (s *stubConn) OpenStreamConn(ctx context.Context) (net.Conn, error) {
	if s.failOpen {
		return nil, context.DeadlineExceeded
	}
	c, _ := net.Pipe()
	return c, nil
}
func (s *stubConn) Close() error { s.cancel(); return nil }

// stubDialer counts dials and records closes; each dial's identity comes
// from the records exactly like the real dialer (shardTarget's i%len).
// failOpenNew makes every future dial's conn refuse stream opens.
type stubDialer struct {
	mu          sync.Mutex
	dials       int
	closed      int
	failOpenNew bool
	conns       []*stubConn
}

func (d *stubDialer) dial(ctx context.Context, i int, ports []uint16, eps []amberiroh.DataEndpointRec) (shardConn, func(), irohkey.EndpointID, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dials++
	c := newStubConn()
	c.failOpen = d.failOpenNew
	d.conns = append(d.conns, c)
	var id irohkey.EndpointID
	if len(eps) > 0 {
		var err error
		id, err = irohkeyFromBytes(eps[i%len(eps)].ID)
		if err != nil {
			return nil, nil, irohkey.EndpointID{}, err
		}
	}
	return c, func() {
		c.cancel()
		d.mu.Lock()
		d.closed++
		d.mu.Unlock()
	}, id, nil
}

func (d *stubDialer) counts() (int, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dials, d.closed
}

func testRecs(t *testing.T, n int) []amberiroh.DataEndpointRec {
	t.Helper()
	recs := make([]amberiroh.DataEndpointRec, n)
	for i := range recs {
		recs[i] = recWith(t, "ip:10.0.0.5:4001")
	}
	return recs
}

func testPool(d *stubDialer, baseline, max int, ttl time.Duration) *shardPool {
	return newShardPool(d.dial, slog.New(slog.NewTextHandler(io.Discard, nil)), baseline, max, ttl)
}

func TestPoolReusesAcrossSequentialAcquires(t *testing.T) {
	d := &stubDialer{}
	p := testPool(d, 3, 11, time.Hour)
	recs := testRecs(t, 3)

	first, _, rel := p.acquire(context.Background(), 3, nil, recs)
	if len(first) != 3 {
		t.Fatalf("acquired %d, want 3", len(first))
	}
	rel()
	second, _, rel2 := p.acquire(context.Background(), 3, nil, recs)
	defer rel2()
	if dials, _ := d.counts(); dials != 3 {
		t.Fatalf("dials %d, want 3 (reuse)", dials)
	}
	seen := map[*poolEntry]bool{}
	for _, e := range first {
		seen[e] = true
	}
	for _, e := range second {
		if !seen[e] {
			t.Fatal("second acquire returned a fresh entry; want reuse")
		}
	}
}

func TestPoolGrowsUnderConcurrency(t *testing.T) {
	d := &stubDialer{}
	p := testPool(d, 3, 11, time.Hour)
	recs := testRecs(t, 3)

	a, _, relA := p.acquire(context.Background(), 3, nil, recs)
	defer relA()
	b, _, relB := p.acquire(context.Background(), 3, nil, recs)
	defer relB()
	if dials, _ := d.counts(); dials != 6 {
		t.Fatalf("dials %d, want 6 (target 3*2 active)", dials)
	}
	for _, e := range append(append([]*poolEntry{}, a...), b...) {
		if e.streams > 1 {
			t.Fatalf("entry carries %d streams; least-loaded selection should spread to 1", e.streams)
		}
	}
}

func TestPoolCapsAtMax(t *testing.T) {
	d := &stubDialer{}
	p := testPool(d, 3, 4, time.Hour)
	recs := testRecs(t, 3)

	a, _, relA := p.acquire(context.Background(), 3, nil, recs)
	defer relA()
	b, _, relB := p.acquire(context.Background(), 3, nil, recs)
	defer relB()
	if dials, _ := d.counts(); dials > 4 {
		t.Fatalf("dials %d, want ≤ max 4", dials)
	}
	if len(a) != 3 || len(b) != 3 {
		t.Fatalf("acquired %d/%d, want 3/3 (shared entries)", len(a), len(b))
	}
}

func TestPoolEvictsDeadConns(t *testing.T) {
	d := &stubDialer{}
	p := testPool(d, 3, 11, time.Hour)
	recs := testRecs(t, 3)

	first, _, rel := p.acquire(context.Background(), 3, nil, recs)
	rel()
	first[0].conn.(*stubConn).cancel()

	_, _, rel2 := p.acquire(context.Background(), 3, nil, recs)
	defer rel2()
	dials, closed := d.counts()
	if dials != 4 {
		t.Fatalf("dials %d, want 4 (one redial)", dials)
	}
	if closed != 1 {
		t.Fatalf("closed %d, want 1 (dead entry torn down)", closed)
	}
}

func TestPoolEvictsRotatedIdentities(t *testing.T) {
	d := &stubDialer{}
	p := testPool(d, 3, 11, time.Hour)

	_, _, rel := p.acquire(context.Background(), 3, nil, testRecs(t, 3))
	rel()
	_, _, rel2 := p.acquire(context.Background(), 3, nil, testRecs(t, 3))
	defer rel2()
	dials, closed := d.counts()
	if dials != 6 || closed != 3 {
		t.Fatalf("dials %d closed %d, want 6/3 (identity rotation evicts all)", dials, closed)
	}
}

func TestPoolIdleScaleDown(t *testing.T) {
	d := &stubDialer{}
	p := testPool(d, 3, 11, 30*time.Millisecond)
	recs := testRecs(t, 3)

	_, _, relA := p.acquire(context.Background(), 3, nil, recs)
	_, _, relB := p.acquire(context.Background(), 3, nil, recs)
	relA()
	relB()
	deadline := time.Now().Add(2 * time.Second)
	for {
		p.mu.Lock()
		n := len(p.entries)
		p.mu.Unlock()
		if n == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pool still holds %d entries, want baseline 3", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, closed := d.counts(); closed != 3 {
		t.Fatalf("closed %d, want 3", closed)
	}
}

func TestPoolDrain(t *testing.T) {
	d := &stubDialer{}
	p := testPool(d, 3, 11, time.Hour)
	_, _, rel := p.acquire(context.Background(), 3, nil, testRecs(t, 3))
	rel()
	p.drain()
	if _, closed := d.counts(); closed != 3 {
		t.Fatalf("closed %d, want all 3 on drain", closed)
	}
	got, _, rel2 := p.acquire(context.Background(), 3, nil, testRecs(t, 3))
	rel2()
	if len(got) != 0 {
		t.Fatalf("closed pool returned %d entries, want 0", len(got))
	}
}

func TestPoolEvictsRelayStuckEntries(t *testing.T) {
	d := &stubDialer{}
	p := testPool(d, 3, 11, time.Hour)
	recs := testRecs(t, 3)

	_, _, rel := p.acquire(context.Background(), 3, nil, recs)
	rel()

	// Age all entries past the grace and mark one relayed, one direct, one
	// path-unknown: only the relayed one may be evicted.
	p.mu.Lock()
	for i, e := range p.entries {
		e.dialed = time.Now().Add(-2 * relayGrace)
		switch i {
		case 0:
			e.path = func() (Path, bool) { return Path{Relayed: true}, true }
		case 1:
			e.path = func() (Path, bool) { return Path{Relayed: false}, true }
		default:
			e.path = nil
		}
	}
	p.mu.Unlock()

	_, _, rel2 := p.acquire(context.Background(), 3, nil, recs)
	defer rel2()
	dials, closed := d.counts()
	if closed != 1 {
		t.Fatalf("closed %d, want 1 (only the relay-stuck entry)", closed)
	}
	if dials != 4 {
		t.Fatalf("dials %d, want 4 (one replacement)", dials)
	}
}

func TestPoolKeepsYoungRelayedEntries(t *testing.T) {
	d := &stubDialer{}
	p := testPool(d, 3, 11, time.Hour)
	recs := testRecs(t, 3)

	_, _, rel := p.acquire(context.Background(), 3, nil, recs)
	rel()
	p.mu.Lock()
	for _, e := range p.entries {
		e.path = func() (Path, bool) { return Path{Relayed: true}, true } // still inside grace
	}
	p.mu.Unlock()

	_, _, rel2 := p.acquire(context.Background(), 3, nil, recs)
	defer rel2()
	if dials, closed := d.counts(); dials != 3 || closed != 0 {
		t.Fatalf("dials %d closed %d, want 3/0 (grace not elapsed, punch may still land)", dials, closed)
	}
}

func TestPoolNeverEvictsBusyRelayedEntries(t *testing.T) {
	d := &stubDialer{}
	p := testPool(d, 3, 11, time.Hour)
	recs := testRecs(t, 3)

	held, _, relHold := p.acquire(context.Background(), 3, nil, recs)
	defer relHold()
	p.mu.Lock()
	for _, e := range p.entries {
		e.dialed = time.Now().Add(-2 * relayGrace)
		e.path = func() (Path, bool) { return Path{Relayed: true}, true }
	}
	p.mu.Unlock()

	_, _, rel2 := p.acquire(context.Background(), 3, nil, recs)
	defer rel2()
	if _, closed := d.counts(); closed != 0 {
		t.Fatalf("closed %d, want 0 (entries carry another transfer's streams)", closed)
	}
	_ = held
}

func TestPoolDiscardRemovesAndCloses(t *testing.T) {
	d := &stubDialer{}
	p := testPool(d, 3, 11, time.Hour)
	recs := testRecs(t, 3)
	first, _, rel := p.acquire(context.Background(), 3, nil, recs)
	rel()
	p.discard(first[0])
	if _, closed := d.counts(); closed != 1 {
		t.Fatalf("closed %d, want 1", closed)
	}
	_, _, rel2 := p.acquire(context.Background(), 3, nil, recs)
	defer rel2()
	if dials, _ := d.counts(); dials != 4 {
		t.Fatalf("dials %d, want 4 (discarded entry redialed)", dials)
	}
}

// failOpen makes a stubConn refuse stream opens — the shape of a pooled
// connection whose path went dead between transfers.
func TestAttachZeroOnReusedEntriesDiscardsWithoutDemote(t *testing.T) {
	d := &stubDialer{}
	p := testPool(d, 3, 11, time.Hour)
	recs := testRecs(t, 3)
	c := &Client{conns: 4, log: slog.New(slog.NewTextHandler(io.Discard, nil)), pool: p}

	// Warm the pool, then break every conn's stream-open path.
	_, _, rel := p.acquire(context.Background(), 3, nil, recs)
	rel()
	d.mu.Lock()
	for _, sc := range d.conns {
		sc.failOpen = true
	}
	d.mu.Unlock()

	streams, closeAll := c.attachExtras(context.Background(), []byte("tok"), nil, recs, 3)
	closeAll()
	if len(streams) != 0 {
		t.Fatalf("attached %d, want 0", len(streams))
	}
	if c.demoted.Load() {
		t.Fatal("zero-attach on reused entries must not demote")
	}
	p.mu.Lock()
	n := len(p.entries)
	p.mu.Unlock()
	if n != 0 {
		t.Fatalf("pool holds %d entries, want 0 (zombies discarded)", n)
	}
}

func TestAttachZeroOnFreshDialsDemotes(t *testing.T) {
	d := &stubDialer{failOpenNew: true}
	p := testPool(d, 3, 11, time.Hour)
	recs := testRecs(t, 3)
	c := &Client{conns: 4, log: slog.New(slog.NewTextHandler(io.Discard, nil)), pool: p}

	streams, closeAll := c.attachExtras(context.Background(), []byte("tok"), nil, recs, 3)
	closeAll()
	if len(streams) != 0 {
		t.Fatalf("attached %d, want 0", len(streams))
	}
	if !c.demoted.Load() {
		t.Fatal("zero-attach on all-fresh dials is the topology verdict; must demote")
	}
}

func TestPoolRotatesEntriesBeforeStreamBudget(t *testing.T) {
	d := &stubDialer{}
	p := testPool(d, 3, 11, time.Hour)
	recs := testRecs(t, 3)
	for range poolEntryMaxUses {
		_, _, rel := p.acquire(context.Background(), 3, nil, recs)
		rel()
	}
	if dials, closed := d.counts(); dials != 3 || closed != 0 {
		t.Fatalf("dials %d closed %d before the cap, want 3/0", dials, closed)
	}
	_, _, rel := p.acquire(context.Background(), 3, nil, recs)
	defer rel()
	if dials, closed := d.counts(); dials != 6 || closed != 3 {
		t.Fatalf("dials %d closed %d at the cap, want 6/3 (all rotated)", dials, closed)
	}
}
