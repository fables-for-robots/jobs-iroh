package clientcli

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// newLiveViewForTest forces TTY mode over an arbitrary writer with a fixed
// width, so renderer tests don't need a real terminal.
func newLiveViewForTest(w io.Writer, width int) *liveView {
	return &liveView{w: w, isTTY: true, width: width}
}

// TestLiveViewUpdateRedrawsInPlace: the second Update erases the first block
// (cursor-up + clear-to-end) before drawing; Collapse erases and prints the
// final summary.
func TestLiveViewUpdateRedrawsInPlace(t *testing.T) {
	var buf bytes.Buffer
	v := newLiveViewForTest(&buf, 80)

	v.Update([]string{"[build] 1/3 done", "  ▶ import gomod"})
	first := buf.String()
	if strings.Contains(first, "\x1b[2A") {
		t.Fatalf("first Update must not move the cursor up:\n%q", first)
	}
	if !strings.Contains(first, "[build] 1/3 done\n") || !strings.Contains(first, "  ▶ import gomod\n") {
		t.Fatalf("block lines missing:\n%q", first)
	}

	v.Update([]string{"[build] 2/3 done"})
	second := strings.TrimPrefix(buf.String(), first)
	if !strings.Contains(second, "\x1b[2A\x1b[J") {
		t.Fatalf("second Update must erase the 2-line block:\n%q", second)
	}

	v.Collapse("build: DONE · 5s")
	last := buf.String()
	if !strings.Contains(last, "\x1b[1A\x1b[J") {
		t.Fatalf("Collapse must erase the 1-line block:\n%q", last)
	}
	if !strings.HasSuffix(last, "build: DONE · 5s\n") {
		t.Fatalf("Collapse must print the final line last:\n%q", last)
	}
}

// TestLiveViewTruncatesToWidth: long lines are rune-truncated so the block
// never wraps (wrapping would break the cursor-up arithmetic).
func TestLiveViewTruncatesToWidth(t *testing.T) {
	var buf bytes.Buffer
	v := newLiveViewForTest(&buf, 10)
	v.Update([]string{"abcdefghijKLMNOP"})
	got := strings.TrimSuffix(buf.String(), "\n")
	if got != "abcdefghi…" {
		t.Fatalf("got %q, want %q", got, "abcdefghi…")
	}
}

// TestTruncateLineANSIAware: SGR escapes are copied through uncounted, and a
// truncated painted line is closed with a reset so color never leaks.
func TestTruncateLineANSIAware(t *testing.T) {
	painted := "\x1b[32m✓\x1b[0m abcdefghij"
	if got := truncateLine(painted, 12); got != painted {
		t.Fatalf("12 visible runes must fit width 12 untouched: %q", got)
	}
	got := truncateLine(painted, 8)
	if !strings.HasPrefix(got, "\x1b[32m✓\x1b[0m abcde") {
		t.Fatalf("escapes must pass through: %q", got)
	}
	if !strings.HasSuffix(got, "…\x1b[0m") {
		t.Fatalf("truncated painted line must end with ellipsis + reset: %q", got)
	}
	if visibleLen(got) != 8 {
		t.Fatalf("visible width = %d, want 8 (%q)", visibleLen(got), got)
	}
}

// TestPaintRespectsColorGate: paint is identity without the color flag
// (non-TTY, NO_COLOR, TERM=dumb — newLiveView only sets color on a real
// color-capable TTY) and wraps in SGR + reset with it.
func TestPaintRespectsColorGate(t *testing.T) {
	plain := &liveView{w: io.Discard}
	if got := plain.paint(sgrRed, "✗"); got != "✗" {
		t.Fatalf("colorless paint must be identity: %q", got)
	}
	var nilView *liveView
	if got := nilView.paint(sgrRed, "✗"); got != "✗" {
		t.Fatalf("nil view paint must be identity: %q", got)
	}
	color := &liveView{w: io.Discard, isTTY: true, color: true}
	if got := color.paint(sgrRed, "✗"); got != "\x1b[31m✗\x1b[0m" {
		t.Fatalf("painted glyph: %q", got)
	}
}

// TestLiveViewNonTTY: Update is a no-op, Println and Collapse print plainly
// with no ANSI.
func TestLiveViewNonTTY(t *testing.T) {
	var buf bytes.Buffer
	v := &liveView{w: &buf}
	v.Update([]string{"should not appear"})
	v.Println("plain line")
	v.Collapse("final line")
	got := buf.String()
	if strings.Contains(got, "\x1b") || strings.Contains(got, "should not appear") {
		t.Fatalf("non-TTY output wrong:\n%q", got)
	}
	if got != "plain line\nfinal line\n" {
		t.Fatalf("got %q", got)
	}
}

// TestLiveViewNilSafe: a nil view (tests, plain callers) never panics.
func TestLiveViewNilSafe(t *testing.T) {
	var v *liveView
	if v.IsTTY() {
		t.Fatal("nil view claims TTY")
	}
	v.Update([]string{"x"})
	v.Println("x")
	v.Collapse("x")
}

// TestXferProgressNonTTYQuarters: non-TTY prints one line per newly crossed
// quarter (25/50/75/100%), never duplicates, and finish() prints the summary.
func TestXferProgressNonTTYQuarters(t *testing.T) {
	var buf bytes.Buffer
	x := newXferProgress(&liveView{w: &buf}, "push")
	x.cb(1, 4)
	x.cb(1, 4) // same quarter: no reprint
	x.cb(2, 4)
	x.cb(4, 4)
	x.finish(true)
	got := buf.String()
	for _, want := range []string{
		"[push] 1/4 objects (25%)\n",
		"[push] 2/4 objects (50%)\n",
		"[push] 4/4 objects (100%)\n",
	} {
		if strings.Count(got, want) != 1 {
			t.Fatalf("want exactly one %q in:\n%q", want, got)
		}
	}
	if !strings.Contains(got, "[push] 4 objects · ") {
		t.Fatalf("finish summary missing:\n%q", got)
	}
}

// TestXferProgressTTY: TTY updates the block in place; unknown totals (pull)
// render a counting-up line; finish(false) erases without a summary.
func TestXferProgressTTY(t *testing.T) {
	var buf bytes.Buffer
	lv := newLiveViewForTest(&buf, 80)
	x := newXferProgress(lv, "pull")
	x.cb(312, 0)
	if !strings.Contains(buf.String(), "[pull] 312 objects\n") {
		t.Fatalf("unknown-total line missing:\n%q", buf.String())
	}
	x.finish(false)
	if strings.Contains(buf.String(), "·") {
		t.Fatalf("finish(false) must not print a summary:\n%q", buf.String())
	}
}

// TestXferProgressBytesInSummary: setBytes adds the payload size to the
// finish summary; without it the summary stays objects-only.
func TestXferProgressBytesInSummary(t *testing.T) {
	var buf bytes.Buffer
	x := newXferProgress(&liveView{w: &buf}, "pull")
	x.cb(4012, 0)
	x.setBytes(25_300_000)
	x.finish(true)
	if !strings.Contains(buf.String(), "[pull] 4012 objects · 25.3 MB · ") {
		t.Fatalf("bytes missing from summary:\n%q", buf.String())
	}
}

// TestFormatBytes covers the unit breakpoints.
func TestFormatBytes(t *testing.T) {
	for _, tc := range []struct {
		n    int64
		want string
	}{
		{512, "512 B"},
		{2_048, "2.0 kB"},
		{25_300_000, "25.3 MB"},
		{3_200_000_000, "3.2 GB"},
	} {
		if got := formatBytes(tc.n); got != tc.want {
			t.Errorf("formatBytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}
