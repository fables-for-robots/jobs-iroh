package sched

import (
	"sort"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/jobs-build/jobs-iroh/wire"
)

// recordFailureLocked folds one failed (or budget-burning retried) attempt
// into the durable FAILURES stream. It must run BEFORE any re-enqueue: the
// record is keyed by the failing attempt's gen, which enqueueLocked bumps,
// and the log-ring snapshot is only valid while the ring still holds that
// gen. Cancelled-class re-enqueues are not recorded (runner-shutdown noise
// says nothing about the job); their budget exhaustion still is.
//
// Best-effort by contract: a publish failure is logged and dropped — losing
// a record costs observability, never correctness. FailureMsgID dedups the
// crash-between-handle-and-ack result redelivery, same reason as ResultMsgID.
func (s *Sched) recordFailureLocked(n *node, res *wire.Result, origin, disposition, summary string, backoff time.Duration) {
	if res != nil && res.Class == wire.ClassCancelled && disposition == wire.FailDispositionRetry {
		return
	}
	rec := wire.FailureRecord{
		Node:        n.name,
		Gen:         n.gen,
		Platform:    n.platform,
		Origin:      origin,
		Disposition: disposition,
		ErrSummary:  summary,
		ConsecRetry: n.consecRetry,
		ConsecCtrl:  n.consecControl,
		BackoffMs:   backoff.Milliseconds(),
		RequestIDs:  interestIDs(n),
		ForF:        waiterFs(n),
		Result:      res,
		FailedNs:    time.Now().UnixNano(),
	}
	if !n.enqueuedAt.IsZero() {
		rec.EnqueuedNs = n.enqueuedAt.UnixNano()
	}
	if !n.startedAt.IsZero() {
		rec.StartedNs = n.startedAt.UnixNano()
	}
	if lb := s.logs[n.name]; lb != nil && lb.gen == n.gen {
		rec.LogHead, rec.LogGap, rec.LogTail = trimLogSnapshot(lb.buf, failureLogHeadCap, failureLogTailCap)
	} else {
		rec.LogMissing = true
	}
	_, err := s.js.PublishMsg(s.ctx,
		&nats.Msg{Subject: wire.FailuresSubject(n.name), Data: wire.MustEncode(rec)},
		jetstream.WithMsgID(wire.FailureMsgID(n.name, n.gen)))
	if err != nil && s.ctx.Err() == nil {
		s.log.Warn("failure record write failed", "node", n.name, "gen", n.gen, "error", err)
	}
}

// trimLogSnapshot bounds a ring capture for one durable record: the head's
// start and the tail's end survive, everything squeezed out lands in the gap
// count.
func trimLogSnapshot(buf *logBuffer, headCap, tailCap int64) (head []byte, gap int64, tail []byte) {
	head, gap, tail = buf.head, buf.gap, buf.tail
	if cut := int64(len(head)) - headCap; cut > 0 {
		head, gap = head[:headCap], gap+cut
	}
	if cut := int64(len(tail)) - tailCap; cut > 0 {
		tail, gap = tail[cut:], gap+cut
	}
	head = append([]byte(nil), head...)
	tail = append([]byte(nil), tail...)
	return head, gap, tail
}

// interestIDs snapshots the node's live request interest, sorted.
func interestIDs(n *node) []string {
	if len(n.interest) == 0 {
		return nil
	}
	ids := make([]string, 0, len(n.interest))
	for id := range n.interest {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
