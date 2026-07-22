package runner

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// Progress prints a start line per build step and a timing line on completion,
// indenting nested dependency builds. It is written by the local build
// orchestrator (jobs-client run / develop) to stderr; the scheduler path does
// not use it. A nil *Progress is a silent no-op.
type Progress struct {
	w     io.Writer
	depth int
}

func NewProgress(w io.Writer) *Progress { return &Progress{w: w} }

// Sub returns a child reporter indented one level deeper (for a nested dep build).
func (p *Progress) Sub() *Progress {
	if p == nil {
		return nil
	}
	return &Progress{w: p.w, depth: p.depth + 1}
}

func (p *Progress) indent() string { return strings.Repeat("  ", p.depth) }

// Start prints "→ <indent>label" and returns a done(err) closure that prints the
// elapsed time (✓) or the error (✗).
func (p *Progress) Start(label string) func(error) {
	if p == nil {
		return func(error) {}
	}
	start := time.Now()
	fmt.Fprintf(p.w, "%s→ %s\n", p.indent(), label)
	return func(err error) {
		el := time.Since(start).Round(time.Millisecond)
		if err != nil {
			fmt.Fprintf(p.w, "%s✗ %s  %s: %v\n", p.indent(), label, el, err)
			return
		}
		fmt.Fprintf(p.w, "%s✓ %s  %s\n", p.indent(), label, el)
	}
}

// Cached prints "✓ <indent>label  (cached)" for a step skipped by the join.
func (p *Progress) Cached(label string) {
	if p == nil {
		return
	}
	fmt.Fprintf(p.w, "%s✓ %s  (cached)\n", p.indent(), label)
}
