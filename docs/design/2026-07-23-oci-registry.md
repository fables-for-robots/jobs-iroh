# jobs-registry: serving build outputs as pullable OCI images

2026-07-23 · implemented in `registryd/` + `cmd/jobs-registry`

> **Layer storage was replaced on 2026-07-24** by uncompressed layers streamed
> from the CAS — see
> [`2026-07-24-uncompressed-streamed-layers.md`](2026-07-24-uncompressed-streamed-layers.md).
> Read every claim below about the blob cache, layer compression, and what
> `--cache-ttl` reclaims (the "Disk blob cache" bullet, the record/blob split,
> and the Limits section's "the expiring layer is the blob cache") as history:
> layers are no longer blobs, and nothing image-sized expires any more. The
> rest — naming, the two-layer shape, K→F resolution, records, singleflight,
> the trust boundary — is unchanged.

## Problem

Build outputs already export as docker-loadable tarballs (`jobs-client
image`), but getting a build into a container platform means a manual
export/load/push hop. Kubernetes, docker and containerd all speak one
protocol natively: the OCI Distribution API. jobs-iroh should serve builds
on that protocol directly.

## Shape

A fourth binary, `jobs-registry`: a **read-only** OCI Distribution registry
(pull only, GET/HEAD; everything else answers 405) that runs anywhere with
HTTP ingress (a k8s container is the intended home) and is configured with
one thing that matters: the jobs-server endpoint ID.

- **Images are named `jobs:<K>`**: one repository (`jobs`) whose tags are
  build keys (64-char lowercase hex, the build identity K).
  `docker pull registry.example:5000/jobs:<K>`. A K is already an immutable
  content address, so `/v2/jobs/tags/list` doubles as the catalog of
  submitted builds (a bare F from a directly pushed `build-output:F` also
  pulls, but is not listed).
- **Two layers**: the runtime closure (each dep at `/jobs/store/<BOK>`, the
  layout the artifact was linked against) and the build artifact (the
  output's `c/` tree at the image root, plus a writable `/tmp`). The deps
  layer is a pure function of the sorted dep-BOK set, so images sharing a
  closure share the blob — clients re-pull only the artifact layer.
  Assembly is `runner.AssembleOCIImage`, sharing the deterministic tar
  normalisation (epoch mtimes, uid/gid 0, sorted store entries) with the
  single-layer `jobs-client image` path; blob digests are therefore
  reproducible across restarts and hosts. The platform shell (`shell:<os/arch>`,
  pulled from the server) is baked into the deps layer with `/bin/sh` and
  `/jobs/shell` symlinks, because script entrypoints carry fixed shebangs and
  both `run` and `jobs-client image` provide it — `--no-shell` opts out, and
  a platform the server has no shell for degrades to a shell-less image. The
  entrypoint comes from `JOBS.entrypoint` and is optional here (a build
  without one is still distributable — `docker run` can name a command).
- **Own amber CAS, synced on demand.** The registry owns a private store
  (`<data-dir>/store`, flocked). On a cache miss it resolves K→F from the
  server's **ref listing** (the `build-from:K` ref's *value* is F — pulling
  that ref would drag the whole source env closure in, listing costs one
  frame), then `amberclient.Pull`s `build-output:F` and
  `build-output-deps:F` (absence of the deps ref tolerated), and `build:K`
  (the tiny definition blob) for the image config's os/arch — falling back
  to `--default-platform` for direct-F names. Pull is verified and
  objects-before-ref, per the store invariants; a `fstree.CheckComplete`
  short-circuit makes re-syncs free.
- **Disk blob cache with last-read expiry.** Assembled blobs (layer tars,
  config, manifest) live in `<data-dir>/blobs/sha256/<hex>`; a blob file's
  **mtime is its last-read time** (bumped on every serve, and on the layers
  when their manifest is served), so the clock survives restarts with no
  database. A sweep (every min(TTL/4, 1h)) deletes blobs unread for
  `--cache-ttl` (default 24h) plus abandoned temp files. Writes are
  temp+hash-verify+rename — no partial blob is ever visible under a digest.
- **Tiny durable records.** One JSON per served K under `<data-dir>/repos`
  maps the repo to its manifest digest and remembers F. Records are not
  swept (bytes live in blobs); they make an expired image reassemble
  **offline** — the content is still in the amber store — and let a blob
  request whose digest the sweep deleted recover by reassembling the image
  that referenced it.

## API surface

`GET/HEAD /v2/` (ping) · `/v2/_catalog` (`["jobs"]`) · `/v2/jobs/tags/list`
(server's `build-from:*` Ks ∪ local records; `n`/`last` paging) ·
`/v2/jobs/manifests/<K>|<digest>` · `/v2/jobs/blobs/<digest>` (Range
supported). Errors use the OCI envelope; unknown build → 404
`MANIFEST_UNKNOWN`, other repository names → 404 `NAME_UNKNOWN`, jobs-server
unreachable and not cached → 503, malformed digest → 400. Media types are
OCI (manifest/config/layer), which docker, containerd and podman all pull.

Concurrent first pulls of one K singleflight into one assembly, run under
the daemon context so an impatient client neither duplicates nor cancels
work others wait on.

## Trust

The registry extends the jobs trust model with a new boundary: its HTTP
face. It serves whatever the configured jobs-server names — object *content*
is verified against keys on pull, name→key bindings are as trustworthy as
that server — and OCI digests give end clients integrity from the manifest
down. The registry itself authenticates nothing and encrypts nothing; TLS
and access control belong to the ingress in front of it, exactly like the
"access-controlled network" assumption of the iroh ALPNs. It is pull-only
by construction, but the amber ALPNs it dials are open — it deliberately
never pushes.

## Limits / follow-ups

- The registry's amber store only grows (upstream store GC is a known
  follow-up); the expiring layer is the blob cache.
- One replica per data dir (store flock). Horizontal scale = more replicas
  with their own volumes; digests are reproducible, so caches agree.
- No TLS/auth in-process; terminate at the ingress or mark the registry
  insecure in the container runtime.
- `tags/list` enumerates every build on the server — fine on the
  deployment model's private networks.
