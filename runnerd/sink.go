package runnerd

import (
	"sync"
	"sync/atomic"

	"github.com/nats-io/nats.go"

	"github.com/fables-for-robots/jobs-iroh/events"
	"github.com/fables-for-robots/jobs-iroh/wire"
)

// jobSink is the per-attempt events.Sink: it maps the stage drivers'
// exec.output events onto wire.LogChunk publishes on logs.<node> (core NATS,
// fire-and-forget — the server folds them into bounded ring buffers), and
// folds exec.heartbeat cgroup readings into the attempt's rusage report.
// Everything else the drivers emit is dropped here — phases/outcomes reach
// the server through the Result, not the event stream.
type jobSink struct {
	nc      *nats.Conn
	subject string
	gen     uint64

	seq atomic.Uint64 // one space per attempt: unique per (node, gen)

	mu      sync.Mutex
	cpuMs   int64
	memPeak int64
	seen    bool
}

func newJobSink(nc *nats.Conn, node string, gen uint64) *jobSink {
	return &jobSink{nc: nc, subject: wire.LogsSubject(node), gen: gen, cpuMs: -1, memPeak: -1}
}

// Emit implements events.Sink. It must never block or fail the job.
func (s *jobSink) Emit(ev events.Event) {
	switch ev.Type {
	case events.TypeExecOutput:
		stream, _ := ev.Data["stream"].(string)
		chunk, _ := ev.Data["chunk"].([]byte)
		if len(chunk) == 0 {
			return
		}
		b, err := wire.Encode(wire.LogChunk{
			Gen:    s.gen,
			Stream: stream,
			Seq:    s.seq.Add(1),
			Data:   chunk,
		})
		if err != nil {
			return
		}
		_ = s.nc.Publish(s.subject, b) // fire-and-forget by design
	case events.TypeExecHeartbeat:
		s.mu.Lock()
		if v, ok := ev.Data["cpu_ms"].(int64); ok {
			s.cpuMs = v
			s.seen = true
		}
		if v, ok := ev.Data["mem_peak_bytes"].(int64); ok {
			s.memPeak = v
			s.seen = true
		}
		s.mu.Unlock()
	}
}

// Usage returns the last cgroup readings the attempt's heartbeats carried
// (cpuMs and memPeakBytes; -1 for a dimension never observed). ok is false
// when no usage reading arrived at all (non-Linux, undelegated cgroups, or a
// job with no process phase).
func (s *jobSink) Usage() (cpuMs, memPeakBytes int64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cpuMs, s.memPeak, s.seen
}
