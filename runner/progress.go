package runner

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// Sink receives the step events behind Progress. The default writer sink
// (NewProgress) prints jobs' classic plain →/✓/✗ lines; the CLI's live view
// implements Sink to render running steps as an in-place TTY block instead.
// depth is the Sub() nesting level; implementations used with a concurrent
// driver must be safe for concurrent use.
type Sink interface {
	StepStart(depth int, label string)
	StepDone(depth int, label string, elapsed time.Duration, err error)
	StepCached(depth int, label string)
}

// Progress prints a start line per build step and a timing line on completion,
// indenting nested dependency builds. It is written by the local build
// orchestrator (jobs-client build / run) to stderr; the scheduler path does
// not use it. A nil *Progress is a silent no-op.
type Progress struct {
	sink  Sink
	depth int
}

func NewProgress(w io.Writer) *Progress { return &Progress{sink: writerSink{w}} }

// NewProgressSink drives a custom sink (the CLI's TTY live view). A nil sink
// yields a nil (no-op) Progress.
func NewProgressSink(s Sink) *Progress {
	if s == nil {
		return nil
	}
	return &Progress{sink: s}
}

// Sub returns a child reporter indented one level deeper (for a nested dep build).
func (p *Progress) Sub() *Progress {
	if p == nil {
		return nil
	}
	return &Progress{sink: p.sink, depth: p.depth + 1}
}

// Start reports "→ label" and returns a done(err) closure that reports the
// elapsed time (✓) or the error (✗).
func (p *Progress) Start(label string) func(error) {
	if p == nil {
		return func(error) {}
	}
	start := time.Now()
	p.sink.StepStart(p.depth, label)
	return func(err error) {
		p.sink.StepDone(p.depth, label, time.Since(start).Round(time.Millisecond), err)
	}
}

// Cached reports "✓ label  (cached)" for a step skipped by the join.
func (p *Progress) Cached(label string) {
	if p == nil {
		return
	}
	p.sink.StepCached(p.depth, label)
}

// writerSink is the default Sink: jobs' original plain-line output, byte for
// byte (start line at Start, ✓/✗ line at completion, two-space nesting).
type writerSink struct{ w io.Writer }

func indentOf(depth int) string { return strings.Repeat("  ", depth) }

func (s writerSink) StepStart(depth int, label string) {
	fmt.Fprintf(s.w, "%s→ %s\n", indentOf(depth), label)
}

func (s writerSink) StepDone(depth int, label string, elapsed time.Duration, err error) {
	if err != nil {
		fmt.Fprintf(s.w, "%s✗ %s  %s: %v\n", indentOf(depth), label, elapsed, err)
		return
	}
	fmt.Fprintf(s.w, "%s✓ %s  %s\n", indentOf(depth), label, elapsed)
}

func (s writerSink) StepCached(depth int, label string) {
	fmt.Fprintf(s.w, "%s✓ %s  (cached)\n", indentOf(depth), label)
}
