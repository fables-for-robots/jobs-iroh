# CLAUDE.md

## What this is

jobs-iroh is a way simpler, **non-distributed** port of jobs: one server, N
runners, a client, connected only by iroh QUIC — no k8s, HTTP, WebSockets,
CRDTs, gossip, or signing keys. It embeds NATS (JetStream, `DontListen`) for
scheduling and amber-store-core as the content-addressed store. Three binaries
(`jobs-server`, `jobs-runner`, `jobs-client`) and five ALPNs on one server
endpoint: `jobs-build/1.0`, `jobs-runner-nats/1.0`, `jobs-runner-amber/1.0`,
`jobs-admin/1.0`, `jobs-amber-admin/1.0`. The build model (Starlark recipes,
canonical-CBOR identity, hermetic sandbox, self-bootstrapping fetchers/shell)
is ported from jobs intact.

## Docs

- `docs/architecture/architecture.md` — **design source of truth**, written
  ground-up for this system. Keep code consistent with it; flag
  disagreements early.
- `docs/design/*.md` — dated design/implementation specs (historical
  record).
- `docs/research/*.md` — subsystem maps of the SOURCE systems the port draws
  from (jobs, amber-store-core, amber-store-iroh, nats-iroh). File:line
  citations point into those upstream trees, not this repo.

## Build & test

Go toolchain comes from the Nix devShell:

```sh
direnv allow                       # or:
nix develop -c go test ./...
nix develop -c go build ./...
```

`GOPRIVATE=github.com/fables-for-robots/*` is required for module fetches
(set in `.envrc`).

## Package map

| Package | What it is |
|---|---|
| `amber/` | Store seam over amber-store-core. Pinned chunk params (ByteOpts 32Ki/128Ki/256Ki, ItemBits 7) are **identity-critical** — never change them. |
| `natsiroh/` | NATS-over-iroh tunnel (dialer + stream proxy). The dialer writes a `0x00` stream preamble because the NATS server speaks first. |
| `wire/` | Frozen scheduler wire contracts: node names, phases, Job/Result CBOR, size-class ladder, NATS subject/stream layout. |
| `api/` | Frozen client API frames (4-byte BE length + CBOR `{t,b}` envelope) for the build/admin ALPNs. |
| `events/` | Build-event schema + OutputWriter (32KiB chunks, 64KiB/100ms flush) — events ride core NATS via the Sink seam. |
| `sched/` | Server scheduler: in-memory node graph (join = get-or-create, doneness = ref existence), unfold, ref gate, JOBS/RESULTS/status-KV folds, retry classes, per-kind PullRefs, log fold rings, durable FAILURES records + Diagnose. |
| `serve/` | jobs-server composition: iroh Router × 5 ALPNs, embedded NATS + embedded store, build/admin API handlers, bootstrap seeding. |
| `runnerd/` | jobs-runner daemon: boot self-test build gate, lane consumers per fitting size class, admission accounting, pull-inputs → drive stage → push-outputs → result-before-ack (MsgId dedup). |
| `amberclient/` | Importable amber sync client: dial by endpoint ID, Push/Pull (+WithProgress), refs list; single-conn v1. |
| `runner/` | Ported stage drivers + sandbox executors; local build/run pipeline (`driveFStages`), develop PTY shell, OCI image export. |
| `clientcli/` | jobs-client command surface: local + remote commands, store flock, liveView TTY progress (NO_COLOR-aware). |
| `tui/` | bubbletea admin TUI over `jobs-admin/1.0`: builds (watch/logs/cancel/delete), fleet, stats, refs. Never block in Update — network I/O only inside tea.Cmd goroutines. |
| `builddef/`, `recipe/` | Build definition identity (canonical CBOR) + Starlark recipe evaluation — ports, seam-swapped. |
| `bootstrap/` | Embedded seed artifacts (shell + fetchers per platform), idempotent seeding. |
| `fetchers/`, `plugins/` | Self-bootstrapping fetcher builds + goplugin — ports. |
| `sandbox/`, `tailbuf/`, `resources/`, `importdef/` | Verbatim ports from jobs — keep drift-free against upstream. |
| `cmd/jobs-server`, `cmd/jobs-runner`, `cmd/jobs-client` | The three mains (each calls `sandbox.Init()` first). |

## Binaries & commands

- `jobs-server --data-dir <dir> [--bind host:port] [--relay url]
  [--advertise-addr ip[:port]]… [--no-announce] [--log-level …]` — one iroh
  endpoint, five ALPNs, embedded NATS + amber store. Prints its endpoint ID on
  startup and announces it for discovery: direct interface addresses
  (auto-detected unless --advertise-addr) over mDNS on the LAN and via pkarr
  over the internet, nearest relay as fallback (relay connect is best-effort —
  an offline host still starts).
- `jobs-runner --server <endpoint-id> [--addr host:port]… [--size c1-m2]
  [--name …] [--data-dir …] [--skip-self-test]` — runs a boot self-test build
  (embedded shell, real sandbox) and refuses to start if it fails, then dials
  the server twice (NATS tunnel + amber sync), pulls work-queue jobs for
  every fitting class.
- `jobs-client`:
  - `build|run|develop --source <dir> [--dir …] [--build-file …] [--platform …]
    [--shell-ref …] [--param k=v]…` — local hermetic build / build-then-exec
    entrypoint / interactive PTY shell in the build sandbox (flock held for the
    whole session).
  - `image -o <tar> [--tag …] [--no-shell] (--source <dir> | <build-K>)` —
    docker-loadable OCI image from a build output (by source or by key).
  - `remote-build --server <id> --source <dir> [--cpu …] [--memory …]
    [--no-logs]` — push source, submit, watch to terminal, pull output
    home; the running steps' output streams alongside the progress block
    by default (`--no-logs` opts out).
  - `watch --server <id> --request-id <id> [--no-logs]` — re-attach to a
    build.
  - `logs --server <id> --node <name> [--follow]` — one node's captured
    output (stored head/gap/tail, raw bytes on stdout); `--follow` keeps
    streaming live chunks until interrupted.
  - `diagnose --server <id> (--request <id> | --node <name>) [--attempts N]
    [--json] [--logs-dir <dir>]` — durable failure report (all failed
    attempts with origin/class/exit, runner, timing, rusage, captured
    output); survives retries and server restarts. `--json` is the
    machine/LLM-friendly shape.
  - `status --server <id>` — one-shot plain-text requests + fleet tables.
  - `admin stats|fleet|requests|refs --server <id>` — thin frame calls.
  - `tui --server <id>` — interactive admin TUI.

## Sandbox re-exec rule

Every `main()` and every sandbox-driving `TestMain` must call
`sandbox.Init()` first — the sandbox works by re-exec'ing the binary.

## Invariants

- **Identity** = canonical CBOR (fxamacker `CanonicalEncOptions` for defs,
  `CoreDetEncOptions`+`NilContainerAsEmpty` for fstree; no-params = CBOR null
  `0xf6`, never empty map) + the pinned chunker params above.
- **Doneness = ref existence.** Checked at node creation; also crash
  recovery. "Running twice is wasteful but never wrong."
- **Objects before ref**: verify object completeness (`fstree.CheckComplete`)
  before writing any ref.
- Refs are **UNSIGNED** `reference.Reference` records — no sshsign/grants;
  transport identity is the iroh endpoint key.
