package amberclient

import "sync"

// ProgressFunc observes one Push or Pull: done is the number of objects that
// have crossed the wire so far, total the number requested so far. total
// grows as want rounds are exchanged — it is a moving floor that settles at
// the true count, not an up-front promise (the want loop discovers missing
// subtrees round by round). Callbacks arrive from the transfer path, at most
// one at a time per transfer; a nil ProgressFunc is legal everywhere one is
// accepted.
type ProgressFunc func(done, total int)

// XferStats summarizes one observed transfer.
type XferStats struct {
	Objects int   // objects that crossed the wire
	Bytes   int64 // uncompressed payload bytes of those objects
}

// meter adapts the objects done/total callback shape onto wantsync.Progress
// (Send and Receive both report Requested/Transferred rounds). Concurrent-
// safe; the callback fires outside the lock with a consistent snapshot.
type meter struct {
	cb ProgressFunc

	mu    sync.Mutex
	done  int
	total int
	bytes int64
}

func (m *meter) Requested(objects int, _ int64) {
	m.mu.Lock()
	m.total += objects
	d, t := m.done, m.total
	m.mu.Unlock()
	if m.cb != nil {
		m.cb(d, t)
	}
}

func (m *meter) Transferred(objects int, bytes int64) {
	m.mu.Lock()
	m.done += objects
	m.bytes += bytes
	d, t := m.done, m.total
	m.mu.Unlock()
	if m.cb != nil {
		m.cb(d, t)
	}
}

func (m *meter) Wire(int64) {}

func (m *meter) stats() XferStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return XferStats{Objects: m.done, Bytes: m.bytes}
}
