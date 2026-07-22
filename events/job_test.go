package events

import "testing"

// TestJobCacheEvents: CacheSeeded/CacheFinalized emit exec.cache with the
// stage-discriminated data shape (cache-observability design §4).
func TestJobCacheEvents(t *testing.T) {
	sink := &captureSink{}
	j := NewJob(sink, "build|F", "r-test", nil)
	j.CacheSeeded("gocache", 3400, 1200, 7, false)
	j.CacheFinalized("gocache", 1100, 900, 5, "updated")

	evs := sink.events()
	if len(evs) != 2 {
		t.Fatalf("want 2 events, got %d: %+v", len(evs), evs)
	}
	seeded, finalized := evs[0], evs[1]
	if seeded.Type != TypeExecCache || finalized.Type != TypeExecCache {
		t.Fatalf("want exec.cache, got %q / %q", seeded.Type, finalized.Type)
	}
	if seeded.Node != "build|F" || seeded.Runner != "r-test" {
		t.Fatalf("seeded identity wrong: %+v", seeded)
	}
	wantSeeded := map[string]any{"cache": "gocache", "stage": "seeded", "ms": uint64(3400), "bytes": uint64(1200), "files": uint64(7), "cold": false}
	for k, v := range wantSeeded {
		if seeded.Data[k] != v {
			t.Fatalf("seeded[%s] = %v (%T), want %v", k, seeded.Data[k], seeded.Data[k], v)
		}
	}
	wantFinal := map[string]any{"cache": "gocache", "stage": "finalized", "ms": uint64(1100), "bytes": uint64(900), "files": uint64(5), "result": "updated"}
	for k, v := range wantFinal {
		if finalized.Data[k] != v {
			t.Fatalf("finalized[%s] = %v (%T), want %v", k, finalized.Data[k], finalized.Data[k], v)
		}
	}
	if _, ok := seeded.Data["result"]; ok {
		t.Fatal("seeded must not carry result")
	}
	if _, ok := finalized.Data["cold"]; ok {
		t.Fatal("finalized must not carry cold")
	}

	// Nil-safety: a nil Job no-ops both helpers.
	var nj *Job
	nj.CacheSeeded("x", 0, 0, 0, true)
	nj.CacheFinalized("x", 0, 0, 0, "updated")
}

// TestJobCASEvents: Job.CAS emits one exec.cas with stored_* always present
// and pushed_* only when pushed; Job.PushProgress emits exec.progress with
// phase=pushing and a total (CAS-size-observability design §events).
func TestJobCASEvents(t *testing.T) {
	sink := &captureSink{}
	j := NewJob(sink, "build|F1", "r1", []string{"req-1"})
	j.CAS(4096, 12, 3, true, 1024, 5, 40)
	j.CAS(2048, 7, 0, false, 0, 0, 0)
	j.PushProgress(5, 40)

	evs := sink.events()
	if len(evs) != 3 {
		t.Fatalf("expected 3 events, got %d: %+v", len(evs), evs)
	}

	pushed := evs[0]
	if pushed.Type != TypeExecCAS || pushed.Node != "build|F1" {
		t.Fatalf("bad event: %+v", pushed)
	}
	for k, want := range map[string]int64{
		"stored_bytes": 4096, "stored_objects": 12, "stored_deduped": 3,
		"pushed_bytes": 1024, "pushed_objects": 5, "push_total_objects": 40,
	} {
		if got := asInt64(t, pushed.Data[k]); got != want {
			t.Fatalf("%s = %d, want %d", k, got, want)
		}
	}

	local := evs[1]
	if got := asInt64(t, local.Data["stored_bytes"]); got != 2048 {
		t.Fatalf("stored_bytes = %d, want 2048", got)
	}
	for _, k := range []string{"pushed_bytes", "pushed_objects", "push_total_objects"} {
		if _, ok := local.Data[k]; ok {
			t.Fatalf("unpushed CAS event must omit %s: %+v", k, local.Data)
		}
	}

	prog := evs[2]
	if prog.Type != TypeExecProgress || prog.Data["phase"] != "pushing" {
		t.Fatalf("bad push progress: %+v", prog)
	}
	if got := asInt64(t, prog.Data["total"]); got != 40 {
		t.Fatalf("total = %d, want 40", got)
	}
	if _, ok := prog.Data["bytes"]; ok {
		t.Fatalf("push progress must omit bytes: %+v", prog.Data)
	}
}

// asInt64 coerces a JSON/CBOR-decoded number.
func asInt64(t *testing.T, v any) int64 {
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

// TestJobHeartbeatEvents: Heartbeat emits exec.heartbeat carrying the phase
// plus the job cgroup's cumulative CPU (ms), current memory (bytes), and peak
// memory (bytes, the kernel high-water mark); a negative value means
// "unknown" and omits that key (heartbeats are liveness first, stats second —
// an undelegated host still heartbeats).
func TestJobHeartbeatEvents(t *testing.T) {
	sink := &captureSink{}
	j := NewJob(sink, "build|F", "r-test", nil)
	j.Heartbeat("building", 42000, 1288490189, 1932735283)
	j.Heartbeat("building", -1, -1, -1)

	evs := sink.events()
	if len(evs) != 2 {
		t.Fatalf("want 2 events, got %d: %+v", len(evs), evs)
	}
	full, bare := evs[0], evs[1]
	if full.Type != TypeExecHeartbeat || bare.Type != TypeExecHeartbeat {
		t.Fatalf("want exec.heartbeat, got %q / %q", full.Type, bare.Type)
	}
	if full.Node != "build|F" || full.Runner != "r-test" {
		t.Fatalf("identity wrong: %+v", full)
	}
	want := map[string]any{"phase": "building", "cpu_ms": uint64(42000), "mem_bytes": uint64(1288490189), "mem_peak_bytes": uint64(1932735283)}
	for k, v := range want {
		if full.Data[k] != v {
			t.Fatalf("full[%s] = %v (%T), want %v", k, full.Data[k], full.Data[k], v)
		}
	}
	if bare.Data["phase"] != "building" {
		t.Fatalf("bare phase = %v", bare.Data["phase"])
	}
	for _, k := range []string{"cpu_ms", "mem_bytes", "mem_peak_bytes"} {
		if _, ok := bare.Data[k]; ok {
			t.Fatalf("unknown stat must omit %s", k)
		}
	}

	// Nil-safety: a nil Job no-ops.
	var nj *Job
	nj.Heartbeat("building", 1, 1, 1)
}
