package sched

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/fables-for-robots/jobs-iroh/api"
	"github.com/fables-for-robots/jobs-iroh/wire"
)

// logBuffer is a bounded head+tail capture of one attempt's output — the
// port of jobs' buildactor output buffer: the head fills first to headCap;
// overflow rings through the tail (last tailCap bytes win); the bytes
// squeezed out between them are counted, not stored. Writes are idempotent
// per (stream, seq) — chunks arrive at-least-once.
type logBuffer struct {
	headCap, tailCap int64
	head             []byte
	tail             []byte
	gap              int64
	seen             map[string]bool
}

func newLogBuffer(headCap, tailCap int64) *logBuffer {
	return &logBuffer{headCap: headCap, tailCap: tailCap, seen: map[string]bool{}}
}

func (b *logBuffer) write(stream string, seq uint64, chunk []byte) {
	dedup := fmt.Sprintf("%s:%d", stream, seq)
	if b.seen[dedup] {
		return
	}
	b.seen[dedup] = true

	if int64(len(b.head)) < b.headCap {
		take := b.headCap - int64(len(b.head))
		if take > int64(len(chunk)) {
			take = int64(len(chunk))
		}
		b.head = append(b.head, chunk[:take]...)
		chunk = chunk[take:]
		if len(chunk) == 0 {
			return
		}
	}
	b.tail = append(b.tail, chunk...)
	if over := int64(len(b.tail)) - b.tailCap; over > 0 {
		b.gap += over
		b.tail = b.tail[over:]
	}
}

// nodeLog is the newest-gen log capture for one node (newest gen wins; a
// chunk for a later attempt resets the buffer).
type nodeLog struct {
	gen     uint64
	buf     *logBuffer
	updated time.Time
}

// maxLogNodes bounds the fold's memory: each entry can hold up to 4 MiB, so
// the fold evicts the least-recently-updated entry past this cap (buffers of
// evicted or table-dropped nodes are gone — logs are best-effort, in-memory
// observability only).
const maxLogNodes = 512

// onLogChunk folds one logs.<node> message: ring-buffer capture per
// (node, gen), live fan-out to followers, and the queued→running phase flip
// (the first output of an attempt is the earliest signal a runner picked the
// job up — wire carries no explicit claim message).
func (s *Sched) onLogChunk(msg *nats.Msg) {
	nodeName, ok := strings.CutPrefix(msg.Subject, wire.SubjectLogsRoot+".")
	if !ok || nodeName == "" {
		return
	}
	var chunk wire.LogChunk
	if err := wire.Decode(msg.Data, &chunk); err != nil {
		return
	}

	s.mu.Lock()
	lb := s.logs[nodeName]
	if lb == nil || chunk.Gen > lb.gen {
		lb = &nodeLog{gen: chunk.Gen, buf: newLogBuffer(logHeadCap, logTailCap)}
		s.logs[nodeName] = lb
		s.evictLogsLocked()
	}
	if chunk.Gen == lb.gen {
		lb.buf.write(chunk.Stream, chunk.Seq, chunk.Data)
		lb.updated = time.Now()
	}

	if kind, k, err := wire.ParseNodeName(nodeName); err == nil {
		if n := s.nodes[nodeID{kind: kind, key: k}]; n != nil &&
			n.gen == chunk.Gen && n.phase == wire.PhaseQueued {
			n.phase = wire.PhaseRunning
			n.startedAt = time.Now()
			s.putNodeStatusLocked(n)
			s.bumpLocked()
			s.log.Info("node running", "node", n.name, "gen", n.gen)
		}
	}

	for ch := range s.logSubs[nodeName] {
		select {
		case ch <- chunk:
		default: // slow follower: drop, live logs are lossy by contract
		}
	}
	s.mu.Unlock()
}

func (s *Sched) evictLogsLocked() {
	for len(s.logs) > maxLogNodes {
		var oldestName string
		var oldest time.Time
		first := true
		for name, lb := range s.logs {
			if first || lb.updated.Before(oldest) {
				oldestName, oldest, first = name, lb.updated, false
			}
		}
		delete(s.logs, oldestName)
	}
}

func (s *Sched) dropNodeLogLocked(nodeName string) {
	delete(s.logs, nodeName)
}

// Logs returns the stored head/gap/tail of the node's newest attempt and,
// with follow, a live channel of subsequent chunks (lossy for slow readers;
// chunks carry Gen and Seq so clients can splice them against the view).
// The returned stop function releases the follower; without follow the
// channel is closed immediately.
func (s *Sched) Logs(ctx context.Context, nodeName string, follow bool) (api.LogView, <-chan wire.LogChunk, func(), error) {
	if _, _, err := wire.ParseNodeName(nodeName); err != nil {
		return api.LogView{}, nil, nil, badRequest("logs: %v", err)
	}

	s.mu.Lock()
	view := api.LogView{Node: nodeName}
	if lb := s.logs[nodeName]; lb != nil {
		view.Gen = lb.gen
		view.Head = append([]byte(nil), lb.buf.head...)
		view.GapSize = lb.buf.gap
		view.Tail = append([]byte(nil), lb.buf.tail...)
	}
	if !follow {
		s.mu.Unlock()
		ch := make(chan wire.LogChunk)
		close(ch)
		return view, ch, func() {}, nil
	}
	ch := make(chan wire.LogChunk, 256)
	subs := s.logSubs[nodeName]
	if subs == nil {
		subs = map[chan wire.LogChunk]struct{}{}
		s.logSubs[nodeName] = subs
	}
	subs[ch] = struct{}{}
	s.mu.Unlock()

	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			s.mu.Lock()
			// Close ch only if WE removed it from the set: Sched.Close also
			// closes and unregisters every follower channel under s.mu, so
			// whoever unregisters is the one that closes — never both.
			if set := s.logSubs[nodeName]; set != nil {
				if _, registered := set[ch]; registered {
					delete(set, ch)
					if len(set) == 0 {
						delete(s.logSubs, nodeName)
					}
					close(ch)
				}
			}
			s.mu.Unlock()
		})
	}
	context.AfterFunc(ctx, stop)
	return view, ch, stop, nil
}

// onRunnerHello folds a runner connect announcement into the fleet view.
func (s *Sched) onRunnerHello(msg *nats.Msg) {
	var hello wire.Hello
	if err := wire.Decode(msg.Data, &hello); err != nil || hello.ID == "" {
		return
	}
	s.mu.Lock()
	s.fleet[hello.ID] = wire.RunnerInfo{
		ID:       hello.ID,
		Name:     hello.Name,
		Platform: hello.Platform,
		Size:     hello.Size,
		FreeCPU:  hello.CPUMilli,
		FreeMem:  hello.MemBytes,
		SeenNs:   time.Now().UnixNano(),
	}
	s.mu.Unlock()
	s.log.Info("runner hello", "id", hello.ID, "name", hello.Name,
		"platform", hello.Platform, "size", hello.Size)
}

// onRunnerHeartbeat folds a runner liveness/capacity report.
func (s *Sched) onRunnerHeartbeat(msg *nats.Msg) {
	var hb wire.Heartbeat
	if err := wire.Decode(msg.Data, &hb); err != nil || hb.ID == "" {
		return
	}
	s.mu.Lock()
	info := s.fleet[hb.ID] // zero value keeps an unknown runner visible by ID
	info.ID = hb.ID
	info.InFlight = hb.InFlight
	info.FreeCPU = hb.FreeCPU
	info.FreeMem = hb.FreeMem
	info.SeenNs = time.Now().UnixNano()
	s.fleet[hb.ID] = info
	s.mu.Unlock()
}

// Fleet returns the runner fleet snapshot (hello ∪ latest heartbeat),
// sorted by runner ID. Staleness is the caller's judgment via SeenNs.
func (s *Sched) Fleet() []wire.RunnerInfo {
	s.mu.Lock()
	out := make([]wire.RunnerInfo, 0, len(s.fleet))
	for _, info := range s.fleet {
		out = append(out, info)
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
