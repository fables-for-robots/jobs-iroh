# Failure diagnostics: FAILURES stream + diagnose frames

**Status:** implemented, 2026-07-23 (`wire.FailureRecord` + FAILURES stream,
`sched` fold + `Diagnose`, admin `diagnose` frames, `jobs-client diagnose`;
§7 local reproduction remains open). Extends
[`2026-07-22-architecture.md`](2026-07-22-architecture.md) §4 (NATS layout) and
§5 (iroh protocols); wire/frame shapes follow
[`2026-07-23-sched-and-wire.md`](2026-07-23-sched-and-wire.md) conventions.

## 1. Problem

When a build fails, what survives today:

| Artifact | Where | Survives retry? | Survives server restart? |
|---|---|---|---|
| Attempt result (class, exit, runner, rusage, 4KiB stderr tail) | `RESULTS` stream, 7d | yes | yes — but nothing reads it back |
| Full build output | in-memory head+tail ring (`sched/logfold.go`) | **no** — newest gen resets the buffer | **no** |
| Node/request status | `status` KV | overwritten per transition | KV yes; the graph itself is rebuilt from ref existence, failed requests vanish |

Two gaps: (a) the failing attempt's captured output is destroyed by the very
retry mechanism that makes the failure terminal — after the 3-attempt budget
burns, all three logs are gone and `errSummary` (≤4KiB stderr tail) is the
only trace; (b) the per-attempt trail is durable in `RESULTS` but has no read
path — the durable consumer is the scheduler's private fold input.

## 2. Design overview

One new JetStream stream (`FAILURES`) written by the scheduler at
failure-fold time, one new admin frame pair (`diagnose`/`diagnose-reply`),
one new client command (`jobs-client diagnose`). Explicitly **not**: no new
ALPN, no runner or `wire.Job`/`wire.Result` change (the record is written and
read by the server only — no lockstep concern), no store/gate change, no
change to retry semantics.

Diagnostics are never load-bearing: writes are best-effort (warn + drop on
publish error, never block or fail the fold), sizes are bounded, retention
ages out. Losing a failure record costs observability, never correctness.

## 3. FAILURES stream

New `wire` constants:

```go
StreamFailures      = "FAILURES"
SubjectFailuresRoot = "failures" // failures.<node>

func FailuresSubject(node string) string                // "failures.<node>"
func FailureMsgID(node string, gen uint64) string       // "failure-<node>-<gen>"
```

Stream config (created in `sched.New` next to `JOBS`/`RESULTS`):

| Setting | Value | Why |
|---|---|---|
| Retention | LimitsPolicy | fold-and-forget, like `RESULTS` |
| Subjects | `failures.>` | one subject per node; gen rides in the payload |
| MaxAge | 7d | same horizon as `RESULTS` |
| MaxMsgsPerSubject | 8 | last 8 failed attempts per node; oldest discarded |
| MaxBytes | 1 GiB | whole-stream backstop |
| Duplicates | 15m | `FailureMsgID` dedups the crash-between-handle-and-ack redelivery window, same reason as `ResultMsgID` |

One record must fit one NATS message: the embedded server keeps the default
1MiB `max_payload` (`serve/serve.go:123` sets no override), so the log
snapshot is trimmed to **256KiB head + 512KiB tail** (`failureLogHeadCap`,
`failureLogTailCap`) — ~800KiB worst-case record with CBOR overhead and the
4KiB summary. The in-memory ring is bigger (1MiB+3MiB, `sched/sched.go:43`);
the trim keeps the head's *start* and the tail's *end*, folding the squeezed
bytes into the gap count.

Payload (plain wire CBOR, never hashed):

```go
// FailureRecord is one failed (or budget-burning retried) attempt, folded
// durable at the moment the scheduler decides the attempt's fate — the only
// moment the Result, the node's counters, and the gen's log ring coexist.
type FailureRecord struct {
    Node     string `cbor:"node"`
    Gen      uint64 `cbor:"gen"`
    Platform string `cbor:"platform"`

    // Origin: "result" (runner-reported class), "commit" (ok result whose
    // gate/CheckComplete/ref-write failed), "server" (no runner attempt:
    // unfold, pull-ref computation, job publish, budget exhaustion bookkeeping).
    Origin string `cbor:"origin"`
    // Disposition: "retry" (re-enqueue scheduled) | "failed" (terminal).
    Disposition string `cbor:"disposition"`

    ErrSummary   string `cbor:"errSummary"`          // the folded summary incl. prefixes ("commit: …")
    ConsecRetry  int    `cbor:"consecRetry,omitempty"`
    ConsecCtrl   int    `cbor:"consecControl,omitempty"`
    BackoffMs    int64  `cbor:"backoffMs,omitempty"` // when Disposition == "retry"
    RequestIDs   []string `cbor:"requestIds,omitempty"` // interest snapshot at fold time

    // Result is the runner's verbatim report when Origin != "server"
    // (includes runner ID, class, exit, rusage, and — for commit failures —
    // the proposed ref batch, diagnostic gold for gate rejections).
    Result *Result `cbor:"result,omitempty"`

    EnqueuedNs int64 `cbor:"enqueuedNs,omitempty"`
    StartedNs  int64 `cbor:"startedNs,omitempty"`
    FailedNs   int64 `cbor:"failedNs"`

    // Trimmed snapshot of the gen's log ring (empty + LogMissing when the
    // ring was LRU-evicted or the gen never produced output).
    LogHead    []byte `cbor:"logHead,omitempty"`
    LogGap     int64  `cbor:"logGap,omitempty"`
    LogTail    []byte `cbor:"logTail,omitempty"`
    LogMissing bool   `cbor:"logMissing,omitempty"`
}
```

Write points — all in `sched`, all already under `s.mu` (publishing under the
lock over the in-process conn is house practice, cf. `enqueueLocked`):

- `retryLocked` (`sched/results.go:156`) — every retryable-class retry and
  both budget exhaustions ("retry budget exhausted", "control budget
  exhausted"). Control-class re-enqueues **are** recorded (a persistent pull
  failure is an infra signal; the 20-cap and MaxMsgsPerSubject bound the
  volume). Cancelled-class re-enqueues are **not** (runner-shutdown noise
  says nothing about the job).
- `failNodeLocked` (`sched/node.go:382`) — hard class, gate rejections,
  unknown class, and the server-side failures (unfold errors, pull-ref
  computation, "missing after done") that today reach only slog. These carry
  `Origin: "server"` and no `Result`.

The log snapshot is taken only when `s.logs[node].gen` matches the failing
gen (newest-gen-wins makes a stale snapshot worse than none). Snapshot-then-
publish happens in the same locked section as the status-KV put, so a record
is folded before the next gen's first chunk can reset the ring.

## 4. diagnose frames (jobs-admin/1.0)

New frame types in `api`: `TDiagnose = "diagnose"` (client → server),
`TDiagnoseReply = "diagnose-reply"`. Admin ALPN only; `jobs-build/1.0` is
unchanged.

```go
// DiagnoseRequest fetches the failure trail. Exactly one of RequestID/Node.
type DiagnoseRequest struct {
    RequestID   string `cbor:"requestId,omitempty"`
    Node        string `cbor:"node,omitempty"`
    MaxAttempts int    `cbor:"maxAttempts,omitempty"` // per node, newest first; default 4
    MaxLogBytes int64  `cbor:"maxLogBytes,omitempty"` // per attempt head+tail cap; default unlimited (record is already bounded)
}

type DiagnoseReply struct {
    RequestID string          `cbor:"requestId,omitempty"` // set for by-request queries
    Phase     string          `cbor:"phase,omitempty"`     // request phase from status KV, when known
    Counts    wire.Counts     `cbor:"counts,omitempty"`
    Nodes     []NodeDiagnosis `cbor:"nodes,omitempty"`
    Truncated bool            `cbor:"truncated,omitempty"` // node list hit diagnoseMaxNodes
}

type NodeDiagnosis struct {
    Node     string          `cbor:"node"`
    Kind     string          `cbor:"kind"`
    Platform string          `cbor:"platform"`
    Phase    string          `cbor:"phase,omitempty"` // current, when the node is live in memory
    Gen      uint64          `cbor:"gen,omitempty"`
    Attempts []AttemptReport `cbor:"attempts"` // newest first
}

// AttemptReport is one FailureRecord rendered for the client: the record's
// fields verbatim, plus LogTruncated when MaxLogBytes cut the snapshot.
type AttemptReport struct {
    wire.FailureRecord
    LogTruncated bool `cbor:"logTruncated,omitempty"`
}
```

Resolution, server side:

- **By node**: read `failures.<node>` with an ordered consumer — bounded at
  ≤ MaxMsgsPerSubject messages by construction — and return the newest
  `MaxAttempts`.
- **By request**: while the request is live, the root-failure set comes from
  the in-memory graph (nodes in `failed`, excluding derived
  `failed-upstream`). After a server restart the graph is gone; the fallback
  scans `FAILURES` filtering on `RequestIDs` — the stream is bounded (7d /
  1GiB / 8-per-subject), v1 accepts the scan. This is what makes
  diagnose-by-request restart-proof, and it is the reason `RequestIDs` is in
  the record.
- Reply budget: `MaxFrame` is 16MiB; worst case 4 attempts × ~800KiB ≈
  3.2MiB per node, so the server caps `diagnoseMaxNodes = 4` per reply and
  sets `Truncated` rather than overflowing — fail closed, tell the client.

## 5. jobs-client diagnose

```
jobs-client diagnose --server <id> (--request <id> | --node <name>)
                     [--attempts N] [--json] [--logs-dir <dir>]
```

- Default: human-readable report — per failing node, an attempt table (gen,
  origin, class, exit, disposition, backoff, runner, timing) followed by the
  newest attempt's log tail.
- `--json`: the full reply as one JSON object on stdout — the agent
  interface; no TTY, no paging, one parseable blob. Log bytes render as
  strings (build output is text in practice; invalid UTF-8 is replaced).
- `--logs-dir`: additionally write each attempt's head/tail verbatim to
  `<dir>/<node>.<gen>.{head,tail}` for byte-exact inspection.

## 6. Non-goals

- **Full-fidelity durable logs.** `logs.>` stays core-NATS and the ring stays
  in-memory ("outputs in server memory only", architecture §4). This design
  snapshots ≤768KiB per *failed* attempt at fold time; successful attempts
  are not recorded (their trail already lives in `RESULTS` for 7d).
- **Failure refs in the amber store.** A `failure:<node>` namespace would
  extend the gate, complicate "doneness = ref existence" adjacency, and need
  a GC story — JetStream limits-retention does all three in one config
  struct.
- **A sixth ALPN.** Same endpoint, same open-access story; `jobs-admin/1.0`
  frames are the established surface.
- **Retry-semantics changes.** The budget, backoff, and classes are
  untouched; this is a tap on the existing fold.

## 7. Follow-up (separate spec): local reproduction

Because identity is canonical CBOR and inputs are content-addressed, a failed
remote node is exactly reproducible locally: `jobs-client develop --server
<id> --node <name>` would pull the node's inputs (K-kind defs travel in the
job; F-stage inputs are the server-computed `PullRefs`) over
`jobs-amber-admin/1.0` and re-create the sandbox via the existing
`developDriver`. The diagnose reply already carries everything needed to
derive the pull set; sketch deliberately deferred until the diagnose surface
exists.
