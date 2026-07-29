package sched

import (
	"context"
	"slices"
	"sort"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/jobs-build/jobs-iroh/api"
	"github.com/jobs-build/jobs-iroh/wire"
)

// Diagnose assembles the failure report for one node or one request from
// the durable FAILURES stream plus whatever the live graph still knows.
// By-request resolution prefers the in-memory closure (root failures only,
// not derived failed-upstream); after a server restart it falls back to
// scanning the stream for records tagged with the request ID — the stream
// is bounded (7d / MaxMsgsPerSubject / MaxBytes), so the scan is too.
func (s *Sched) Diagnose(ctx context.Context, req api.DiagnoseRequest) (api.DiagnoseReply, error) {
	if (req.RequestID == "") == (req.Node == "") {
		return api.DiagnoseReply{}, badRequest("diagnose: exactly one of requestId and node must be set")
	}
	maxAttempts := req.MaxAttempts
	if maxAttempts <= 0 || maxAttempts > failuresPerNode {
		maxAttempts = diagnoseDefaultAttempts
	}

	if req.Node != "" {
		if _, _, err := wire.ParseNodeName(req.Node); err != nil {
			return api.DiagnoseReply{}, badRequest("diagnose: %v", err)
		}
		nd, err := s.diagnoseNode(ctx, req.Node, maxAttempts, req.MaxLogBytes)
		if err != nil {
			return api.DiagnoseReply{}, err
		}
		if len(nd.Attempts) == 0 {
			return api.DiagnoseReply{}, notFound("no failure records for node %s", req.Node)
		}
		return api.DiagnoseReply{Nodes: []api.NodeDiagnosis{nd}}, nil
	}
	return s.diagnoseRequest(ctx, req.RequestID, maxAttempts, req.MaxLogBytes)
}

// diagnoseNode builds one node's trail: the stream records (newest first),
// annotated with live phase/gen when the node is still in the table. A live
// failed node with no surviving records gets one attempt synthesized from
// memory so a failed build never diagnoses to nothing.
func (s *Sched) diagnoseNode(ctx context.Context, name string, maxAttempts int, maxLogBytes int64) (api.NodeDiagnosis, error) {
	kind, k, err := wire.ParseNodeName(name)
	if err != nil {
		return api.NodeDiagnosis{}, badRequest("diagnose: %v", err)
	}
	nd := api.NodeDiagnosis{Node: name, Kind: kind}

	var synth *api.AttemptReport
	s.mu.Lock()
	if n := s.nodes[nodeID{kind: kind, key: k}]; n != nil {
		nd.Platform = n.platform
		nd.Phase = n.phase
		nd.Gen = n.gen
		nd.Label = n.label
		if n.phase == wire.PhaseFailed {
			synth = s.synthAttemptLocked(n)
		}
	}
	s.mu.Unlock()

	records, err := s.readFailures(ctx, wire.FailuresSubject(name))
	if err != nil {
		return api.NodeDiagnosis{}, err
	}
	if len(records) > maxAttempts {
		records = records[len(records)-maxAttempts:]
	}
	for i := len(records) - 1; i >= 0; i-- { // stored oldest-first → report newest-first
		nd.Attempts = append(nd.Attempts, trimAttempt(records[i], maxLogBytes))
	}
	if len(nd.Attempts) == 0 && synth != nil {
		nd.Attempts = append(nd.Attempts, trimAttempt(synth.FailureRecord, maxLogBytes))
	}
	if nd.Platform == "" {
		for _, a := range nd.Attempts {
			if a.Platform != "" {
				nd.Platform = a.Platform
				break
			}
		}
	}
	return nd, nil
}

// diagnoseRequest resolves the request's failing node set, then delegates to
// diagnoseNode per member.
func (s *Sched) diagnoseRequest(ctx context.Context, id string, maxAttempts int, maxLogBytes int64) (api.DiagnoseReply, error) {
	reply := api.DiagnoseReply{RequestID: id}

	var names []string
	s.mu.Lock()
	r, live := s.requests[id]
	if live {
		snap := s.assembleLocked(r)
		reply.Phase = snap.Phase
		reply.Counts = snap.Counts
		names = failingClosureLocked(r)
	}
	s.mu.Unlock()

	if !live {
		// Restart fallback: request phase from the KV mirror (best-effort),
		// membership from the stream records' interest tags.
		if entry, err := s.kv.Get(ctx, requestKVKey(id)); err == nil {
			var st wire.RequestStatus
			if wire.Decode(entry.Value(), &st) == nil {
				reply.Phase = st.Phase
				reply.Counts = st.Counts
			}
		}
		records, err := s.readFailures(ctx, wire.SubjectFailuresRoot+".>")
		if err != nil {
			return api.DiagnoseReply{}, err
		}
		seen := map[string]bool{}
		for _, rec := range records {
			if !slices.Contains(rec.RequestIDs, id) || seen[rec.Node] {
				continue
			}
			seen[rec.Node] = true
			names = append(names, rec.Node)
		}
		sort.Strings(names)
		if reply.Phase == "" && len(names) == 0 {
			return api.DiagnoseReply{}, notFound("request %s not found (and no failure records mention it)", id)
		}
	}

	if len(names) > diagnoseMaxNodes {
		names = names[:diagnoseMaxNodes]
		reply.Truncated = true
	}
	for _, name := range names {
		nd, err := s.diagnoseNode(ctx, name, maxAttempts, maxLogBytes)
		if err != nil {
			return api.DiagnoseReply{}, err
		}
		reply.Nodes = append(reply.Nodes, nd)
	}
	return reply, nil
}

// failingClosureLocked walks the request's closure and returns its root
// failures (nodes actually in failed — the derived failed-upstream members
// are consequences, not causes), falling back to nodes whose retry counters
// show in-flight trouble when nothing is terminal yet. Sorted for stable
// replies.
func failingClosureLocked(r *request) []string {
	var failed, retrying []string
	seen := map[*node]bool{}
	var walk func(n *node)
	walk = func(n *node) {
		if n == nil || seen[n] {
			return
		}
		seen[n] = true
		switch {
		case n.phase == wire.PhaseFailed:
			failed = append(failed, n.name)
		case n.consecRetry > 0 || n.consecControl > 0:
			retrying = append(retrying, n.name)
		}
		for d := range n.deps {
			walk(d)
		}
	}
	walk(r.target)
	names := failed
	if len(names) == 0 {
		names = retrying
	}
	sort.Strings(names)
	return names
}

// synthAttemptLocked reconstructs a minimal attempt report from node memory
// for a failed node whose stream records did not survive (write failure or
// age-out): the current summary plus the live log ring, so the report is
// never empty.
func (s *Sched) synthAttemptLocked(n *node) *api.AttemptReport {
	rec := wire.FailureRecord{
		Node:        n.name,
		Gen:         n.gen,
		Platform:    n.platform,
		Origin:      wire.FailOriginServer,
		Disposition: wire.FailDispositionFailed,
		ErrSummary:  n.errSummary,
		ConsecRetry: n.consecRetry,
		ConsecCtrl:  n.consecControl,
		RequestIDs:  interestIDs(n),
	}
	if !n.enqueuedAt.IsZero() {
		rec.EnqueuedNs = n.enqueuedAt.UnixNano()
	}
	if !n.startedAt.IsZero() {
		rec.StartedNs = n.startedAt.UnixNano()
	}
	if !n.doneAt.IsZero() {
		rec.FailedNs = n.doneAt.UnixNano()
	}
	if lb := s.logs[n.name]; lb != nil && lb.gen == n.gen {
		rec.LogHead, rec.LogGap, rec.LogTail = trimLogSnapshot(lb.buf, failureLogHeadCap, failureLogTailCap)
	} else {
		rec.LogMissing = true
	}
	return &api.AttemptReport{FailureRecord: rec}
}

// readFailures fetches every stored record under one subject filter via an
// ephemeral ordered consumer, oldest first. NumPending at creation is the
// snapshot bound — records folded mid-read are next call's business.
func (s *Sched) readFailures(ctx context.Context, filter string) ([]wire.FailureRecord, error) {
	cons, err := s.js.OrderedConsumer(ctx, wire.StreamFailures, jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{filter},
		DeliverPolicy:  jetstream.DeliverAllPolicy,
	})
	if err != nil {
		return nil, internal("diagnose: failures consumer: %v", err)
	}
	pending := int(cons.CachedInfo().NumPending)
	var out []wire.FailureRecord
	for pending > 0 {
		n := pending
		if n > 64 {
			n = 64
		}
		batch, err := cons.Fetch(n, jetstream.FetchMaxWait(5*time.Second))
		if err != nil {
			return nil, internal("diagnose: fetch failures: %v", err)
		}
		got := 0
		for msg := range batch.Messages() {
			got++
			var rec wire.FailureRecord
			if err := wire.Decode(msg.Data(), &rec); err != nil {
				s.log.Warn("undecodable failure record dropped", "subject", msg.Subject(), "error", err)
				continue
			}
			out = append(out, rec)
		}
		if got == 0 {
			break // stream shrank under us (age-out/discard) — snapshot done
		}
		pending -= got
	}
	return out, nil
}

// trimAttempt applies the caller's per-attempt log budget, tail-priority —
// when debugging, the end of the output is where the story is.
func trimAttempt(rec wire.FailureRecord, maxLogBytes int64) api.AttemptReport {
	a := api.AttemptReport{FailureRecord: rec}
	if maxLogBytes <= 0 || int64(len(rec.LogHead))+int64(len(rec.LogTail)) <= maxLogBytes {
		return a
	}
	a.LogTruncated = true
	tail, head, gap := rec.LogTail, rec.LogHead, rec.LogGap
	if cut := int64(len(tail)) - maxLogBytes; cut > 0 {
		tail, gap = tail[cut:], gap+cut
	}
	budget := maxLogBytes - int64(len(tail))
	if cut := int64(len(head)) - budget; cut > 0 {
		head, gap = head[:budget], gap+cut
	}
	a.LogHead, a.LogGap, a.LogTail = head, gap, tail
	return a
}
