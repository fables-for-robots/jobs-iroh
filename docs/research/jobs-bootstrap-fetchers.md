# jobs subsystem map — self-bootstrapping (embedded seed) + fetchers

Source repo: `~/fables-for-robots/jobs` (all file:line references below are relative to it).
Scope: `bootstrap/`, `fetchers/`, `fetchers.toml`, `scripts/gen-seed.sh`,
`docs/superpowers/specs/2026-07-10-recipe-declared-fetchers-design.md`, `architecture/bootstrap.md`.
Purpose: implementation map for porting this subsystem into jobs-iroh (amber-store-core CAS, single
jobs-server, iroh transport). Every draganm/amber-store leak is flagged as a **CUT-POINT**.

---

## 1. The embedded seed

### 1.1 Inventory and on-disk/embedded format

The seed is 4 artifacts x 2 platforms, committed as compressed tarballs and embedded via
`//go:embed seed` (bootstrap/seed.go:22-23):

```
bootstrap/seed/linux-amd64/tarball-https.tar.zst   (~2.2 MB)
bootstrap/seed/linux-amd64/github.tar.zst          (~2.2 MB)
bootstrap/seed/linux-amd64/shell.tar.zst           (~1.8 MB)
bootstrap/seed/linux-amd64/hostmusl.tar.zst        (~0.5 MB)
bootstrap/seed/linux-arm64/{same four}             (~6.2 MB total)
```

Directory name = platform with `/`→`-` (`platformDir`/`dirPlatform`, seed.go:58-61).
`SeededPlatforms()` (seed.go:64-76) enumerates the embedded `seed/` subdirs → `["linux/amd64",
"linux/arm64"]` (asserted in bootstrap/seed_test.go:15-31).

**Blob format:** each `.tar.zst` is a plain `tar -C <artifact-dir> -cf - . | zstd --ultra -22 -q -f`
of the artifact tree (gen-seed.sh `pack()`, scripts/gen-seed.sh:40-47). Compression is
compress-time-only max ratio; `--long` is deliberately omitted so the frame window never exceeds the
pure-Go decoder's cap (gen-seed.sh header comment lines 24-26; decode side uses
`zstd.WithDecoderMaxWindow(512<<20)` — seed.go:135). **The blob format has zero coupling to the
store**: it is decompressed (klauspost/compress/zstd), extracted to a temp dir, and re-ingested
through whatever store API is present (seed.go:132-149). Constraint: no hardlinks in the tar
(`extractTar` fails loudly on `tar.TypeLink` because a hardlink would ingest as an incomplete tree —
seed.go:202-206); dirs/regular files/symlinks only, path-traversal rejected (seed.go:163-172).

### 1.2 Artifact → ref mapping

`seedArtifacts` (seed.go:31-36) is the single source of truth:

| embedded file | published ref |
|---|---|
| `tarball-https.tar.zst` | `fetcher:tarball+https:<platform>` |
| `shell.tar.zst` | `shell:<platform>` |
| `hostmusl.tar.zst` | `fetcher:hostmusl:<platform>` |
| `github.tar.zst` | `fetcher:github:<platform>` |

Why these four (fetchers.toml:2-17, design §8 at docs/superpowers/specs/2026-07-10-…-design.md:207-214):
- **tarball+https** — the one network-capable bootstrap fetcher; everything buildable is pulled with it.
- **shell** — the static bash+jq+busybox sandbox userland; without it you can import but not build.
- **hostmusl** — musl loader + libgcc_s, nix-built, not JOBS-buildable.
- **github** — submit-time source imports are constructed outside any recipe, so it must exist before
  any recipe does (design §2.5, lines 56-58).

### 1.3 `SeedFetcherNames()` and the string-fetcher rule

`SeedFetcherNames()` (seed.go:45-54) derives the import-capable seed fetcher names from
`seedArtifacts` by taking every ref with prefix `fetcher:` → `["tarball+https", "hostmusl",
"github"]`. `shell` publishes `shell:<p>` and is therefore *not* an import fetcher.

The rule (recipe-declared-fetchers design §3, §5): a **string-valued** `imp(fetcher = "name")` or a
plugin-emitted import without `FetcherDef` may only name a seed fetcher; resolution is via the
mutable ref `fetcher:<name>:<platform>`. Everything else must carry `FetcherDef` (canonical CBOR of
the fetcher's build definition, part of import identity `K`). Enforcement sites:

- Recipe/plugin rehydration: `rehydrator.rehydrateInput` injects the plugin bundle's pin for any
  non-seed name; no pin ⇒ hard error `"not in the plugin's bundled fetchers.toml and not a seed
  fetcher"` (recipe/value.go:150-158). Seed set is threaded in as `EvalConfig.SeedFetchers`
  (recipe/recipe.go:24-28, 294-298) from `bootstrap.SeedFetcherNames()` (runner/buildeval.go:155).
- Runner: a named (string) fetcher resolves `fetcher:<name>:<platform>` with
  `platform = def.Platform` (empty ⇒ legacy: runner's own platform) — runner/importjob.go:118-121,
  146-151. A miss is a **hard failure, not a decline**: "no fetcher X for P (not a seed;
  recipe-declared fetchers carry their build)" (importjob.go:152-157) — nothing provisions named
  fetchers anymore.
- `FetcherDef` imports bypass refs entirely: derive `K_f` from the def, resolve the build's `c/`
  artifact by content (`amber.ResolveBuildArtifact`), then reuse the same materialize/exec path
  (`ResolveFetcherArtifact`) — importjob.go:125-145, runner/fetcher_linux.go:44-95.
- `fetcher()` Starlark builtin: `fetcher(name, url=, sha256=)` sugar =
  `builddef.FetcherBuild(url, sha256, platform)` — a `tarball+https` import `{url, sha256, strip:1}`
  + a no-params build (params = canonical CBOR null 0xf6, not empty map — join/dedup-critical)
  (recipe/fetcher.go:29-70, builddef/fetcherbuild.go:16-42). `fetcher(name, build=bld(...))` for
  exotic cases; exactly one form required (fetcher.go:45-49).

### 1.4 The manifest (`fetchers.toml`) — inventory only

Since recipe-declared-fetchers, `fetchers.toml` lists **only** the 8 embedded seed leaves
(fetchers.toml:20-58). Schema: `[[fetcher]] name/platform/source` where `source = "embedded"` is the
only accepted value — `"build"` (or anything else) is a **parse error** so a stale manifest fails
loudly (bootstrap/manifest.go:71-90). `LoadEmbedded()` parses the copy `go:embed`-ded into every
binary (`jobs.FetchersTOML` via root embed.go; manifest.go:63-65). Note the manifest is
*validation/inventory only*: `Seed()` iterates `seedArtifacts`, not the manifest. jobs-iroh can keep
it as a guard-rail or drop it; `SeedFetcherNames()` is the load-bearing API.

---

## 2. Self-seeding on startup

### 2.1 `Seed()` semantics (bootstrap/seed.go:91-120)

```go
func Seed(ctx, st amber.Store, signer *amber.Signer, platforms []string, log *slog.Logger) error
```

For each platform x artifact:
1. Read the embedded blob; **missing blob = warn + skip** (a partially-populated seed never blocks
   startup; seed.go:96-100).
2. Compute the **blob-aware refresh marker** ref
   `seed-src:<ref>:<hex(sha256(blob)[:8])>` (`seedMarkerRef`, seed.go:127-130).
3. If the marker ref exists (`amber.GetKey(st, marker)`) ⇒ skip — this blob already produced this
   ref (seed.go:101-105).
4. Else ingest: zstd-decompress → `extractTar` into a temp dir → `amber.IngestDir(ctx, st, tmp)` →
   content key `k` (`ingestSeedBlob`, seed.go:132-149).
5. Publish `signer.SignAndPut(st, ref, k)` **then** `signer.SignAndPut(st, marker, k)`
   (seed.go:110-115). Objects-before-ref holds because ingest already stored the objects.

**Why markers, not skip-if-ref-exists:** idempotence is tracked per embedded *blob*, not per ref. A
ref seeded from an older binary's blob has no marker for the new blob ⇒ it is re-ingested and
re-published, so a changed seed rolls out on restart; an unchanged blob never re-ingests — critical
because **amber keys are metadata-sensitive** (tree hash includes per-entry mtime/uid/gid,
amber/build.go:150-167), so re-ingesting identical bytes yields a *different* key and would churn
the ref every boot (doc comment seed.go:79-90; behavior locked by
bootstrap/seed_test.go:75-126 — stale-ref refresh + re-seed key-stability). Stale markers from old
blobs linger harmlessly; a store wipe clears them (seed.go:125-126).

Seeding is **publish-only**: a host ingests+signs artifacts for platforms it cannot execute
(cross-arch pre-seeding of a shared store; seed.go:86-89, architecture/bootstrap.md:84-93).

### 2.2 Store APIs consumed (the seam)

`Seed` touches exactly three store operations, all via the `amber` wrapper package:
- `amber.GetKey(ctx, st, name) (key.Key, bool, error)` — ref presence probe (amber/ref.go:123-125).
- `amber.IngestDir(ctx, st, dir) (key.Key, error)` — tree ingest (amber/ingest.go:73-76; streams an
  `amberpack` built by `fstree.BuildDir` into `st.Ingest` — ingest.go:22-54).
- `(*amber.Signer).SignAndPut(ctx, st, name, k)` — builds a `reference.Reference{Name, Key, User,
  CreatedAt}`, SSH-signs it (`sshsign.SignWith`), `st.PutRef(rec)`, then optionally
  `RemoteSync.PushAfter(name)` if a sync is attached (amber/ref.go:71-95).

### 2.3 Call sites and signing identities

| caller | signer | pushed beyond local? |
|---|---|---|
| server startup: internal/jobscli/schedserver.go:86 | long-lived engine signer, `SetRemoteSync` attached at :87-88 **before** Seed | yes — seeds+markers push to central |
| runner (legacy WS loop): runner/runner.go:190-201 | `EphemeralSigner("runner")` (throwaway ed25519, runner/ephemeral.go:14-19) | no — local embedded store only |
| runner (actor edge): runnerlink/runnerlink.go:92-96 | transport signer, sync not yet bound | no ("local refs …, never pushed") |
| runner wipe re-seed: runner/wipe.go:64 | ephemeral | no |
| local CLI (develop/run/image): internal/jobscli/local.go:99,188,377 → `localBootstrap` → `localSeed` (local.go:112-130) | user key or ephemeral | no; **non-fatal** — a failure surfaces later as missing-ref |
| test harness: sched/schedtest/harness.go:126 | test signer | n/a |

Server-side Seed failure is a stderr warning, not fatal (schedserver.go:85-89 "started = ready",
best-effort). Runner re-resolves the shell key **per connection**, not per process, because a wipe
re-seeds the shell under a new key (runner/runner.go:206-215, 242).

---

## 3. `gen-seed.sh` — how seeds are produced (scripts/gen-seed.sh)

Rare, manual, out-of-band. Env: `JOBS_SEED_ONLY=` comma subset of
`tarball+https,shell,hostmusl,github`; `JOBS_SEED_ARM64_HOST=user@host` (+`JOBS_SEED_SSH_OPTS`) for
non-host-arch nix builds. Output lands in `bootstrap/seed/<os>-<arch>/<name>.tar.zst` and is
**committed to the repo** (auditable bytes — the system's audit root,
architecture/bootstrap.md:367-369).

- **tarball+https** (gen-seed.sh:140-151) and **github** (:153-166): pure-Go static binaries,
  cross-compiled locally for both arches:
  `CGO_ENABLED=0 GOOS=linux GOARCH=<a> go build -trimpath -ldflags='-s -w' -o $out/fetch
  ./fetchers/{tarballhttps,github}`. Artifact tree = single file `fetch` at root (the entrypoint
  binary **must** be named `fetch`).
- **shell** (:49-57 local, :59-86 remote): built from the repo flake's `packages.shell`
  (flake.nix:91-102 — `cp -L` of `pkgs.pkgsStatic.{bash,jq,busybox}` into `bin/` plus relative
  busybox applet symlinks: sh cat ls cp ln mkdir rm mv chmod test true false env printf dirname
  basename sed grep tar). Locally it just runs `JOBS_OUTPUT_DIR=$out bash fetchers/hostshell/fetch`;
  for arm64 it ships `flake.{nix,lock}` over SSH, `nix build .#shell` there, and streams the
  `bin/` tree back. Artifact tree = `bin/{bash,jq,busybox,<applet symlinks>}`.
- **hostmusl** (:89-137): artifact tree = `fetch` (a copy of `fetchers/hostmusl/fetch`) +
  `ld-musl-<arch>.so.1` + `libgcc_s.so.1` copied out of `nix build .#muslRuntime`
  (flake.nix:73-91 — musl loader from `pkgs.musl.out`, unwinder from
  `pkgs.pkgsMusl.gccForLibs.lib`). Same remote-arm64 dance.
- `pack()` (:40-47): `tar -C $src -cf - . | zstd --ultra -22 -q -f` → the committed blob.

Porting note: gen-seed.sh depends only on the repo layout (`fetchers/…`, `flake.nix` with `shell` +
`muslRuntime` packages) — it is copyable to jobs-iroh nearly verbatim once those are ported.

---

## 4. The fetcher execution contract

Defined in architecture/import.md §3.3; consumed by runner/importjob.go:

- Artifact = a tree whose root holds an executable `./fetch` (plugins additionally/instead carry
  `plugin`). The runner materializes (default) or FUSE-mounts the artifact and execs `./fetch` from
  it (runner/fetcher_linux.go:44-95; `JOBS_STORE_MOUNT` governs mount mode).
- Env in: `JOBS_FETCH_PARAMS` = the import def's params rendered as JSON
  (importjob.go:172-179); `JOBS_OUTPUT_DIR` = writable output dir (the only guaranteed-writable
  dir); optional `JOBS_SECRETS_FILE` = 0600 JSON `{tag: {scope, secret}}` written only when the def
  has `RequiredTags` (importjob.go:182-189, 233-249).
- Exit codes: `0` success; `75` (EX_TEMPFAIL, `exTempFail` importjob.go:21) ⇒ retryable failure;
  any other non-zero ⇒ hard failure (importjob.go:207-214). Infra errors around the exec are
  retryable; cancel maps to Cancelled (importjob.go:200-206).
- Output: the runner ingests `JOBS_OUTPUT_DIR` with `amber.IngestSourceDirStats` — i.e. honoring
  any `.amberignore` the fetcher emitted — and publishes `import-output:<K> → rootKey` via
  `RefWriter.WriteRefs` (importjob.go:216-230).
- Fetchers run as **network-capable host subprocesses** (no pivot_root — fetcher_linux.go:18-21),
  so script fetchers need host bash/jq (and gomod needs host `go`); the seed's Go fetchers are
  fully static.
- Named-fetcher resolution is remote-FIRST (`amber.ReadKeyFresh`, fetcher_linux.go:23-36;
  amber/remotesync.go:406-413) because `fetcher:` refs are mutable; nil sync ⇒ plain local read.

## 5. Fetcher catalog (`fetchers/*`)

**Seed (embedded) fetchers:**
- `tarballhttps` (fetchers/tarballhttps/main.go) — params `{url, sha256, strip}`
  (main.go:40-44). Streams the download to a temp file *inside* `JOBS_OUTPUT_DIR` hashing inline
  (memory-capped imports; main.go:64-74), verifies sha256, gunzips + extracts via
  `tarextract.Extract` with strip (main.go:121-128). Retryable: transport errors, 429, 5xx
  (main.go:100-116). Registered name is `tarball+https` (seed.go:32).
- `github` (fetchers/github/*.go) — params `{owner, repo, ref, apiBaseURL?}` (params.go:12-41).
  `GET {api}/repos/{owner}/{repo}/tarball/{ref}` with optional Bearer token selected from
  `JOBS_SECRETS_FILE` by exact host-scope match, lexicographically-first tag wins
  (secrets.go:25-50); Authorization stripped on cross-host redirect (fetch.go:44-54). HTTP
  classification: 404/401/perm-403 hard; ratelimit-403/429/5xx retryable (fetch.go:70-91). Extracts
  stripping GitHub's single top-level wrapper dir; rejects absolute/escaping symlinks; empty
  tarball is hard (extract.go:17-89).
- `hostmusl` (fetchers/hostmusl/fetch, bash) — no params. Copies the **prebuilt** `ld-musl-<arch>.so.1`
  + `libgcc_s.so.1` that live *next to the script in the artifact* into `$out/lib/` and creates
  `libc.so`, `libc.musl-<arch>.so.1`, `libgcc_s.so` symlinks (fetch:34-51). Arch token derived from
  the artifact contents, NOT `uname` — platform-pinned imports must vendor the artifact's arch even
  when fetched on a different-arch host (fetch:18-31). ELF-magic sanity check (fetch:40-43).
- (`shell` is a seed *artifact*, not an import fetcher — see §6. `hostshell` below is only its
  build script.)

**Non-seed, in-repo fetchers (recipe-declared / historical, all following the same contract):**
- `hostshell` (fetchers/hostshell/fetch, bash) — gen-seed-time only: `nix build $flake#shell`, copy
  `bin/`, then assert bash/jq/busybox are static via `ldd` ("not a dynamic executable") and ensure
  `bin/sh → busybox` (fetch:17-33). Requires nix; never runs in production.
- `tarballxz` (fetchers/tarballxz/main.go) — same shape as tarballhttps but `.tar.xz` via pure-Go
  `ulikunitz/xz` (needed for zig; main.go:1-9, 114-120).
- `gomod` (fetchers/gomod/fetch, bash) — params `{module, version}`; uses host `go mod download`
  into a throwaway `GOMODCACHE`, emits `cache/download/<module>/@v/...`, strips `sumdb/`, `list`,
  `*.lock` for reproducibility (fetch:34-43). Exit 75 on download failure (fetch:28-32).
- `alpineapk`, `npm`, `pypi`, `rubygems`, `cargocrate` (fetchers/*/main.go) — static Go binaries,
  one package each, digest-verified (see each file's doc header).
- Plugin fetchers `goplugin`/`cargoplugin`/`nodeplugin`/`uvplugin`/`pybackendplugin`/`bundlerplugin`
  (fetchers/*/fetch) — trivial bash: copy a prebuilt `plugin` binary from the artifact dir to
  `$out/plugin` (e.g. fetchers/goplugin/fetch:13-18). In the current world these ship as
  recipe-declared fetcher builds from their own repos.

**No production fetcher imports amber-store** — they use stdlib + `fetchers/tarextract` (+
`ulikunitz/xz`); only `github/integration_test.go` touches jobs/amber. The whole `fetchers/` tree is
store-agnostic and ports verbatim (module path rename only).

### 5.1 `tarextract` (fetchers/tarextract/tarextract.go)

Shared extractor for tarballhttps/tarballxz: `Extract(r io.Reader, outDir string, strip int)`
(tarextract.go:30-98). Exists because busybox tar strips a leading `../` from relative symlink
targets, corrupting e.g. Node's `bin/npm -> ../lib/node_modules/npm/...` (doc, tarextract.go:1-15).
Semantics: leading-component strip (:100-111); path-escape rejection for entries *and* resolved
symlink targets (:48-50, 81-87); `O_NOFOLLOW` writes (:62); symlink `Linkname` preserved verbatim
(:76-80); modes preserved via explicit `Chmod` (:73).

---

## 6. The shell artifact and `/jobs/shell` compat

- Content: fully static `bin/{bash,jq,busybox}` + relative applet symlinks (flake.nix:91-102);
  no PT_INTERP, no `/nix` — the hermetic sandbox mounts no `/nix` (hostshell/fetch:3-9).
- Published as `shell:<platform>`; every consumer defaults the ref to `"shell:" + platform`
  (runner/runner.go:242, runner/develop_linux.go:215, runner/run_linux.go:60, runner/artifact.go:47).
- Shebang compat: scripts/plugins carry fixed `#!/jobs/shell/bin/bash`; a symlink
  `/jobs/shell → /jobs/store/<shellBOK>` is planted in the plugin sandbox
  (runner/plugincaller.go:29-35, 154-158), the run sandbox (runner/run_linux.go:142-145), and baked
  into images (runner/image.go:225, gated by `IncludeShell`, image.go:62).

---

## 7. draganm/amber-store couplings (the port cut-points)

Direct imports in this subsystem:
1. **bootstrap/seed.go:17** — `github.com/draganm/amber-store/key` (`key.Key` return of
   `ingestSeedBlob`, seed.go:134). The only draganm import in `bootstrap/`.
2. **bootstrap/seed.go:18** — `github.com/draganm/jobs/amber`, whose seam drags the rest in:
   - `amber.Store` interface = the `*client.Client` method subset — `Ingest(io.Reader)
     client.Stats`, `Ls/Tar/File`, `PutRef(reference.Reference)`, `GetRef`, `ListRefs`,
     `Remote{List,Push,Pull,LsRefs}` (amber/storeapi.go:23-37) — vocabulary types from
     `draganm/amber-store/{client,key,reference}`.
   - `amber.IngestDir` → `draganm/amber-store/{amberpack,fstree}` streaming pack ingest
     (amber/ingest.go:8-11, 22-54, 73-76).
   - `amber.Signer.SignAndPut` → `draganm/amber-store/reference.Reference` + SSH signatures via
     `draganm/amber-store/sshsign` (amber/ref.go:8-12, 71-95).
   - `amber.GetKey` (ref.go:123-125), `amber.ReadKeyFresh`/`RemoteSync` (remotesync.go:406-413).
3. **Behavioral coupling (most important):** amber tree keys hash per-entry `mtime`/`uid`/`gid`
   (amber/build.go:150-167). This is *why* the `seed-src:` marker scheme exists at all
   (seed.go:79-90, seed_test.go:59-66) and why the "fleet seed race" is genuine
   (architecture/bootstrap.md:370-378): identical bytes ⇒ different keys per ingest.
4. Downstream (outside `bootstrap/` but part of the flow): the runner's fetcher resolution
   (`runner/fetcher_linux.go` uses `key.Key`, `amberfuse`), import ingest
   (`amber.IngestSourceDirStats`), and `ResolveBuildArtifact` (two-hop content resolution for
   `FetcherDef`) — all against the same `amber.Store` seam.

Refs/namespaces this subsystem writes/reads (must exist in the jobs-iroh ref model):
`fetcher:<name>:<platform>`, `shell:<platform>`, `seed-src:<ref>:<blobhash8>` (written by Seed);
`import-output:<K>` (written after a fetch); `import:<K>` (read for the def);
`build-from:<K_f>` → `build-output:<F>` (read for FetcherDef resolution).

---

## 8. What jobs-iroh concretely needs ("bootstrapping of the fetchers and shell done the same as in jobs")

1. **Reuse the seed blobs byte-for-byte.** The `.tar.zst` blobs are store-format-agnostic
   (plain tar of a file tree; keys are derived at ingest time, seed.go:132-149). Copy
   `bootstrap/seed/**` unchanged into jobs-iroh and keep `go:embed`. Re-generation is only needed
   if the *fetcher binaries* change (they are built from the ported `fetchers/` sources — the
   embedded `tarball-https`/`github` binaries are jobs-repo builds, fine to reuse as-is since the
   fetcher contract is unchanged). Constraint to preserve when re-packing: no hardlinks, no
   `zstd --long`.
2. **Port `bootstrap/` re-based on amber-store-core.** The package needs exactly three store
   capabilities: ingest-a-directory→key, get-ref, put-ref (§2.2). Map them onto amber-store-core's
   equivalents; `key.Key` becomes amber-store-core's key type. `extractTar`, `seedMarkerRef`,
   `seedArtifacts`, `SeedFetcherNames`, `SeededPlatforms` port unchanged.
3. **Keep the `seed-src:` marker semantics iff amber-store-core keys are metadata-sensitive.** If
   amber-store-core hashes only content (no mtime/uid/gid), re-ingest becomes key-stable and
   idempotence could be simplified to skip-if-ref-points-at-same-key — but the marker scheme is
   still the cheapest "don't re-ingest 13 MB on every boot" guard and preserves
   changed-blob-rolls-out-on-restart, so porting it verbatim is the safe default. Verify
   amber-store-core's hashing before deciding.
4. **Where seeding runs shrinks.** jobs has 3 seeding sites because the engine, every runner, and the CLI
   each own an embedded store. In jobs-iroh: (a) **jobs-server** seeds its embedded
   amber-store-core on startup (before serving; warn-don't-fail, push semantics collapse away —
   there is no "central", the server *is* the store); (b) **jobs-client** seeds its local embedded
   store for local builds (the `localBootstrap` analogue, non-fatal); (c) **jobs-runner** needs no
   seeding *if* it resolves fetcher/shell artifacts through the server's CAS over the
   `jobs-runner-amber` ALPN — the runner's `EphemeralSigner` local-seed path
   (runner/runner.go:190-201) exists only because jobs runners own private stores. If the
   jobs-iroh runner keeps a local materialization cache keyed by content, no refs and no signer are
   needed runner-side.
5. **Signing can likely disappear or simplify.** `SignAndPut` exists for amber's
   signed/ownership-aware refs across federation. Single-server jobs-iroh can use whatever ref
   write amber-store-core exposes; keep the ref *names* identical.
6. **Port the fetcher contract verbatim**: `fetch` entrypoint at artifact root,
   `JOBS_FETCH_PARAMS` JSON, `JOBS_OUTPUT_DIR`, optional `JOBS_SECRETS_FILE`, exit 0/75/other,
   ingest output honoring `.amberignore`, `import-output:<K>` result. Port `fetchers/` +
   `tarextract` verbatim (no store deps). Keep the hard-fail-on-missing-named-fetcher rule
   (importjob.go:152-157) and the seed-names-only rule for string fetchers
   (recipe/value.go:150-158).
7. **Port `SeedFetcherNames()` into recipe evaluation** (`EvalConfig.SeedFetchers`,
   runner/buildeval.go:155) — it is the single switch deciding string-name vs FetcherDef.
8. **Carry the flake pieces**: `packages.shell` (flake.nix:91-102) and `muslRuntime`
   (flake.nix:73-91) must exist in jobs-iroh's flake for gen-seed.sh to work; copy gen-seed.sh
   with path adjustments.
9. **Shell artifact + `/jobs/shell` compat symlink** must be reproduced wherever jobs-iroh's
   sandbox assembles roots (plugin sandbox, run, image) — fixed `#!/jobs/shell/bin/bash` shebangs
   in plugin/scripts depend on it.
10. **Remote-first fetcher-ref reads (`ReadKeyFresh`) lose their purpose** in a single-store
    design — a plain ref read on the server suffices; runners never read refs at all if resolution
    happens server-side.

**Verdict on blob reuse:** the seed blobs can be reused **byte-for-byte** — they are transport
format (tar+zstd), not store format; amber-store-core will simply derive its own keys at ingest.
Only if amber-store-core's ingest cannot accept the extracted trees (e.g. it rejected symlinks)
would regeneration matter, and that would be a store-API problem, not a blob-format one.
