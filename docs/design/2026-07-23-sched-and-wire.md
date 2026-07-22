# Scheduler & wire design (Milestone 4)

**Status:** implementation spec, 2026-07-23. Extends
[`2026-07-22-architecture.md`](2026-07-22-architecture.md) §3–§5 with the
concrete subjects, schemas and protocols. Scheduler semantics ground truth:
[`../research/jobs-scheduler-semantics.md`](../research/jobs-scheduler-semantics.md).

## 1. Node model

Node name = `<kind>_<hexkey>` (subject-safe; the grain grammar). Kinds:
`import` (K), `buildfrom` (K), `pluginresolve` (F), `pin` (F), `buildrun` (F) —
placeable; `buildvalue` (K) — server-internal orchestrator that walks
buildfrom → pluginresolve → pin → buildrun sequentially. Consumers depend on
`buildvalue`, never on stages.

In-memory graph: get-or-create by (kind, key) — the join. States:
`waiting → ready → queued → running → publishing → done | failed`.
**Doneness = output-ref existence**, fast-pathed at node creation — also the
crash-recovery story: on restart the server re-drives live requests and
finished subtrees short-circuit. Failure classes (ported): hard (1 strike),
retryable (budget 3, backoff 1..30s, server re-enqueues gen+1), control
(doesn't burn budget). `failed-upstream` is derived per request. FAILED is
memory-only; retry by resubmit. Cancel = request interest drop; nodes with
zero interest and not running are dropped from memory.

Deviation from jobs (accepted): runner reconnect **adoption is dropped** — an
orphaned in-flight job redelivers via the work queue after its ack deadline
lapses ("wasteful but never wrong").

## 2. NATS layout

| Piece | Config |
|---|---|
| stream `JOBS` | WorkQueuePolicy, subjects `jobs.<platform>.<class>` (platform `linux-amd64` form; class from the ladder `c0.2-m1 … c4-m16`, requirement rounded **up**), `Duplicates` window ≥ 10m. Job msg `Nats-Msg-Id: job-<node>-<gen>` |
| consumers | durable per (platform, class), `FilterSubject` exact, shared by all fitting runners; explicit ack; `AckWait` 90s; runner heartbeats `msg.InProgress()` every 5s while executing; `MaxDeliver` 25 (poison backstop); `MaxAckPending` high (slots gate concurrency, not the consumer) |
| stream `RESULTS` | LimitsPolicy, subjects `results.>` (`results.<node>`), `MaxAge` 7d, `Duplicates` 15m. Result msg `Nats-Msg-Id: result-<node>-<gen>`. **Runner publishes the result, then acks the job msg** |
| KV `status` | per-node `<node>` → `{phase, gen, runner, startedNs, errSummary}`; per-request `req.<id>` → `{phase, target, counts{waiting,running,done,failed}, cancelled}`. KV revision = watch cursor |
| core `logs.<node>` | stdout/stderr chunks `{gen, stream, seq, data}` (≤32KiB chunks, 64KiB/100ms flush, 16MiB/stream cap — events.OutputWriter constants). Fire-and-forget; the server folds `logs.>` into per-(node,gen) ring buffers (1MiB head + 3MiB tail) for late joiners |
| core `runners.hello` / `runners.<id>.hb` | hello `{id, name, platform, size, caps}` on connect; heartbeat every 5s `{inflight, freeCPUMilli, freeMemBytes}`. Server keeps the fleet snapshot in memory for admin |

## 3. Job & result payloads (CBOR)

```
Job {
  node      string   // <kind>_<hexkey>
  kind      string
  key       bytes    // K or F
  gen       uint
  def       bytes    // canonical def CBOR when kind ∈ {import, buildfrom} (K-file content)
  platform  string
  class     string   // ladder class (informational; subject already encodes it)
  resources {cpuMilli, memBytes}   // resolved requirement
  pullRefs  []string // refs whose closures the runner must pull before running
                     // (server-computed: build-pinned:F, input build-from:K/
                     //  build-output:F pairs, import-output:K, build-from-tree:T, …)
}

Result {
  node, gen, runner  string/uint/string
  class    string    // ok | hard | retryable | control | cancelled
  exit     int
  errSummary string  // tailbuf
  refs     [](name string, key bytes)  // proposed, IN ORDER (gate + batch invariants server-side)
  rusage   {wallNs, cpuNs, maxRSS}
}
```

Runner flow per job: fetch → pull `pullRefs` over `jobs-runner-amber` (by-ref
pulls; `CheckComplete` locally; miss → re-pull → hard) → write local
bookkeeping refs (`import:K`/`build:K`) so driver `ensure*` short-circuits →
run the stage driver → **push output objects** under a scratch ref
`runner-push/<runner-id>/<node>-<gen>` (stock TPush; the server deletes it
after commit) → publish `results.<node>` → ack.

Server on result: gate-check names (ported allow-table: per-kind exact names;
`build-from:K`+`build-from-tree:F` same-batch cross-check; `build-cache:<id>:
<platform>` only for pinned ids; fail closed) → `CheckComplete` every key
against the server store → write refs **in payload order** (caches →
`build-output-deps:F` → `build-output:F` last) → delete scratch ref → advance
graph → update `status` KV.

v1 trust note: the runner amber ALPN is the stock open amber server, so a
runner *could* write refs directly — the gate guards against accidents, not
malice; acceptable at the amber-store-iroh trust level (whoever knows the
endpoint ID). Follow-up: name-filter hook on the runner ALPN.

## 4. amberclient package

Importable client for the amber sync protocol (today it lives only in
amber-store-iroh's `cmd/amber` mains): dial by endpoint ID (+ optional direct
addrs), `Push(ctx, name, root, {cas, force})`, `Pull(ctx, name) (root, error)`,
`RefsList`. v1 is **single-connection** (no sharded TAttach transfer — perf
follow-up); wire = amber-store-iroh `protocol` + `wantsync` unchanged.
Consumers: jobs-runner (pull inputs / push outputs), jobs-client (push source /
pull results home), admin (refs browse via TRefList).

## 5. jobs-build/1.0 and jobs-admin/1.0

Framing: 4-byte BE length + CBOR envelope `{t: string, b: bytes}` (one request
per stream; watch/log streams stay open; server → client frames only after a
request). Payloads:

- build: `submit{sourceTree?, def?, dir, params, platform, buildFile,
  resources?}` → `submitted{requestId, k}` (tree-source submits require the
  objects already pushed via `jobs-amber-admin`; the server completeness-checks
  and publishes `build-from-tree:T` itself). `watch{requestId}` → coalesced
  `snapshot{phase, counts, nodes[{node, phase, gen, elapsedMs, errSummary}]}`
  stream until terminal. `logs{node, follow}` → `loghead/loggap/logtail` then
  `logchunk` frames. `cancel{requestId}` → `ok`.
- admin: `requests{}` → listing; `watch`/`logs` (same as build); `fleet{}` →
  runner snapshots; `stats{}` → store disk usage, ref count, uptime, queue
  depths; `refs{prefix}` → ref listing; `cancel/delete{requestId}` → `ok`.

## 6. Packages (M4)

```
sched/       graph + unfold + gate + queue publisher + results consumer + status writer
amberclient/ importable amber sync client (dial/push/pull/refs)
runnerd/     the jobs-runner daemon loop (fetch, pull, drive, push, report)
serve/       gains: sched wiring, build+admin ALPN handlers, logs fold
cmd/jobs-runner, jobs-client remote-build subcommand (M5 completes the client)
```
