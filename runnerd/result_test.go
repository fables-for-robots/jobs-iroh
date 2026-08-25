package runnerd

import (
	"slices"
	"testing"

	"github.com/jobs-build/jobs-iroh/runner"
	"github.com/jobs-build/jobs-iroh/wire"
)

func testJob() wire.Job {
	return wire.Job{Node: "import_abc", Kind: wire.KindImport, Gen: 3}
}

func TestBuildResultSuccess(t *testing.T) {
	refs := []wire.RefProposal{{Name: "import-output:abc", Key: []byte{1}}}
	res := buildResult(testJob(), "r1", runner.Outcome{}, refs, "runner-push/r1/import_abc-3", wire.Rusage{WallNs: 42}, nil)
	if res.Class != wire.ClassOK {
		t.Fatalf("class = %q, want ok", res.Class)
	}
	if len(res.Refs) != 1 || res.Refs[0].Name != "import-output:abc" {
		t.Fatalf("refs = %+v", res.Refs)
	}
	if res.ScratchRef != "runner-push/r1/import_abc-3" {
		t.Fatalf("scratchRef = %q", res.ScratchRef)
	}
	if res.Node != "import_abc" || res.Gen != 3 || res.Runner != "r1" || res.Rusage.WallNs != 42 {
		t.Fatalf("envelope = %+v", res)
	}
}

func TestBuildResultCancelled(t *testing.T) {
	res := buildResult(testJob(), "r1", runner.Outcome{Cancelled: true}, nil, "", wire.Rusage{}, nil)
	if res.Class != wire.ClassCancelled {
		t.Fatalf("class = %q, want cancelled", res.Class)
	}
	if len(res.Refs) != 0 || res.ScratchRef != "" {
		t.Fatalf("cancelled result must carry no refs: %+v", res)
	}
}

func TestBuildResultDeclineIsRetryable(t *testing.T) {
	res := buildResult(testJob(), "r1", runner.Outcome{Decline: true, DeclineReason: "no shell"}, nil, "", wire.Rusage{}, nil)
	if res.Class != wire.ClassRetryable {
		t.Fatalf("class = %q, want retryable", res.Class)
	}
	if res.ErrSummary != "declined: no shell" {
		t.Fatalf("errSummary = %q", res.ErrSummary)
	}
}

func TestBuildResultFailureClasses(t *testing.T) {
	cases := []struct {
		driver string
		want   string
	}{
		{"hard", wire.ClassHard},
		{"retryable", wire.ClassRetryable},
		{"control", wire.ClassControl},
		{"", wire.ClassHard}, // unset fails closed
	}
	for _, tc := range cases {
		out := runner.Outcome{Failed: true, Class: tc.driver, ExitCode: 7, Phase: "building", Stderr: "boom"}
		res := buildResult(testJob(), "r1", out, nil, "", wire.Rusage{}, nil)
		if res.Class != tc.want {
			t.Errorf("driver class %q -> %q, want %q", tc.driver, res.Class, tc.want)
		}
		if res.Exit != 7 {
			t.Errorf("exit = %d, want 7", res.Exit)
		}
		if res.ErrSummary != "building: boom" {
			t.Errorf("errSummary = %q, want phase-prefixed stderr", res.ErrSummary)
		}
	}
}

func TestBuildResultFailureRefsNeverLeak(t *testing.T) {
	refs := []wire.RefProposal{{Name: "import-output:abc", Key: []byte{1}}}
	res := buildResult(testJob(), "r1", runner.Outcome{Failed: true, Class: "hard"}, refs, "scratch", wire.Rusage{}, nil)
	if len(res.Refs) != 0 || res.ScratchRef != "" {
		t.Fatalf("failed result must not propose refs: %+v", res)
	}
}

// ReadRefs must ride the result for every class — the server needs to know
// cached refs were used regardless of the build outcome.
func TestBuildResultCarriesReadRefs(t *testing.T) {
	job := wire.Job{Node: "n", Gen: 3}
	reads := []string{"build-output:aa", "shell:linux/amd64"}

	ok := buildResult(job, "r1", runner.Outcome{}, nil, "", wire.Rusage{}, reads)
	failed := buildResult(job, "r1", runner.Outcome{Failed: true, Class: "hard"}, nil, "", wire.Rusage{}, reads)
	cancelled := buildResult(job, "r1", runner.Outcome{Cancelled: true}, nil, "", wire.Rusage{}, reads)

	for name, res := range map[string]wire.Result{"ok": ok, "failed": failed, "cancelled": cancelled} {
		if !slices.Equal(res.ReadRefs, reads) {
			t.Errorf("%s: ReadRefs = %v, want %v", name, res.ReadRefs, reads)
		}
	}
}

func TestClassFanOut(t *testing.T) {
	// A c2-m8 runner serves every rung that fits in 2000m/8GiB.
	got := wire.ClassesWithin(wire.Class("c2-m8").Resources())
	want := []wire.Class{"c0.2-m1", "c1-m1", "c1-m2", "c1-m4", "c2-m4", "c2-m8"}
	if !slices.Equal(got, want) {
		t.Fatalf("classes = %v, want %v", got, want)
	}

	// The smallest runner serves exactly one lane.
	got = wire.ClassesWithin(wire.Class("c0.2-m1").Resources())
	if !slices.Equal(got, []wire.Class{"c0.2-m1"}) {
		t.Fatalf("classes = %v, want [c0.2-m1]", got)
	}
}

func TestScratchRefName(t *testing.T) {
	got := scratchRefName("r1", "buildrun_ff", 4)
	if got != "runner-push/r1/buildrun_ff-4" {
		t.Fatalf("scratchRefName = %q", got)
	}
}

func TestConsumerLaneNames(t *testing.T) {
	// The shared durable consumer names are part of the wire contract; the
	// daemon must derive them exactly (subject-token platform, dots escaped).
	if got := wire.ConsumerName("linux/amd64", "c0.2-m1"); got != "wq-linux-amd64-c0_2-m1" {
		t.Fatalf("consumer name = %q", got)
	}
	if got := wire.JobsSubject("linux/amd64", "c1-m2"); got != "jobs.linux-amd64.c1-m2" {
		t.Fatalf("jobs subject = %q", got)
	}
}
