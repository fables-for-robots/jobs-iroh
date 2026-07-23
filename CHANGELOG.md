# Changelog

## v0.8.1 — 2026-07-23

- **Registry images bake the shell — script entrypoints run under docker.**
  v0.8.0 registry images shipped without `/bin/sh` or `/jobs/shell`, so any
  build whose entrypoint is a script (every Python-style launcher) ran fine
  under `jobs-client run` but died under `docker run` with
  `exec …: no such file or directory` (the shebang's interpreter was
  missing). jobs-registry now pulls `shell:<platform>` from the server and
  bakes it into the deps layer with the `/bin/sh` + `/jobs/shell` symlinks
  and shell-aware `PATH`, exactly matching `jobs-client run` and the
  single-layer `image` default. `--no-shell` opts out; a platform the server
  has no shell for degrades to a shell-less image instead of failing.
  Already-cached images reassemble with new digests on their next pull.

## v0.8.0 — 2026-07-23

- **New binary: `jobs-registry` — docker pull your builds.** A read-only
  OCI Distribution registry that serves build outputs as pullable images:
  `docker pull <registry>:5000/jobs:<build-K>` (one `jobs` repository whose
  tags are build keys). Images have two layers —
  the runtime closure under `/jobs/store/<key>` and the artifact at the
  root — assembled on the fly (`runner.AssembleOCIImage`, sharing the
  deterministic tar machinery with `jobs-client image`, so blob digests are
  reproducible). The registry owns a private amber store synced on demand
  from the jobs-server over `jobs-amber-admin/1.0` (K→F resolved from the
  ref listing, so the source closure never syncs), caches blobs on disk
  with last-read-based expiry (`--cache-ttl`, default 24h), and reassembles
  expired images from its local store without the server. Pull-only, plain
  HTTP (TLS belongs to the ingress); sample k8s manifests in
  `deploy/jobs-registry/`.

## v0.7.0 — 2026-07-23

- **Build output streams by default.** `jobs-client remote-build` and
  `watch` no longer need `--logs`: the output of the steps a watched
  request is running streams live alongside the progress display out of
  the box. Running a build and not seeing its output was the surprise, so
  visibility is now the default — `--no-logs` opts out (the `--logs` flag
  is gone). Note the runner must be ≥ v0.6.0 to emit the exec banner and
  `bash -x` trace; against older runners only the script's own output
  streams.

## v0.6.0 — 2026-07-23

- **Executors announce what they actually run.** Every import executor
  writes a one-line banner into the job's output stream before exec'ing
  the fetcher — the resolved `fetch` entrypoint, the `JOBS_FETCH_PARAMS`
  JSON, and the secrets file path (never contents) — and fetcher stderr
  now tees to the runner's own stderr, so it shows live in local builds
  and the daemon log, like build stdout always has.
- **Build scripts run in shell debug mode.** The recipe script executes
  under `bash -ex` instead of `-e`: the captured output opens with a
  banner naming the true command and sandbox layout (`jobs: exec
  …/bin/bash -ex /build/.jobs-script.sh ($SRC=/build/src,
  $out=/build/out, net=none)`) followed by a `+ …` trace of every command
  the build runs. Build stderr tees to the runner's stderr stream too, so
  the trace is visible in local `jobs-client build` runs and daemon logs,
  streams live over `remote-build --logs`, and lands in failure tails and
  `diagnose` reports. Note: chatty scripts get much louder under `-x` —
  the log pipeline's bounds (lossy fan-out, ring-buffered storage) absorb
  it, but heavy loops now trace every iteration.

## v0.5.0 — 2026-07-23

- **Remote builds now stream their output live.** `jobs-client remote-build
  --logs` (and `watch --logs`) follows the output of the steps a watched
  request is running: each active node gets its own log-follow stream (up
  to 8, running nodes claiming slots first), and its stdout/stderr prints
  as `kind:key │ line` scroll above the progress block. Followers attach
  while a node is still queued — snapshots are coalesced, so a fast job
  may never be observed running — and linger half a second past completion
  so trailing chunks drain. When a build fails, the recap prints "(output
  streamed above)" instead of repeating logs that were already watched.
  The TTY progress block also redraws once a second now, so elapsed ticks
  between snapshots and log scroll can no longer push the block off screen
  for the length of a quiet compile.
- **`jobs-client logs --server <id> --node <name> [--follow]`** prints one
  node's captured output: the stored head/gap/tail as raw bytes on stdout
  (pipeable), and with `--follow` a live stream of new chunks until
  interrupted, `tail -f`-style (a retried attempt is marked on stderr).
  The server has spoken `logs{node, follow}` on `jobs-build/1.0` since
  v0.1.0 for the admin TUI; this is the first plain-CLI surface for it.
  Full node names come from `jobs-client diagnose`.

## v0.4.0 — 2026-07-23

- **Build failures are now durably diagnosable.** Every failed (or
  budget-burning retried) attempt folds a record into a new `FAILURES`
  JetStream stream at the moment the scheduler decides its fate: origin
  (runner result / commit / gate / server), disposition (retry + backoff,
  or terminal), the runner's verbatim result (class, exit, runner ID,
  rusage, and the proposed ref batch on gate/commit rejections), retry
  counters, request tags, timing, and a trimmed snapshot of the attempt's
  captured output (256KiB head + 512KiB tail). Previously the log ring was
  reset by the very retry that made a failure terminal, leaving a 4KiB
  stderr tail as the only trace. Records survive retries and server
  restarts (7 days, last 8 attempts per node) and are diagnostics only —
  never load-bearing.
- **`jobs-client diagnose --server <id> (--request <id> | --node <name>)`**
  renders the trail as a self-contained failure report — per attempt:
  verdict, runner, error, timing, rusage, captured output between explicit
  markers — designed to be pasted into an issue or an LLM conversation
  as-is. `--json` emits the machine shape (logs as readable strings,
  RFC3339 timestamps); `--logs-dir` dumps the output bytes verbatim.
  Served by a new `diagnose` frame on `jobs-admin/1.0`; by-request
  resolution works after a server restart by scanning the records' request
  tags. `remote-build` and `watch` now print the exact diagnose command
  when a build fails.

## v0.3.0 — 2026-07-23

- **Sandbox remounts no longer fail on hardened mounts.** The read-only (and
  strictatime) `MS_REMOUNT|MS_BIND` pass now repeats the flags the bind
  inherited from its source — nosuid/nodev/noexec/ro plus the exact atime
  mode, read back via statfs. In a user namespace every inherited flag is
  locked, and a remount that omitted them was refused: `sandbox child setup:
  remount-ro …: operation not permitted` on hosts whose TMPDIR sits on a
  nosuid/nodev/noatime mount (nix-shell or hardened /tmp). Repeating the
  flags changes nothing — the bind already carried them.
- **jobs-runner now runs a boot self-test build** before dialing the server:
  it seeds the embedded shell under a self-test-only ref and drives a
  one-script build through the real local pipeline (namespace sandbox
  included). A host whose sandbox is broken — mount restrictions, missing
  user namespaces, exotic TMPDIR mounts — previously advertised capacity and
  hard-failed every job it was handed (e.g. `sandbox child setup: remount-ro
  …: operation not permitted`); it now refuses to start with the real error.
  `--skip-self-test` (`JOBS_RUNNER_SKIP_SELF_TEST`) is the escape hatch.

## v0.2.0 — 2026-07-23

- **jobs-server now announces itself for discovery** (ported from
  amber-store-iroh's amber-serve): direct addresses on every interface
  (auto-detected with container-bridge filtering, or `--advertise-addr`
  verbatim), re-announced over mDNS on the local link every second, and
  published via pkarr over the internet together with the nearest built-in
  relay (`--relay` to pin one; relay connect is best-effort and bounded, so
  an offline host still starts; `--no-announce` skips the export entirely).
  Clients resolving by bare endpoint ID — which previously failed with "no
  address found" — now find the server via mDNS on the LAN and pkarr/DNS
  elsewhere, racing every candidate.
- Client dials (amberclient, hence runner, CLI and TUI) bind with relays
  enabled on the discovery path, so the server's published relay actually
  works as a fallback when no direct candidate is reachable.

## v0.1.0 — 2026-07-23

First release. jobs-iroh is a way simpler, **non-distributed** reimplementation
of [jobs](https://github.com/draganm/jobs): one server, N runners, a client —
connected only by [iroh](https://iroh.computer) QUIC. No k8s, no HTTP, no
WebSockets, no CRDTs, no gossip, no signing keys. The hermetic build model
(Starlark recipes, canonical-CBOR identity, namespace sandbox, content-
addressed store, self-bootstrapping fetchers/shell) is ported from jobs
intact; every example in
[jobs-build/examples](https://github.com/jobs-build/examples) builds and runs,
locally and remotely.

### The three binaries

- **jobs-server** — one iroh endpoint, five ALPNs (`jobs-build/1.0`,
  `jobs-runner-nats/1.0`, `jobs-runner-amber/1.0`, `jobs-admin/1.0`,
  `jobs-amber-admin/1.0`); embedded NATS/JetStream (`DontListen`, tunneled
  over iroh streams) and embedded
  [amber-store-core](https://github.com/fables-for-robots/amber-store-core);
  in-memory scheduler (join = get-or-create by content key, doneness = ref
  existence, ported fail-closed ref gate); work queue
  `jobs.<platform>.<class>`, RESULTS stream with publish-before-ack +
  `Nats-Msg-Id` dedup, status KV, live logs on core NATS held in server
  memory only.
- **jobs-runner** — dials the server twice (NATS tunnel + CAS sync); serves
  every size class ≤ its `--size` on the ladder `c0.2-m1 … c4-m16`
  (auto-detected by default) with admission accounting; pulls input closures
  by ref, runs the stage drivers in the rootless namespace sandbox, pushes
  outputs, reports results. A killed runner's work redelivers — running twice
  is wasteful but never wrong.
- **jobs-client** — local hermetic builds (`build`, `run`, `develop` with a
  PTY shell in the exact build sandbox, reproducible OCI `image` export) over
  an embedded store, plus the remote surface (`remote-build` with live watch
  and pull-home, `watch`, `status`, `admin`, and a bubbletea `tui` for
  builds/logs/fleet/stats/refs). Local and remote submits of the same source
  produce the same K, so caches join across paths.

### Compatibility

All 10 current examples pass, local and remote, build **and** run-verified:
subbuild, develop/myapp, go-build (+ image + develop), python-build,
python-sdist-build, python-rust-build, python-rust-sdist-build, rust-build,
rails-build, phoenix-build.

### Deviations from jobs (v1 stances)

Runner adoption dropped (queue redelivery covers it); single-connection CAS
sync; access is open to whoever knows the endpoint ID (amber-store-iroh's
trust model); FUSE store mounts deleted (materialize-only); build events ride
NATS instead of a collector service; no secrets/runner tags; no store GC.
Known follow-ups are listed in `docs/design/2026-07-22-architecture.md`.
