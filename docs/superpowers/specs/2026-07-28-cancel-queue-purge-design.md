# Cancel purges queued jobs from the JOBS work queue

**Date:** 2026-07-28
**Status:** approved

## Problem

Cancellation is pure server-side bookkeeping: `Sched.Cancel` /
`Sched.Delete` / watcher-loss cleanup drop the request's interest and evict
zero-interest nodes from the in-memory table (`dropInterestLocked`,
sched/node.go), but job messages already published to the JetStream JOBS
work queue are never touched. Runners still pull and fully execute them;
the results are dropped on arrival ("wasteful but never wrong"). On a busy
fleet a cancelled request keeps burning runner time for every job that was
queued at cancel time.

## Goal

Best-effort removal of a cancelled node's queued job message from the JOBS
stream, so work no runner has picked up yet never executes. Explicitly out
of scope:

- **Stopping in-flight attempts.** A job already delivered to a runner
  runs to completion; its result is dropped as today. No runner-side
  cancellation signal, no wire change, no ALPN bump.
- **Server-restart orphans.** The node table is memory-only while the
  stream is durable; messages published by a previous server incarnation
  have no remembered sequence and drain through runners as today.

## Design

Approach: track the publish sequence, delete by sequence. (Alternatives
rejected: scanning the stream at cancel time is O(stream) and racy;
per-node subjects + `PurgeEx` rewrites the frozen wire subject grammar and
forces a runner lockstep + ALPN fence bump.)

1. **State.** `node` (sched/node.go) gains `queueSeq uint64` — the JOBS
   stream sequence of the node's currently queued job message.
   `enqueueLocked` resets it to 0 at the top of each attempt (a failed
   publish must not leave a stale seq from the previous gen) and sets it
   from the `jetstream.PubAck.Sequence` on successful publish — the ack is
   currently discarded.

2. **Stream handle.** `Sched` gains a `jobs jetstream.Stream` field,
   obtained once in `New` right after the JOBS `CreateOrUpdateStream`
   (`DeleteMsg` lives on the `Stream` handle, not on `jetstream.JetStream`).

3. **Purge point.** In `dropInterestLocked`'s existing loop over evicted
   nodes: if `n.queueSeq != 0` and `n.phase` is `queued` or `running`,
   call `s.jobs.DeleteMsg(s.ctx, n.queueSeq)`. Errors are logged at debug
   and ignored — "no message found" is the normal case for a message a
   runner already acked. This one hook covers `Cancel`, `Delete`, and
   watcher-loss eviction identically; nodes shared with a live request are
   never evicted (interest > 0), so their messages are never purged.

   Including `running`-phase nodes is deliberate: their message is
   delivered but unacked, and deleting it prevents the work queue
   redelivering a cancelled attempt to another runner after `AckWait` if
   the first runner dies mid-run. It does not stop the running attempt.

   The call happens under `Sched.mu`, matching `PublishMsg` in
   `enqueueLocked` — NATS is in-process, the round-trip is cheap.

4. **Error handling.** The purge never fails the cancel. The stale-result
   drop in results.go stays the correctness backstop; the "wasteful but
   never wrong" invariant is unchanged — this only reduces the waste.

## Testing

In sched_test.go (existing embedded-NATS harness):

1. Submit a request whose job lands in a lane no runner consumes; cancel;
   assert the JOBS stream message count drops to 0.
2. Two requests sharing the same target; cancel one; assert the job
   message is still in the stream.
3. Cancel after the node's result already folded (message acked); assert
   cancel succeeds with no surfaced error.

## Documentation

- Update the `Cancel` and `dropInterestLocked` comments (both currently
  document "queued work is NOT chased").
- architecture.md "Node graph" paragraph: add that eviction best-effort
  deletes the node's queued job message from the JOBS stream.
