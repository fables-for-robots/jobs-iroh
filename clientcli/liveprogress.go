package clientcli

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/fables-for-robots/jobs-iroh/runner"
)

// liveProgress adapts the runner's Progress seam (runner.Sink) to the
// liveView. On a TTY the in-flight steps form the live block — nested
// "→ label · 12s" rows with a once-a-second elapsed refresh — and each
// completion becomes a permanent ✓/✗ line above the block. On a non-TTY it
// degrades to exactly the plain writer sink's change-lines (start line at
// Start, timing/error line at completion), byte-compatible with jobs'
// classic output. Safe for concurrent use; Close stops the refresh ticker
// and erases any leftover block.
type liveProgress struct {
	lv *liveView

	mu    sync.Mutex
	steps []liveStep // in-flight, in start order

	stop chan struct{}
	once sync.Once
}

type liveStep struct {
	depth int
	label string
	start time.Time
}

var _ runner.Sink = (*liveProgress)(nil)

func newLiveProgress(lv *liveView) *liveProgress {
	p := &liveProgress{lv: lv, stop: make(chan struct{})}
	if lv.IsTTY() {
		go p.refresh()
	}
	return p
}

// refresh re-renders the block once a second so the per-step elapsed ticks
// even when no step starts or finishes (long compiles).
func (p *liveProgress) refresh() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			p.mu.Lock()
			p.redrawLocked()
			p.mu.Unlock()
		}
	}
}

// Close stops the refresh ticker and erases any leftover block. Idempotent;
// call it before handing the terminal to a child process (run entrypoint).
func (p *liveProgress) Close() {
	p.once.Do(func() { close(p.stop) })
	p.lv.Collapse("")
}

func indentOf(depth int) string { return strings.Repeat("  ", depth) }

func (p *liveProgress) StepStart(depth int, label string) {
	if !p.lv.IsTTY() {
		p.lv.Println(indentOf(depth) + "→ " + label)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.steps = append(p.steps, liveStep{depth: depth, label: label, start: time.Now()})
	p.redrawLocked()
}

func (p *liveProgress) StepDone(depth int, label string, elapsed time.Duration, err error) {
	p.mu.Lock()
	p.removeLocked(depth, label)
	p.mu.Unlock()
	if err != nil {
		p.lv.Println(fmt.Sprintf("%s%s %s  %s: %v", indentOf(depth), p.lv.paint(sgrRed, "✗"), label, elapsed, err))
	} else {
		p.lv.Println(fmt.Sprintf("%s%s %s  %s", indentOf(depth), p.lv.paint(sgrGreen, "✓"), label, elapsed))
	}
	p.mu.Lock()
	p.redrawLocked()
	p.mu.Unlock()
}

func (p *liveProgress) StepCached(depth int, label string) {
	p.lv.Println(fmt.Sprintf("%s%s %s  (cached)", indentOf(depth), p.lv.paint(sgrGreen, "✓"), label))
}

// removeLocked drops the most recent in-flight step matching (depth, label);
// duplicates (the same dep reached twice concurrently) unwind LIFO.
func (p *liveProgress) removeLocked(depth int, label string) {
	for i := len(p.steps) - 1; i >= 0; i-- {
		if p.steps[i].depth == depth && p.steps[i].label == label {
			p.steps = append(p.steps[:i], p.steps[i+1:]...)
			return
		}
	}
}

func (p *liveProgress) redrawLocked() {
	if !p.lv.IsTTY() {
		return
	}
	lines := make([]string, 0, len(p.steps))
	for _, s := range p.steps {
		lines = append(lines, fmt.Sprintf("%s→ %s · %s", indentOf(s.depth), s.label, formatDur(time.Since(s.start))))
	}
	p.lv.Update(lines)
}
