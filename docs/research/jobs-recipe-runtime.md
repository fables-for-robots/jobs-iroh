# jobs Starlark recipe runtime — subsystem map for the jobs-iroh port

Scope: `jobs/recipe` (the hermetic Starlark runtime) + `jobs/plugins/goplugin` (the worked plugin
example), plus the exact runner-side call seam (`jobs/runner/buildeval.go` etc.) and the
`builddef`/`importdef` schemas the runtime constructs. All paths relative to
`/home/dragan/fables-for-robots/jobs` unless absolute. Goal: a fresh engineer can re-implement the
port so `BUILD.jobs` files from `github.com/jobs-build/examples` evaluate identically on
amber-store-core.

---

## 1. Where recipe eval sits in the build pipeline

A build is four scheduler stages (K → F → output). The recipe runtime is invoked in exactly two:

| Stage | Entry point | Runner driver | Output ref |
|---|---|---|---|
| build-plugin-resolve (keyed F) | `recipe.EvalPlugins` | `runner/buildeval.go:35` `RunPluginResolve` | `build-plugin-resolved:F` = canonical CBOR `builddef.PluginResolved` (`builddef/refs.go:17`) |
| build-pin (keyed F) | `recipe.EvalBuild` | `runner/buildeval.go:100` `RunPin` | `build-pinned:F` = canonical CBOR `builddef.Pinned` (`builddef/refs.go:71`) |

Both stages load the same **build-from tree at F** (materialized to disk with `st.Tar`,
`runner/buildfromenv.go:29-97`): `env/` (the source subtree), `params` (canonical-CBOR file),
`platform` (string file), and the effective recipe = top-level `BUILD.jobs` override if present,
else `env/BUILD.jobs` (`runner/buildfromenv.go:77-86`). The `env/` subtree's own content key is
resolved separately (`resolveSubdirKey`, `buildfromenv.go:54`) and becomes
`EvalConfig.SourceContentKey` — the build root that `subbuild()` addresses.

---

## 2. Entry points

### 2.1 `EvalConfig` — `recipe/recipe.go:16-33`

```go
type EvalConfig struct {
    Platform         string               // e.g. "linux/amd64"; stamped into every import
    Params           []byte               // build def params, canonical CBOR (opaque user data)
    Source           Source               // read-only source-tree access (interface, §3.3)
    SourceContentKey key.Key              // amber content key of env/ — subbuild()'s root; zero ⇒ subbuild errors
    SeedFetchers     []string             // import-capable seed names (bootstrap.SeedFetcherNames)
    Deps             map[string]DepSource // materialized resolution deps (pin stage only; nil otherwise)
}
```

`key.Key` here is **`github.com/draganm/amber-store/key`** (`recipe/recipe.go:6`) — port cut-point
(§9).

### 2.2 `EvalPlugins(cfg EvalConfig, recipeSrc []byte) (PluginsResult, error)` — `recipe/recipe.go:129`

- Plugin-free runtime. Execs the whole file (`starlark.ExecFile`, `recipe/recipe.go:75-81`) with
  the base predeclared set (§3), then calls the module-global `plugins()` with **no args**
  (`recipe.go:143`). No `plugins()` function ⇒ empty result, not an error (`recipe.go:140-142`).
- Accepted returns (`recipe.go:148-190`):
  - a `dict {name: Input}` → plugins only;
  - `struct(plugins = {name: Input}, deps = {name: Input})` — both fields optional, **any other
    field name is a hard error** (`recipe.go:158-161`, strict typo guard);
  - anything else → error.
- Every returned Input passes `pinnedInputDict` (`recipe.go:201-219`): value must be `*recipe.Input`,
  key must be a string, and each is re-run through the rehydrator so **no platform-less import
  leaves the evaluation** (import-platform-pinning boundary guarantee).
- Dep names validated with `builddef.ValidateDepName` (`recipe.go:182-186`; rule at
  `builddef/refs.go:26-45`: non-empty, ≤128 bytes, no `/`/`\`/control chars, not `.`/`..` — the
  name becomes the path segment `/jobs/deps/<name>`).
- Result type `PluginsResult{Plugins, Deps map[string]builddef.Input}` (`recipe.go:120-123`).

### 2.3 `EvalBuild(cfg EvalConfig, recipeSrc []byte, plugins map[string]PluginSpec) (BuildResult, error)` — `recipe/recipe.go:285`

- Same base predeclared set, plus two live values (`recipe.go:298-299`):
  - `plugins` = `pluginsMapping{specs, platform, seeds}` (§4),
  - `deps` = `depsMapping{deps: cfg.Deps}` (§6).
- Uses `execRecipeForBuild` (`recipe.go:89-115`): parses the file with
  `syntax.LegacyFileOptions()`, **strips every top-level `def plugins():` from the AST**, then
  compiles with `starlark.FileProgram(f, pre.Has)` — so the recipe's own declaration function
  cannot shadow the injected live `plugins` mapping. Globals are frozen after init
  (`recipe.go:113`).
- Calls module-global `build()` with no args; missing `build()` is an error (`recipe.go:305-308`).
- Return decoded by `decodeBuildResult` (`recipe.go:343-430`). Two accepted shapes:
  - **struct** with required `inputs` (dict {name: Input}, name non-empty — the JOBS_DEPS keys),
    `env` (dict {str: str}), `script` (str), `runtime_deps` (list of Input); optional `caches`
    (dict {mountPath: cacheID} → `[]builddef.PinnedCache`, `recipe.go:538-556`), `resources`
    (`struct(cpu="1000m", memory="2Gi")` → `*builddef.PinnedResources` via `resources.ParseCPU`/
    `ParseMem`, `recipe.go:435-464`, parsers at `resources/resources.go:60,90`), `name` (str,
    display-only — never enters Pinned/identity, `recipe.go:244-247`, `runner/buildeval.go:216-219`
    ships it as the WriteRefs Label);
  - **legacy 4-tuple** `(inputs, env, script, runtime_deps)` (`recipe.go:382-386`) — cannot carry
    caches/resources/name.
- Post-decode boundary pinning (`recipe.go:325-338`): every input and runtime_dep is rehydrated
  with the build platform (same guarantee as EvalPlugins).
- `BuildResult.Validate()` (`recipe.go:253-275`): **runtime_deps ⊆ inputs**, matched by
  `Kind + "|" + Input.Key()` string; plus `builddef.ValidateCaches` (`builddef/cache.go:71-98`:
  id regexp `^[a-zA-Z0-9][a-zA-Z0-9._-]*$` ≤128, absolute clean path, not `/`, not `/build`, not
  under `/jobs /build/src /build/out /dev /proc /tmp`, no dup id/path, no nesting).

### 2.4 Hermetic thread — `recipe/recipe.go:37-39`

`newThread()`: `starlark.Thread{Name: "recipe", Print: func(...){}}` — print is swallowed, there is
**no `load()` resolver** (any `load` fails to resolve), no filesystem, no clock, no network. The
only outward reach a recipe has is: `source.read/exists` (host callback), `deps[...].read/exists`
(host callback), and `plugins[...](...)` (sandboxed subprocess). Everything else is pure value
construction.

---

## 3. Predeclared builtins (base set — both stages) — `recipe/recipe.go:47-72`

Compile-time note: Starlark resolves all names at `ExecFile` time, so **`struct` and `deps` must be
predeclared in BOTH stages** even though `deps` is only usable in `build()` — hence the
`depsUnavailable{}` stub at the plugins stage (`recipe.go:66-70`, `recipe/deps.go:15-30`, whose
`Get` always errors: "deps are not available during plugins()").

| Name | Value | Semantics |
|---|---|---|
| `platform` | `starlark.String(cfg.Platform)` | The build's platform string (`recipe.go:58`). |
| `params` | decoded `cfg.Params` → Starlark (`recipe.go:48-56`) | Via `Definition.ParamsValue()` (`builddef/definition.go:102-111`, string-keyed map decode) then `toStarlark(v, "")` — **importPlatform "" ⇒ params are never rehydrate-pinned**; an Input smuggled through params stays unpinned until the eval-boundary normalization catches it. |
| `source` | `sourceValue{cfg.Source}` (`recipe/source.go:33-73`) | `.read(path) → bytes` (error if missing), `.exists(path) → bool`. Paths slash-separated, relative to build root. |
| `imp` | `makeImp(platform)` (`recipe/input.go:45-109`) | See §3.1. |
| `bld` | `makeBld(platform)` (`recipe/input.go:156-174`) | See §3.2. |
| `fetcher` | `makeFetcher(platform)` (`recipe/fetcher.go:34-70`) | See §3.4. |
| `subbuild` | `makeSubbuild(platform, cfg.SourceContentKey)` (`recipe/subbuild.go:18-41`) | See §7. |
| `struct` | `starlarkstruct.Make` (`recipe.go:65`) | Standard struct constructor. |
| `deps` | stage-dependent: stub / `depsMapping` | See §6. |
| `plugins` | **build() stage only**: `pluginsMapping` (`recipe.go:298`) | See §4. |

### 3.1 `imp(fetcher, params, requiredTags=[], platform=?)` → Input(kind=import) — `recipe/input.go:45-109`

- `fetcher` is either a **string** (must be a seed fetcher name at pin enforcement time; string
  form builds a def with empty `FetcherDef`, resolved via the mutable `fetcher:<name>:<platform>`
  ref — seed leaves only) or a **`fetcher(...)` value** whose `Def` is embedded as
  `importdef.Definition.FetcherDef` (`input.go:65-74,103`).
- `params`: any Starlark value → `fromStarlark` → `importdef.CanonicalParams` (canonical CBOR).
- `requiredTags`: list of strings (runner-tag constraints; sorted+deduped at canonicalization,
  `importdef/definition.go:104-119`).
- `platform=` is **transitional validation only** (`input.go:78-90`): `None` ok; `True` ok;
  a string must equal the build platform; `False` (old "platform-independent") is now an error.
  The def's Platform is ALWAYS the build's platform (`input.go:103`) — part of identity K.
- Constructs `importdef.Definition{Fetcher, Params, RequiredTags, Platform, FetcherDef}.Canonical()`
  (`importdef/definition.go:17-39,72-81`; canonical CBOR via `fxamacker/cbor/v2`
  `CanonicalEncOptions`, all `omitempty` on Platform/FetcherDef/RequiredTags to keep legacy Ks
  stable).

### 3.2 `bld(source, dir="", platform=platform, params=None, buildJobs="", build_file="")` → Input(kind=build) — `recipe/input.go:156-174`

- `source` must be an Input (import, build, or tree). Shared assembly `newBuildInput`
  (`input.go:130-152`): `buildJobs` (inline recipe override content) and `build_file` (alternative
  recipe path relative to dir) are mutually exclusive; `build_file` validated as a clean relative
  path (`recipe/subbuild.go:48-64`). Encodes
  `builddef.Definition{Source, Dir, Platform, Params, BuildJobs, BuildFile}.Canonical()`
  (`builddef/definition.go:57-80`, BuildJobs/BuildFile omitempty).

### 3.3 `Source` interface — `recipe/source.go:11-14`

```go
type Source interface { Read(path string) ([]byte, error); Exists(path string) bool }
```
Implementations: `MapSource` (in-memory, tests, `source.go:17-30`) and the runner's `diskSource`
(`runner/source.go:52-101`) over a tree materialized by `st.Tar` — with **symlink-escape
hardening** (`runner/source.go:57-84`: lexical clean + `EvalSymlinks` must stay under the real
root, because eval runs on the runner HOST, outside any sandbox, and dep trees are untrusted).
**Recipe eval never reads the store by key — the caller materializes the tree to disk first**
(`runner/buildfromenv.go:40-52`, `runner/source.go:25-47`).

### 3.4 `fetcher(name, url=, sha256=, build=None)` → Fetcher value — `recipe/fetcher.go:34-70`

- Exactly one of `url+sha256` or `build=` (`fetcher.go:45-49`).
- `url+sha256` sugar ⇒ `builddef.FetcherBuild(url, sha256, platform)`
  (`builddef/fetcherbuild.go:16-42`): a build whose Source is a `tarball+https` import with params
  `{url, sha256, strip: 1}` pinned to the platform, **no build params = canonical CBOR null
  (0xf6), not empty map (0xa0)** — this exact synthesis is shared with plugin-pin injection so keys
  agree across layers.
- `build=` accepts an explicit `bld(...)` Input for exotic fetchers.
- The Fetcher value (`fetcher.go:16-19`) carries `{Name, Def cbor.RawMessage}`; `imp()` embeds Def
  as the import's FetcherDef, making the fetcher an ordinary build dependency of the import.

---

## 4. The `plugins` mapping and Input rehydration

### 4.1 `pluginsMapping` — `recipe/plugin.go:21-86`

`plugins["go"]` → a builtin bound to that plugin's `PluginCaller` (missing name ⇒ not-found, i.e.
Starlark KeyError). Calling it (`plugin.go:60-86`):
1. **kwargs only** — positional args error (`plugin.go:62-64`).
2. kwargs converted Go-ward with `fromStarlark` (`recipe/value.go:184-243`; note `*Input` →
   `{kind, definition}` map, `plugin.go:65-73` — Inputs can ride INTO a plugin request).
3. `spec.Caller.Call(map[string]any)` (§5).
4. Response converted Starlark-ward with the **enforcing rehydrator**
   `rehydrator{platform, pins: spec.Pins, seeds, enforce: true}.convert(resp)` (`plugin.go:80`).

### 4.2 Wire form and rehydrator — `recipe/value.go`

- `asInputSpec` (`value.go:88-101`): a decoded map with **exactly two keys** `kind` (string,
  "import"|"build") and `definition` ([]byte) is recognized as an Input wire form anywhere in a
  converted value tree (`value.go:62-69`) — this is how plugins return Inputs.
- `rehydrateInput` (`value.go:114-179`), creation-time only, never applied to frozen stored trees:
  - KindBuild: recurse into `Definition.Source` chain (so a plugin-emitted sub-build's import gets
    the same treatment); stop at KindTree; re-canonicalize only if changed.
  - KindImport: (a) stamp `def.Platform = platform` when differing (`value.go:146-149`);
    (b) **enforce=true path**: an import with empty FetcherDef whose fetcher is NOT in `seeds` must
    match an entry in the plugin's bundled pins — then
    `builddef.FetcherBuild(pin.URL, pin.SHA256, def.Platform)` is injected as FetcherDef
    (`value.go:150-169`); no pin ⇒ hard error "not in the plugin's bundled fetchers.toml and not a
    seed fetcher" (`value.go:156-158`).
- `toStarlark(v, importPlatform)` (`value.go:30-32`): non-enforcing (params path).
- Scalar conversions both ways: None/bool/string/bytes/ints/float/list/tuple/dict-with-string-keys
  (`value.go:34-84,184-243`).

### 4.3 Seed fetchers

`bootstrap.SeedFetcherNames()` (`bootstrap/seed.go:38-54`) = `tarball+https`, `hostmusl`, `github`
(derived from the seed artifact list at `bootstrap/seed.go:30-35`; `shell` publishes `shell:<p>`
and is not an import fetcher). These are the only names allowed on a string `imp(fetcher=)` or a
pin-less plugin-emitted import.

---

## 5. The plugin protocol (CBOR-stdio subprocess bridge)

### 5.1 Contract seen by a plugin binary

- **Request** (stdin, single CBOR document): `{call: {kwargs...}, source: "<path>", deps?: {name: "<path>"}}`
  — hermetic Linux encoding at `runner/plugincaller.go:54-58` (`deps` omitempty), non-hermetic
  fallback at `recipe/subprocess.go:35-38` (no deps field at all).
- **Response** (stdout, single CBOR document): any CBOR value; decoded with
  `DefaultMapType: map[string]any` (`recipe/subprocess.go:42-48`, mirrored at
  `runner/plugincaller.go:42-48`) so `{kind, definition}` maps rehydrate to Inputs (§4.2).
- **Exit codes**: 0 = ok; **75 (EX_TEMPFAIL) = retryable** (`recipe/subprocess.go:15-16`,
  `runner/plugincaller.go:217-218`); any other non-zero = hard eval failure. Error carrier:
  `recipe.PluginError{ExitCode, Retryable, Stderr}` (`recipe/subprocess.go:20-32`), stderr tail
  bounded 4 KiB (`tailbuf.New(4<<10)`, `subprocess.go:74`, `plugincaller.go:169`). RunPin maps a
  retryable PluginError to a retryable outcome, all other eval errors are hard
  (`runner/buildeval.go:158-164`).
- Stdout is exclusively the protocol channel; only stderr is teed to build events
  (`plugincaller.go:79-82,170-173`).

### 5.2 Two `PluginCaller` implementations

`PluginCaller` interface: `Call(kwargs map[string]any) (any, error)` (`recipe/plugin.go:13-15`).
`PluginSpec{Caller PluginCaller; Pins *FetcherPins}` (`recipe/fetcherpins.go:56-59`).

1. **`recipe.SubprocessPlugin{Dir, SourceDir, Ctx}`** (`recipe/subprocess.go:53-91`): plain
   `exec.CommandContext(ctx, "./plugin")` with CWD = plugin artifact root; **non-hermetic** dev
   bridge (used on non-Linux via `runner/plugincaller_other.go:37-66`, which materializes the
   artifact with `st.Tar` and **fails fast if resolution deps are declared**,
   `plugincaller_other.go:45-47`).
2. **`runner.SandboxedPluginCaller`** (Linux, `runner/plugincaller.go:73-222`) — the production
   path. Sandbox layout (post-pivot, User+Mount+PID+Net+UTS+IPC namespaces ⇒ net=none,
   `plugincaller.go:197-199`):
   - `/jobs/store` — one content-addressed read-only store holding shell + plugin artifacts
     (`amber.BuildStoreTree([shellKey, pluginKey])`, `plugincaller.go:145`, provisioned FUSE-or-
     materialized via `provisionStore`, `plugincaller.go:149`);
   - command = `/jobs/store/<pluginBOK>/plugin`, CWD = that dir, env
     `PATH=/jobs/store/<shellBOK>/bin`, `HOME=/tmp` (`plugincaller.go:175-181`);
   - `/jobs/shell` → symlink to the shell store dir (fixed `#!/jobs/shell/bin/bash` shebangs,
     `plugincaller.go:157`);
   - `/jobs/source` — the materialized source tree, bind-mounted read-only
     (`plugincaller.go:186-189`), and passed as `source` in the request;
   - `/jobs/deps/<name>` — one read-only bind per resolution dep, sorted by name for deterministic
     mount order, announced in the request `deps` map (`plugincaller.go:97-113`; constant
     `pluginDepsDir = "/jobs/deps"` at `runner/depsmount.go:16`);
   - writable tmpfs `/tmp` (`plugincaller.go:195`); minimal hermetic `/etc/hosts` + empty
     `resolv.conf` so resolver calls fail fast under net=none (`plugincaller.go:161-166`).

### 5.3 Plugin resolution + pins loading (RunPin side)

`buildPluginCallers` (`runner/buildeval.go:294-331`): for each resolved plugin Input, key it
(`in.Key()`), resolve its artifact **import-output first, then build-output two-hop**
(`buildeval.go:301-313`; `amber.ResolveBuildOutputVia` = `build-from:K → F → build-output:F`, `c/`
subtree handled in the caller), then load the pin bundle: `readPluginPins`
(`buildeval.go:338-360`) looks up **`fetchers.toml` at the artifact root** (`amber.TreeSubdir`),
absent ⇒ nil pins; present-but-malformed ⇒ **hard** failure (wrapped `pluginKeyError`); store I/O
⇒ retryable.

`fetchers.toml` format (`recipe/fetcherpins.go:12-52`): TOML `[[fetcher]]` array with required
`name`,`url`,`sha256`; duplicates are an error. Parsed to
`FetcherPins{Entries map[string]FetcherPin{URL, SHA256}}`. These pins are consumed ONLY by the
enforcing rehydrator at pin-stage response rehydration (§4.2) — this is how a plugin artifact
declares the fetchers its emitted imports need (goplugin ships a `gomod` pin, since `gomod` is not
a seed).

---

## 6. Resolution deps (`struct(plugins=..., deps=...)`)

- **Declared** by `plugins()` in EvalPlugins (§2.2); ride `build-plugin-resolved:F` as
  `PluginResolved.Deps` (`builddef/refs.go:17-20`, omitempty ⇒ dep-less resolve byte-identical to
  pre-deps encoding; normalization to nil at `runner/buildeval.go:79-82`).
- **Materialized** before EvalBuild by `materializeDeps` (`runner/depsmount.go:23-70`): per dep,
  re-validate name (defensive — the name becomes a path segment), resolve output kind-aware
  (import → import-output tree, build → `c/` subtree; missing output = retryable — the scheduler's
  pin-edges guarantee deps are built before pin), `st.Tar` + extract to `<tmp>/<name>`.
- **Consumed** two ways:
  1. Starlark: `deps["name"]` → `depHandle` (`recipe/deps.go:74-133`) with `.read(path)`,
     `.exists(path)`, `.path(rel="")` — path() returns the FIXED in-plugin-sandbox path
     `/jobs/deps/<name>[/rel]`, rejecting absolute or `..`-traversing rel (`deps.go:112-129`).
     Unknown name error lists declared names (`deps.go:60-67`).
  2. Plugin sandbox: read-only bind at `/jobs/deps/<name>` + request `deps` map (§5.2), wired via
     `recipe.DepSource{Source, SandboxPath}` (`recipe/deps.go:35-38`), constructed at
     `runner/buildeval.go:131-134`.
- At the plugins() stage `deps` is the erroring stub (`recipe/deps.go:15-30`).

---

## 7. `subbuild(dir, platform=platform, params=None, build_jobs="", build_file="")` — `recipe/subbuild.go:18-41`

- Errors if `SourceContentKey` is zero (`subbuild.go:29-31` — the one behavior gated on the store
  key type).
- `dir` must be a **strict descendant**: non-empty, relative, no `.`/`..`/empty segments
  (`validateDescendant`, `subbuild.go:70-86`) — this is what keeps the sub-build graph acyclic.
- Constructs `builddef.TreeInput(sourceContentKey)` (`builddef/tree.go:25-31`: canonical CBOR
  `{key: <32 bytes>}`, Kind `"tree"` = `KindTree`, `tree.go:16`) as the Source of a build def with
  the given dir — i.e. the sub-build addresses **the parent's own env/ content tree**, narrowed by
  dir at the sub-build's build-from stage. RunPluginResolve/RunPin publish
  `build-from-tree:<tk> → tk` for every distinct KindTree source so a fresh daemon can pull the
  closure (`runner/buildeval.go:229-250`).

---

## 8. What recipe eval can and cannot touch (hermeticity summary)

CAN: `source.read/exists` (host-side materialized tree, escape-hardened);
`deps[...].read/exists/path` (same); `plugins[...](kwargs)` → sandboxed net-none subprocess with
exactly {store(shell+plugin), source ro, deps ro, tmpfs /tmp}; pure value/def construction.

CANNOT: `load()` (no resolver), print (discarded, `recipe/recipe.go:38`), filesystem, network,
clock, env, store-by-key reads (all store access is in the runner caller, pre-materialized). Params
are data, never code. Determinism caveat carried by the design: plugin binaries are trusted to be
deterministic functions of (kwargs, source, deps).

Store operations the SEAM needs (all outside `recipe/`): materialize tree/subtree to disk
(`st.Tar` — `runner/buildfromenv.go:40`, `runner/source.go:34`, `runner/depsmount.go:58`,
`plugincaller_other.go:55`); resolve a subtree's content key (`resolveSubdirKey`,
`buildfromenv.go:54`); `Input.Key()` = file key of definition bytes without storing
(`amber.FileKey`, `builddef/definition.go:50-52`, `amber/build.go:63-65`); ingest definition bytes
(objects-before-ref, `runner/buildeval.go:68-72,201-205`); resolve ref → key + two-hop
build-output (`amber.ReadKey` / `ResolveBuildOutputVia`, `buildeval.go:301-313`); assemble a
store-union tree (`amber.BuildStoreTree`, `plugincaller.go:145`); subtree lookup by name
(`amber.TreeSubdir`, `buildeval.go:339`). Recipe eval itself needs **zero** store operations.

---

## 9. draganm/amber-store leaks — the port cut-points

Direct imports in the subsystem (all are `key` only — the 32-byte content-key value type):

1. `recipe/recipe.go:6` — `EvalConfig.SourceContentKey key.Key` (`recipe.go:23`).
2. `recipe/subbuild.go:7` — `makeSubbuild(platform string, sourceContentKey key.Key)`
   (`subbuild.go:18`) + the comparable-zero check `sourceContentKey == (key.Key{})`
   (`subbuild.go:29`).
3. `builddef/definition.go:11` — `Input.Key()`/`Definition.Key()` return `key.Key` via
   `amber.FileKey` (`definition.go:50-52,83-89`).
4. `builddef/tree.go:6` — `TreeInput(k key.Key)` / `DecodeTreeKey → key.Key` (`tree.go:25-40`;
   `key.Parse` at `tree.go:39`).
5. Transitively load-bearing: `amber.FileKey` (`amber/build.go:63-65`) computes the file key with
   draganm/amber-store's `fstree` + `chunkers` — **ByteOpts{Min 32Ki, Normal 128Ki, Max 256Ki},
   item-chunker bit width 7** (`amber/build.go:24-26`). Identity K of every job = this key of the
   canonical-CBOR definition bytes. The port must reproduce amber-store-core's file-key derivation
   *consistently everywhere K is computed* (client, server, runner); Ks only survive the migration
   byte-for-byte if amber-store-core's chunking/encoding matches these parameters.

NOT leaks: `importdef` is pure CBOR (no amber import — `importdef/definition.go`); `goplugin`
depends only on `importdef` + cbor (`plugins/goplugin/gosum.go:7`); `recipe/plugin.go`,
`deps.go`, `fetcher.go`, `fetcherpins.go`, `source.go`, `subprocess.go`, `value.go`, `input.go`
have no store types at all. The runner-side callers (`buildeval.go`, `buildfromenv.go`,
`source.go`, `depsmount.go`, `plugincaller*.go`) lean on `jobs/amber` helpers + `key.Key`
throughout — that whole seam re-bases onto amber-store-core equivalents (Tar/materialize, FileKey,
IngestFile, ReadKey, TreeSubdir, BuildStoreTree, ResolveBuildOutputVia).

Port recipe: swap `key.Key` for amber-store-core's key type (needs: comparable 32-byte value,
`String()`, `Parse([]byte)`), keep every canonical-CBOR schema byte-identical
(`builddef.Definition`, `importdef.Definition`, `treeRef`, `Pinned`, `PluginResolved`,
`PinnedCache`, `PinnedResources` — all `fxamacker/cbor/v2 CanonicalEncOptions`, all omitempty
choices are identity-load-bearing), and re-point `amber.FileKey` at amber-store-core's file-key
function.

---

## 10. Pin-stage output assembly (what EvalBuild's result becomes)

`RunPin` (`runner/buildeval.go:170-222`): BuildResult → `builddef.Pinned{`
`Inputs: CanonicalPinnedInputs(...)` (sort by Name + dedup, `builddef/canon.go:12-29`),
`Env, Script,`
`RuntimeDeps: SortKeys(K-bytes)` (hex-sort + dedup, `canon.go:34-67`),
`Caches: CanonicalCaches(...)` (sort by Path, `builddef/cache.go:103-110`),
`Resources: res.Resources}` — **stable-order + dedup before canonical CBOR is mandatory**
(canonical CBOR preserves array order; recipe iteration order would otherwise break byte-identical
re-pins). Each input's Definition is ingested (objects-before-ref) and `build-pinned:F` published
with the display name as ref Label (`buildeval.go:201-221`). `res.Name` deliberately never enters
Pinned (`buildeval.go:216-218`).

---

## 11. goplugin — the worked end-to-end example

Recipe side (typical go-build BUILD.jobs):
`plugins()` returns `{"go": <Input for the goplugin artifact>}` (an imp/bld/fetcher-built input);
`build()` calls `plugins["go"](go_sum = source.read("go.sum"))`.

1. RunPluginResolve evaluates `plugins()` → `build-plugin-resolved:F = {plugins: {"go": Input}}`;
   the definition bytes are ingested, tree-source refs published (`runner/buildeval.go:61-94`).
2. RunPin resolves the plugin Input to its artifact tree (import-output or build-output `c/`),
   loads its root `fetchers.toml` (must pin `gomod` — not a seed), builds a
   `SandboxedPluginCaller` (`buildeval.go:294-331`).
3. The call: kwargs `{go_sum: <bytes>}` → CBOR request `{call: {go_sum}, source: "/jobs/source"}`
   on stdin of `/jobs/store/<BOK>/plugin` in the net-none sandbox.
4. goplugin (`plugins/goplugin/main.go:31-69`): decodes the request (accepts go_sum as bytes or
   string, `main.go:41-49`), parses go.sum keeping only unique module-zip lines
   (`"<mod> <ver> h1:..."`, excluding `<ver>/go.mod` lines), sorted by (path, version)
   (`gosum.go:34-60`); per module emits
   `importdef.Definition{Fetcher: "gomod", Params: CanonicalParams({module, version})}.Canonical()`
   wrapped as `{kind: "import", definition: <bytes>}` (`gosum.go:64-77`); responds with a CBOR
   array of `{path, version, input}` (`main.go:52-67`). Any error → exit 1 = hard (parsing go.sum
   has no transient failure mode, `main.go:24-28`).
5. Response rehydration (enforce=true): each `input` map → Input; Platform stamped to the build's;
   FetcherDef injected from the bundled `gomod` pin via `builddef.FetcherBuild`
   (`recipe/value.go:150-169`) — so every module import's K embeds platform + fetcher code, and
   the engine unfolds one shared fetcher build for all of them.
6. The recipe puts the returned inputs into `build()`'s `inputs` dict under recipe-chosen names;
   the runner later mounts each at `/jobs/store/<BOK>` and injects `JOBS_DEPS` name→path JSON.

---

## 12. The exact seam a port needs (distilled)

To make `jobs-build/examples` BUILD.jobs files evaluate identically, jobs-iroh must keep:

1. **`recipe` package semantics verbatim** — it is already store-agnostic except for the two
   `key.Key` references (§9 items 1-2). Type-alias swap; keep the zero-key subbuild error.
2. **Canonical-CBOR schemas + omitempty layout byte-identical** (`builddef`, `importdef`) — they
   ARE identity. Includes: FetcherBuild's `strip:1` + null-params convention
   (`builddef/fetcherbuild.go:30-41`), importdef Platform/FetcherDef omitempty, Pinned/
   PluginResolved omitempty, and the stable-order+dedup canonicalizers (`builddef/canon.go`,
   `cache.go:103`).
3. **The eval-boundary pin guarantee**: imp() stamps at construction; the enforcing rehydrator
   stamps + injects pins on plugin responses; `pinnedInputDict` / EvalBuild post-pass normalize
   everything else (`recipe/recipe.go:194-219,317-338`).
4. **The two-stage protocol against the F-tree**: {env/, params, platform, BUILD.jobs override}
   layout (`runner/buildfromenv.go:28-97`), EvalPlugins with no plugins/deps, EvalBuild with AST
   `def plugins():` stripping (`recipe/recipe.go:89-115`).
5. **The plugin ABI**: CBOR stdin `{call, source, deps?}` / CBOR stdout / exit 75 retryable /
   stderr-only diagnostics / string-keyed map decode / `{kind, definition}` 2-key wire form /
   sandbox paths `/jobs/store`, `/jobs/shell`, `/jobs/source`, `/jobs/deps/<name>`, tmpfs `/tmp`,
   `PATH=<shellBOK>/bin`, `HOME=/tmp`, CWD=artifact store dir, net=none.
6. **`fetchers.toml`** at plugin artifact root: `[[fetcher]] name/url/sha256`, dup = error,
   absent = seeds-only, malformed = hard pin failure (`recipe/fetcherpins.go:36-52`,
   `runner/buildeval.go:338-360`).
7. **Seed fetcher names** `tarball+https`, `hostmusl`, `github` fed as `EvalConfig.SeedFetchers`
   (`bootstrap/seed.go:45-54`) — the only pin-less names.
8. **Store seam functions** (re-based on amber-store-core): materialize-tree-to-disk, subdir
   content key, FileKey(bytes), ingest-file, ref-read, two-hop build-output resolve, store-union
   tree, subtree-by-name — recipe eval itself needs none of them (§8).
9. **BuildResult surface**: struct{inputs, env, script, runtime_deps[, caches, resources, name]}
   or legacy 4-tuple; runtime_deps ⊆ inputs; cache/resource validation rules (§2.3) — examples use
   the struct form.
