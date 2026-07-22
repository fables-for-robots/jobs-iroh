# jobs-iroh

A simpler, non-distributed port of jobs: one server, N runners, and a client
connected only by iroh QUIC. The server embeds NATS (JetStream) for scheduling
and amber-store-core as the content-addressed store; the build model (Starlark
recipes, canonical-CBOR identity, hermetic sandbox) is ported from jobs intact.

**Status:** early port. See `docs/design/2026-07-22-architecture.md` for the
design source of truth.

## Binaries & ALPNs

Three planned binaries: `jobs-server`, `jobs-runner`, `jobs-client`.
One server endpoint, five ALPNs:

| ALPN | Who | What |
|---|---|---|
| `jobs-build/1.0` | client | submit builds, watch, logs, cancel |
| `jobs-runner-nats/1.0` | runner | NATS tunnel to the embedded server |
| `jobs-runner-amber/1.0` | runner | CAS object/ref sync |
| `jobs-admin/1.0` | client (TUI) | observe builds, stats, fleet, cancel |
| `jobs-amber-admin/1.0` | client | push/pull amber refs |

## Dev setup

Go toolchain comes from the Nix devShell:

```sh
direnv allow          # sets up the flake shell + GOPRIVATE
# or without direnv:
nix develop -c go test ./...
```

`GOPRIVATE=github.com/fables-for-robots/*` is required for module fetches
(exported by `.envrc`).

## Tests

```sh
nix develop -c go test ./...
```
