# jobs-registry: uncompressed layers, streamed from the CAS

2026-07-24 · implemented in `runner/ocilayer.go` + `registryd/` ·
closes [#4](https://github.com/fables-for-robots/jobs-iroh/issues/4)

## Problem

Every layer jobs-registry served was gzipped
(`application/vnd.oci.image.layer.v1.tar+gzip`), and every layer it served had
first been written to disk. Both cost time on the path the registry exists to
make fast:

- **Assembly.** `tarball.LayerFromOpener` always compresses an uncompressed
  input, so building a layer meant *three* passes over the content: gzip it to
  get the blob digest, read it again to get the diffID, gzip it a third time to
  write the cached blob. A first `docker pull` waited on all three.
- **Pull.** The client then spent CPU gunzipping what we had spent CPU
  gzipping, to recover bytes we already had.
- **Disk.** Every layer existed twice: once as chunks in the amber store, once
  as a blob under `<data-dir>/blobs`.

None of that bought anything. The bytes come out of a content-addressed store
that already deduplicates, over a LAN or tailnet — the registry's own trust
model (§11) assumes an access-controlled network, not the open internet where
gzip pays for itself.

## Shape

**Layers are uncompressed.** `application/vnd.oci.image.layer.v1.tar`: the
blob *is* the tar, so a layer's digest equals its diffID. `runner.tarLayer` is
a hand-rolled `v1.Layer` for this — go-containerregistry's `tarball` layers
gzip unconditionally, with no option to opt out. Assembly now streams each
layer exactly once, to measure it (digest + size), and throws the bytes away.

**Layer bytes are never materialised.** A layer tar is a pure function of two
things: the content-addressed store, and a `runner.LayerSpec` — a dep set plus
a shell key (the closure layer), or one artifact key (the build layer). So the
spec is worth exactly what the blob is worth, at a few hundred bytes instead of
gigabytes. The registry records the spec and regenerates the tar into the
response on every blob GET:

- `registryd/layers.go` — `layerRecord` (descriptor + spec) lives in the
  per-image record; `layerIndex` is the in-memory digest→spec map that makes a
  blob GET answerable. It is rebuilt from the durable records at startup and
  extended by every assembly.
- `registryd/stream.go` — `layerStream` is an `io.ReadSeeker` over a blob that
  does not exist: `Seek` is arithmetic over the recorded size, and the first
  `Read` after a seek (re)starts the generator, discarding to the offset (a CAS
  stream cannot rewind). That is what lets `http.ServeContent` keep doing the
  HTTP work — HEAD, `Range` resumes, `Content-Length`, `Content-Range`, 416 —
  over a stream we never stored.

**What is still cached** is the manifest and the config: two small JSON blobs
per image, under the same digest-named, hash-verified, last-read-mtime scheme
as before. `--cache-ttl` now bounds a few KB per image rather than the whole
image; a swept manifest still recovers by reassembling, as before.

## Measured

Loopback server + registry on an aarch64 host, one build whose artifact is a
256MiB file, half incompressible / half trivially compressible, pulled with
docker 29.2.1:

| | before (gzip, cached) | after (tar, streamed) |
|---|---|---|
| assemble (first pull) | 1.95 s | 0.79 s |
| `docker pull`, cold | 2.83 s | 1.86 s |
| `docker pull`, warm | 0.82 s | 1.00 s |
| serve the artifact layer (no client work) | 0.025 s / 135 MB | 0.19 s / 268 MB |
| blob cache on disk | 137 MB | 1 073 B |

Assembly — the wait before a first pull can start — is **2.5× faster**, and the
cache is three orders of magnitude smaller. The serve line is the honest cost:
`sendfile` from a page-cached blob moves bytes far faster than regenerating a
tar (~1.3 GB/s here, after batching the pipe handoff to 256KiB — unbatched it
was ~0.9 GB/s). 1.3 GB/s is ~10 Gb/s, so on any ordinary network the link, not
the generator, is the bottleneck; on loopback it shows up as the ~0.2 s the
warm pull regressed by.

## Consequences

- A first pull does one pass over the content instead of three, and no write.
- Every pull re-reads the layer from the store (chunk reads + tar framing)
  instead of `sendfile`-ing a cached blob. That trade is deliberate: it removes
  a full duplicate copy of every image from disk, and the read it replaces was
  a gzip-decompressing client's problem anyway. Ranged resumes pay a discard of
  the offset, which is why `Range` is served rather than refused.
- Layers are bigger on the wire — 268 MB against 135 MB for the fixture above.
  On the intended network that is a good trade; it is the one thing to revisit
  if jobs-registry ever fronts a WAN.
- **Serving costs CPU per client.** Assembly is singleflighted, but ten nodes
  pulling one layer run ten generators over the same store objects — where the
  old path answered all ten from the page cache with `sendfile`. The sample
  deployment's CPU request went from `100m` to `1` accordingly. Coalescing
  concurrent readers of one layer is the obvious follow-up, and is not done:
  readers move at different speeds, so sharing a generator means buffering for
  the slowest one.
- **Multi-range blob GETs are answered whole** (`singleRange` in
  `registryd/handler.go`). `http.ServeContent` serves multiple ranges from a
  goroutine it never joins — which would outlive the handler's `Close` on a
  generator-backed stream — and seeks per range, where each backwards seek
  costs a full regeneration. RFC 9110 permits ignoring `Range`, and no registry
  client asks for more than one (a resume is a single `bytes=N-`). With that
  refused, a request costs at most one pass over the layer, the same as a
  plain GET.
- **`--cache-ttl` stopped being the disk knob.** It now expires a few KB per
  image. What grows without bound is the amber store and the per-image records
  — neither has a reclamation path (§11's store GC is the fix). Registry
  volumes must be sized for every build ever served.
- **Digests change.** A layer's digest is now the tar's, not the gzip's, so
  every image reassembles on first pull after the upgrade. Records written by
  the previous version have no layer specs, read as incomplete and are rewritten
  in the new shape; their old gzip blobs age out of the cache on the ordinary
  TTL sweep. Manifests held by clients from before the upgrade are gone — the
  digests they name no longer exist. This is a version-boundary break, not a
  data loss: the same `docker pull <host>:5000/jobs:<K>` produces the same
  image content.

## Not changed

`jobs-client image -o <tar>` still writes the single-layer, gzip-compressed
docker-load tarball. `docker load` consumes it locally, there is no download to
slow down, and the format is what it has always been.
