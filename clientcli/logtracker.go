package clientcli

// Live output tracking for remote-build/watch (--logs): a snapshot-driven
// follower set over the api logs stream. The server side already speaks
// Logs{node, follow} → stored view + live chunks (the design doc's "live
// follow"); this file is the client half — which nodes to follow, and how
// their bytes become terminal lines that coexist with the liveView block.

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/jobs-build/jobs-iroh/amberclient"
	"github.com/jobs-build/jobs-iroh/api"
	"github.com/jobs-build/jobs-iroh/wire"
)

// maxFollowNodes caps the concurrent log-follow streams during one watch,
// like maxRunningRows caps the display: a wide fan-out follows at most this
// many active nodes (running first, snapshot order) and notes the rest.
const maxFollowNodes = 8

// followStopGrace delays a finished node's follower teardown: the last output
// chunks are fanned out server-side before the result folds into the
// snapshot, so at "done" they can still be in flight on the QUIC stream —
// cutting the read immediately would lose them.
var followStopGrace = 500 * time.Millisecond

// logOpener opens one node's log stream (stored view first, then live chunks
// while follow); *apiConn implements it, tests fake it.
type logOpener interface {
	openLogs(ctx context.Context, node string, follow bool) (api.LogView, func() (wire.LogChunk, error), func(), error)
}

// openLogs performs the logs frame exchange on its own stream: the stored
// view arrives synchronously, subsequent chunks through the returned reader.
// The returned done disarms ctx and fully terminates the stream.
func (a *apiConn) openLogs(ctx context.Context, node string, follow bool) (api.LogView, func() (wire.LogChunk, error), func(), error) {
	stream, stop, err := a.openRequest(ctx, api.TLogs, api.LogsRequest{Node: node, Follow: follow})
	if err != nil {
		return api.LogView{}, nil, nil, err
	}
	done := func() {
		stop()
		amberclient.CloseStream(stream)
	}
	rt, rb, err := api.ReadFrame(stream)
	if err != nil {
		done()
		return api.LogView{}, nil, nil, fmt.Errorf("logs: read view: %w", err)
	}
	var view api.LogView
	if err := decodeReply(rt, rb, api.TLogView, &view); err != nil {
		done()
		return api.LogView{}, nil, nil, err
	}
	next := func() (wire.LogChunk, error) {
		rt, rb, err := api.ReadFrame(stream)
		if err != nil {
			return wire.LogChunk{}, err
		}
		var chunk wire.LogChunk
		if err := decodeReply(rt, rb, api.TLogChunk, &chunk); err != nil {
			return wire.LogChunk{}, err
		}
		return chunk, nil
	}
	return view, next, done, nil
}

// logPrinter assembles one node's output bytes into prefixed permanent lines
// on the live view (Println keeps them scrolling above the progress block).
// Chunks cut lines anywhere, so a partial line waits in pending until its
// newline arrives or flush is called.
type logPrinter struct {
	lv      *liveView
	prefix  string
	pending []byte
}

func (p *logPrinter) write(data []byte) {
	p.pending = append(p.pending, data...)
	for {
		i := bytes.IndexByte(p.pending, '\n')
		if i < 0 {
			return
		}
		p.println(p.pending[:i])
		p.pending = p.pending[i+1:]
	}
}

// flush prints any trailing unterminated line.
func (p *logPrinter) flush() {
	if len(p.pending) > 0 {
		p.println(p.pending)
		p.pending = nil
	}
}

func (p *logPrinter) println(line []byte) {
	p.lv.Println(p.prefix + string(bytes.TrimSuffix(line, []byte("\r"))))
}

// printView renders a stored head/gap/tail view through the printer.
func (p *logPrinter) printView(view api.LogView) {
	p.write(view.Head)
	if view.GapSize > 0 {
		p.flush()
		p.lv.Println(p.prefix + fmt.Sprintf("... [%d bytes omitted] ...", view.GapSize))
	}
	p.write(view.Tail)
}

// logTracker streams the output of a watch's running nodes: sync diffs each
// snapshot's running set against the follower set, opening a follow stream
// per newly running node (up to maxFollowNodes) and grace-stopping followers
// whose nodes left. Output lines are best-effort observability (the fan-out
// is lossy by contract) — they never decide the build verdict.
type logTracker struct {
	ctx  context.Context
	open logOpener
	lv   *liveView

	mu       sync.Mutex
	active   map[string]context.CancelFunc
	labels   map[string]string // node → display label, from snapshots
	followed map[string]bool   // every node ever followed, for the failure recap
	closed   bool
	capNoted bool
	wg       sync.WaitGroup
}

func newLogTracker(ctx context.Context, open logOpener, lv *liveView) *logTracker {
	return &logTracker{
		ctx: ctx, open: open, lv: lv,
		active:   map[string]context.CancelFunc{},
		labels:   map[string]string{},
		followed: map[string]bool{},
	}
}

// sync folds one snapshot into the follower set. Nil-safe so the plain watch
// path can pass a nil tracker. Surviving followers keep their slots before
// new nodes claim any, so the cap never churns an attached stream out.
//
// Every active-phase node is a candidate, not just running ones: snapshots
// are coalesced (≤4/s), so a fast job can pass through running entirely
// between two snapshots and only ever be seen queued — attaching at queued
// catches its output from the first chunk. Running nodes claim free slots
// first, so a wide fan-out's waiting jobs can't starve the streams that are
// actually producing output.
func (t *logTracker) sync(snap api.Snapshot) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	active := map[string]bool{}
	for _, n := range snap.Nodes {
		if n.Label != "" {
			t.labels[n.Node] = n.Label
		}
		if isActivePhase(n.Phase) {
			active[n.Node] = true
		}
	}
	slots := 0
	for node, cancel := range t.active {
		if active[node] {
			slots++
			continue
		}
		delete(t.active, node)
		time.AfterFunc(followStopGrace, cancel)
	}
	for _, runningPass := range []bool{true, false} {
		for _, n := range snap.Nodes {
			if !active[n.Node] || (n.Phase == wire.PhaseRunning) != runningPass ||
				t.active[n.Node] != nil {
				continue
			}
			if slots >= maxFollowNodes {
				if !t.capNoted {
					t.capNoted = true
					t.lv.Println(fmt.Sprintf("[logs] following %d streams — more steps are active, not all output is shown", maxFollowNodes))
				}
				continue
			}
			slots++
			t.followLocked(n.Node)
		}
	}
}

func (t *logTracker) followLocked(node string) {
	fctx, cancel := context.WithCancel(t.ctx)
	t.active[node] = cancel
	t.followed[node] = true
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		defer cancel()
		t.runFollower(fctx, node)
	}()
}

// runFollower prints one node's stored view then live chunks until the stream
// ends (grace stop, watch teardown, server close). A chunk for a newer gen
// means the attempt was retried: flush, mark, continue with the new attempt.
func (t *logTracker) runFollower(ctx context.Context, node string) {
	t.mu.Lock()
	label := t.labels[node]
	t.mu.Unlock()
	prefix := labelNode(node, label) + " │ "
	view, next, done, err := t.open.openLogs(ctx, node, true)
	if err != nil {
		if ctx.Err() == nil {
			t.lv.Println(prefix + fmt.Sprintf("(logs unavailable: %v)", err))
		}
		return
	}
	defer done()
	p := &logPrinter{lv: t.lv, prefix: prefix}
	defer p.flush()
	p.printView(view)
	gen := view.Gen
	for {
		chunk, err := next()
		if err != nil {
			return
		}
		if chunk.Gen < gen {
			continue // stale attempt
		}
		if chunk.Gen > gen {
			if gen > 0 {
				p.flush()
				t.lv.Println(prefix + fmt.Sprintf("(retried — attempt gen %d)", chunk.Gen))
			}
			gen = chunk.Gen
		}
		p.write(chunk.Data)
	}
}

// close stops every follower (each after the drain grace) and waits for them
// to finish printing. Idempotent; nil-safe.
func (t *logTracker) close() {
	if t == nil {
		return
	}
	t.mu.Lock()
	if !t.closed {
		t.closed = true
		for node, cancel := range t.active {
			delete(t.active, node)
			time.AfterFunc(followStopGrace, cancel)
		}
	}
	t.mu.Unlock()
	t.wg.Wait()
}

// streamedNodes reports every node whose output was followed live (nil-safe),
// so the failure recap can point at the scroll instead of re-printing logs.
func (t *logTracker) streamedNodes() map[string]bool {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return maps.Clone(t.followed)
}
