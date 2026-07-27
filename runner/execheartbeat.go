package runner

import (
	"time"

	"github.com/jobs-build/jobs-iroh/events"
	"github.com/jobs-build/jobs-iroh/sandbox"
)

// execHeartbeatInterval is how often a running job's process emits an
// exec.heartbeat (liveness + cgroup usage). A var so tests can shrink it.
var execHeartbeatInterval = 5 * time.Second

// startExecHeartbeat emits exec.heartbeat every interval while a job's
// process runs: stats is read per tick (the job cgroup's cumulative CPU +
// current memory; ok=false or a -1 field degrades to a pure liveness event).
// The returned stop ends the ticker and, when stats are readable, emits one
// final settled heartbeat — a sub-interval job still gets its total-CPU
// reading, and the last tick's throttling never swallows the settled totals.
// Call stop after the process exits but BEFORE closing the cgroup (accounting
// persists until rmdir). Nil ev (no sink — the local path) starts nothing;
// stop is idempotent.
func startExecHeartbeat(ev *events.Job, phase string, interval time.Duration, stats func() (sandbox.CgroupStats, bool)) (stop func()) {
	if ev == nil {
		return func() {}
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				st, ok := stats()
				if !ok {
					st = sandbox.CgroupStats{CPUUsec: -1, MemoryBytes: -1, MemoryPeakBytes: -1}
				}
				ev.Heartbeat(phase, cpuMs(st.CPUUsec), st.MemoryBytes, st.MemoryPeakBytes)
			}
		}
	}()
	stopped := false
	return func() {
		if stopped {
			return
		}
		stopped = true
		close(done)
		<-finished
		if st, ok := stats(); ok {
			ev.Heartbeat(phase, cpuMs(st.CPUUsec), st.MemoryBytes, st.MemoryPeakBytes)
		}
	}
}

// startPhaseHeartbeat emits pure-liveness exec.heartbeat events (phase only,
// no usage keys) every interval while a long process-less phase runs. The
// seeding/materializing spans extract whole trees to disk before any job
// process (or its cgroup) exists, so without these the watcher sees nothing
// for minutes on a big store (issue #101). Same contract as
// startExecHeartbeat: nil ev starts nothing; stop is idempotent (and emits no
// settled reading — there is no usage to settle).
func startPhaseHeartbeat(ev *events.Job, phase string, interval time.Duration) (stop func()) {
	return startExecHeartbeat(ev, phase, interval, func() (sandbox.CgroupStats, bool) {
		return sandbox.CgroupStats{}, false
	})
}

// cpuMs converts a cpu.stat usage_usec reading to milliseconds, preserving
// the -1 "unknown" sentinel.
func cpuMs(usec int64) int64 {
	if usec < 0 {
		return -1
	}
	return usec / 1000
}
