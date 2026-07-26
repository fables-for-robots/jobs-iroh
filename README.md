# jobs-iroh

A small, self-contained build system: one server, N runners, and a client,
connected only by iroh QUIC. Builds are hermetic (rootless Linux namespace
sandbox, no network inside), identified by content (canonical-CBOR
definitions and a content-addressed amber store), and described by Starlark
recipes. The server embeds NATS JetStream for scheduling — there is no other
infrastructure: no HTTP, no database, no container runtime.

Full design: [`docs/architecture/architecture.md`](docs/architecture/architecture.md).

## Binaries & ALPNs

Three core binaries: `jobs-server`, `jobs-runner`, `jobs-client` — plus an
optional `jobs-registry` (an OCI registry serving build outputs as pullable
images). One server endpoint, five ALPNs:

| ALPN | Who | What |
|---|---|---|
| `jobs-build/1.0` | client | submit builds, watch, logs, cancel |
| `jobs-runner-nats/3.0` | runner | NATS tunnel to the embedded scheduler |
| `jobs-runner-amber/1.0` | runner | store sync (objects + refs) |
| `jobs-admin/1.0` | client (TUI) | observe builds, stats, fleet, refs, cancel/delete, diagnose |
| `jobs-amber-admin/1.0` | client / registry | push source trees up, pull outputs home |

## A build file

A build is a `BUILD.jobs` Starlark file next to your source. Minimal shape:

```python
def build():
    toolchain = imp(fetcher = "tarball+https",
                    params = {"url": "https://go.dev/dl/go1.24.linux-amd64.tar.gz"})
    return struct(
        inputs       = {"toolchain": toolchain},
        env          = {"GOFLAGS": "-mod=mod"},
        script       = "go build -o $out/app .",
        runtime_deps = [],
    )
```

Imports (fetches) are the only steps with network access; the script runs in
a sandbox with its inputs mounted read-only, the source at `$SRC`, and the
output tree collected from `$out`. Language plugins (Go, npm, PyPI, cargo,
RubyGems, …) expand lockfiles into per-dependency cached imports.

## Quickstart: local build

No server needed — builds run hermetically against an embedded store under
`--data-dir` (default `~/.local/share/jobs-iroh`):

```sh
jobs-client build --source ./myapp                  # build → prints F + output key
jobs-client run   --source ./myapp -- arg1          # build, then exec JOBS.entrypoint
jobs-client develop --source ./myapp                # interactive shell in the build sandbox
jobs-client image  --source ./myapp -o app.tar --tag myapp:dev
docker load -i app.tar
```

## Quickstart: server, runner, remote build

```sh
# 1. Server (prints its endpoint ID on startup):
jobs-server --data-dir /var/lib/jobs-iroh

# 2. Runner(s), anywhere that can reach the server over iroh:
jobs-runner --server <endpoint-id> [--addr host:port] [--size c1-m2]

# 3. Client: push source, build remotely, pull the output home:
jobs-client remote-build --server <endpoint-id> --source ./myapp
jobs-client watch  --server <endpoint-id> --request-id <id>   # re-attach
jobs-client status --server <endpoint-id>                     # one-shot tables
jobs-client tui    --server <endpoint-id>                     # interactive admin
```

`remote-build` streams the running steps' build output alongside the
progress block by default (`--no-logs` opts out). When something fails,
`jobs-client diagnose --server <id> --request <id>` prints the durable
failure trail — every failed attempt with its captured output, surviving
retries and server restarts (`--json` for the machine-friendly shape).

Local and remote builds share the same canonical definitions, so the same
source yields the same build identity K/F everywhere — remote outputs pulled
home join the local cache and vice versa.

## Registry: docker pull a build

`jobs-registry` serves build outputs straight to docker/containerd/k8s — no
export/load/push hop:

```sh
jobs-registry --server <endpoint-id> [--addr host:port] --listen :5000
docker pull my-registry-host:5000/jobs:<build-K>
```

Images live in a single `jobs` repository whose tags are build keys K (the
value `remote-build` prints); `/v2/jobs/tags/list` enumerates every
submitted build (a local build's bare F key also pulls, but is not listed).
Images have two layers — the runtime closure under `/jobs/store/<key>` plus
the platform shell (script entrypoints need their `#!/bin/sh` shebang;
`--no-shell` opts out), and the build artifact at the root — assembled on
the fly from the registry's own store, which syncs from the jobs-server on
demand. Layers are **uncompressed** (`…image.layer.v1.tar`) and generated
straight from that store on every request rather than stored as blobs, so
serving costs CPU per pull instead of disk, and `--cache-ttl` (default 24h)
now expires only the small manifest/config blobs. Size the volume for the
store, which holds every synced build and is never reclaimed.
The registry is pull-only and speaks plain HTTP:
terminate TLS at an ingress or mark it insecure in the container runtime.
A sample k8s Deployment lives in
[`deploy/jobs-registry/`](deploy/jobs-registry/).

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

## License

[AGPL-3.0-or-later](LICENSE).
