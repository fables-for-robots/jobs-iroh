//go:build linux

package runner_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/fables-for-robots/jobs-iroh/builddef"
	"github.com/fables-for-robots/jobs-iroh/events"
	"github.com/fables-for-robots/jobs-iroh/runner"
	"github.com/fables-for-robots/jobs-iroh/sandbox"
)

// memSink captures emitted events in memory — the external-package stand-in
// for jobs' HTTP capture collector (the Sink seam is synchronous, no flush).
type memSink struct {
	mu  sync.Mutex
	evs []events.Event
}

func (s *memSink) Emit(ev events.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evs = append(s.evs, ev)
}

func (s *memSink) events() []events.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]events.Event(nil), s.evs...)
}

func testEvInt(t *testing.T, v any) int64 {
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

// TestRunBuildPhaseEvents: a successful cacheless build emits the exec.phase
// sequence assembling → materializing → building → finalizing → pushing (no
// "seeding" — no caches declared) plus one exec.cas summary whose local-ingest
// stats are non-zero and whose push keys are absent (the local writer reports
// no server-push totals). Port of jobs' TestRunBuildPhaseEvents with the HTTP
// emitter swapped for the Sink seam (build-events §9).
func TestRunBuildPhaseEvents(t *testing.T) {
	if !sandbox.UserNSAvailable() {
		t.Skip("user namespaces unavailable")
	}

	ctx := context.Background()
	st := buildEvalStore(t)
	platform := runner.Platform()
	shellKey := buildShellArtifact(t, ctx, st)

	srcInput, _ := mkImportInputWithOutput(t, ctx, st, "src-fetcher", "https://example.com/src.tgz",
		"BUILD.jobs", "def build(): return struct(inputs={}, env={}, script='', runtime_deps=[])\n")
	pinned := builddef.Pinned{
		Script: `printf ok > "$out/result"`,
	}
	_, f, _ := putPinnedBuild(t, ctx, st, srcInput, platform, pinned)

	sink := &memSink{}
	brc := runner.BuildRunCfg{Platform: platform, ShellKey: shellKey, CacheDir: t.TempDir(),
		Events: events.NewJob(sink, "build|"+f.String(), "r1", nil)}
	runCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	out := runner.RunBuild(runCtx, st, runner.NewLocalRefWriter(st), brc, runner.NamespaceBuildExecutor{}, f)
	if out.Failed || out.Decline || out.Cancelled {
		t.Fatalf("RunBuild: %+v", out)
	}

	evs := sink.events()
	var phases []string
	for _, ev := range evs {
		if ev.Type == events.TypeExecPhase {
			phases = append(phases, ev.Data["phase"].(string))
		}
	}
	want := []string{"assembling", "materializing", "building", "finalizing", "pushing"}
	if len(phases) != len(want) {
		t.Fatalf("phases = %v, want %v", phases, want)
	}
	for i := range want {
		if phases[i] != want[i] {
			t.Fatalf("phases = %v, want %v", phases, want)
		}
	}

	var cas *events.Event
	for i := range evs {
		if evs[i].Type == events.TypeExecCAS {
			cas = &evs[i]
		}
	}
	if cas == nil {
		t.Fatal("expected an exec.cas event from a successful build")
	}
	if n, ok := cas.Data["stored_bytes"]; !ok || testEvInt(t, n) == 0 {
		t.Fatalf("exec.cas stored_bytes missing/zero: %+v", cas.Data)
	}
	// LocalRefWriter reports no server-push totals, so the push keys are absent.
	if _, ok := cas.Data["pushed_bytes"]; ok {
		t.Fatalf("exec.cas must omit push keys in local mode: %+v", cas.Data)
	}
}
