package amberclient

import "sync"

// ProgressFunc observes one Push or Pull: done is the number of objects that
// have crossed the wire so far, total the number requested so far. total
// grows as want rounds are exchanged — it is a moving floor that settles at
// the true count, not an up-front promise (the want loop discovers missing
// subtrees round by round). Callbacks arrive from the transfer path, at most
// one at a time per transfer (sharded transfers report from several
// goroutines, but the meter serializes the callback); a nil ProgressFunc is
// legal everywhere one is accepted.
type ProgressFunc func(done, total int)

// XferStats summarizes one observed transfer.
type XferStats struct {
	Objects int   // objects that crossed the wire
	Bytes   int64 // uncompressed payload bytes of those objects
}

// meter adapts the objects done/total callback shape onto wantsync.Progress
// (Send and Receive both report Requested/Transferred rounds). Concurrent-
// safe; cbMu keeps the one-at-a-time, monotonic-snapshot callback contract
// even when a sharded transfer's channels report concurrently.
type meter struct {
	cb   ProgressFunc
	cbMu sync.Mutex

	mu    sync.Mutex
	done  int
	total int
	bytes int64
}

func (m *meter) Requested(objects int, _ int64) {
	m.report(objects, 0, 0)
}

func (m *meter) Transferred(objects int, bytes int64) {
	m.report(0, objects, bytes)
}

func (m *meter) report(requested, done int, bytes int64) {
	m.cbMu.Lock()
	defer m.cbMu.Unlock()
	m.mu.Lock()
	m.total += requested
	m.done += done
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
