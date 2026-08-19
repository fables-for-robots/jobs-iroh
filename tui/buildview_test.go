package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/jobs-build/jobs-iroh/api"
	"github.com/jobs-build/jobs-iroh/wire"
)

// fakeStreams is a BuildStreams that records calls; streams never produce.
type fakeStreams struct {
	cancelled bool
	logOpens  []string
}

func (f *fakeStreams) OpenWatch(ctx context.Context) (func() (api.Snapshot, error), func(), error) {
	block := make(chan struct{})
	return func() (api.Snapshot, error) { <-block; return api.Snapshot{}, context.Canceled },
		func() { close(block) }, nil
}

func (f *fakeStreams) OpenLogs(ctx context.Context, node string) (api.LogView, func() (wire.LogChunk, error), func(), error) {
	f.logOpens = append(f.logOpens, node)
	block := make(chan struct{})
	return api.LogView{Node: node},
		func() (wire.LogChunk, error) { <-block; return wire.LogChunk{}, context.Canceled },
		func() { close(block) }, nil
}

func (f *fakeStreams) Cancel(ctx context.Context) error {
	f.cancelled = true
	return nil
}

func pressKey(s string) tea.KeyMsg {
	if len(s) == 1 {
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
	switch s {
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	}
	panic("unknown test key " + s)
}

func testView(t *testing.T, s BuildStreams) buildView {
	t.Helper()
	m := newBuildView(s, BuildWatchOptions{RequestID: "r1", Label: "app"})
	m.width, m.height = 100, 30
	m.sizeViewport()
	return m
}

// snapMsg wraps a snapshot as the current watch stream's event.
func snapMsg(m buildView, snap api.Snapshot) watchEventMsg {
	return watchEventMsg{seq: m.watchSeq, ev: watchEvent{snap: snap}}
}

func apply(t *testing.T, m buildView, msg tea.Msg) (buildView, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(msg)
	return next.(buildView), cmd
}

func TestBuildViewDetachVsTerminalQuit(t *testing.T) {
	m := testView(t, &fakeStreams{})
	m, _ = apply(t, m, snapMsg(m, chainSnap()))

	// q mid-build detaches.
	q, _ := apply(t, m, pressKey("q"))
	if !q.outcome.Detached {
		t.Fatal("q mid-build did not set Detached")
	}

	// Terminal done auto-quits with the final snapshot.
	done := chainSnap()
	done.Phase, done.Terminal = "done", true
	d, cmd := apply(t, m, snapMsg(m, done))
	if !d.outcome.HaveFinal || d.outcome.Final.Phase != "done" {
		t.Fatalf("terminal done outcome = %+v", d.outcome)
	}
	if cmd == nil {
		t.Fatal("terminal done returned no quit cmd")
	}

	// Terminal failed stays; q then quits without Detached.
	failed := chainSnap()
	failed.Phase, failed.Terminal = "failed", true
	failed.Nodes[4].Phase, failed.Nodes[4].ErrSummary = wire.PhaseFailed, "boom"
	f, _ := apply(t, m, snapMsg(m, failed))
	if !f.terminal || !f.outcome.HaveFinal {
		t.Fatalf("terminal failed state = terminal:%v outcome:%+v", f.terminal, f.outcome)
	}
	f2, _ := apply(t, f, pressKey("q"))
	if f2.outcome.Detached {
		t.Fatal("quit after terminal failure must not read as detach")
	}
}

func TestBuildViewCancelConfirm(t *testing.T) {
	fs := &fakeStreams{}
	m := testView(t, fs)
	m, _ = apply(t, m, snapMsg(m, chainSnap()))

	// c opens the confirm prompt; a non-y key aborts.
	m, _ = apply(t, m, pressKey("c"))
	if !m.confirm {
		t.Fatal("c did not open the confirm prompt")
	}
	m, _ = apply(t, m, pressKey("n"))
	if m.confirm || m.cancelSent {
		t.Fatal("non-y did not abort the confirm")
	}

	// ctrl+c → y sends the cancel.
	m, _ = apply(t, m, pressKey("ctrl+c"))
	if !m.confirm {
		t.Fatal("ctrl+c did not open the confirm prompt")
	}
	m, cmd := apply(t, m, pressKey("y"))
	if !m.cancelSent || cmd == nil {
		t.Fatal("y did not arm the cancel")
	}
	if msg := cmd(); msg.(bvCancelDoneMsg).err != nil || !fs.cancelled {
		t.Fatal("cancel cmd did not reach the streams seam")
	}
}

func TestBuildViewLogSwitchDebounce(t *testing.T) {
	fs := &fakeStreams{}
	m := testView(t, fs)
	m, cmd := apply(t, m, snapMsg(m, chainSnap()))
	// The first snapshot targets the running buildrun's logs immediately.
	if m.logNode != tn(wire.KindBuildRun, 3) {
		t.Fatalf("initial log node = %q, want the running buildrun", m.logNode)
	}
	if cmd == nil {
		t.Fatal("no log-open cmd armed for the first snapshot")
	}

	// Cursor to the import row: the switch is debounced behind a token…
	m, _ = apply(t, m, pressKey("down"))
	if m.logNode != tn(wire.KindBuildRun, 3) {
		t.Fatal("cursor move switched the log stream synchronously")
	}
	// …a stale token does nothing…
	m, _ = apply(t, m, bvLogSelMsg{token: m.logSelToken - 1})
	if m.logNode != tn(wire.KindBuildRun, 3) {
		t.Fatal("stale debounce token switched the stream")
	}
	// …the current token performs the switch.
	m, _ = apply(t, m, bvLogSelMsg{token: m.logSelToken})
	if m.logNode != tn(wire.KindImport, 4) {
		t.Fatalf("log node after debounce = %q, want the import", m.logNode)
	}
}

func TestBuildViewCursorPreservedAcrossSnapshots(t *testing.T) {
	m := testView(t, &fakeStreams{})
	m, _ = apply(t, m, snapMsg(m, chainSnap()))
	m, _ = apply(t, m, pressKey("down"))
	wantPath := m.visible[m.cursor].Path

	// A fresh snapshot (same shape, new phases) keeps the cursor's path.
	snap := chainSnap()
	snap.Nodes[4].ElapsedMs = 12000
	m, _ = apply(t, m, snapMsg(m, snap))
	if m.visible[m.cursor].Path != wantPath {
		t.Fatalf("cursor moved: %q, want %q", m.visible[m.cursor].Path, wantPath)
	}
}

func TestBuildViewStaleStreamMsgsDrop(t *testing.T) {
	m := testView(t, &fakeStreams{})
	m, _ = apply(t, m, snapMsg(m, chainSnap()))

	stale := watchEventMsg{seq: m.watchSeq - 1, ev: watchEvent{snap: api.Snapshot{Phase: "failed", Terminal: true}}}
	m2, _ := apply(t, m, stale)
	if m2.terminal {
		t.Fatal("stale watch message reached the model")
	}

	cancelled := false
	staleLog := logStartedMsg{seq: m.logSeq - 1, node: "x", cancel: func() { cancelled = true }}
	if m3, _ := apply(t, m, staleLog); m3.logCh != nil {
		t.Fatal("stale log stream adopted")
	} else if !cancelled {
		t.Fatal("stale log stream not cancelled")
	}
}

func TestBuildRowLineRendering(t *testing.T) {
	running := &buildRow{Node: tn(wire.KindBuildValue, 1), Label: "app", Phase: wire.PhaseRunning, Stage: "build", ElapsedMs: 41000, Runner: "r-a"}
	line := buildRowLine(treeRow{Depth: 1, HasKids: true, Expanded: true}, running, 0)
	for _, want := range []string{"▾", "▶", "app", "build", "41s", "on r-a"} {
		if !strings.Contains(line, want) {
			t.Fatalf("running line %q missing %q", line, want)
		}
	}

	cached := &buildRow{Node: tn(wire.KindBuildValue, 2), Label: "dep", Phase: wire.PhaseDone, Cached: true}
	if line := buildRowLine(treeRow{}, cached, 0); !strings.Contains(line, "(cached)") {
		t.Fatalf("cached line %q", line)
	}

	failed := &buildRow{Node: tn(wire.KindBuildValue, 3), Phase: wire.PhaseFailed, Stage: "pin", Err: "boom"}
	line = buildRowLine(treeRow{}, failed, 0)
	if !strings.Contains(line, "✗") || !strings.Contains(line, "pin: boom") {
		t.Fatalf("failed line %q", line)
	}
	// No label: the kind:key8 fallback.
	if !strings.Contains(line, "buildvalue:"+"00000000") {
		t.Fatalf("failed line %q missing the shortNode fallback", line)
	}
}
