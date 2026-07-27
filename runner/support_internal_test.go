package runner

import (
	"sync"
	"testing"
	"time"

	"github.com/jobs-build/jobs-iroh/amber"
	"github.com/jobs-build/jobs-iroh/events"
)

// capSink captures emitted events in memory — the trimmed stand-in for jobs'
// HTTP capture collector. Values in Data are the Go-native ones the Job
// methods stored (int/int64/bool), not CBOR-decoded; evInt absorbs both.
type capSink struct {
	mu  sync.Mutex
	evs []events.Event
}

func (c *capSink) Emit(ev events.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evs = append(c.evs, ev)
}

func (c *capSink) events() []events.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]events.Event(nil), c.evs...)
}

// capJob builds a Job over a capturing sink (no flush needed — synchronous).
func capJob(t *testing.T) (*events.Job, *capSink) {
	t.Helper()
	sink := &capSink{}
	return events.NewJob(sink, "build|F", "r-test", nil), sink
}

// evInt coerces a Data number (int/int64/uint64/float64).
func evInt(t *testing.T, v any) int64 {
	t.Helper()
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case uint64:
		return int64(n)
	case float64:
		return int64(n)
	}
	t.Fatalf("not a number: %v (%T)", v, v)
	return 0
}

// shrinkHeartbeatInterval shrinks the package heartbeat interval for one test.
func shrinkHeartbeatInterval(t *testing.T, d time.Duration) {
	t.Helper()
	old := execHeartbeatInterval
	execHeartbeatInterval = d
	t.Cleanup(func() { execHeartbeatInterval = old })
}

func heartbeatEvents(t *testing.T, sink *capSink) []events.Event {
	t.Helper()
	var out []events.Event
	for _, ev := range sink.events() {
		if ev.Type == events.TypeExecHeartbeat {
			out = append(out, ev)
		}
	}
	return out
}

// openTestStore opens a fresh single-process store under a test temp dir —
// jobs' ambertest harness collapsed to amber.Open (no daemon, no signer).
func openTestStore(t *testing.T) *amber.Store {
	t.Helper()
	st, err := amber.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}
