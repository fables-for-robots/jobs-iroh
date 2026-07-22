package events

import "sync"

// captureSink collects emitted events for test inspection. Every event is
// round-tripped through the CBOR codec first, so tests observe exactly the
// value shapes a wire consumer would (small ints as uint64, []string as
// []any, byte strings as []byte) — the same shapes the original jobs tests
// asserted through the HTTP collector.
type captureSink struct {
	mu  sync.Mutex
	evs []Event
}

func (c *captureSink) Emit(ev Event) {
	b, err := Encode(ev)
	if err != nil {
		panic(err)
	}
	dec, err := Decode(b)
	if err != nil {
		panic(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evs = append(c.evs, dec)
}

func (c *captureSink) events() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Event(nil), c.evs...)
}
