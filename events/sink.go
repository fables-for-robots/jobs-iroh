package events

// Sink consumes emitted events — the seam that replaces jobs' HTTP/Pebble
// Emitter. Implementations own delivery: filling the dedup identity fields
// (Emitter, Boot, Seq) and getting the event to its consumer (in jobs-iroh, a
// core-NATS publish per job subject). Emit must never block the emitting job
// and must never fail it — events are observability only. Implementations
// must be safe for concurrent use (OutputWriter's flush timer emits from its
// own goroutine).
type Sink interface {
	Emit(Event)
}

// NopSink discards every event — a non-nil Sink for call sites that want the
// nil-safe *Job plumbing without any consumer.
type NopSink struct{}

// Emit discards ev.
func (NopSink) Emit(Event) {}
