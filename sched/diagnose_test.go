package sched

// Failure-diagnostics tests: the FAILURES fold (durable per-attempt records
// with log-ring snapshots) and the Diagnose read path, including the
// server-restart fallback that resolves a request purely from the stream.

import (
	"strings"
	"testing"

	"github.com/jobs-build/jobs-iroh/api"
	"github.com/jobs-build/jobs-iroh/wire"
)

// publishAttemptLog pushes one log chunk for the job's gen and waits for the
// server fold to absorb it, so the failure record deterministically carries
// the attempt's output.
func (e *env) publishAttemptLog(job wire.Job, text string) {
	e.t.Helper()
	chunk := wire.LogChunk{Gen: job.Gen, Stream: "stderr", Seq: 0, Data: []byte(text)}
	if err := e.nc.Publish(wire.LogsSubject(job.Node), wire.MustEncode(chunk)); err != nil {
		e.t.Fatal(err)
	}
	waitFor(e.t, func() bool {
		view, _, _, err := e.s.Logs(e.ctx, job.Node, false)
		if err != nil {
			e.t.Fatal(err)
		}
		return view.Gen == job.Gen && strings.Contains(string(view.Head), text)
	})
}

func TestDiagnoseRetryBudgetTrail(t *testing.T) {
	e := newEnv(t)
	e.startRunner([]wire.Class{"c0.2-m1"}, func(job wire.Job) *wire.Result {
		e.publishAttemptLog(job, "attempt output gen "+wire.JobMsgID(job.Node, job.Gen))
		return &wire.Result{Class: wire.ClassRetryable, Exit: 75, ErrSummary: "flaky infra", Runner: "r-diag"}
	})

	sub, err := e.s.Submit(e.ctx, api.SubmitRequest{Def: e.treeDef("diagnose-retry")})
	if err != nil {
		t.Fatal(err)
	}
	snap := e.watchTerminal(sub.RequestID)
	if snap.Phase != "failed" {
		t.Fatalf("phase = %s, want failed", snap.Phase)
	}
	k, _ := wire.ParseKey(sub.K)
	bfNode := wire.NodeName(wire.KindBuildFrom, k)

	reply, err := e.s.Diagnose(e.ctx, api.DiagnoseRequest{RequestID: sub.RequestID})
	if err != nil {
		t.Fatal(err)
	}
	if reply.Phase != "failed" || reply.RequestID != sub.RequestID {
		t.Fatalf("reply header = %s/%s, want %s/failed", reply.RequestID, reply.Phase, sub.RequestID)
	}
	if len(reply.Nodes) != 1 || reply.Nodes[0].Node != bfNode {
		t.Fatalf("nodes = %+v, want exactly the failed buildfrom %s", reply.Nodes, bfNode)
	}
	nd := reply.Nodes[0]
	if nd.Kind != wire.KindBuildFrom || nd.Platform != testPlatform || nd.Phase != wire.PhaseFailed {
		t.Fatalf("node meta = %+v", nd)
	}
	// 4 attempts (initial + 3 budgeted retries), newest first: the budget
	// exhaustion is terminal, the older three are budget-burning retries
	// with doubling backoff.
	if len(nd.Attempts) != 4 {
		t.Fatalf("attempts = %d, want 4", len(nd.Attempts))
	}
	newest := nd.Attempts[0]
	if newest.Disposition != wire.FailDispositionFailed || newest.ConsecRetry != 4 ||
		!strings.Contains(newest.ErrSummary, "retry budget exhausted") {
		t.Fatalf("newest attempt = %+v, want terminal budget exhaustion", newest.FailureRecord)
	}
	if newest.Origin != wire.FailOriginResult || newest.Result == nil ||
		newest.Result.Class != wire.ClassRetryable || newest.Result.Exit != 75 || newest.Result.Runner != "r-diag" {
		t.Fatalf("newest attribution = %+v", newest.FailureRecord)
	}
	base := e.s.retryBase.Milliseconds()
	for i, wantBackoff := range []int64{0, 4 * base, 2 * base, 1 * base} { // newest→oldest
		a := nd.Attempts[i]
		if a.BackoffMs != wantBackoff {
			t.Fatalf("attempt[%d] backoff = %dms, want %d", i, a.BackoffMs, wantBackoff)
		}
		if i > 0 && a.Disposition != wire.FailDispositionRetry {
			t.Fatalf("attempt[%d] disposition = %s, want retry", i, a.Disposition)
		}
		if a.Gen == 0 || (i > 0 && nd.Attempts[i-1].Gen != a.Gen+1) {
			t.Fatalf("attempt gens not consecutive newest-first: %d then %d", nd.Attempts[i-1].Gen, a.Gen)
		}
		wantLog := "attempt output gen " + wire.JobMsgID(nd.Node, a.Gen)
		if a.LogMissing || !strings.Contains(string(a.LogHead), wantLog) {
			t.Fatalf("attempt[%d] log = missing=%v %q, want it to carry %q", i, a.LogMissing, a.LogHead, wantLog)
		}
		if len(a.RequestIDs) != 1 || a.RequestIDs[0] != sub.RequestID {
			t.Fatalf("attempt[%d] requestIds = %v", i, a.RequestIDs)
		}
	}

	// By node, capped: the newest two attempts only.
	byNode, err := e.s.Diagnose(e.ctx, api.DiagnoseRequest{Node: bfNode, MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(byNode.Nodes) != 1 || len(byNode.Nodes[0].Attempts) != 2 {
		t.Fatalf("by-node reply = %+v, want 1 node × 2 attempts", byNode.Nodes)
	}
	if byNode.Nodes[0].Attempts[0].Gen != newest.Gen {
		t.Fatalf("by-node newest gen = %d, want %d", byNode.Nodes[0].Attempts[0].Gen, newest.Gen)
	}

	// Restart fallback: Delete forgets the request (memory + KV mirror);
	// the stream records' interest tags still resolve it.
	if err := e.s.Delete(e.ctx, sub.RequestID); err != nil {
		t.Fatal(err)
	}
	after, err := e.s.Diagnose(e.ctx, api.DiagnoseRequest{RequestID: sub.RequestID})
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Nodes) != 1 || after.Nodes[0].Node != bfNode || len(after.Nodes[0].Attempts) != 4 {
		t.Fatalf("post-delete reply = %+v, want the stream-resolved trail", after.Nodes)
	}
}

func TestDiagnoseHardFailure(t *testing.T) {
	e := newEnv(t)
	e.startRunner([]wire.Class{"c0.2-m1"}, func(job wire.Job) *wire.Result {
		e.publishAttemptLog(job, "recipe stderr tail")
		return &wire.Result{Class: wire.ClassHard, Exit: 3, ErrSummary: "recipe exploded", Runner: "r-hard"}
	})
	sub, err := e.s.Submit(e.ctx, api.SubmitRequest{Def: e.treeDef("diagnose-hard")})
	if err != nil {
		t.Fatal(err)
	}
	e.watchTerminal(sub.RequestID)

	k, _ := wire.ParseKey(sub.K)
	reply, err := e.s.Diagnose(e.ctx, api.DiagnoseRequest{Node: wire.NodeName(wire.KindBuildFrom, k)})
	if err != nil {
		t.Fatal(err)
	}
	nd := reply.Nodes[0]
	if len(nd.Attempts) != 1 {
		t.Fatalf("attempts = %d, want 1 (hard = one strike)", len(nd.Attempts))
	}
	a := nd.Attempts[0]
	if a.Origin != wire.FailOriginResult || a.Disposition != wire.FailDispositionFailed ||
		a.ErrSummary != "recipe exploded" || a.Result == nil || a.Result.Exit != 3 {
		t.Fatalf("attempt = %+v", a.FailureRecord)
	}
	if !strings.Contains(string(a.LogHead), "recipe stderr tail") {
		t.Fatalf("log head = %q, want the captured output", a.LogHead)
	}
	if a.EnqueuedNs == 0 || a.FailedNs == 0 {
		t.Fatalf("timing anchors missing: %+v", a.FailureRecord)
	}
}

func TestDiagnoseValidation(t *testing.T) {
	e := newEnv(t)
	if _, err := e.s.Diagnose(e.ctx, api.DiagnoseRequest{}); err == nil {
		t.Fatal("want error for neither requestId nor node")
	}
	if _, err := e.s.Diagnose(e.ctx, api.DiagnoseRequest{RequestID: "r1", Node: "import_ff"}); err == nil {
		t.Fatal("want error for both requestId and node")
	}
	if _, err := e.s.Diagnose(e.ctx, api.DiagnoseRequest{Node: "not-a-node"}); err == nil {
		t.Fatal("want error for an unparsable node name")
	}
	if _, err := e.s.Diagnose(e.ctx, api.DiagnoseRequest{RequestID: "rdeadbeef"}); err == nil {
		t.Fatal("want not-found for an unknown request with no records")
	}
}

func TestTrimLogSnapshot(t *testing.T) {
	buf := newLogBuffer(4, 4)
	buf.write("stdout", 0, []byte("headtailplusmore")) // head=4, tail rings to last 4
	head, gap, tail := trimLogSnapshot(buf, 2, 2)
	if string(head) != "he" || string(tail) != "re" {
		t.Fatalf("head/tail = %q/%q, want he/re", head, tail)
	}
	// Ring already squeezed 8 bytes; trimming cut 2 from head and 2 from tail.
	if gap != 8+4 {
		t.Fatalf("gap = %d, want 12", gap)
	}
	if string(buf.head) != "head" || string(buf.tail) != "more" {
		t.Fatalf("trim mutated the ring: %q/%q", buf.head, buf.tail)
	}
}

func TestTrimAttemptTailPriority(t *testing.T) {
	rec := wire.FailureRecord{LogHead: []byte("headbytes"), LogTail: []byte("tailbytes"), LogGap: 5}
	a := trimAttempt(rec, 12)
	if !a.LogTruncated {
		t.Fatal("want LogTruncated")
	}
	// Tail survives whole (9 ≤ 12); head keeps the remaining 3 from its start.
	if string(a.LogTail) != "tailbytes" || string(a.LogHead) != "hea" {
		t.Fatalf("head/tail = %q/%q", a.LogHead, a.LogTail)
	}
	if a.LogGap != 5+6 {
		t.Fatalf("gap = %d, want 11", a.LogGap)
	}
	untrimmed := trimAttempt(rec, 0)
	if untrimmed.LogTruncated || string(untrimmed.LogHead) != "headbytes" {
		t.Fatalf("maxLogBytes=0 must keep the record as stored: %+v", untrimmed)
	}
}
