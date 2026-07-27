package runner

import (
	"testing"
	"time"

	"github.com/jobs-build/jobs-iroh/sandbox"
)

// TestExecHeartbeatTicks: while running, the heartbeat emits exec.heartbeat
// once per interval carrying the cgroup usage from the stats source.
func TestExecHeartbeatTicks(t *testing.T) {
	ev, sink := capJob(t)

	stats := func() (sandbox.CgroupStats, bool) {
		return sandbox.CgroupStats{CPUUsec: 42_000_000, MemoryBytes: 1 << 30, MemoryPeakBytes: 3 << 30}, true
	}
	stop := startExecHeartbeat(ev, "building", 10*time.Millisecond, stats)
	time.Sleep(120 * time.Millisecond)
	stop()

	hbs := heartbeatEvents(t, sink)
	if len(hbs) < 2 {
		t.Fatalf("want >=2 heartbeats over 120ms at 10ms interval, got %d", len(hbs))
	}
	for _, hb := range hbs {
		if hb.Data["phase"] != "building" {
			t.Fatalf("phase = %v", hb.Data["phase"])
		}
		if evInt(t, hb.Data["cpu_ms"]) != 42000 {
			t.Fatalf("cpu_ms = %v (%T), want 42000", hb.Data["cpu_ms"], hb.Data["cpu_ms"])
		}
		if evInt(t, hb.Data["mem_bytes"]) != 1<<30 {
			t.Fatalf("mem_bytes = %v, want %d", hb.Data["mem_bytes"], 1<<30)
		}
		if evInt(t, hb.Data["mem_peak_bytes"]) != 3<<30 {
			t.Fatalf("mem_peak_bytes = %v, want %d", hb.Data["mem_peak_bytes"], 3<<30)
		}
	}
}

// TestExecHeartbeatFinalOnStop: stop emits one final settled heartbeat when
// stats are readable — a sub-interval job still gets its total-CPU reading.
func TestExecHeartbeatFinalOnStop(t *testing.T) {
	ev, sink := capJob(t)

	stats := func() (sandbox.CgroupStats, bool) {
		return sandbox.CgroupStats{CPUUsec: 1_500_000, MemoryBytes: 4096}, true
	}
	stop := startExecHeartbeat(ev, "building", time.Hour, stats)
	stop()

	hbs := heartbeatEvents(t, sink)
	if len(hbs) != 1 {
		t.Fatalf("want exactly the final settled heartbeat, got %d", len(hbs))
	}
	if evInt(t, hbs[0].Data["cpu_ms"]) != 1500 || evInt(t, hbs[0].Data["mem_bytes"]) != 4096 {
		t.Fatalf("final heartbeat data wrong: %+v", hbs[0].Data)
	}
}

// TestExecHeartbeatNoStats: an undelegated host (stats ok=false) still ticks
// pure-liveness heartbeats (phase only), but stop adds no final event (it
// would say nothing exec.finished doesn't).
func TestExecHeartbeatNoStats(t *testing.T) {
	ev, sink := capJob(t)

	stats := func() (sandbox.CgroupStats, bool) { return sandbox.CgroupStats{}, false }
	stop := startExecHeartbeat(ev, "fetching", 10*time.Millisecond, stats)
	time.Sleep(60 * time.Millisecond)
	stop()

	hbs := heartbeatEvents(t, sink)
	if len(hbs) < 1 {
		t.Fatal("want liveness heartbeats even without stats")
	}
	for _, hb := range hbs {
		if hb.Data["phase"] != "fetching" {
			t.Fatalf("phase = %v", hb.Data["phase"])
		}
		if _, ok := hb.Data["cpu_ms"]; ok {
			t.Fatalf("no-stats heartbeat must omit cpu_ms: %+v", hb.Data)
		}
		if _, ok := hb.Data["mem_bytes"]; ok {
			t.Fatalf("no-stats heartbeat must omit mem_bytes: %+v", hb.Data)
		}
	}

	// A second stop is harmless (executor defer + explicit stop).
	stop()
}

// TestExecHeartbeatNilJob: a nil events.Job (no sink configured — the local
// path) starts nothing; stop is a no-op.
func TestExecHeartbeatNilJob(t *testing.T) {
	stats := func() (sandbox.CgroupStats, bool) { return sandbox.CgroupStats{}, true }
	stop := startExecHeartbeat(nil, "building", time.Nanosecond, stats)
	stop()
}

// TestPhaseHeartbeatTicks (issue #101): a phase-liveness heartbeat ticks
// pure-liveness exec.heartbeat events (phase only, no usage keys) while a
// long process-less phase runs — seeding/materializing have no job cgroup to
// read, but watchers still need periodic updates.
func TestPhaseHeartbeatTicks(t *testing.T) {
	ev, sink := capJob(t)

	stop := startPhaseHeartbeat(ev, "materializing", 10*time.Millisecond)
	time.Sleep(120 * time.Millisecond)
	stop()
	stop() // idempotent

	hbs := heartbeatEvents(t, sink)
	if len(hbs) < 2 {
		t.Fatalf("want >=2 heartbeats over 120ms at 10ms interval, got %d", len(hbs))
	}
	for _, hb := range hbs {
		if hb.Data["phase"] != "materializing" {
			t.Fatalf("phase = %v", hb.Data["phase"])
		}
		for _, k := range []string{"cpu_ms", "mem_bytes", "mem_peak_bytes"} {
			if _, ok := hb.Data[k]; ok {
				t.Fatalf("phase heartbeat must be pure liveness, got %s: %+v", k, hb.Data)
			}
		}
	}

	// After stop, no further ticks.
	before := len(heartbeatEvents(t, sink))
	time.Sleep(50 * time.Millisecond)
	if after := len(heartbeatEvents(t, sink)); after != before {
		t.Fatalf("heartbeats kept ticking after stop: %d -> %d", before, after)
	}
}

// TestPhaseHeartbeatNilJob: nil Job (local path) starts nothing.
func TestPhaseHeartbeatNilJob(t *testing.T) {
	stop := startPhaseHeartbeat(nil, "seeding", time.Nanosecond)
	stop()
}
