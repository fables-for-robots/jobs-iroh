package clientcli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/fables-for-robots/jobs-iroh/runner"
)

// newLiveProgressForTest builds a liveProgress without the TTY refresh
// ticker, so renders are deterministic.
func newLiveProgressForTest(lv *liveView) *liveProgress {
	return &liveProgress{lv: lv, stop: make(chan struct{})}
}

// TestLiveProgressNonTTYMatchesWriterSink: through the whole Progress seam,
// the non-TTY live view degrades to exactly jobs' classic plain lines (same
// shape as runner.NewProgress's writer sink; elapsed values differ by run).
func TestLiveProgressNonTTYMatchesWriterSink(t *testing.T) {
	var buf bytes.Buffer
	p := runner.NewProgressSink(newLiveProgressForTest(&liveView{w: &buf}))

	done := p.Start("plugin-resolve")
	p.Sub().Cached("fetch toolchain (import)")
	done(nil)
	p.Start("pin")(errors.New("boom"))
	p.Cached("build")

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 6 {
		t.Fatalf("expected 6 lines, got %d:\n%s", len(lines), buf.String())
	}
	for i, prefix := range []string{
		"→ plugin-resolve",
		"  ✓ fetch toolchain (import)  (cached)",
		"✓ plugin-resolve  ",
		"→ pin",
		"✗ pin  ",
		"✓ build  (cached)",
	} {
		if !strings.HasPrefix(lines[i], prefix) {
			t.Fatalf("line %d = %q, want prefix %q", i, lines[i], prefix)
		}
	}
	if !strings.Contains(lines[4], "boom") {
		t.Fatalf("fail line must carry the error: %q", lines[4])
	}
	if strings.Contains(buf.String(), "\x1b") {
		t.Fatalf("non-TTY progress must carry no ANSI:\n%q", buf.String())
	}
}

// TestLiveProgressTTYBlock: on a TTY, running steps live in the in-place
// block (nested, with elapsed) and completions become permanent ✓/✗ lines;
// finishing the last step leaves no block behind.
func TestLiveProgressTTYBlock(t *testing.T) {
	var buf bytes.Buffer
	lv := newLiveViewForTest(&buf, 120)
	lp := newLiveProgressForTest(lv)
	p := runner.NewProgressSink(lp)

	outer := p.Start("build target")
	if !strings.Contains(buf.String(), "→ build target · 0s\n") {
		t.Fatalf("running row missing:\n%q", buf.String())
	}
	sub := p.Sub().Start("import gomod")
	if !strings.Contains(buf.String(), "\x1b[1A\x1b[J") {
		t.Fatalf("second step must redraw over the 1-line block:\n%q", buf.String())
	}
	if !strings.Contains(buf.String(), "  → import gomod · 0s\n") {
		t.Fatalf("nested running row missing or unindented:\n%q", buf.String())
	}

	sub(nil)
	if !strings.Contains(buf.String(), "  ✓ import gomod  ") {
		t.Fatalf("permanent nested ✓ line missing:\n%q", buf.String())
	}
	outer(errors.New("compile failed"))
	if !strings.Contains(buf.String(), "✗ build target  ") || !strings.Contains(buf.String(), "compile failed") {
		t.Fatalf("permanent ✗ line missing:\n%q", buf.String())
	}

	// All steps finished: the block is gone, so a Close-time collapse and
	// further ticks write nothing more.
	before := buf.Len()
	lp.Close()
	if buf.Len() != before {
		t.Fatalf("Close after the last step must not write:\n%q", buf.String()[before:])
	}
}

// TestLiveProgressStepDoneUnwindsLIFO: duplicate labels (the same dep
// reached twice) unwind newest-first, leaving the older row in the block.
func TestLiveProgressStepDoneUnwindsLIFO(t *testing.T) {
	var buf bytes.Buffer
	lv := newLiveViewForTest(&buf, 120)
	lp := newLiveProgressForTest(lv)

	lp.StepStart(0, "dup")
	lp.StepStart(0, "dup")
	lp.StepDone(0, "dup", 0, nil)

	lp.mu.Lock()
	n := len(lp.steps)
	lp.mu.Unlock()
	if n != 1 {
		t.Fatalf("expected 1 in-flight step after LIFO unwind, got %d", n)
	}
}
