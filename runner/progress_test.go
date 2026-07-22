package runner

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestProgress_StartDoneCachedAndNesting(t *testing.T) {
	var buf bytes.Buffer
	p := NewProgress(&buf)
	done := p.Start("plugin-resolve")
	p.Sub().Cached("fetch toolchain (import)")
	done(nil)
	p.Start("pin")(errors.New("boom"))
	p.Cached("build")

	out := buf.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	// start line, nested cached, done line, start line, fail line, cached line
	if len(lines) != 6 {
		t.Fatalf("expected 6 lines, got %d:\n%s", len(lines), out)
	}
	if !strings.HasPrefix(lines[0], "→ plugin-resolve") {
		t.Fatalf("line0 = %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "  ✓ fetch toolchain (import)  (cached)") {
		t.Fatalf("nested cached not indented: %q", lines[1])
	}
	if !strings.HasPrefix(lines[2], "✓ plugin-resolve  ") {
		t.Fatalf("done line = %q", lines[2])
	}
	if !strings.HasPrefix(lines[3], "→ pin") {
		t.Fatalf("start line = %q", lines[3])
	}
	if !strings.HasPrefix(lines[4], "✗ pin  ") || !strings.Contains(lines[4], "boom") {
		t.Fatalf("fail line = %q", lines[4])
	}
	if lines[5] != "✓ build  (cached)" {
		t.Fatalf("cached line = %q", lines[5])
	}
}

func TestProgress_NilSafe(t *testing.T) {
	var p *Progress
	done := p.Start("x") // must not panic
	done(nil)
	p.Cached("y")
	if p.Sub() != nil {
		t.Fatal("nil.Sub() should be nil")
	}
}
