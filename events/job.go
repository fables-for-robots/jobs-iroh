package events

import "time"

// Job scopes a Sink to one job execution: node + runner identity plus the
// request IDs the scheduler attached to the job (informational — the server's
// request↔node join is authoritative). A nil *Job (no sink configured)
// no-ops everything, so call sites stay unconditional.
type Job struct {
	sink     Sink
	node     string
	runner   string
	requests []string
}

// NewJob returns a per-job event scope over sink (jobs' Emitter.Job with the
// emitter swapped for the Sink seam). Nil-safe: a nil sink yields a nil Job,
// which no-ops every method.
func NewJob(sink Sink, node, runner string, requests []string) *Job {
	if sink == nil {
		return nil
	}
	return &Job{sink: sink, node: node, runner: runner, requests: requests}
}

func (j *Job) emit(typ string, data map[string]any) {
	if j == nil {
		return
	}
	if data == nil {
		data = map[string]any{}
	}
	if len(j.requests) > 0 {
		data["requests"] = j.requests
	}
	// TS is stamped here (jobs stamped it in Emitter.Emit — the same instant;
	// the trimmed Sink has no buffering layer to do it in). Sinks still own
	// Emitter/Boot/Seq.
	j.sink.Emit(Event{Type: typ, TS: time.Now().UnixNano(), Node: j.node, Runner: j.runner, Data: data})
}

// Started reports the job beginning execution on this runner.
func (j *Job) Started(kind string) { j.emit(TypeExecStarted, map[string]any{"kind": kind}) }

// Phase reports a step transition ("pulling", "fetching", "building", …).
func (j *Job) Phase(phase string) { j.emit(TypeExecPhase, map[string]any{"phase": phase}) }

// Progress reports quantitative progress within a phase: objects and bytes are
// cumulative totals for this job so far (e.g. store objects pulled from the
// server), not deltas — consumers render the latest value they have seen.
func (j *Job) Progress(phase string, objects int, bytes int64) {
	j.emit(TypeExecProgress, map[string]any{"phase": phase, "objects": objects, "bytes": bytes})
}

// Finished reports the terminal outcome: completed|failed|declined|cancelled.
func (j *Job) Finished(outcome string, exit int) {
	j.emit(TypeExecFinished, map[string]any{"outcome": outcome, "exit": exit})
}

// CacheSeeded reports one declared persistent cache's seed — the
// build-cache fetch+materialize (ref read, tar extract, atime pre-age). ms is
// that stage's wall-clock, bytes/files the materialized tree (regular files),
// cold whether no published state existed (bytes and files are 0 then).
func (j *Job) CacheSeeded(id string, ms int64, bytes int64, files int, cold bool) {
	j.emit(TypeExecCache, map[string]any{"cache": id, "stage": "seeded", "ms": ms, "bytes": bytes, "files": files, "cold": cold})
}

// CacheFinalized reports the same cache's post-build finalize (atime prune +
// ingest). result is updated|unchanged|empty — empty means pruned to
// nothing (bytes 0) with the last published state kept.
func (j *Job) CacheFinalized(id string, ms int64, bytes int64, files int, result string) {
	j.emit(TypeExecCache, map[string]any{"cache": id, "stage": "finalized", "ms": ms, "bytes": bytes, "files": files, "result": result})
}

// Heartbeat reports the job's process is still running: cpuMs is the job
// cgroup's CUMULATIVE CPU time (ms) since the cgroup was created, memBytes
// its CURRENT memory usage, memPeakBytes its PEAK memory (the kernel
// high-water mark, memory.peak). A negative value means "unknown"
// (undelegated host / unreadable stat file / pre-5.19 kernel) and omits that
// key — the event still fires as a pure liveness signal.
func (j *Job) Heartbeat(phase string, cpuMs, memBytes, memPeakBytes int64) {
	data := map[string]any{"phase": phase}
	if cpuMs >= 0 {
		data["cpu_ms"] = cpuMs
	}
	if memBytes >= 0 {
		data["mem_bytes"] = memBytes
	}
	if memPeakBytes >= 0 {
		data["mem_peak_bytes"] = memPeakBytes
	}
	j.emit(TypeExecHeartbeat, data)
}

// CAS reports the node attempt's CAS summary (issue #95): what the output
// ingest NEWLY stored into the runner's local store (storedBytes/storedObjects,
// storedDeduped = objects already present), and — when pushed is true — what
// the WriteRefs batch's server push actually uploaded (pushedBytes/
// pushedObjects) out of pushTotalObjects reachable. Push keys are omitted
// when pushed is false (no server sync), mirroring Heartbeat's unknown-key
// pattern.
func (j *Job) CAS(storedBytes int64, storedObjects, storedDeduped int, pushed bool, pushedBytes int64, pushedObjects, pushTotalObjects int) {
	data := map[string]any{
		"stored_bytes":   storedBytes,
		"stored_objects": storedObjects,
		"stored_deduped": storedDeduped,
	}
	if pushed {
		data["pushed_bytes"] = pushedBytes
		data["pushed_objects"] = pushedObjects
		data["push_total_objects"] = pushTotalObjects
	}
	j.emit(TypeExecCAS, data)
}

// PushProgress reports live server-push progress: objects settled (present
// at negotiation or uploaded) out of total reachable, cumulative across the
// WriteRefs batch. Bytes are unknown live — the exec.cas summary carries them.
func (j *Job) PushProgress(objects, total int) {
	j.emit(TypeExecProgress, map[string]any{"phase": "pushing", "objects": objects, "total": total})
}

// Output returns a WriteCloser that streams this job's process output as
// exec.output chunk events. Returns nil (a typed nil, not an
// interface-wrapped one) on a nil Job — callers must guard:
// if ow := j.Output(...); ow != nil { spec.Sink = ow; defer ow.Close() }.
func (j *Job) Output(stream, phase string) *OutputWriter {
	if j == nil {
		return nil
	}
	return newOutputWriter(j, stream, phase)
}
