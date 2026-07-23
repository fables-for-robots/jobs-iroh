# Changelog

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
