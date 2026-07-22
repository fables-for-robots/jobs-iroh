# jobs-iroh architecture

**Status:** draft for implementation, 2026-07-22.
**Inputs:** the subsystem maps in [`../research/`](../research/) (produced from
`jobs`, `amber-store-core`, `amber-store-iroh`, `nats-iroh`). File:line citations
below refer into those trees.

jobs-iroh is a **way simpler, non-distributed** jobs: one server, N runners, a
client — connected only by iroh QUIC. No k8s, no HTTP, no WebSockets, no CRDTs,
no gossip, no signing keys, no grants, no collector/console services. The build
model (Starlark recipes, canonical-CBOR identity, hermetic sandbox, content-
addressed store, self-bootstrapping fetchers/shell) is ported from jobs intact —
all `github.com/jobs-build/examples` must stay buildable.

## 1. The three binaries

### jobs-server

One binary, one iroh endpoint (one identity key, generated on first run like
amber-serve's `server.key`), five ALPNs dispatched by go-iroh's `iroh.Router`
(per-ALPN `ProtocolHandler`, go-iroh `iroh/router.go`):

| ALPN | Who | What |
|---|---|---|
| `jobs-build/1.0` | client | submit a build, watch progress, fetch logs, cancel |
| `jobs-runner-nats/1.0` | runner | NATS tunnel: every accepted stream proxies to `nats.Server.InProcessConn()` (nats-iroh pattern, incl. the `0x00` stream preamble — NATS server speaks first) |
| `jobs-runner-amber/1.0` | runner | CAS object/ref sync — amber-store-iroh `server.HandleStream` mounted per-stream |
| `jobs-admin/1.0` | client (TUI) | observe builds/statuses, server stats (disk, refs browse), runner fleet, cancel |
| `jobs-amber-admin/1.0` | client | push/pull amber refs (source trees up, outputs home) — same `server.HandleStream` |

Internally:

- **Embedded amber-store-core**: `packstore.Open(dir)` + `refstore.Open(dir)`
  under `--data-dir/store`. Single-process by flock — the server's store is the
  *only* shared store; runners/clients each own theirs and sync over iroh.
- **Embedded NATS** with JetStream (`server.Options{DontListen: true, JetStream:
  true, StoreDir: --data-dir/nats}`). No TCP listener; the only ways in are
  in-process (`InProcessConn`) and the iroh tunnel.
- **Scheduler**: plain goroutines + the NATS layout of §4. No actors.
- Logs (slog): runner connect/disconnect, client submits, stage start/finish/fail.

### jobs-runner

`jobs-runner --server <endpoint-id> [--addr host:port] --size c1-m2 --data-dir …`

- Dials the server twice: once on `jobs-runner-nats/1.0` (NATS client via
  `nats.SetCustomDialer` opening iroh streams), once on `jobs-runner-amber/1.0`
  (CAS sync).
- Owns a private local store (embedded amber-store-core) used as materialization
  cache; pulls missing inputs from the server store before a job, pushes output
  objects back before publishing the result.
- Runs the five ported stage drivers inside the ported namespace sandbox.
- `--size` from the ladder: `c0.2-m1 c1-m1 c1-m2 c1-m4 c2-m4 c2-m8 c2-m16 c4-m8
  c4-m16` (`cN` cores → millicpu, `mN` GiB). Default: auto-detect, floored to
  the largest ladder size that fits (jobs' capacity detect, `runner/capacity.go:38-74`).

### jobs-client

`jobs-client <run|develop|image|build|status|admin> …`

- **Local builds** (`run --source`, `develop`, `image`) are performed by the
  *same composition as the server*, scoped locally: embedded store + embedded
  NATS (DontListen) + an in-process runner loop consuming the same work queue.
  One scheduler implementation serves both; watching a local build and a remote
  build is the same code against a different NATS conn.
- **Remote builds** (`build --source`): push source tree over
  `jobs-amber-admin/1.0`, submit + watch over `jobs-build/1.0`, pull the result
  home, mirroring `jobs remote-build`'s four phases (handshake first, push
  objects, watch with SIGINT→cancel-prompt, re-handshake before pull).
- `run` of built outputs, dev-mode shell, and OCI image production are ports of
  `runner/run_linux.go`, `runner/develop_linux.go`, `runner/image.go`.

All three: urfave/cli/v2, slog, `errgroup` wiring, graceful Ctrl-C (context
cancellation → drain → store close). Every `main()` calls `sandbox.Init()`
first (re-exec model), including the client.

## 2. What we keep, port, and drop

### Ported verbatim (import-path changes only)

- `sandbox/` — 100% store-free (research: jobs-sandbox §1). Keep EINTR/ETXTBSY
  execve retries, mount ordering, cgroup best-effort ladder, leaf-holder.
- `fetchers/*`, `plugins/goplugin` — zero amber imports.
- `importdef`, `resources`, `tailbuf` — dependency-free.
- `bootstrap/seed/*.tar.zst` — seed blobs are store-format-agnostic
  (`tar|zstd`), reusable byte-for-byte.
- From amber-store-iroh: `protocol/`, `wantsync/`, `server/`, `relaymode/` are
  imported as module deps, not copied.

### Ported with the store seam swapped

`builddef`, `recipe`, `runner` (stage drivers + executors + storemount +
develop/run/image), `bootstrap` (seed logic), and a new **`amber/`** package
that reimplements jobs' 12-method `Store` seam (`jobs/amber/storeapi.go:23-37`)
directly over amber-store-core:

- `packstore.Get`-backed `fstree` reads (`LookupEntry`, `ListEntries`,
  `ResolvePath`, `WriteContent`).
- Ingest via `ingest.Dir`/`ingest.Objects` with **jobs' pinned chunk params**
  (`ByteOpts{32Ki,128Ki,256Ki}`, ItemBits 7 — `jobs/amber/build.go:24-26`).
  `FileKey(bytes)` and `IngestFile` share one code path (K identity depends on it).
- Materialize (key→dir) via `tarexport.Write | tarextract.Extract` in-process.
- `BuildStoreTree`/`BuildFromTree`/`TreeSubdir`/`ResolveBuildOutput`/
  `ResolveBuildArtifact` re-derived on core's fstree builders (fixed 0555/0444,
  zero uid/gid/mtime, bytewise entry order — identity-critical).
- Refs: plain `refstore` names. **No signatures** — reference signing, sshsign,
  grants, and the allowlist die; transport identity is the iroh endpoint key.

Identity consequences (research: jobs-defs §16): all K/F **values** differ from
the old jobs deployment (irrelevant — separate system), but graph **shape** is
preserved by keeping the canonical-CBOR profiles exactly: defs use fxamacker
`CanonicalEncOptions`, fstree uses `CoreDetEncOptions`+`NilContainerAsEmpty`;
no-params = CBOR null `0xf6` never empty map; stable-order+dedup before every
encode (`CanonicalPinnedInputs`, `SortKeys`, `CanonicalCaches`, `canonTags`).

### Dropped

- **FUSE / `amberfuse`** — materialize-only (already jobs' default; examples
  and tests run materialize-only today). `JOBS_STORE_MOUNT` gone.
- **Signing / auth layer** — sshsign, grants, runner tokens, device-code auth,
  bcrypt users. v1 access model = amber-store-iroh's: whoever knows the
  endpoint ID may connect (LAN/tailnet deployment). Follow-up: endpoint-ID
  allowlists per ALPN.
- **events/collector/console/buildview** — replaced by NATS subjects + the
  in-memory log store (§4); admin TUI replaces the SPA.
- **wire/ WS framing, goakt, sched actors, CRDT `state/`, pebble** — the
  scheduler is in-memory over refs + JetStream.
- **Secrets / runner tags** for v1 (`requiredTags` still parsed for identity
  compat, ignored for placement). Examples are public repos.
- pprof/metrics listeners for v1 (slog only).

## 3. Scheduler (the non-distributed rewrite)

Semantics per research: jobs-scheduler-semantics. In-memory node table keyed
`(kind, key)` — get-or-create is the join (no coordinator). Node kinds:
`import/K`, `buildfrom/K`, `pluginresolve/F`, `pin/F`, `buildrun/F`, and the
non-placeable `buildvalue/K` orchestrator that walks its stages sequentially.

- **Doneness = ref existence**, checked at node creation (fast-path DONE) — this
  is also crash recovery: server restart rebuilds the table from live requests,
  finished subtrees short-circuit. "Running twice is wasteful but never wrong."
- **Unfold** per kind: import deps = FetcherDef → `buildvalue/K_f`; buildfrom →
  source value; pin deps read from `build-plugin-resolved:F`; buildrun deps +
  resources from `build-pinned:F`. Defs travel in-band in job messages (so the
  runner's def read never misses).
- **Ref gate ported verbatim** (`sched/gate/gate.go:26-159`): per-kind exact
  allowed names; `build-from:K` + `build-from-tree:F` same-batch cross-check;
  `build-cache:<id>:<platform>` only for pinned cache ids; ordered batch
  `[caches…, build-output-deps:F, build-output:F]`; fail closed. Before writing
  any ref the server verifies object completeness (`fstree.CheckComplete`) —
  objects-before-ref survives the port.
- **Failure**: one hard failure = FAILED (memory-only; retry by resubmit);
  retryable (exit 75) budget = 3, backoff 1s..30s capped. `failed-upstream`
  derived per request snapshot. Cancellation = request interest dropped; shared
  subtrees survive while another request needs them.
- **Placement** = the NATS work queue below; requirement = max(kind default,
  `Pinned.Resources`, request) rounded **up** to the size ladder.

## 4. NATS layout

Streams/KV (JetStream) and core subjects, one embedded NATS per server (and per
client local session):

| Piece | Config | Purpose |
|---|---|---|
| stream `JOBS` | WorkQueuePolicy, subjects `jobs.<platform>.<class>` | one msg per placeable node attempt; payload = job def + in-band defs + resolved requirement; `Nats-Msg-Id = <node>/<gen>` |
| durable consumers on `JOBS` | one per `(platform, class)` filter subject, **shared** by all runners that fit it | runners pull from every class ≤ their size; `AckWait` 60s + `InProgress` heartbeats every 5s while executing |
| stream `RESULTS` | LimitsPolicy, `results.<node>`, `MaxAge` 7d, `Duplicates` ≥ AckWait×MaxDeliver | terminal result: exit class, proposed ref batch (name+key), rusage. **Publish result → then ack the job msg**; `Nats-Msg-Id = result-<node>/<gen>` dedups redelivery |
| KV `status` | per-node key `<kind>_<hexkey>`, per-request key `req_<id>` | current phase (`waiting|queued|running|publishing|done|failed`); watchable; KV revision = watch cursor |
| core NATS `logs.<node>` | no stream | live stdout/stderr chunks (≤32KiB, 64KiB/100ms flush — jobs' OutputWriter constants); fire-and-forget |
| core NATS `runners.<id>.heartbeat` | no stream | capacity, in-flight, rusage for the admin view |

Build output is durable only as a bounded in-memory head+tail per node on the
server (1MiB head + 3MiB tail ring, `sched/buildactor/output.go` port), fed by
subscribing `logs.>` — never JetStream, per the "outputs in server memory only"
rule. The client/admin log fetch = head + gap marker + tail + live follow.

Runner flow per job: pull → pull missing input objects (`jobs-runner-amber`) →
execute stage → push output objects → publish `results.<node>` → ack. The
server consumes `RESULTS`, gates the ref batch, `CheckComplete`s, writes refs,
advances the graph, updates `status` KV. A runner that dies mid-job simply
stops heartbeating; the job redelivers to another runner; the second result is
deduped.

## 5. iroh protocols (jobs-build/1.0, jobs-admin/1.0)

Framing: 4-byte BE length + canonical-CBOR message with a string tag envelope
(`{T, B}` — port of `sched/msg/codec.go`; amber-store-iroh uses the same frame
shape). One request per stream; watch streams stay open.

`jobs-build/1.0`: `Submit{source tree key T | build def, dir, params, platform,
resources?}` → `Submitted{request_id, K}`; `Watch{request_id}` → stream of
`Snapshot{phase, nodes[], counts}` (coalesced, server-side elapsed);
`Logs{node, follow}` → head/gap/tail + chunks; `Cancel{request_id}`.
Submit requires the def/source objects already pushed (`jobs-amber-admin`) —
the server publishes `build-from-tree:T` after a completeness check, exactly
like jobs' tree-source submit.

`jobs-admin/1.0`: `Requests{}` list, `Watch`, `Logs` (same as build), `Fleet{}`
(runner heartbeats snapshot), `Stats{}` (store disk usage via `packstore`
stats, ref count, uptime), `Refs{prefix}` browse, `Cancel/Delete`.

## 6. Repository layout

```
jobs-iroh/
  cmd/jobs-server/ cmd/jobs-runner/ cmd/jobs-client/
  amber/        # the Store seam over amber-store-core (new)
  natsiroh/     # NATS-over-iroh tunnel: dialer + stream proxy (from nats-iroh)
  builddef/ importdef/ resources/ recipe/ tailbuf/   # ports
  sandbox/ fetchers/ bootstrap/ plugins/goplugin/    # ports (verbatim-ish)
  runner/       # stage drivers, executors, run/develop/image (port, seam-swapped)
  sched/        # node table, unfold, gate, NATS dispatch (new, semantics ported)
  serve/        # jobs-server wiring: iroh Router, ALPN handlers, embedded NATS
  clientcli/    # client command implementations
  docs/design/ docs/research/
```

## 7. Milestones (PR sequence)

1. **Foundation**: go.mod deps; `amber/` seam over amber-store-core + tests
   (ingest/FileKey/materialize/BuildStoreTree/refs); `natsiroh/` package with
   the tunnel test; ported `importdef`/`resources`/`tailbuf`/`sandbox`.
2. **Identity + recipes**: `builddef`, `recipe`, `plugins/goplugin` ports;
   determinism tests (same def → same K; golden CBOR vectors).
3. **Local build MVP**: `runner/` stage drivers + local `driveFStages`;
   `bootstrap` seeding; `jobs-client run --source` builds
   `examples/go-build` end-to-end locally. ← the big validation gate.
4. **Server + runner**: `sched/` + `serve/`; `jobs-server` and `jobs-runner`
   binaries; remote submit/watch/pull e2e on loopback iroh.
5. **Client polish**: `develop`, `image`, `run`-of-outputs, status UX
   (liveView port), examples sweep (all of jobs-build/examples).
6. **Admin TUI** over `jobs-admin/1.0`.

Open v1 stances taken here (flag disagreement early): access is open-by-
endpoint-ID like amber-store-iroh; no FUSE; no secrets/tags; K values are new
(no store migration from jobs); local builds run the same NATS-backed scheduler
in-process rather than a separate recursion.
