package clientcli

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jobs-build/jobs-iroh/api"
	"github.com/jobs-build/jobs-iroh/wire"
)

// syncBuf is a mutex-guarded buffer: follower goroutines write through
// liveView.Println (guarded by the view's own mutex) while the test polls
// String concurrently.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// waitFor polls cond until it holds or the test deadline budget runs out.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// shortGrace shrinks the drain grace for the test's lifetime so stop paths
// run in milliseconds.
func shortGrace(t *testing.T) {
	t.Helper()
	old := followStopGrace
	followStopGrace = 5 * time.Millisecond
	t.Cleanup(func() { followStopGrace = old })
}

// TestLogPrinterAssembly: chunk-boundary line splits, CRLF trim, gap marker,
// and the trailing-partial flush.
func TestLogPrinterAssembly(t *testing.T) {
	buf := &syncBuf{}
	p := &logPrinter{lv: &liveView{w: buf}, prefix: "n │ "}

	p.printView(api.LogView{Head: []byte("head partial"), GapSize: 42, Tail: []byte("tail\r\n")})
	p.write([]byte("hel"))
	p.write([]byte("lo\nwor"))
	p.write([]byte("ld\ntrailing"))
	p.flush()

	want := "n │ head partial\n" + // flushed ahead of the gap marker
		"n │ ... [42 bytes omitted] ...\n" +
		"n │ tail\n" + // \r trimmed
		"n │ hello\n" +
		"n │ world\n" +
		"n │ trailing\n"
	if got := buf.String(); got != want {
		t.Fatalf("printer output:\n%q\nwant:\n%q", got, want)
	}
}

// fakeOpener backs the tracker in tests: one buffered chunk feed per opened
// node; next() blocks on the feed until the follower ctx dies.
type fakeOpener struct {
	mu    sync.Mutex
	views map[string]api.LogView
	feeds map[string]chan wire.LogChunk
	ctxs  map[string]context.Context
	opens int
}

func newFakeOpener() *fakeOpener {
	return &fakeOpener{
		views: map[string]api.LogView{},
		feeds: map[string]chan wire.LogChunk{},
		ctxs:  map[string]context.Context{},
	}
}

func (f *fakeOpener) openLogs(ctx context.Context, node string, follow bool) (api.LogView, func() (wire.LogChunk, error), func(), error) {
	if !follow {
		return api.LogView{}, nil, nil, fmt.Errorf("tracker must follow")
	}
	f.mu.Lock()
	ch := make(chan wire.LogChunk, 64)
	f.feeds[node] = ch
	f.ctxs[node] = ctx
	f.opens++
	view := f.views[node]
	f.mu.Unlock()
	next := func() (wire.LogChunk, error) {
		select {
		case c := <-ch:
			return c, nil
		case <-ctx.Done():
			return wire.LogChunk{}, ctx.Err()
		}
	}
	return view, next, func() {}, nil
}

func (f *fakeOpener) feed(t *testing.T, node string, chunk wire.LogChunk) {
	t.Helper()
	f.mu.Lock()
	ch := f.feeds[node]
	f.mu.Unlock()
	if ch == nil {
		t.Fatalf("no follower opened for %s", node)
	}
	ch <- chunk
}

func (f *fakeOpener) opened(node string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.feeds[node] != nil
}

func (f *fakeOpener) openCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opens
}

func (f *fakeOpener) followerDead(node string) bool {
	f.mu.Lock()
	ctx := f.ctxs[node]
	f.mu.Unlock()
	return ctx != nil && ctx.Err() != nil
}

func runningSnap(nodes ...string) api.Snapshot {
	var snap api.Snapshot
	for _, n := range nodes {
		snap.Nodes = append(snap.Nodes, api.NodeSnap{Node: n, Phase: wire.PhaseRunning})
	}
	return snap
}

// TestLogTrackerFollowAndStop: a running node gets a follower whose lines
// print prefixed; leaving the running set grace-stops the follower and close
// flushes its trailing partial line; retries get a marker; stale-gen chunks
// are dropped.
func TestLogTrackerFollowAndStop(t *testing.T) {
	shortGrace(t)
	nodeA := nodeName("buildrun", 1)
	prefix := "buildrun:" + hex8(1) + " │ "
	buf := &syncBuf{}
	fake := newFakeOpener()
	tr := newLogTracker(context.Background(), fake, &liveView{w: buf})
	defer tr.close()

	tr.sync(runningSnap(nodeA))
	waitFor(t, "follower open", func() bool { return fake.opened(nodeA) })

	fake.feed(t, nodeA, wire.LogChunk{Gen: 1, Stream: "stdout", Seq: 1, Data: []byte("hello\nwor")})
	fake.feed(t, nodeA, wire.LogChunk{Gen: 1, Stream: "stdout", Seq: 2, Data: []byte("ld\n")})
	waitFor(t, "lines printed", func() bool {
		return strings.Contains(buf.String(), prefix+"hello\n") &&
			strings.Contains(buf.String(), prefix+"world\n")
	})

	// A stale chunk (older gen) is dropped; a newer gen marks the retry.
	fake.feed(t, nodeA, wire.LogChunk{Gen: 0, Stream: "stdout", Seq: 9, Data: []byte("stale\n")})
	fake.feed(t, nodeA, wire.LogChunk{Gen: 2, Stream: "stdout", Seq: 1, Data: []byte("again\ntrailing")})
	waitFor(t, "retry lines", func() bool {
		return strings.Contains(buf.String(), prefix+"(retried — attempt gen 2)\n") &&
			strings.Contains(buf.String(), prefix+"again\n")
	})
	if strings.Contains(buf.String(), "stale") {
		t.Fatalf("stale-gen chunk must be dropped:\n%s", buf.String())
	}

	// Node leaves the running set → follower dies after the grace; close
	// flushes the unterminated tail.
	tr.sync(api.Snapshot{Nodes: []api.NodeSnap{{Node: nodeA, Phase: wire.PhaseDone}}})
	waitFor(t, "follower stop", func() bool { return fake.followerDead(nodeA) })
	tr.close()
	if !strings.Contains(buf.String(), prefix+"trailing\n") {
		t.Fatalf("trailing partial line not flushed:\n%s", buf.String())
	}
	if !tr.streamedNodes()[nodeA] {
		t.Fatal("streamedNodes must remember the followed node")
	}
}

// TestLogTrackerCap: a fan-out beyond maxFollowNodes follows exactly the cap,
// notes the overflow once, and a freed slot admits a waiting node.
func TestLogTrackerCap(t *testing.T) {
	shortGrace(t)
	var nodes []string
	for i := 0; i < maxFollowNodes+3; i++ {
		nodes = append(nodes, nodeName("import", i))
	}
	buf := &syncBuf{}
	fake := newFakeOpener()
	tr := newLogTracker(context.Background(), fake, &liveView{w: buf})
	defer tr.close()

	tr.sync(runningSnap(nodes...))
	waitFor(t, "cap followers", func() bool { return fake.openCount() == maxFollowNodes })
	tr.sync(runningSnap(nodes...)) // unchanged snapshot must not re-open
	if n := fake.openCount(); n != maxFollowNodes {
		t.Fatalf("open count = %d, want %d", n, maxFollowNodes)
	}
	if got := strings.Count(buf.String(), "[logs] following"); got != 1 {
		t.Fatalf("cap note printed %d times:\n%s", got, buf.String())
	}

	// First node finishes → its slot admits one waiting node.
	snap := runningSnap(nodes[1:]...)
	snap.Nodes = append(snap.Nodes, api.NodeSnap{Node: nodes[0], Phase: wire.PhaseDone})
	tr.sync(snap)
	waitFor(t, "freed slot refilled", func() bool { return fake.openCount() == maxFollowNodes+1 })
	waitFor(t, "finished follower stopped", func() bool { return fake.followerDead(nodes[0]) })
}

// TestLogTrackerFollowsQueuedWithRunningPriority: queued nodes are followed
// (a fast job may never be seen running — snapshots are coalesced), but when
// slots are scarce the running nodes claim them first regardless of snapshot
// order.
func TestLogTrackerFollowsQueuedWithRunningPriority(t *testing.T) {
	shortGrace(t)
	fake := newFakeOpener()
	tr := newLogTracker(context.Background(), fake, &liveView{w: &syncBuf{}})
	defer tr.close()

	queued := nodeName("import", 1)
	tr.sync(api.Snapshot{Nodes: []api.NodeSnap{{Node: queued, Phase: wire.PhaseQueued}}})
	waitFor(t, "queued node followed", func() bool { return fake.opened(queued) })

	// Fill the remaining slots: maxFollowNodes running nodes listed AFTER
	// maxFollowNodes queued ones still win the free slots.
	var snap api.Snapshot
	snap.Nodes = append(snap.Nodes, api.NodeSnap{Node: queued, Phase: wire.PhaseQueued})
	for i := 0; i < maxFollowNodes; i++ {
		snap.Nodes = append(snap.Nodes, api.NodeSnap{Node: nodeName("pin", 10+i), Phase: wire.PhaseQueued})
	}
	var running []string
	for i := 0; i < maxFollowNodes; i++ {
		running = append(running, nodeName("buildrun", 20+i))
		snap.Nodes = append(snap.Nodes, api.NodeSnap{Node: running[i], Phase: wire.PhaseRunning})
	}
	tr.sync(snap)
	waitFor(t, "cap reached", func() bool { return fake.openCount() == maxFollowNodes })
	// The kept queued follower plus running nodes hold every slot; the seven
	// running nodes that fit are all attached, the later queued ones are not.
	for _, n := range running[:maxFollowNodes-1] {
		if !fake.opened(n) {
			t.Fatalf("running node %s must out-rank queued nodes for a slot", n)
		}
	}
	if fake.opened(nodeName("pin", 10)) {
		t.Fatal("queued node must not claim a slot ahead of running ones")
	}
}
