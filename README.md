# jobs-iroh

A simpler, non-distributed port of jobs: one server, N runners, and a client
connected only by iroh QUIC. The server embeds NATS (JetStream) for scheduling
and amber-store-core as the content-addressed store; the build model (Starlark
recipes, canonical-CBOR identity, hermetic sandbox) is ported from jobs intact.

**Status:** all milestones done (M1 foundation → M6 admin TUI). Local builds,
remote builds, develop shell, OCI image export, status/watch/admin CLI and the
interactive TUI are implemented and green. Design source of truth:
`docs/design/2026-07-22-architecture.md` (see its trailing implementation-
status section for the shipped deviations).

Examples matrix (jobs-build/examples): go-build, subbuild, python-build,
rust-build (build + run, cold and warm), develop/myapp — all pass locally and
via remote-build.

## Binaries & ALPNs

Three binaries: `jobs-server`, `jobs-runner`, `jobs-client`.
One server endpoint, five ALPNs:

| ALPN | Who | What |
|---|---|---|
| `jobs-build/1.0` | client | submit builds, watch, logs, cancel |
| `jobs-runner-nats/1.0` | runner | NATS tunnel to the embedded server |
| `jobs-runner-amber/1.0` | runner | CAS object/ref sync |
| `jobs-admin/1.0` | client (TUI) | observe builds, stats, fleet, refs, cancel/delete |
| `jobs-amber-admin/1.0` | client | push/pull amber refs |

## Quickstart: local build

No server needed — builds run hermetically against an embedded store under
`--data-dir` (default `~/.local/share/jobs-iroh`):

```sh
jobs-client build --source ./examples/go-build       # build → prints F + output key
jobs-client run   --source ./examples/go-build -- arg1  # build, then exec JOBS.entrypoint
jobs-client develop --source ./examples/go-build     # interactive shell in the build sandbox
jobs-client image  --source ./examples/go-build -o app.tar --tag myapp:dev
docker load -i app.tar
```

## Quickstart: server, runner, remote build

```sh
# 1. Server (prints its endpoint ID on startup):
jobs-server --data-dir /var/lib/jobs-iroh

# 2. Runner(s), anywhere that can reach the server over iroh:
jobs-runner --server <endpoint-id> [--addr host:port] [--size c1-m2]

# 3. Client: push source, build remotely, pull the output home:
jobs-client remote-build --server <endpoint-id> --source ./examples/go-build
jobs-client watch  --server <endpoint-id> --request-id <id>   # re-attach
jobs-client status --server <endpoint-id>                     # one-shot tables
jobs-client tui    --server <endpoint-id>                     # interactive admin
```

Local and remote builds share the same canonical definition, so the same
source yields the same build identity K/F everywhere — remote outputs pulled
home join the local cache and vice versa.

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
nix develop -c go build ./... && nix develop -c go vet ./...
nix develop -c go test ./...
```
