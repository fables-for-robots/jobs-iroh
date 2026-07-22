# jobs: observability + wire surfaces — subsystem map for the jobs-iroh port

Scope: `events/` (schema, Emitter, Job/OutputWriter), `wire/` (runner↔engine framing),
`state/` (status derivation), `tailbuf/`, `sched/httpapi` (client wire types incl.
`enginewire.go`), `sched/msg` (the post-cutover actor message set — the *actual* current
wire vocabulary), `sched/buildactor` (log store + snapshot assembly), `console/viewtypes.go`
(SPA view contract), `architecture/events.md`. All paths relative to
`/home/dragan/fables-for-robots/jobs`.

**Historical layering warning.** The repo carries TWO generations of observe/wire surface:

1. **Engine-era** (collector pipeline, WS runner protocol): `events/`, `wire/`,
   `architecture/events.md` — the collector/console read-model was deleted at the
   2026-07-17 actor cutover (`architecture/events.md:1-6`), but `events/` + `wire/` survive
   as leaf deps of the runner stage drivers and the CLI.
2. **Sched-era** (current production): `sched/msg` CBOR messages over goakt remoting,
   `sched/httpapi` HTTP/SSE client surface, `sched/buildactor` in-memory build views.

jobs-iroh should take the **event vocabulary and capture machinery from generation 1**
(it is richer and fully NATS-shaped) and the **client/watch surface from generation 2**
(Snapshot/cursor is exactly a KV-watch pattern).

---

## 1. Build-event schema (`events/event.go`)

### Envelope

```go
type Event struct {            // events/event.go:19-29
    Emitter string             // "engine-0" | runner name
    Boot    string             // 8 random bytes hex, per process (emitter.go:113-117)
    Seq     uint64             // monotonic per process
    TS      int64              // unix NANOseconds (never time.Time — event.go:15-18)
    Type    string
    Request string             // optional
    Node    string             // optional; "<kind>|<hexK>"
    Runner  string             // optional
    Data    map[string]any     // optional
}
```

- **`(Emitter, Boot, Seq)` is the global dedup identity** (`events/event.go:4-6`,
  `architecture/events.md:26-29`). Boot is random per process so no counter must survive
  restarts; redelivery is idempotent ("twice is never wrong").
- Encoding: **plain (non-canonical) CBOR** — events are not identity-bearing
  (`events/event.go:16-18`). Decode uses `DefaultMapType: map[string]any` so `Data` stays
  JSON-marshalable (`events/event.go:81-88`). Batch + single codecs at
  `events/event.go:92-110`.

### Engine-emitted types (`events/event.go:32-52`)

| Type | Keys set | Data |
|---|---|---|
| `request.received` | Request+Node(target) | submit summary: `kind`, `fetcher` \| `platform`, `dir`, `tree` (events.md:59-72) |
| `request.node` | Request+Node | `label`, `platform` — the request↔node join, back-filled on late mappings (events.md:74-86) |
| `request.done` | Request+Node(target) | — |
| `request.failed` | Request+Node(target) | `status` (FAILED/INFRA_FAILED/FAILED_UPSTREAM), `err` (events.md:95-102) |
| `request.cancelled` | Request+Node(target) | — ; terminal for watchers (events.md:188-194) |
| `node.registered` | Node | — (deduped, once per K) |
| `node.offered` | Node+Runner | `claim` |
| `node.started` | Node+Runner | — (runner acked) |
| `node.done` | Node | `cached` bool — doneness from a pre-existing output ref (events.md:126-134) |
| `node.failed` | Node+Runner | `class` (hard/retryable), `exit`, `phase`, `stderr` tail (events.md:136-144) |
| `node.declined` | Node+Runner | `reason`, `fetcher`, `platform` |
| `node.lease-expired` | Node+Runner | — |
| `node.named` | Node | `name` (recipe-declared display name; re-emitted on cache-hit joins, events.md:179-186) |
| `node.queued` | Node | `reason`(capacity), `cpu_milli`, `mem_bytes`, best-fit `runner`, `free_cpu_milli`, `free_mem_bytes`, `running` []string; **debounced ~1/min per node** (event.go:45, events.md:163-176) |
| `node.cancelled` | Node | `reason`: `target-cancelled` \| `upstream-failed` (events.md:196-204) |
| `node.revived` | Node | — (resubmit re-armed a cancelled target, event.go:49) |
| `node.offer-dropped` | Node+Runner | `claim` — offer send failed, placement rolled back (event.go:51) |

### Runner-emitted types (`events/event.go:55-65`); every one also carries `data.requests` (job.go:29-31)

| Type | Data |
|---|---|
| `exec.started` | `kind`, `requests` |
| `exec.phase` | `phase`. Import: pulling→resolving→fetching→ingesting→pushing; build: assembling→(seeding)→materializing→building→finalizing→pushing (events.md:217-222) |
| `exec.progress` | `phase`, `objects`, `bytes` **cumulative for the job** (pull); `objects`+`total` (push, bytes unknown live). Throttled ≤1/s + one final settled event (events.md:229-232, job.go:41-46,110-115) |
| `exec.output` | `stream`, `phase`, `chunk` ([]byte), `offset` (outputwriter.go:99-101) |
| `exec.output-truncated` | `stream`, `cap`, `written` (outputwriter.go:58-60) |
| `exec.finished` | `outcome`: completed/failed/declined/cancelled, `exit` (job.go:48-51) |
| `exec.cache` | `cache`, `stage`: seeded/finalized, `ms`, `bytes`, `files`, `cold` \| `result`: updated/unchanged/empty (job.go:56-67) |
| `exec.heartbeat` | `phase`, `cpu_ms` (cumulative cgroup), `mem_bytes` (current), `mem_peak_bytes` (kernel high-water); **negative reading ⇒ key omitted, event still fires as pure liveness**; every **5 s** while building/fetching + one final settled event (job.go:69-87, runner/execheartbeat.go:12, events.md:239-248) |
| `exec.cas` | `stored_bytes`, `stored_objects`, `stored_deduped`; when pushed: `pushed_bytes`, `pushed_objects`, `push_total_objects` (job.go:89-108) |

Meta: `emitter.dropped` `{count}` — self-reported gap after drop-oldest overflow, prepended
to the next batch (`events/event.go:68`, emitter.go:482-490).

---

## 2. Emitter — at-least-once delivery semantics (`events/emitter.go`)

- **`Emit` never blocks** (no disk, no network — appends to a bounded in-memory queue;
  the engine emits under its global mutex, emitter.go:186-198). Nil-safe throughout:
  `SinkURL==""` ⇒ `New` returns nil and every method no-ops (emitter.go:84-86).
- **Drop-OLDEST on overflow**, counted, surfaced as one `emitter.dropped` gap event
  (emitter.go:190-196, 478-503). Bounds: 32768 events / 64 MiB queue; batches ≤256 events /
  1 MiB; 200 ms linger (emitter.go:34-38, 89-102).
- Delivery: `POST SinkURL+"/ingest"`, `Content-Type: application/cbor`, optional bearer
  (emitter.go:544-568). **Capped backoff 500 ms → ×2 → 15 s cap, retries forever while
  open** (emitter.go:510-542). Re-POSTs after partial failure are harmless — collector
  dedups on (Emitter,Boot,Seq) (emitter.go:47-54).
- **Durable spool mode** (`Config.SpoolDir`, emitter.go:22-29): a stager goroutine moves
  staged events to a dedicated Pebble DB keyed `q/<8-byte BE index>` (spool.go:11-19,34-39);
  appends are NoSync, one **WAL-sync barrier per batch** bounds power-crash loss to ~one
  linger (spool.go:80,92-101, emitter.go:440-443); entries deleted **only after collector
  2xx-ack** (spool.go:139-153); leftovers (incl. prior boots') deliver first on next open
  (spool.go:41-63). Unbounded by design; backlog warning escalates 10× from 10k
  (emitter.go:80-82, 385-396). A broken spool degrades to in-memory, never takes the
  service down (emitter.go:136-143).
- `Close(ctx)`: default 5 s deadline; aborts in-flight POST at deadline; spool mode keeps
  the rest on disk (emitter.go:214-243).

**jobs-iroh mapping:** JetStream *is* this machinery. `Nats-Msg-Id = "<emitter>:<boot>:<seq>"`
gives the dedup identity for free; JetStream persistence replaces the spool; publish-retry
with backoff replaces `deliver`. Keep the never-block-a-job invariant: publish events on a
buffered channel drained by a goroutine, drop-oldest + gap event when the drain stalls.

---

## 3. Job scope + OutputWriter — stdout/stderr capture (`events/job.go`, `events/outputwriter.go`)

- `Emitter.Job(node, runner, requests)` returns a per-execution scope; nil emitter ⇒ nil
  Job; every method nil-safe so stage-driver call sites stay unconditional (job.go:3-20).
  `emit` stamps Node/Runner and injects `data.requests` (job.go:22-33).
- `Job.Output(stream, phase) *OutputWriter` — returns **typed nil** on nil Job; callers
  must guard `if ow := j.Output(...); ow != nil { spec.Sink = ow; defer ow.Close() }`
  (job.go:117-126).
- **OutputWriter coalescing constants** (outputwriter.go:8-17):
  - ≤ **32 KiB** per `exec.output` chunk event;
  - flush at **64 KiB** buffered or **100 ms** after first unflushed byte;
  - **16 MiB per-stream per-job cap** (`DefaultOutputCap`) → one `exec.output-truncated`
    then silent; `offset` = total bytes emitted so far (outputwriter.go:45-71, 90-105).
  - **`Write` always returns `len(p), nil`** — capture must never fail the captured
    process (outputwriter.go:43-45).
- Tee points in the stage drivers (all still live, all nil-safe):
  - imports: stdout+stderr at phase "fetching" (runner/importjob.go:192-197);
  - build script: stdout+stderr at phase "building" (runner/buildrun.go:97-102);
  - plugin subprocess: stderr at phase "plugin" (runner/buildeval.go:29,137);
  - phase marks + heartbeats inside the executor (runner/buildexec_linux.go:118-122, 279-283).
- `tailbuf.Buffer` is the separate, bounded stderr-tail for *error summaries* (the
  `node.failed` `stderr` field / `wire.Failed.StderrTail`): retains last max bytes, Write
  never fails (tailbuf/tailbuf.go:10-33). Independent of the event stream — keep both.

**⚠️ Current production gap (matters for jobs-iroh):** the sched-era runner passes **nil**
`events.Job` into every stage driver (`sched/session/execcore.go:146` —
`runner.RunImport(..., nil, nil)`; `buildRunCfg` never sets `Events`, execcore.go:182-198),
and nothing in production emits `msg.AttemptOutput`/`msg.AttemptProgress` (the only
producer is the test fake, `sched/session/fake.go:130`). Live log/phase/heartbeat capture
is **dark** on the production path today; the receiving side (buildactor Buffer) and the
producing side (events.Job/OutputWriter) both exist but are not connected. jobs-iroh's
core-NATS log subjects are precisely the reconnect: wire an `events.Job`-compatible sink
that publishes to NATS instead of HTTP-POSTing to a collector.

---

## 4. `wire/` — the engine-era runner protocol (framing reality check)

**It is NOT length-delimited.** `wire/` encodes one tagged-union CBOR message per
**gorilla/websocket binary message** — the WS layer supplies the framing
(`wire/wire.go:1-4`). There is no length-prefix codec anywhere in `wire/` (verified: no
`PutUint32`/`ReadFull` in the package). For iroh QUIC streams (raw byte streams) jobs-iroh
must add its own framing: either a `uint32` length prefix per CBOR message, or rely on
CBOR's self-delimiting property with a streaming `cbor.Decoder` per direction. Precedent
in-house: the nats-iroh tunnel writes a 1-byte stream preamble so the acceptor sees the
stream at all (`/home/dragan/fables-for-robots/nats-iroh/nats_iroh_test.go:20-24,40`).

Union shape (reusable pattern): numeric `Type` + one pointer field per variant,
**append-only wire numbers** (`wire/wire.go:164-210`); `sched/msg`'s alternative is a
string-tagged envelope `{T, B}` (`sched/msg/codec.go:18-21,31-41`) — more self-describing,
same additive-evolution rule. Either works over iroh; the string-tag envelope is easier to
debug and is what the current message set already uses.

Message inventory (semantics worth porting even where NATS replaces the transport):

- `Register`: runner id, platforms, tags, network, **CPUMilli/MemBytes advertised capacity;
  zero ⇒ legacy single-slot** (wire.go:13-26).
- `Offer`: node, lease id + expiry, `Requests` (informational stamping — the engine's
  request.node mapping stays authoritative, wire.go:60-64), **engine-resolved CPUMilli/
  MemBytes requirement** — runner enforces MemBytes as sandbox memory.max, CPU
  reservation-only (wire.go:56-73).
- `Ack`/`Decline{reason}`/`Heartbeat{node,phase,bytes}`/`Completed{node,out}`/
  `Failed{node,class,exit,phase,stderrTail}` (wire.go:28-54).
- `WriteRefs{msgID, node, refs[{name,key,label}]}` → `RefsWritten{msgID, error, code}`;
  `Label` rides only on `build-pinned:F` to drive `node.named` (wire.go:108-130).
  **`code="not-assigned"` = duplicate-execution loser ⇒ drop silently, never a job failure**
  (wire.go:132-136).
- `Welcome`: runner name, central URL/key, capability grant, **event sink URL/token relay**
  (build logs never cross the scheduler socket — events.md:343-347), wipe epoch
  (wire.go:85-100). `Wipe{epoch}` (wire.go:104-106).
- `ProcListReq`/`ProcList` — on-demand process snapshot of a running job's cgroup
  (wire.go:140-162); nice-to-have for the admin TUI.

---

## 5. `state/` — status derivation

- `NodeID` = `"<kind>|<hexK>"`; kinds import/build/build-from/build-plugin-resolve/build-pin
  (state/model.go:12-27). **Port cut-point: `state/model.go:9` imports
  `github.com/draganm/amber-store/key`; `NodeID.K` is `key.Key` and `ParseNodeID` validates
  against `len(key.Key{})` (model.go:136-148).** Re-base on amber-store-core's key type.
- Statuses: `DONE, INFRA_FAILED, FAILED_UPSTREAM, FAILED, RUNNING, ELIGIBLE, WAITING`
  (model.go:31-39). Attempt classes: `hard`, `retryable`, `control` — control = a
  control-plane verdict (RefsWritten timeout, lease expiry) that says nothing about the job
  (model.go:43-53).
- `Attempt{Node, Gen, AttemptID, Class, TS, Runner, ErrSummary}` is a G-Set element
  (model.go:55-64); `Claim` is the OR-Set lease element `"<node>#<gen>"` with `Acked`
  (unacked lapse = dropped offer, not an attempt — model.go:91-107).
- **`Derive` precedence** (status.go:174-207), config `{MaxAttempts:3, RetryableCap:5,
  ControlCap:20}` (status.go:19):
  1. output ref exists ⇒ `DONE`
  2. ≥5 consecutive retryables at current gen ⇒ `INFRA_FAILED` (control attempts are
     invisible to the run — neither count nor break, status.go:100-108)
  3. ≥20 consecutive control attempts ⇒ `INFRA_FAILED`
  4. any dep derives Failed/InfraFailed/FailedUpstream ⇒ `FAILED_UPSTREAM` (recursive)
  5. ≥3 hard attempts at gen ⇒ `FAILED`
  6. live claim at (node,gen) ⇒ `RUNNING`
  7. all deps done ⇒ `ELIGIBLE`, else `WAITING`
- **Request-level status = Derive(target)**; the target derives WAITING for its whole
  pipeline, which is why the watch surface carries closure Progress counts
  (enginewire.go:75-93). Cancellation is a scheduling overlay, never graph truth
  (CLAUDE.md; ORSet.KnownDead is the durable Cancelled marker, state/crdt.go:119-135).
- Memoized `Deriver` makes a full-graph sweep O(nodes+edges) — load-bearing at ~3k-node
  fan-outs (status.go:139-170). CRDT primitives (GSet/ORSet add-wins/MaxRegister) in
  state/crdt.go:15-187; Pebble persistence with NoSync puts + periodic sync barrier,
  reconstructible from amber refs (state/pebble.go:13-31).

**jobs-iroh note:** with one server and a JetStream WQ, claims/leases/gen bookkeeping
collapse into JetStream ack semantics (AckWait = lease, `InProgress` = heartbeat-extend,
redelivery = lease expiry, `MaxDeliver`/consumer backoff ≈ RetryableCap). Keep the *derived
doneness* rule (output ref exists in the CAS ⇒ DONE) and the class taxonomy
(hard/retryable/control) — they are transport-independent semantics.

---

## 6. Current client surface — `sched/httpapi` (the jobs-build/1.0 reference)

Endpoints (httpapi.go:80-97): `POST /submit`, `POST /submit-build`, `GET /builds`,
`GET /builds/{id}`, `GET /builds/{id}/events` (SSE), `GET /builds/{id}/logs/{node}?stream=`,
`POST /requests/{id}/cancel`, `DELETE /builds/{id}`, `GET /api/runners`, `GET /healthz`.
Plus, mounted by the binary: `POST /client-grants` (keyless client handshake,
internal/jobscli/schedserver.go:259-288), `POST /wipe` (schedserver.go:300),
`/runner-tokens*` admin (schedserver.go:198-256).

### Submit types (httpapi.go:30-69)

```go
SubmitRequest       { fetcher, params(json.RawMessage), requiredTags?, platform }
ImportSpec          { fetcher, params?, requiredTags?, tree? }   // tree = remote-build pushed source
SubmitBuildRequest  { source(ImportSpec), dir?, platform, params?, buildJobs?, buildFile?, resources?{cpu,memory} }
SubmitResponse      { k, request_id, status_url, events_url }    // 202 Accepted
```

Submit flow: canonicalize def → ingest locally → push to central → sign bookkeeping ref
(`import:K` / `build:K`) → spawn the request's build actor (httpapi.go:139-181, 250-294).
**Submit REQUIRES central up** (httpapi.go:137-139). Tree-source builds publish the
engine-signed `build-from-tree:T` ref first; a completeness rejection means the client's
upload is missing (httpapi.go:197-219).

### Watch: SSE of `msg.Snapshot` (httpapi.go:331-372)

`GET /builds/{id}/events` polls the build actor every 250 ms with a cursor; emits
`event: snapshot` frames only when `snap.Cursor` changed; **closes on terminal phase**
(done/failed/cancelled); `event: gone` when the actor vanished. The CLI consumes exactly
this (internal/jobscli/schedwatch.go:51-109), probing `/builds/{id}` to detect a sched
server (schedwatch.go:34-49).

```go
// sched/msg/msg.go:203-228
GetSnapshot { Cursor int64 }        // cursor==current ⇒ long-poll until change
Snapshot    { Host, RequestID, Phase("running"|"done"|"failed"|"cancelled"),
              Cursor, Nodes []NodeSnap, Edges [][2]string, CreatedUnixMilli }
NodeSnap    { Node, Phase, Class, Summary, Runner, Name, F []byte,
              AttemptGen, TSUnixMilli, OutputBytes map[string]int64 /* "stream@gen" → bytes */ }
```

Node phases: `waiting|placing|running|publishing|done|failed` (msg.go:39) plus **derived
`failed-upstream`** computed over the build's own edges at snapshot assembly
(buildactor.go:446-521). Node names on this surface are grain names `"<kind>_<hexkey>"`
with kinds `import|buildvalue|buildfrom|pluginresolve|pin|buildrun` (sched/ids/ids.go:20-48)
— note: **different from the event-era `"<kind>|<hexK>"`**. Doneness per kind =
`NodeRef.OutputRef()` existence: `import-output:K`, `build-from:K`,
`build-plugin-resolved:F`, `build-pinned:F`, `build-output:F`; buildvalue is the two-hop
check (ids.go:82-101).

### Logs: bounded head+gap+tail (httpapi.go:374-395)

`OutputQuery{node, stream}` → `OutputReply{Head, Tail, Gap, OK}` (msg.go:231-242); HTTP
renders `head + "--- N bytes omitted ---" + tail`. Backing store: per-(node, attempt-gen,
stream) `buildactor.Buffer` — **head fills to 1 MiB, overflow rings through a 3 MiB tail,
squeezed-out bytes counted as gap; idempotent per chunk Seq** (buildactor/output.go:10-51,
system/config.go:58-59). Newest gen wins at query (buildactor.go:524-540). Chunks arrive as
`msg.AttemptOutput{Node, AttemptGen, Stream, Chunk, Seq}` relayed session→grain→build
actors (msg.go:82-88, gateway/session.go:146, grains/skeleton.go:231). Logs **die with the
build actor** (`DeleteBuild`, msg.go:248).

### Legacy client types still compiled in — `sched/httpapi/enginewire.go`

Kept verbatim for the CLI; the old `GET /status?target=` SSE **is no longer served by any
binary** (no handler registers it), but `jobs remote-build` still carries the consumer as a
fallback path (remotebuild.go:292-300, 315-372):

```go
ClientGrantRequest  { pubKey []byte }                              // enginewire.go:10-12
ClientGrantResponse { centralUrl, centralServerKey, grant, expiresAt }  // :17-22
NodeStatus  { node, status(state.Status), err?, progress?, cancelled? } // :29-38
RunningNode { node, label, platform?, runner?, elapsedMs }         // :43-49  — ElapsedMs computed server-side (clock-skew-proof)
QueuedNode  { node, label, platform?, cpuMilli, memBytes, runner?, freeCpuMilli, freeMemBytes, running? } // :55-65
Progress    { total, done, running, failed, waiting, running_nodes?, queued_nodes? } // :81-93
```

`Progress.Total` grows as unfolding discovers inputs — deliberately: it is the watch-side
evidence a WAITING target is moving (enginewire.go:75-80). `RunningNodes` longest-first;
`QueuedNodes` explain a frozen-looking build (enginewire.go:84-92). **These are the UX
semantics jobs-build/1.0 should preserve even if it ships Snapshot-shaped payloads.**

### Fleet: `GET /api/runners` (httpapi.go:428-448)

Aggregates `msg.BrokerStatsReply{Platform, Sessions []DirSessionEntry{Session, Capacity,
Committed, Attempts}, QueuedDefault, QueuedControl}` (msg.go:330-343) into
`{"brokers": [...]}`; the console adapts it to totals + per-runner cards
(console/schedmode.go:141-201).

---

## 7. Console view contract — `console/viewtypes.go` (what an admin TUI wants)

The SPA renders a server-assembled `BuildView`; the sched-mode adapter
(console/schedmode.go:14-21) documents the **known v1 degradations**: empty waterfall
timeline (snapshots carry only last-transition timestamps), no usage/cache sublines,
bounded head+tail logs. The *full* contract — what the richer collector pipeline used to
fill and what jobs-iroh can restore by keeping the exec.* vocabulary:

- `BuildSummary` list row: id, target, name, platform, status, createdTs, terminalTs, err,
  nodeCount, running, failed (viewtypes.go:169-182).
- `BuildView { id, target, name, platform, status, createdTs, terminalTs, err,
  nodes []NodeState, timeline }` (viewtypes.go:213-224).
- `NodeState` (viewtypes.go:131-167): node, kind, label, **name** (node.named), platform,
  runner, status, startTs/endTs, `phases []PhaseMark` (exec.phase marks; segment ends
  derived at read), `progress *Progress` (exec.progress latest), `usage *Usage`
  (exec.heartbeat latest; `MaxMemBytes` monotone per attempt, ≤0 = unknown,
  viewtypes.go:44-58), `queued *QueuedInfo` (node.queued latest, cleared on offer/start/
  terminal, viewtypes.go:60-76), `caches []CacheStat` (exec.cache pairs,
  viewtypes.go:89-104), `cas *CASStat` (exec.cas, viewtypes.go:106-119), `failure *Failure`
  (class/exit/phase/stderr/reason, viewtypes.go:78-87), `outputStreams` (which log links
  resolve), `cached` (reused output — same shared node is fresh in one build, cached in
  later joiners, viewtypes.go:160-166), `Boot` selects the newest attempt's output
  (viewtypes.go:156).
- Statuses: build `running|done|failed|cancelled`; node `pending|running|done|cached|
  failed|declined|cancelled` + view-only `stale` (running whose last event outlived
  staleAfter — heartbeats every ~5 s make silence meaningful, viewtypes.go:8-31).
- `Timeline` waterfall: per-node relative-ms tracks with phase segments + per-phase totals
  (viewtypes.go:184-208).
- Phase→status mapping for sched phases: schedmode.go:363-381 (`publishing` counts as
  running; `initializing|waiting|placing|needdef` ⇒ pending).

An admin TUI over jobs-admin/1.0 needs, minimally: the build list (DirEntry rows:
requestID, name, phase, createdUnixMilli — msg.go:269-274), one build's Snapshot (+ a
change-watch), per-node logs (head/gap/tail + live follow), fleet stats, cancel + delete.
Everything beyond that (usage/caches/CAS/queued/waterfall) falls out of retaining the
exec.* event vocabulary on the log/status subjects.

---

## 8. draganm/amber-store leak inventory (port cut-points)

| Package | Leak | Action |
|---|---|---|
| `events/`, `wire/`, `tailbuf/`, `console/viewtypes.go`, `sched/msg` | **none** — keys travel as `[]byte`/hex strings | port as-is |
| `state/model.go:9` | `amber-store/key` — `NodeID.K key.Key`, `ParseNodeID` length check (model.go:136-148) | re-base on amber-store-core key |
| `sched/ids/ids.go` (via key import) | `NodeRef.Key key.Key`; `ParseGrainName` canonical-hex validation (ids.go:58-79); `OutputRef()` names (ids.go:82-101) | re-base; keep the fail-closed canonical-hex rule |
| `sched/httpapi/httpapi.go:17` | `key.Parse` for tree keys; `amber.IngestFile`/`Signer.SignAndPut` in submit (httpapi.go:139-149) | re-base submit path on amber-store-core |
| `sched/session/execcore.go:10-11` | `amber-store/key` + `amber-store/remotesync` (`PushTree` in captureRefWriter, execcore.go:228-242) | runner CAS push moves to jobs-runner-amber iroh streams |
| `internal/jobscli/schedserver.go:259-288` | `/client-grants` mints amber capability grants bound to the client's SSH pubkey | superseded if jobs-iroh authenticates by iroh node key on jobs-amber-admin |

---

## 9. Recommendations for jobs-iroh

### 9.1 NATS layout

All payloads CBOR (plain, not canonical — transport only, per the msg package rule,
sched/msg/msg.go:1-6). `<queue>` = the runner size class/platform queue (e.g.
`linux-arm64.c1-m2`); `<job-id>` = the request id (`"r"+8-byte-hex`, httpapi.go:99-103).

1. **`jobs.<queue>`** — JetStream **WorkQueuePolicy** stream, one consumer per queue.
   Message ≈ `msg.StartAttempt` (msg.go:138-146): node grain name, canonical def bytes
   **in-band** (the def-travels-in-band trick that removes a remote round-trip,
   execcore.go:121-139), platform, resources, attempt gen. Map lease semantics onto acks:
   `AckWait` = lease TTL, `InProgress()` = `wire.LeaseExtend`, redelivery = lease expiry,
   `MaxDeliver` + termination ≈ `RetryableCap`(5)/`MaxAttempts`(3) from status.go:19 —
   but keep the hard/retryable/control class taxonomy in the result payload since NATS
   can't distinguish them (state/model.go:43-53). `Nak(delay)` for retryable, `Term` for
   hard-cap exhaustion.
2. **`results.<queue>.<job-id>`** — **LimitsPolicy** stream. Message ≈ `msg.AttemptDone`
   (msg.go:177-190): success, class, summary (tailbuf-bounded), refs `[{name, key}]`.
   Publish with `Nats-Msg-Id = "<runner>:<boot>:<seq>"` — the (emitter,boot,seq) dedup
   identity mapped onto JetStream's dedup window, keeping at-least-once + idempotent
   ingest without collector code (events/event.go:4-6).
3. **Status KV bucket** (`status`), key `<queue>.<job-id>` — value = a `msg.Snapshot`-shaped
   assembled view (msg.go:206-228). KV revision **is** the Snapshot cursor; KV `Watch()`
   replaces both the 250 ms SSE poll (httpapi.go:341-371) and the GetSnapshot long-poll
   (msg.go:201-203). Include the derived `failed-upstream` phase server-side
   (buildactor.go:446-481) — clients never re-derive.
4. **`logs.<queue>.<job-id>.<node>.<stream>`** — **core NATS, server memory only.**
   Payload ≈ `msg.AttemptOutput{node, attemptGen, stream, chunk, seq}` (msg.go:82-88)
   produced by an `events.OutputWriter`-equivalent with the same constants: ≤32 KiB
   chunks, 64 KiB/100 ms flush, 16 MiB/stream cap + truncation marker
   (outputwriter.go:8-17). 32 KiB stays far under NATS's 1 MiB default max_payload.
   Server keeps the head(1 MiB)+tail(3 MiB) ring per (node,gen,stream) for late joiners
   (buildactor/output.go:10-51) — dedup on `seq` makes at-least-once relays safe. Live
   followers subscribe to the subject; the catch-up query returns head/gap/tail
   (msg.go:237-242).
5. Optionally **`events.<queue>.<job-id>`** (core NATS) for the full exec.*/node.* firehose
   at events.md granularity (phases, heartbeats, cache, CAS, progress) — this is what
   makes the admin TUI's usage/waterfall/cache panels possible again (§7). Runner-side
   producer = a straight port of `events.Job` (job.go:36-126) publishing to NATS.
   **This closes today's production gap where `events.Job` is nil** (execcore.go:146).

### 9.2 jobs-build/1.0 (client submit + watch, one iroh stream per operation)

Framing: `uint32`-length-prefixed CBOR frames carrying the `{T, B}` string-tagged envelope
(sched/msg/codec.go:18-41); tags are append-only schema. (wire/'s "framing" was the WS
message boundary — do not copy it verbatim; QUIC streams need the prefix.
wire/wire.go:1-4.)

- `submit` ≈ `SubmitBuildRequest`/`SubmitRequest` (httpapi.go:30-61) → `submitted` ≈
  `SubmitResponse{k, requestID}` (httpapi.go:64-69). Keep 202-then-watch semantics; keep
  "submit requires CAS reachable" (httpapi.go:137-139). Tree-source submits keep the
  objects-before-ref completeness rejection (httpapi.go:197-215).
- `watch{requestID, cursor}` → server pushes `snapshot` frames from the status-KV watch;
  stream closes after the terminal frame (mirrors httpapi.go:355-364). Carry the
  Progress-counts UX: total/done/running/failed/waiting + named running nodes +
  queued-with-reason (enginewire.go:75-93, schedwatch.go:111-144).
- `logs{requestID, node, stream, follow}` → one `loghead{head, gap, tail}` frame
  (msg.go:237-242) then, if follow, live chunk frames off the core-NATS subject.
- `cancel{requestID}` → `cancelstatus{cancelled|already-done|already-failed}`
  (httpapi.go:397-417). Idempotent; resubmit re-arms (event.go:49).

### 9.3 jobs-admin/1.0 (admin TUI)

Same framing. Frames: `list-builds` → DirEntry rows (msg.go:269-274); `get-build{id}` /
`watch-build{id, cursor}` → Snapshot frames; `logs` as above; `runners` →
BrokerStatsReply-shaped fleet (msg.go:330-343) — under one embedded NATS this reduces to
the WQ consumer states + connected runner tunnels; `cancel`/`delete-build`
(httpapi.go:397-426; delete kills logs too, msg.go:248); optionally `proclist{node}` ported
from wire.ProcListReq/ProcList (wire.go:140-162); `wipe` (schedserver.go:294-300 pattern:
central wipe → local reset → epoch bump → exit; epoch rides the runner handshake,
msg.go:291).

### 9.4 Invariants to carry over verbatim

- Events/logs are **observability, never coordination**; doneness is derived from CAS refs
  only (events.md:16-18; ids.go:82-87). A dead log pipeline may drop output but must never
  fail or stall a build.
- Output capture **never blocks and never fails the job** (outputwriter.go:43-45,
  emitter.go:46-48); drop-oldest + explicit gap events when overwhelmed.
- Everything at-least-once + idempotent: (emitter,boot,seq) / chunk `seq` / Nats-Msg-Id;
  "twice is never wrong".
- Error summaries (tailbuf) and full output (chunk stream) are **separate channels** with
  separate bounds — the summary must survive even when the log stream is truncated
  (tailbuf.go:1-4, wire.go:53).
- ElapsedMs/durations computed server-side to be clock-skew-proof (enginewire.go:43-49).
- Resources are scheduling metadata, never identity (wire.go:66-72, CLAUDE.md).
