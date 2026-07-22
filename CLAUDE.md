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

- `docs/design/2026-07-22-architecture.md` — **design source of truth**. Keep
  code consistent with it; flag disagreements early.
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

## Package map (what exists so far)

| Package | What it is |
|---|---|
| `amber/` | Store seam over amber-store-core. Pinned chunk params (ByteOpts 32Ki/128Ki/256Ki, ItemBits 7) are **identity-critical** — never change them. |
| `natsiroh/` | NATS-over-iroh tunnel (dialer + stream proxy). The dialer writes a `0x00` stream preamble because the NATS server speaks first. |
| `sandbox/` | Verbatim port from jobs — keep drift-free against upstream. |
| `tailbuf/` | Verbatim port from jobs — keep drift-free against upstream. |
| `resources/` | Verbatim port from jobs — keep drift-free against upstream. |
| `importdef/` | Verbatim port from jobs — keep drift-free against upstream. |

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
