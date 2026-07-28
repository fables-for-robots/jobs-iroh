package amberclient

// Shard-connection pooling (2026-07-28-pooled-shard-connections.md): the
// punch ramp — relay connect, TAttach, ~5s riding the relay until QNT
// lands the direct path — is paid per connection, so connections must
// outlive transfers. TAttach binds a STREAM to a transfer token, not a
// connection: a pooled connection serves transfer after transfer by
// opening a fresh stream each time. The pool grows with concurrent
// transfers, clamps at a max, and an idle sweep shrinks it back to
// baseline; QUIC keepalive (on by default in go-iroh) keeps idle entries
// alive, and any entry found dead at acquire is evicted and redialed.

import (
	"context"
	"log/slog"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/jobs-build/jobs-iroh/amberiroh"
	irohkey "github.com/tmc/go-iroh/key"
)

// shardConn is the slice of *iroh.Conn the pool needs — an interface so
// pool tests can fake dials.
type shardConn interface {
	Context() context.Context
	OpenStreamConn(ctx context.Context) (net.Conn, error)
	Close() error
}

// poolDialer dials shard slot i and returns the connection, its full
// teardown (connection close + endpoint shutdown) and the identity it
// authenticated.
type poolDialer func(ctx context.Context, i int, ports []uint16, eps []amberiroh.DataEndpointRec) (shardConn, func(), irohkey.EndpointID, error)

type poolEntry struct {
	conn     shardConn
	close    func()
	id       irohkey.EndpointID
	streams  int
	lastUsed time.Time
}

// shardPool owns shard-connection lifecycle for one Client. Entries are
// shared: concurrent transfers each open their own stream on an entry, so
// acquire never blocks on "busy" connections — it only spreads load by
// picking the least-loaded k.
type shardPool struct {
	dial     poolDialer
	log      *slog.Logger
	baseline int // entries kept through idle (Conns-1)
	max      int // entry cap (PoolMax-1)
	idleTTL  time.Duration

	mu      sync.Mutex
	entries []*poolEntry
	active  int // acquire/release balance = concurrent transfers
	sweep   *time.Timer
	closed  bool
}

func newShardPool(dial poolDialer, log *slog.Logger, baseline, max int, idleTTL time.Duration) *shardPool {
	if baseline < 1 {
		baseline = 1
	}
	if max < baseline {
		max = baseline
	}
	return &shardPool{dial: dial, log: log, baseline: baseline, max: max, idleTTL: idleTTL}
}

// acquire returns up to k live entries for one transfer and a release that
// MUST be called when the transfer ends. Dials happen under ctx (the
// caller's attach budget); failures reduce parallelism, exactly like a
// failed attach always has.
func (p *shardPool) acquire(ctx context.Context, k int, ports []uint16, eps []amberiroh.DataEndpointRec) ([]*poolEntry, func()) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, func() {}
	}
	p.active++
	if p.sweep != nil {
		p.sweep.Stop()
	}

	// Evict dead connections and, when records are present, entries whose
	// identity the server no longer advertises — an identity rotation
	// means a server restart, so those connections are dead or dying.
	live := p.entries[:0]
	for _, e := range p.entries {
		stale := e.conn.Context().Err() != nil
		if !stale && len(eps) > 0 && !e.id.IsZero() {
			stale = true
			for _, rec := range eps {
				if id, err := irohkeyFromBytes(rec.ID); err == nil && id == e.id {
					stale = false
					break
				}
			}
		}
		if stale {
			e.close()
			continue
		}
		live = append(live, e)
	}
	p.entries = live

	// Grow toward the concurrency target: identities with the fewest
	// pooled connections first, so the spread across server sockets holds.
	target := min(p.baseline*p.active, p.max)
	// Grow toward the target, but never dial more than this transfer can
	// use — the next acquire keeps growing if concurrency holds.
	need := min(target-len(p.entries), k)
	perID := map[irohkey.EndpointID]int{}
	for _, e := range p.entries {
		perID[e.id]++
	}
	var slots []int
	if need > 0 && len(eps) > 0 {
		idx := make([]int, len(eps))
		for i := range idx {
			idx[i] = i
		}
		sort.SliceStable(idx, func(a, b int) bool {
			ca, cb := -1, -1
			if id, err := irohkeyFromBytes(eps[idx[a]].ID); err == nil {
				ca = perID[id]
			}
			if id, err := irohkeyFromBytes(eps[idx[b]].ID); err == nil {
				cb = perID[id]
			}
			return ca < cb
		})
		for len(slots) < need {
			slots = append(slots, idx[len(slots)%len(idx)])
		}
	} else {
		for i := range need {
			slots = append(slots, i)
		}
	}
	p.mu.Unlock()

	// Dials run in parallel — each shard's ramp (relay connect, punch) is
	// independent, and the caller's attach budget bounds them all via ctx.
	dialed := make([]*poolEntry, len(slots))
	var wg sync.WaitGroup
	for si, i := range slots {
		wg.Add(1)
		go func(si, i int) {
			defer wg.Done()
			conn, closeFn, id, err := p.dial(ctx, i, ports, eps)
			if err != nil {
				p.log.Debug("amberclient: pool dial failed", "slot", i, "error", err)
				return
			}
			dialed[si] = &poolEntry{conn: conn, close: closeFn, id: id, lastUsed: time.Now()}
		}(si, i)
	}
	wg.Wait()
	var lateClose []*poolEntry
	p.mu.Lock()
	for _, e := range dialed {
		if e == nil {
			continue
		}
		if p.closed {
			lateClose = append(lateClose, e)
			continue
		}
		p.entries = append(p.entries, e)
	}
	p.mu.Unlock()
	for _, e := range lateClose {
		e.close()
	}

	p.mu.Lock()
	sort.SliceStable(p.entries, func(a, b int) bool {
		if p.entries[a].streams != p.entries[b].streams {
			return p.entries[a].streams < p.entries[b].streams
		}
		return p.entries[a].lastUsed.After(p.entries[b].lastUsed)
	})
	picked := make([]*poolEntry, 0, k)
	for _, e := range p.entries {
		if len(picked) == k {
			break
		}
		e.streams++
		e.lastUsed = time.Now()
		picked = append(picked, e)
	}
	p.mu.Unlock()

	var once sync.Once
	release := func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			for _, e := range picked {
				e.streams--
				e.lastUsed = time.Now()
			}
			p.active--
			if p.active == 0 && !p.closed {
				if p.sweep != nil {
					p.sweep.Stop()
				}
				p.sweep = time.AfterFunc(p.idleTTL, p.scaleDown)
			}
		})
	}
	return picked, release
}

// scaleDown closes idle entries back to baseline, keeping the most
// recently used and preferring distinct identities.
func (p *shardPool) scaleDown() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active > 0 || p.closed || len(p.entries) <= p.baseline {
		return
	}
	sort.SliceStable(p.entries, func(a, b int) bool {
		return p.entries[a].lastUsed.After(p.entries[b].lastUsed)
	})
	seen := map[irohkey.EndpointID]bool{}
	var keep, rest []*poolEntry
	for _, e := range p.entries {
		if len(keep) < p.baseline && !seen[e.id] {
			seen[e.id] = true
			keep = append(keep, e)
			continue
		}
		rest = append(rest, e)
	}
	for _, e := range rest {
		if len(keep) == p.baseline {
			break
		}
		keep = append(keep, e)
	}
	for _, e := range p.entries {
		kept := false
		for _, ke := range keep {
			if ke == e {
				kept = true
				break
			}
		}
		if !kept {
			e.close()
		}
	}
	p.entries = keep
}

// drain closes every entry; the pool refuses further work.
func (p *shardPool) drain() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	if p.sweep != nil {
		p.sweep.Stop()
	}
	for _, e := range p.entries {
		e.close()
	}
	p.entries = nil
}
