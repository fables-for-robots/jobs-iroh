# JOBS definition/identity layer — subsystem map for the jobs-iroh port

Scope: `jobs/builddef`, `jobs/importdef`, `jobs/resources`, plus the identity plumbing they
depend on (`jobs/amber` key derivation) and the stage/scheduler call sites that make identity a
cross-layer invariant. All paths relative to `/home/dragan/fables-for-robots/jobs` unless noted.
Module-cache paths for draganm/amber-store refer to
`/home/dragan/go/pkg/mod/github.com/draganm/amber-store@v0.0.3-0.20260630160628-b64348bb6d71`
(repo pins `v0.0.4-0.20260716135919-118a9ba965a8`, go.mod:—`github.com/draganm/amber-store`; the
key/fstree packages are stable across those revs).

This layer is the compatibility keystone: `jobs-build/examples` recipes must produce
semantically identical graphs (same structure, same joins/dedup, same platform forks) after the
re-base onto `amber-store-core`. Absolute K/F **values** will change if amber-store-core's key
formula differs; graph **shape** must not.

---

## 1. The identity model in one paragraph

- **K** = the amber *file key* of a job definition's canonical CBOR (`builddef/definition.go:1-6`).
  One K per import def, build def, or tree ref. Builds embed `platform` ⇒ K is platform-specific
  (`builddef/definition.go:57-64`); imports embed the consuming build's platform since
  import-platform-pinning (`importdef/definition.go:21-29`,
  `docs/superpowers/specs/2026-07-09-import-platform-pinning-design.md:48-56`).
- **F** = the content key of the *build-from tree* (`env/` + `params` + `platform` [+ spliced
  `BUILD.jobs` override]), published as `build-from:K → F` (`amber/buildfrom.go:21-61`,
  `runner/buildfrom.go:19-61`). Equivalent builds (same source content + params + platform)
  share one F — the join/dedup point.
- **BOK** = the artifact *content* key an input mounts at `/jobs/store/<BOK>`
  (`runner/buildrun.go:166-168`): the whole output tree for an import, the `c/` subtree for a
  build (`runner/buildrun.go:266-314`).
- Doneness is derived from refs, never stored: `import-output:K`, `build-from:K → F`,
  `build-output:F`, `build-output-deps:F`, `build-plugin-resolved:F`, `build-pinned:F`,
  mutable `build-cache:<id>:<platform>` (`sched/gate/gate.go:27-35`).

## 2. Hash & key machinery (all of it draganm/amber-store — the primary port cut)

### 2.1 key.Key (32 bytes, structured — NOT a bare digest)

`amber-store/key/key.go:12-18`: `type Key [32]byte`. Layout (`key.go:100-112`):

```
byte 0        : type nibble (high 4 bits) | reserved bit | (lengthSize-1) in low 3 bits
bytes 1..ls   : big-endian minimal payload length (1..8 bytes; zero = single 0x00)
bytes 1+ls..31: BLAKE3-256(serialized) truncated to 31-ls bytes
```

Hash = **zeebo/blake3 Sum256**, truncated (`key.go:55-57`). CAS object types
(`key/type.go:12-18`): Blob=0, FileNode=1, DirLeaf=2, DirNode=3, XattrSet=4. The type nibble and
the length field are part of the key bytes, so **two stores with different type/length
conventions produce different K for identical CBOR**. `key.Parse` validates canonical form
(`key.go:77-87`); `key.String()` = lowercase hex (`key.go:90-92`) — used verbatim in every ref
name and `/jobs/store/<BOK>` path.

### 2.2 FileKey: how definition bytes become K

`amber/build.go:62-65`: `FileKey(data) = BuildFile(data, discard)` — the *same* function ingest
uses (`amber/ingest.go:57-62` routes through `BuildFile`; `amber/ingest_test.go:36-41` asserts
ingest key == FileKey). Pipeline (`amber/build.go:24-60`):

1. CDC chunking: `chunkers.SplitBytes` with `ByteOpts{Min:32Ki, Normal:128Ki, Max:256Ki}`
   (`amber/build.go:24`) — matches amber's own ingest defaults so content dedups across tools.
2. Each chunk → `fstree.EncodeBlob` → `key.New(Blob, len(chunk), chunk)`
   (`fstree/encode.go:54-61`).
3. Chunk keys → `fstree.NewFileIndexBuilder(NewItemChunker(7))` (`amber/build.go:26`) →
   FileNode levels; **a single child collapses: Finish returns the child key un-wrapped**
   (`fstree/index_builder.go:112-135`).

Consequence: a definition < 32KiB (the overwhelmingly common case) has
`K = key.New(Blob, len(cbor), blake3(cbor))`; a large definition (inline `BuildJobs` override,
deeply nested `FetcherDef`) gets a FileNode root key. **K depends on the chunker parameters and
the fstree object encodings** — the port must take K from amber-store-core's own file-ingest
key builder, never an independent hash.

### 2.3 fstree tree encodings (F, BOK, store trees ride on these)

- Objects are encoded with `cbor.CoreDetEncOptions()` + `NilContainerAsEmpty`
  (`fstree/encode.go:11-25`) — RFC 8949 core-deterministic, **bytewise** map-key sort. Note this
  differs from the definitions' encoder (§4.1).
- `Entry` = CBOR map with **integer keys 0-9** (`fstree/encode.go:27-42`): name, raw st_mode,
  uid, gid, mtime(ns), contentKey / linkTarget / rdev / xattrs.
- JOBS's tree assembly (`amber/build.go:144-196`) fills mode/uid/gid/mtime from lstat for
  ingested dirs — but the *synthetic* trees that define identity use fixed metadata:
  - build-from tree: `env/` dir `S_IFDIR|0555`, `params`/`platform`/`BUILD.jobs` files
    `S_IFREG|0444`, uid/gid/mtime zero, entries sorted bytewise (`amber/buildfrom.go:28-59`).
  - store/deps union tree: one entry per BOK named by its hex, `0555`/`0444`, sorted, deduped
    (`amber/store.go:19-51`).
- Source ingests honor `.amberignore` and drop the control files (`amber/build.go:98-142`) —
  load-bearing for F stability (e.g. webui subbuild excludes `node_modules/`).

## 3. builddef — every exported symbol

### 3.1 Input union (`builddef/definition.go`)

| Symbol | Loc | Semantics |
|---|---|---|
| `KindImport`/`KindBuild` | definition.go:17-20 | Input kind tags `"import"`/`"build"` |
| `KindTree` | tree.go:16 | `"tree"`: already-present content, no work; constructed only by recipe `subbuild()` |
| `Input{Kind string; Definition cbor.RawMessage}` | definition.go:44-47 | CBOR keys `kind`, `definition`. Definition holds the **complete canonical CBOR** of the inner def — a build's K content-addresses its entire transitive input set (definition.go:3-5) |
| `Input.Key()` | definition.go:50-52 | `K = amber.FileKey(in.Definition)` — hashes the inner def bytes only (NOT the {kind,definition} wrapper) |
| `TreeInput(k)` | tree.go:25-31 | wraps `treeRef{Key: k[:]}` (CBOR map `{"key": bstr}`) canonically; its FileKey is the tree input's K |
| `DecodeTreeKey(def)` | tree.go:34-40 | recovers the content key (`key.Parse`) |

Wire form for plugins: an Input crosses the plugin boundary as `{kind, definition}` with a
byte-string definition (`recipe/value.go:86-101`, `recipe/value.go:204-205`).

### 3.2 Definition (build) (`builddef/definition.go:57-98`)

```go
type Definition struct {
    Source    Input           `cbor:"source"`
    Dir       string          `cbor:"dir,omitempty"`
    Platform  string          `cbor:"platform"`            // REQUIRED, never omitted
    Params    cbor.RawMessage `cbor:"params"`              // always present; null (0xf6) = no params
    BuildJobs []byte          `cbor:"buildJobs,omitempty"` // inline override recipe
    BuildFile string          `cbor:"buildFile,omitempty"` // recipe path relative to Dir
}
```

- `Canonical()` (definition.go:70-80) re-marshals a fresh literal through the shared canonical
  encoder — omitempty keeps override-less defs byte-identical to pre-field encodings
  (K-stability ledger, §4.2). `Key()` = FileKey(canonical) (definition.go:83-89).
- `DecodeDefinition` (definition.go:92-98); `ParamsValue()` decodes params with
  `DefaultMapType map[string]any` for the Starlark `params` global (definition.go:100-111).
- `BuildJobs`/`BuildFile` are mutually exclusive (enforced at construction:
  `recipe/input.go:131-133`, submit: `sched/httpapi/httpapi.go:246-249`).
- The K→F rule: the override is spliced as top-level `BUILD.jobs` **only when it differs from
  `env/BUILD.jobs`** — that omission is what makes equivalent builds join
  (`runner/buildfrom.go:73-92`).

### 3.3 PluginResolved (`builddef/refs.go:17-53`)

`build-plugin-resolved:F` ref content:

```go
type PluginResolved struct {
    Plugins map[string]Input `cbor:"plugins"`
    Deps    map[string]Input `cbor:"deps,omitempty"` // resolution deps (2026-07-11 design)
}
```

Self-contained (each value embeds a complete def). The build definition is NOT carried — pin
re-reads everything from the F-tree. `EncodePluginResolved`/`DecodePluginResolved`
(refs.go:47-53) use the same canonical encoder. Empty Deps is normalized to nil before encode
(`runner/buildeval.go:78-82`) so a dep-less resolve is byte-identical to the pre-deps encoding.
`ValidateDepName` (refs.go:26-45): non-empty, ≤128 bytes, no `/`/`\`/control chars, not `.`/`..`
(becomes `/jobs/deps/<name>` in the plugin sandbox).

### 3.4 Pinned (`builddef/refs.go:55-96`)

`build-pinned:F` ref content — the resolved job description:

```go
type PinnedInput struct {  // refs.go:60-64
    Name       string          `cbor:"name"` // recipe-chosen JOBS_DEPS key
    Kind       string          `cbor:"kind"`
    Definition cbor.RawMessage `cbor:"definition"`
}
type Pinned struct {       // refs.go:71-78
    Inputs      []PinnedInput     `cbor:"inputs"`
    Env         map[string]string `cbor:"env"`
    Script      string            `cbor:"script"`
    RuntimeDeps [][]byte          `cbor:"runtimeDeps"`        // 32-byte Ks, subset of Inputs
    Caches      []PinnedCache     `cbor:"caches,omitempty"`
    Resources   *PinnedResources  `cbor:"resources,omitempty"`
}
type PinnedResources struct { // refs.go:85-88
    CPUMilli int64 `cbor:"cpu_milli,omitempty"`
    MemBytes int64 `cbor:"mem_bytes,omitempty"`
}
```

- Mount paths are NOT pin-time-derivable (BOK known only after deps build); the runner injects
  `JOBS_DEPS = {name: /jobs/store/<BOK>}` at build time (refs.go:55-59,
  `runner/buildrun.go:49-54,230-238`).
- `RuntimeDeps` must be a subset of Inputs by K (`recipe/recipe.go:249-266` Validate;
  hard failure otherwise `runner/buildrun.go:244-264`).
- `Resources` rides Pinned (F-deterministic) but is scheduling metadata — see §7.
- Env is always a non-nil map from the single writer (`recipe/recipe.go:513-531` returns
  `make(...)`), so empty encodes as `{}` (0xa0), never null. There is exactly ONE producer of
  Pinned bytes (`runner/buildeval.go:193-210`) — keep it that way in the port; canonical CBOR
  does not save you from nil-vs-empty divergence across writers.
- `EncodePinned`/`DecodePinned` refs.go:90-96.

### 3.5 PinnedCache (`builddef/cache.go`)

- `PinnedCache{Path, ID}` cache.go:16-19; carried in `Pinned.Caches`.
- `ValidateCacheID` cache.go:33-41: `^[a-zA-Z0-9][a-zA-Z0-9._-]*$`, ≤128 — **no `:`**, which is
  what makes `build-cache:<id>:<platform>` parseable (cache.go:22-23).
- `ValidateCachePath` cache.go:48-67: absolute, clean, not `/`, not `/build`, not under
  reserved prefixes `/jobs`, `/build/src`, `/build/out`, `/dev`, `/proc`, `/tmp` (cache.go:30).
- `ValidateCaches` cache.go:71-98: per-entry + no dup id/path + no nesting.
- `CanonicalCaches` cache.go:103-110: sort by Path, nil for empty (order canon, §5).
- `CacheRefName(id, platform)` cache.go:115-117; `ParseCacheRefName` cache.go:123-133. The
  **one mutable result ref**; advisory only — affects speed, never output; gate-checks the id
  against the build's own `build-pinned:F` (`sched/gate/gate.go:63-113`) and publishes cache
  refs before output refs (`runner/buildrun.go:148-156`).

### 3.6 Order canon helpers (`builddef/canon.go`)

- `CanonicalPinnedInputs` canon.go:12-29: stable-sort by Name + dedup by Name (names are unique
  JOBS_DEPS keys); nil for empty.
- `SortKeys` canon.go:34-67: sort [][]byte by hex + dedup; for `Pinned.RuntimeDeps`.
- Both MUST run before encoding: canonical CBOR preserves array order, so recipe-order/dupes
  would break byte-identical re-pins (canon.go:8-11, CLAUDE.md "stable-order + dedup").
  `TestCanonicalPinnedInputs_OrderAndDedup` (`builddef/canon_test.go:8-65`) pins the property.

### 3.7 FetcherBuild — the one FetcherDef synthesis (`builddef/fetcherbuild.go:16-42`)

```
FetcherBuild(url, sha256, platform) = Definition{
    Source:   Input{KindImport, canonical(importdef.Definition{
                  Fetcher: "tarball+https",
                  Params:  canonical({"url": url, "sha256": sha256, "strip": 1}),
                  Platform: platform})},
    Platform: platform,
    Params:   canonical(nil),   // CBOR null 0xf6 — NOT empty map 0xa0
}
```

- `strip:1` drops the GitHub archive's leading `<repo>-<ref>/` dir (fetcherbuild.go:13-15).
- **No-params convention**: canonical null; an empty map would derive a different K and break
  join with an equivalent submitted build (fetcherbuild.go:30-33; golden test
  `builddef/fetcherbuild_test.go:35-39`).
- Shared by: the recipe `fetcher(name, url=, sha256=)` builtin (`recipe/fetcher.go:60-68`) and
  plugin-bundle pin injection at rehydration (`recipe/value.go:150-168`) — one synthesis so
  keys agree across layers (recipe-declared-fetchers §3, §7).

## 4. importdef — every exported symbol

### 4.1 Definition (`importdef/definition.go:17-39`)

```go
type Definition struct {
    Fetcher      string          `cbor:"fetcher"`
    Params       cbor.RawMessage `cbor:"params"`               // opaque to JOBS; JSON to fetcher
    RequiredTags []string        `cbor:"requiredTags,omitempty"`
    Platform     string          `cbor:"platform,omitempty"`   // consuming build's platform; in K
    FetcherDef   cbor.RawMessage `cbor:"fetcherDef,omitempty"` // canonical builddef.Definition; in K
}
```

- `CanonicalParams(v)` definition.go:61-67: canonical CBOR of arbitrary value (nil → 0xf6).
- `Canonical()` definition.go:72-81: **sorts + dedups RequiredTags first** (`canonTags`
  definition.go:104-119; empty → omitted). Property test: tag order/dups don't change bytes
  (`importdef/definition_test.go:19-34`).
- `Decode` definition.go:84-90; `ParamsJSON()` definition.go:93-102 renders `JOBS_FETCH_PARAMS`
  (len==0 params → `"null"`).
- **importdef has ZERO amber-store imports** — it is pure fxamacker/cbor. Only `Input.Key()`
  (builddef) touches the store's key math.

### 4.2 omitempty ledger (K-stability contract — reproduce exactly)

Every later-added field is omitempty so pre-existing definitions keep their K:

| Field | Where | Legacy meaning when absent |
|---|---|---|
| `Definition.Dir` | builddef/definition.go:59 | root of source |
| `Definition.BuildJobs` | definition.go:62 | recipe = env/BUILD.jobs |
| `Definition.BuildFile` | definition.go:63 | ditto (test definition_test.go:104-123) |
| `importdef.Platform` | importdef/definition.go:29 | LEGACY-READ ONLY: runner-arch fetcher resolution + run-anywhere placement; never written anew (`runner/importjob.go:119-122`) |
| `importdef.FetcherDef` | importdef/definition.go:38 | named fetcher via `fetcher:<name>:<platform>` ref — valid for seed leaves only; missing named fetcher = hard failure, no provisioning (`runner/importjob.go:152-157`; test definition_test.go:76-91) |
| `importdef.RequiredTags` | importdef/definition.go:20 | no tags |
| `Pinned.Caches` | builddef/refs.go:76 | cache-less pinned byte-identical to pre-cache |
| `Pinned.Resources` | refs.go:77 | resource-less pins byte-identical to pre-resources |
| `PluginResolved.Deps` | refs.go:19 | dep-less resolve byte-identical to pre-deps |

For a green-field port these are still load-bearing: they define which "empty" states are
representable (there is no encoded difference between "no override" and "empty override").

### 4.3 CBOR encoder identity (subtle, port-critical)

Definitions (`builddef/definition.go:24-30`, `importdef/definition.go:43-49`) use
`cbor.CanonicalEncOptions()` (fxamacker v2.9.2, go.mod:10) = **RFC 7049 §3.9 canonical**:
map/struct keys sorted **length-first then bytewise**, shortest ints, no indefinite lengths,
NilContainerAsNull. fstree objects use `cbor.CoreDetEncOptions()` = RFC 8949 **bytewise**
sort + NilContainerAsEmpty (`fstree/encode.go:15-25`). Two different deterministic profiles
coexist; mixing them up silently changes every K or every tree key. RawMessage fields are
spliced verbatim — canonicality of `Params`/`Definition`/`FetcherDef` is the **producer's**
obligation (all producers route through `CanonicalParams`/`.Canonical()`).

## 5. The four build stages × identity artifacts

| Stage | Keyed by | Reads | Writes (name-gated per kind, `sched/gate/gate.go:27-35,143-153`) |
|---|---|---|---|
| build-from | K | `build:K` ref closure → def bytes by K (`runner/buildeval.go:252-267`); source content (import-output:K / two-hop build / tree key) (`runner/buildfrom.go:97-151`) | `build-from:K → F` + `build-from-tree:F → F` — MUST be one WriteRefs batch; the gate only allows build-from-tree matching the same batch's build-from value (`runner/buildfrom.go:42-59`, gate.go:44-60) |
| build-plugin-resolve | F | F-tree env (self-contained; no deps — `sched/grains/stages.go:104-108`) | ingest every plugin/dep Input def; `build-plugin-resolved:F → r` where r = IngestFile(canonical PluginResolved) (`runner/buildeval.go:35-95`) |
| build-pin | F | `build-plugin-resolved:F` blob; materialized resolution deps; plugin artifacts by content (import-output:K → fallback two-hop; `runner/buildeval.go:294-331`) | ingest every pinned Input def; `build-pinned:F → r` (r = IngestFile(canonical Pinned)); display name rides as ref Label, never in bytes (`runner/buildeval.go:193-222`) |
| build (run) | F | `build-pinned:F`; per-input BOK + build-output-deps closures; store union tree | `build-cache:*` (first), `build-output-deps:F`, `build-output:F` last — ordered batch, doneness derives from build-output:F alone (`runner/buildrun.go:148-163`) |

- Scheduler-side unfolding mirrors the same decodes: build-from deps from `Definition.Source`
  (`sched/grains/stages.go:73-86`), pin deps from PluginResolved (stages.go:130-158), run deps
  + ResourceReq from Pinned (stages.go:179-203); `inputDecl` maps Input→node, **skipping
  KindTree** (stages.go:20-38). buildvalue is the non-placeable output-marker chain with
  fast-path joins (`sched/grains/buildvalue.go:13-24`).
- Import stage: def pulled by K via `import:K` ref closure, fetcher resolved by FetcherDef
  two-hop (`ResolveBuildArtifact`: build-from:K_f → F_f → build-output:F_f → `c/`,
  `amber/buildfrom.go:95-120`) or seed name ref; output ingested with `.amberignore` honored;
  `import-output:K → R` (`runner/importjob.go:87-231`).
- **Meta is GONE.** Historical build output was `{c/, meta}` with `meta = {definition}`
  (architecture/build.md:438-499); current output is `{c/}` only — provenance is recoverable
  from F and the build-from tree (`runner/buildrun.go:367-371`, CLAUDE.md "no `meta`"). Do not
  port Meta.

## 6. Platform pinning + FetcherDef enforcement points (all of them)

Rule: every newly created import def carries the consuming build's platform; no "any", no
cross-platform override (`2026-07-09-import-platform-pinning-design.md:43-56`). Construction
points that pin:

1. `imp()` builtin: platform baked from the eval's build platform; transitional `platform=`
   tolerated iff `True` or equal string, else eval error (`recipe/input.go:45-108`).
2. Plugin-response rehydration: every KindImport is re-canonicalized with the build's platform;
   KindBuild recurses into its Source chain; non-seed fetcher names get the bundle's
   FetcherDef injected via `builddef.FetcherBuild` or fail hard (`recipe/value.go:103-179`).
   Creation-time only — frozen PluginResolved/Pinned trees are never rewritten
   (`runner/buildeval.go:53-57`, value.go:110-113).
3. Submit APIs: `/submit` requires platform (400 otherwise), `/submit-build` pins the source
   import to the request platform (`sched/httpapi/httpapi.go:151-181,183-231`).
4. `FetcherBuild` itself instantiates both the build def and its source import at the target
   platform (`builddef/fetcherbuild.go:25,38-41`).

FetcherDef mechanics: `K_f = Input{KindBuild, FetcherDef}.Key()` — derived identically at the
scheduler (`sched/grains/import.go:31-42`, dep edge to buildvalue|K_f) and the runner
(`runner/importjob.go:125-145`, artifact two-hop). N imports sharing one FetcherDef join on one
content-addressed fetcher build. Cycles are unrepresentable (defs are finite CBOR trees,
recipe-declared-fetchers §3). Seed names (`tarball+https`, `hostmusl`, `github`, `shell`) are
the only valid string fetchers.

## 7. resources — scheduling metadata, never identity

`resources/resources.go` (zero deps on state/builddef/amber-store):

- `Resources{CPUMilli, MemBytes int64}` (cbor `cpu_milli`/`mem_bytes`, omitempty) —
  resources.go:21-24; integer units keep it canonical-CBOR safe, float-free (resources.go:1-5).
- Kind strings mirror state kinds (resources.go:28-34); defaults (resources.go:38-42):
  import & light stages (build-from/plugin-resolve/pin) **200m/512Mi**, build run
  **1000m/1Gi**; `DefaultFor` falls back to light for unknown kinds (resources.go:46-57).
- `ParseCPU`: `"200m"` → millicpu, else float cores × 1000 rounded (resources.go:60-77).
- `ParseMem`: binary suffixes checked before decimal (`Ki|Mi|Gi|Ti` then `K|M|G|T`), bare int =
  bytes (resources.go:81-109). Ops: `Max`/`Add`/`Sub`/`Fits`/`IsZero`/`String`
  (resources.go:112-142).
- **Identity boundary** (the invariant to preserve verbatim): CPU/RAM never enters
  `builddef.Definition` or anything hashed into K/F. Recipe `resources=struct(cpu,memory)` is
  parsed (`recipe/recipe.go:414-419,433-460`) into `Pinned.Resources` — which DOES change the
  `build-pinned:F` blob bytes, but that blob is F-keyed, deterministic from F, and never an
  identity input (`builddef/refs.go:80-88`). Submit-API `resources` rides the request only
  (`sched/httpapi/httpapi.go:268-284` → `msg.ResourceReq`). Effective requirement =
  max(per-kind default, Pinned.Resources, API requests) (CLAUDE.md resources bullet;
  `sched/grains/stages.go:198-202` supplies the pinned leg).

## 8. Where draganm/amber-store leaks into this subsystem (port cut-points)

| Leak | Loc | What to do in jobs-iroh |
|---|---|---|
| `key.Key` in Input.Key / Definition.Key signatures | builddef/definition.go:11,50-52,83-89 | swap to amber-store-core's key type; keep `Key()` delegating to the store's file-key builder |
| `key.Key` in TreeInput/DecodeTreeKey (+`key.Parse`) | builddef/tree.go:6,25-40 | same; treeRef stores raw `k[:]` bytes in CBOR |
| `amber.FileKey` (jobs/amber → amber-store fstree+chunkers+key) | amber/build.go:24-65 | THE keystone: K must equal amber-store-core's ingest key for the same bytes; reimplement FileKey as core's dry-run ingest |
| fstree Entry/DirBuilder/ItemChunker for F-tree, store trees, ingest | amber/build.go:70-196, amber/buildfrom.go:21-61, amber/store.go:19-51 | re-express the three synthetic-tree builders over amber-store-core primitives; keep fixed modes 0555/0444 + zero uid/gid/mtime + bytewise entry order |
| `key.Key` throughout stage drivers / grains / gate / refs-by-hex | runner/*.go, sched/* (e.g. runner/buildrun.go:168, sched/gate/gate.go:115-141) | mechanical type swap; ref names embed `k.String()` lowercase hex |
| key.Type() used to type store-tree entries (dir vs file artifact) | amber/store.go:36-40 | needs an equivalent "is this key a file or dir" discriminator in amber-store-core keys, or a different convention |

Not leaked: `importdef` (pure CBOR), `resources` (pure), `builddef/canon.go` (hex on raw
bytes), `builddef/cache.go` (strings), `builddef/refs.go` (RawMessage + [][]byte).

## 9. "Definition hash must equal store ingest key" — the exact points

K is **dereferenced as a CAS file key** (content must be fetchable by K) at:

1. Runner build-from/plugin-resolve/pin: def bytes pulled by K — `st.File(ctx, k)` via
   `pullAndDecodeDefinition` (`runner/buildeval.go:252-278`), after `ensureBuildDef` pulls the
   `build:K` ref closure cross-daemon.
2. Runner import: def bytes pulled by K (`runner/importjob.go:92-104` + `ensureImportDef`
   importjob.go:69-80 via `import:K`).
3. Local paths mirror it: `runner/develop_linux.go:53-56` (IngestFile then Key), jobscli local.

Producers that must therefore ingest the exact canonical bytes under that K:
submit (`sched/httpapi/httpapi.go:139-149` IngestFile + sign `import:K`/`build:K`),
buildactor targets (`sched/buildactor/buildactor.go:113`), session exec dispatch
(`sched/session/execcore.go:122`), plugin-resolve/pin input defs
(`runner/buildeval.go:68-72,201-205`). Because `FileKey` and `IngestFile` share one code path
(`amber/build.go:30-65`, `amber/ingest.go:57-62`), equality is by construction — preserve that
single-code-path property in the port; never hand-roll "blake3 of the CBOR".

Any-32-byte-digest-would-do points (identity used only as a name/join token, never
dereferenced): `Pinned.RuntimeDeps` matching (`runner/buildrun.go:244-264`), grain addressing
(`sched/grains/*` NodeRef.GrainName), ref-name suffixes as pure strings, fetcher cache-dir
keying. They all inherit K anyway; there is no second digest in the system.

F/BOK/R are always real tree keys produced by ingest (BuildFromTree / IngestDir /
IngestSourceDir) — no independent derivation exists for them.

## 10. Compatibility checklist for jobs-iroh (examples must produce identical graphs)

1. One canonical encoder for defs: fxamacker `CanonicalEncOptions()` (RFC 7049 sort); keep
   fstree-side core-deterministic separate (§4.3).
2. No-params = CBOR null 0xf6 everywhere (`builddef/fetcherbuild.go:30-36`,
   `recipe/input.go:111-122` — absent/None params normalize through `CanonicalParams(nil)`).
3. Reproduce the omitempty ledger (§4.2) — defines representable states, not just legacy K.
4. Stable-order+dedup BEFORE encode: PinnedInputs by Name, RuntimeDeps by hex, Caches by Path,
   RequiredTags sorted+deduped (§3.6, importdef/definition.go:104-119).
5. Platform pinning at all four construction points (§6); empty platform is decode-tolerated,
   never produced.
6. `FetcherBuild` byte-exact synthesis (strip:1, tarball+https, null params) — it is the K
   agreement point between recipe `fetcher()`, plugin bundles, and any hand-built def.
7. Build-from join rule: override spliced only when ≠ env/BUILD.jobs
   (`runner/buildfrom.go:73-92`); synthetic-tree metadata fixed (§2.3).
8. Resources/labels/caches stay out of K/F: `Pinned.Resources` and ref Label (display name)
   must never enter hashed def bytes (`runner/buildeval.go:214-219`, refs.go:80-88).
9. `Input.Key()` = store-file-key of inner def bytes only; `{kind,definition}` wrapper is
   never hashed. Tree inputs hash the treeRef wrapper, not the referenced content.
10. Ref-name grammar: `<prefix>:<hex-key>` with lowercase hex; `build-cache:<id>:<platform>`
    relies on ids containing no `:` (cache.go:22-23,115-133); gate rules per node kind
    (`sched/gate/gate.go:27-35`).
