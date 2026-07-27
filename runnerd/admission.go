package runnerd

import (
	"context"
	"strings"
	"sync"

	"github.com/jobs-build/jobs-iroh/resources"
)

// jobIDPrefix marks admission entries that are running jobs (vs. lane
// pre-reservations awaiting a fetch).
const jobIDPrefix = "job/"

// admission is the runner's authoritative capacity check (ported semantics:
// jobs' execcore.Admit — the runner, not the queue, owns overcommit): a
// ledger of held reservations against a fixed capacity, with an optional cap
// on the number of concurrent holds (Slots). Lanes reserve a class rung
// BEFORE fetching (so a message is only pulled when the capacity to run it
// is already committed), then rename the reservation onto the job's
// (node, gen) attempt — which also rejects duplicate deliveries of an
// attempt already running here.
type admission struct {
	capacity resources.Resources
	slots    int // 0 = unbounded; counts held entries (lanes + jobs)

	mu      sync.Mutex
	held    map[string]resources.Resources
	changed chan struct{} // closed+replaced on every release (broadcast)
}

func newAdmission(capacity resources.Resources, slots int) *admission {
	return &admission{
		capacity: capacity,
		slots:    slots,
		held:     make(map[string]resources.Resources),
		changed:  make(chan struct{}),
	}
}

// tryAcquireLocked commits r under id iff it fits now. Call with mu held.
func (a *admission) tryAcquireLocked(id string, r resources.Resources) bool {
	if _, dup := a.held[id]; dup {
		return false
	}
	if a.slots > 0 && len(a.held) >= a.slots {
		return false
	}
	var used resources.Resources
	for _, h := range a.held {
		used.CPUMilli += h.CPUMilli
		used.MemBytes += h.MemBytes
	}
	if used.CPUMilli+r.CPUMilli > a.capacity.CPUMilli ||
		used.MemBytes+r.MemBytes > a.capacity.MemBytes {
		return false
	}
	a.held[id] = r
	return true
}

// TryAcquire commits r under id iff it fits now.
func (a *admission) TryAcquire(id string, r resources.Resources) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.tryAcquireLocked(id, r)
}

// Acquire blocks until r fits (or ctx ends), then commits it under id.
func (a *admission) Acquire(ctx context.Context, id string, r resources.Resources) error {
	for {
		a.mu.Lock()
		ch := a.changed
		ok := a.tryAcquireLocked(id, r)
		a.mu.Unlock()
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		}
	}
}

// Rename transfers a held reservation from old to new (lane token → job
// node). It fails when old is not held or new already is — the latter is the
// duplicate-execution guard.
func (a *admission) Rename(old, new string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	r, ok := a.held[old]
	if !ok {
		return false
	}
	if _, dup := a.held[new]; dup {
		return false
	}
	delete(a.held, old)
	a.held[new] = r
	return true
}

// Release frees a reservation (idempotent) and wakes every waiter.
func (a *admission) Release(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.held[id]; !ok {
		return
	}
	delete(a.held, id)
	close(a.changed)
	a.changed = make(chan struct{})
}

// Snapshot reports free capacity and the number of running jobs (entries
// under jobIDPrefix — lane pre-reservations are transient and not in-flight
// work).
func (a *admission) Snapshot() (free resources.Resources, inflight int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	free = a.capacity
	for id, h := range a.held {
		free.CPUMilli -= h.CPUMilli
		free.MemBytes -= h.MemBytes
		if strings.HasPrefix(id, jobIDPrefix) {
			inflight++
		}
	}
	if free.CPUMilli < 0 {
		free.CPUMilli = 0
	}
	if free.MemBytes < 0 {
		free.MemBytes = 0
	}
	return free, inflight
}
