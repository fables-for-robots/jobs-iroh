# jobs runner build machinery — subsystem map for the jobs-iroh port

Source: `/home/dragan/fables-for-robots/jobs/runner` (+ `/home/dragan/fables-for-robots/jobs/amber`, the store seam).
All paths below are relative to `/home/dragan/fables-for-robots/jobs` unless absolute.

## 0. What is current vs. legacy (actor-scheduler cutover, 2026-07-17)

The **stage drivers are current**; the WS engine loop around them is **legacy** (unreferenced by
binaries). The production runner binary is `runnerlink/runnerlink.go` → `sched/session/execcore.go`,
which calls the drivers directly:

- `sched/session/execcore.go:143-164` — the ONLY production dispatch:
  `RunImport` / `RunBuildFrom` / `RunPluginResolve` / `RunPin` / `RunBuild` keyed by parsed grain kind.
- `sched/session/execcore.go:207-252` — `captureRefWriter`: the current `RefWriter` impl
  (push objects to central, *collect* name+key; the server-side grain signs+publishes).
- `sched/session/execcore.go:146` — **note:** the actor path currently passes `nil` secrets and
  `nil` events into `RunImport`, and `execcore.buildRunCfg` (`execcore.go:182-198`) leaves
  `BuildRunCfg.Events` nil — live per-job events are NOT wired on the actor path today. jobs-iroh's
  NATS-publish plan re-adds what the legacy WS path had.

**Legacy files** (WS runner loop; port nothing from these except noted helpers):

| File | Status |
|---|---|
| `runner/runner.go` | LEGACY: WS `Config`/`Run`/`conn`/`serve`/`onWelcome`/`onOffer`/`runJob` (`runner.go:155,447,622`). Still hosts current helpers: `resolveShellKey` idea (`runner.go:239`), `effectiveMemBytes` (`runner.go:737`), `nodeString` (`runner.go:727`). |
| `runner/refwriter.go` | Interface + `SignerRefWriter` + `GrantHolder` are CURRENT (`refwriter.go:46,68,119`); `wsRefWriter` (`refwriter.go:203-338`) is LEGACY (kept as the reference for batch/ack/push semantics). |
| `runner/wipe.go` | LEGACY (`conn.localWipe`); runnerlink re-implements wipe-epoch reset (`runnerlink/runnerlink.go:431-447`). |
| `runner/pullprogress.go`, `runner/pushprogress.go` | Wired only by the legacy `runJob`/`wsRefWriter`; the throttling pattern (1 s) is worth porting into NATS publishes. |
| `runner/metrics.go` | LEGACY (WS conn metrics). |
| `runner/procs.go` | Registry (`registerJobCgroup`, `procs.go:22`) is CURRENT (executors call it); `snapshotJobProcs` (`procs.go:37`) is consumed only by the legacy `conn.onProcListReq` + `wire.Proc`. |

Everything else in `runner/` (stage drivers, executors, sandbox assembly, store mount, caches,
fetcher resolution, develop/run/image) is current.

## 1. Shared types & conventions

- **Outcome** (`runner/importjob.go:30-40`): `{OutputKey, Decline+DeclineReason, Cancelled, Failed,
  Class "hard"|"retryable"|state.ClassControl, ExitCode, Phase, Stderr}`. Constructors
  `retryable(phase,err)` (`importjob.go:42`), `hard(phase,msg,exit)` (`importjob.go:65`),
  `pushOutcome(phase,err)` (`importjob.go:52`) — the shared publish-tail classifier:
  - `errRefsNotAssigned` (`refwriter.go:323`) → `Outcome{Cancelled:true}` — a lost
    duplicate-execution race ends in **silence**, never a failure (issue #152).
  - `errRefsAckTimeout` (`refwriter.go:328`) → `Class = state.ClassControl` — control-plane verdicts
    must not burn the job's retryable budget (issue #153).
- **exit 75 = EX_TEMPFAIL = retryable**, for fetchers (`importjob.go:21,210-213`) and plugins
  (`plugincaller.go:217-218` → `recipe.PluginError{Retryable:true}` → retryable at
  `buildeval.go:159-162`). The **build script itself has no 75 rule** — any non-zero exit is hard
  (`buildrun.go:114-116`).
- **BuildRunCfg** (`runner/buildeval.go:19-30`): `{Platform, ShellKey, CacheDir, MemoryMaxBytes,
  Events *events.Job}` — the per-attempt config for the four F/K build stages.
- **RefWriter** (`runner/refwriter.go:46-64`): `WriteRef(name,k)`, `WriteRefs([]Ref) (PushTotals,
  error)` (atomic batch — needed where refs must be validated together), `RemoteSync()` (never nil;
  `amber.DisabledRemoteSync` when local-only). `Ref{Name, Key, Label}` (`refwriter.go:19-27`) —
  Label is display-only (`build-pinned:F` only), never identity.
- **Phases** (event vocabulary, also `Outcome.Phase`): import = pulling → resolving → fetching →
  ingesting → pushing; build = assembling → seeding → materializing → building → finalizing →
  pushing; plus "pulling build-from tree"/"pulling build def" wrappers.
- Node id strings come from `state.NodeID{Kind,K}.String()` (`runner.go:727-731`), used only for
  cgroup-registry keys + logs.

## 2. RunImport — `runner/importjob.go:87-231`

Inputs: `k` = import-def key; store; RefWriter; Executor; cacheDir; secrets map; events.

1. **Pull def** (`importjob.go:92-104`): `ensureImportDef` (`importjob.go:74`) reads ref
   `import:<K>` through `amber.ReadKey(st, rs, …)` — the ref pull drags the def-blob closure over
   on a cross-daemon miss — then `st.File(k)` + `importdef.Decode`. Decode failure = hard.
2. **Resolve fetcher** (`importjob.go:112-159`):
   - `def.FetcherDef` set (recipe-declared fetcher): compute the fetcher build's key
     `(builddef.Input{KindBuild, FetcherDef}).Key()` then **two-hop content resolve**
     `amber.ResolveBuildArtifact(st, rs, fk)` (`amber/buildfrom.go:95-120`): `build-from:K_f → F →
     build-output:F` with a direct `build-output:K_f` fallback (local builds), then descend `c/`.
     `ErrNoArtifact` (no `c/`) = hard forever; not-yet-resolvable = retryable (sync race — the
     scheduler gated this import on that build). Then `ResolveFetcherArtifact`
     (`runner/fetcher_linux.go:50-92`): materialize (default) or FUSE-mount the artifact under a
     temp dir in cacheDir; root must hold `./fetch`.
   - Named fetcher: `ResolveFetcher` (`fetcher_linux.go:22-42`) resolves ref
     **`fetcher:<name>:<platform>`** — REMOTE-FIRST via `amber.ReadKeyFresh` (mutable ref;
     `amber/remotesync.go:189-214`, 10 s stall + 1 min per-name TTL) with local fallback. Platform =
     `def.Platform`, empty = legacy → runner's own `Platform()` (`importjob.go:119-122`). A missing
     named fetcher is **hard** (seed-only names; nothing provisions them anymore,
     `importjob.go:152-157`).
3. **Sandbox env contract** (`importjob.go:161-198`): work temp dir; `outDir = <work>/output`;
   env `JOBS_FETCH_PARAMS` = def params rendered to JSON (`def.ParamsJSON()`), `JOBS_OUTPUT_DIR` =
   outDir; if `def.RequiredTags`: `writeSecrets` (`importjob.go:235-249`) writes
   `{tag: {scope, secret}}` JSON 0600 and sets `JOBS_SECRETS_FILE` — a missing runner-side secret is
   a retryable fail. Stdout/stderr teed to event OutputWriters.
4. **Execute** (`importjob.go:200-214`): `Executor.Run(ExecSpec)` (`runner/executor.go:20-51`).
   Exit 0 ok; **75 retryable**; else hard (both carry the 4 KiB stderr tail). Go error: ctx
   cancelled → `Cancelled`, else retryable.
5. **Ingest** (`importjob.go:216-221`): `amber.IngestSourceDirStats(st, outDir)` — honors
   `.amberignore` emitted by the fetcher (`amber/ingest.go:79-83`, `amber/build.go:98-142`).
6. **Publish** (`importjob.go:223-230`): `WriteRefs([{ "import-output:<K>" → r }])`;
   `emitCAS` (`runner/casreport.go:12`) reports ingest + push totals.

Executors:
- **CgroupExecutor** (Linux prod, `runner/importexec_linux.go:27-123`): user+mount+pid namespaces,
  **NO netns** (host network — imports fetch), **no pivot_root** (`NewRoot:""` — fetcher sees the
  host fs and writes `JOBS_OUTPUT_DIR` directly); best-effort cgroup `jobs-import-<pid>-<nano>`
  with `MemoryMaxBytes` (the offer's resolved limit); env = `os.Environ()` + spec.Env; heartbeats
  every 5 s (`runner/execheartbeat.go:12,23`); registers the job cgroup (`procs.go:22`).
- **Subprocess** (fallback/tests/develop, `executor.go:57-100`): plain `./fetch` child with an
  ETXTBSY retry loop (multi-slot fork/exec race, golang/go#22315).
- `defaultImportExecutor` (`importexec_linux.go:40-51`): userns probe once; degrade to Subprocess
  with a one-time warning.

## 3. RunBuildFrom — `runner/buildfrom.go:19-61` (K → F)

Hermetic, network-free, platform-independent, no sandbox.

1. `ensureBuildDef` pulls ref `build:<K>` for the def closure (`runner/buildfromenv.go:117-123`),
   then `pullAndDecodeDefinition` = `st.File(K)` + `builddef.DecodeDefinition`
   (`runner/buildeval.go:255-267`).
2. `resolveSourceContentTree` (`buildfrom.go:97-151`) — source Input → CONTENT tree key:
   - import → `ReadKey "import-output:<srcK>"` (missing = retryable);
   - build → `ResolveBuildOutputVia` (two-hop) then `c/` subdir (`resolveSubdirKey`,
     `runner/artifact.go:116-131`);
   - tree (subbuild) → `builddef.DecodeTreeKey` + `ensureBuildFromTree("build-from-tree:<tk>")`
     (`buildfromenv.go:105-111`) + `st.Ls` sanity check.
3. `resolveSubtreeKey(def.Dir)` walks slash segments (`buildfrom.go:155-168`); missing dir = hard.
4. **Recipe override normalization** (`resolveRecipeOverride`, `buildfrom.go:73-92`): effective =
   inline `buildJobs` else `env/<buildFile>` (must exist — no fallback) else `env/BUILD.jobs`; the
   override is spliced as top-level `BUILD.jobs` ONLY when it differs from `env/BUILD.jobs` — this
   omission is the cache-JOIN invariant. Shared verbatim with the local path (`localBuildFrom`,
   `runner/localbuild.go:20-44`) so local F == engine F.
5. `amber.BuildFromTree(st, envKey, params, platform, override)` (`amber/buildfrom.go:21-61`)
   assembles the deterministic F-tree `{env/ (by key), params, platform[, BUILD.jobs]}`.
6. **Publish ONE batch** (`buildfrom.go:54-59`): `build-from:<K> → F` **and** the self-referential
   `build-from-tree:<F> → F` MUST travel in the same `WriteRefs` call — engine name-gating
   validates the second against the first within one message. `build-from-tree:F` exists so
   downstream F-keyed stages on另 a different daemon can pull F's closure by an F-derivable name.

## 4. RunPluginResolve — `runner/buildeval.go:35-95` (keyed by F)

1. `loadBuildFromEnv` (`runner/buildfromenv.go:29-97`): `ensureBuildFromTree(F)`, `st.Tar(F,"")` →
   extract whole tree to temp disk; reads `params`, `platform`, recipe = top-level `BUILD.jobs`
   override else `env/BUILD.jobs`; `SourceContentKey` = `env` subtree key. Cleanup removes the tree.
2. `recipe.EvalPlugins(EvalConfig{Platform, Params, Source, SourceContentKey}, recipe)` — eval error
   = hard. Every emitted KindImport is already platform-pinned by the recipe layer.
3. For each plugin AND each resolution dep: `amber.IngestFile(in.Definition)` (objects-before-ref,
   `buildeval.go:61-72`); `writeTreeSourceRefs` (`buildeval.go:229-250`) publishes
   `build-from-tree:<tk> → tk` for every distinct KindTree source among KindBuild inputs (subbuild
   support on fresh daemons).
4. Encode `builddef.PluginResolved{Plugins, Deps}` (empty deps normalized to nil for byte-stable
   CBOR, `buildeval.go:79-83`) → `amber.IngestFile` → `WriteRef "build-plugin-resolved:<F>" → r`.

## 5. RunPin — `runner/buildeval.go:100-223` (keyed by F)

1. `loadBuildFromEnv(F)`; `ReadKey "build-plugin-resolved:<F>"` (missing = retryable) → bytes →
   `builddef.DecodePluginResolved`.
2. **Resolution deps** (`materializeDeps`, `runner/depsmount.go:23-70`): per dep —
   `builddef.ValidateDepName` (path-segment defense), `resolveInputOutputKey` (kind-aware: import
   output tree / build `c/`), `st.Tar` + extract to `<tmp>/<name>`; exposed to Starlark as
   `recipe.DepSource{diskSource, SandboxPath: "/jobs/deps/<name>"}` (`buildeval.go:131-134`,
   `depsmount.go:16`).
3. **Plugin callers** (`buildPluginCallers`, `buildeval.go:294-331`): per plugin — `in.Key()` (fail
   = hard via `pluginKeyError`), resolve output as `import-output:<pk>` first then
   `ResolveBuildOutputVia` (missing = retryable error), read the artifact-root pin bundle
   `fetchers.toml` (`readPluginPins`, `buildeval.go:338-360`; malformed bundle = hard, store I/O =
   retryable), build `recipe.PluginSpec{Caller: SandboxedPluginCaller{…}, Pins}`.
4. `recipe.EvalBuild(EvalConfig{…, SeedFetchers: bootstrap.SeedFetcherNames(), Deps}, recipe,
   callers)` — `recipe.PluginError{Retryable}` → retryable, else hard (`buildeval.go:150-164`);
   `res.Validate()` = hard.
5. **Canonicalization before encoding** (`buildeval.go:170-200`): `builddef.Pinned{Inputs:
   CanonicalPinnedInputs, Env, Script, RuntimeDeps: SortKeys(input keys), Caches: CanonicalCaches
   (sorted by Path, `builddef/cache.go:103-110`), Resources}` — stable-order + dedup is mandatory
   (canonical CBOR preserves array order). `res.Name` deliberately NOT in Pinned.
6. Ingest each pinned input's def (`IngestFile`), `EncodePinned` → `IngestFile` → publish
   `WriteRefs([{ "build-pinned:<F>" → r, Label: res.Name }])` (`buildeval.go:219`) — Label rides
   the wire for display, never identity.

**Plugin sandbox** (`runner/plugincaller.go:91-222`, `SandboxedPluginCaller.Call`):
- Store: `amber.BuildStoreTree([shellKey, pluginKey])` → `provisionStore` at `/jobs/store`
  (FUSE or materialize, same seam as builds, `plugincaller.go:145-153`).
- **`/jobs/shell` compat symlink → `/jobs/store/<shellBOK>`** (`plugincaller.go:157`) so fixed
  `#!/jobs/shell/bin/bash` shebangs resolve.
- `/jobs/source` = materialized source tree, ro bind; `/jobs/deps/<name>` ro binds (sorted for
  determinism); tmpfs `/tmp`; hermetic `/etc` (`runner/hermeticetc.go:26-39` — `hosts` with
  localhost, EMPTY `resolv.conf`, so resolver paths fail fast under net=none).
- Namespaces: User+Mount+PID+Net+UTS+IPC (Net ⇒ net=none). Cwd = plugin's store dir; env
  `PATH=<shellStore>/bin`, `HOME=/tmp`.
- Protocol: CBOR request `{call, source, deps?}` on stdin (`plugincaller.go:54-58`); CBOR response
  on stdout (stdout is protocol — never teed to events); stderr teed to tail + sink. Exit 75 →
  retryable PluginError.

## 6. RunBuild — `runner/buildrun.go:81-164` (keyed by F)

### 6.1 assembleBuildSpec (`buildrun.go:25-75`) — shared with develop
1. `ensureBuildFromTree(F)`; `ReadKey "build-pinned:<F>"` (missing = retryable, "not pinned yet");
   `builddef.DecodePinned` (decode fail = hard).
2. `collectStore` (`buildrun.go:186-239`): resolves pinned inputs → store BOKs,
   **bounded concurrency 8** (`buildrun.go:176`) — first failure wins and cancels the rest. Per
   input:
   - `resolveInputOutputKey` (`buildrun.go:271-314`): import → `ReadKey "import-output:<ik>"`;
     build → `ResolveBuildOutputVia` + `amberfuse.ResolveDirKey(out, "c")`; missing = retryable.
   - `resolveInputDeps` (`buildrun.go:320-356`): build inputs only —
     `ResolveBuildOutputDepsVia(ik)` → `st.Ls(depsTree)` → entry NAMES are hex BOKs
     (`parseStoreEntryKey`, `buildrun.go:359-365`); the closure tree is FLAT (entries ARE the
     transitive set). Missing deps ref = retryable; bad entry = hard.
   - Fills `jobsDeps[name] = "/jobs/store/<BOK>"` (`storePath`, `buildrun.go:168`).
3. Append `ShellKey`; `amber.BuildStoreTree(st, boks)` (`amber/store.go:19-52`) → one deterministic
   union tree, entries named by hex BOK (dirs 0555 / files 0444, deduped, sorted).
4. `seedCaches` (`runner/cachedir.go:41-91`): per declared `PinnedCache` — fresh host temp dir;
   `ReadKey builddef.CacheRefName(id, platform)` = **`build-cache:<id>:<platform>`**
   (`builddef/cache.go:115-117`); miss = cold start (empty dir); hit = `st.Tar` +
   **`tarextract.Extract` (amber-store's LOSSLESS extractor — mtime is hashed, needed for the
   unchanged-skip compare; NOT the runner's lossy `extractTar`)** + `preAgeAtimes` to the epoch
   (`cachedir.go:98-112`) so any in-build access is observable under relatime. Transport error =
   retryable.
5. Returns `BuildSpec{StoreKey, ShellBOK, JobsDeps, SourceKey: F, SourceDir: "env", Env, Script,
   Caches, MemoryMaxBytes}` (`runner/buildexec.go:45-73`) + the decoded Pinned.

### 6.2 NamespaceBuildExecutor (`runner/buildexec_linux.go:247-291` / `assembleSandbox` 92-212)
- Command: `[/jobs/store/<shellBOK>/bin/bash, -e, /build/.jobs-script.sh]` — the script ALWAYS runs
  from a file (`sandboxScriptFile`, `buildexec_linux.go:52-55`; MAX_ARG_STRLEN).
- `/jobs/store` via `provisionStore` (`runner/fusecaps_linux.go:90-124`): FUSE
  (`amberfuse.Mount{DirectMount, CacheDir: workDir, CacheFiles: 4096}`) or materialize
  (`st.Tar` + extract to `<work>/store`) + **read-only bind**. Mode from `JOBS_STORE_MOUNT`
  (`runner/storemount.go:37-49`): default/`materialize` = disk; `auto` = probe (real trial mount,
  `fusecaps_linux.go:52-79`) with fallback; `fuse` = forced, failure fatal. Same switch governs
  build, plugin, and fetcher mounts.
- `$SRC = /build/src` — `st.Tar(F, "env")` extracted WRITABLE; `$out = /build/out` empty writable;
  `HOME=/build`.
- `/build/.jobs-deps.json` always written; env `JOBS_DEPS` inline only when ≤ 64 KiB
  (`maxInlineJobsDeps`, `buildexec_linux.go:60`), `JOBS_DEPS_FILE` always set (`buildEnv`,
  `buildexec_linux.go:297-324`). Structural env (SRC/out/HOME/PATH/JOBS_DEPS*) overrides spec.Env;
  `PATH=<shellStore>/bin` only (static busybox userland — no /nix, no LD_LIBRARY_PATH).
- Cgroup `jobs-build-<pid>-<nano>` with `memory.max = spec.MemoryMaxBytes`, PIDsMax; registered for
  proc snapshots (`buildexec_linux.go:157-167`).
- Hermetic `/dev` (bind null/zero/full/random/urandom + fd/stdin/stdout/stderr symlinks,
  `buildexec_linux.go:221-245`); hermetic `/etc` (§5 above); cache dirs rw-bound at their declared
  paths with **Strictatime** (`buildexec_linux.go:186-191`); tmpfs `/tmp`.
- Namespaces: User+Mount+PID+Net+UTS+IPC (net=none), pivot_root into the assembled root; FUSE
  servers live in the PARENT process, the child inherits the mounts (`buildexec_linux.go:29-33`).
- Events: `materializing` phase + liveness heartbeat during assembly (`buildexec_linux.go:118-122`),
  `building` + cgroup-usage heartbeat while the script runs (`buildexec_linux.go:279-285`); stdout
  teed to os.Stderr + sink; stderr → 4 KiB tailbuf + sink.

### 6.3 Finalization (`buildrun.go:106-163`)
- Non-zero exit → hard w/ stderr tail; Go error + ctx cancelled → Cancelled; else retryable.
- **Success only**: `finalizeCaches` (`runner/cachedir_linux.go:27-68`): prune files with
  `atime < buildStart-2 s` (`pruneCache`, atime via `unix.Lstat`), drop empty dirs; pruned-to-empty
  → keep last published state (no ref); `amber.IngestDir` and if key == SeedKey → unchanged, no ref;
  else queue `Ref{build-cache:<id>:<platform> → c}`. A failed build never touches shared cache
  state (`removeCacheDirs` on defer, `buildrun.go:88`).
- `finalizeOutput` (`buildrun.go:372-393`): temp `T/c/` = copy of `$out` (`os.CopyFS`),
  `amber.IngestDirStats(T)` → output key r. No `meta` sibling. Removes the executor work root.
- **Runtime closure**: `runtimeDepInputs` (`buildrun.go:244-264`) maps each `Pinned.RuntimeDeps`
  key back to its PinnedInput (absent = hard, malformed description); `collectStore` for their
  BOKs+closures; `amber.BuildStoreTree` → depsKey.
- **Publish ONE ordered batch** (`buildrun.go:153-158`): `[cacheRefs…, build-output-deps:<F> →
  depsKey, build-output:<F> → r]`. Order is a standing invariant: RefWriter impls publish
  sequentially, stop on first error ⇒ deps land BEFORE output; doneness is derived from
  `build-output:F` alone, so a crash between the two never yields a done-looking build with a
  missing closure. Cache refs first (they gate nothing).

## 7. RefWriter implementations (publish semantics)

- **SignerRefWriter** (`refwriter.go:68-110`, local paths: develop/run/image): `signer.SignAndPut`
  per ref (`amber/ref.go:71-95` — build reference record, sign, `st.PutRef`, then
  `RemoteSync.PushAfter` if attached). No batch constraint. `RemoteSync()` normalizes nil →
  `DisabledRemoteSync`.
- **captureRefWriter** (CURRENT engine path, `sched/session/execcore.go:207-252`): per ref, if sync
  enabled → `Embedded.Underlying().PushTree(CentralRemote, key, remotesync.Opts{})` (objects
  before ref), then append `msg.RefEntry{Name, Key}`; the grain signs+publishes after AttemptDone.
  NOTE: batch atomicity degenerates to sequential collection — name-gating happens server-side on
  the whole collected set. Local bookkeeping refs `import:<K>` / `build:<K>` are written by
  ExecCore itself when the def travels in-band (`execcore.go:121-140`), so `ensureImportDef`/
  `ensureBuildDef` resolve without a remote round-trip — the jobs-iroh server should do the same.
- **wsRefWriter** (LEGACY, `refwriter.go:203-317`): local ephemeral put per ref → per-ref
  `PushTree` to central under a **stall-watchdog context** (`amber.NewStallContext`,
  `amber/stallctx.go:21-40`; window `JOBS_SYNC_DEADLINE`, default 120 s; byte-level `OnBytes`
  touches keep slow-but-moving transfers alive — issue #135) with capped exp backoff + jitter
  (`retryPush`, `refwriter.go:164-185`) → ONE wire frame for the whole batch → ack wait
  (default = sync deadline). Keep: stall-not-wallclock deadline, jittered retries, the
  not-assigned / ack-timeout classification.

## 8. Fetcher resolution details

- `ResolveFetcher` (`runner/fetcher_linux.go:22-42`): ref `fetcher:<name>:<platform>`,
  remote-FIRST (`ReadKeyFresh`) because the ref is MUTABLE; miss → `found=false`.
- `ResolveFetcherArtifact` (`fetcher_linux.go:50-92`): temp `fetcher-*` dir under cacheDir; FUSE
  (`JOBS_STORE_MOUNT=fuse|auto`) else `materializeStore` (`runner/storemount.go:55-68` — `st.Tar` +
  `extractTar`); cleanup unmounts/removes.
- Two-hop content resolve for recipe-declared fetchers: `amber.ResolveBuildArtifact`
  (`amber/buildfrom.go:95-120`), see §2.
- The runner's own `extractTar` (`runner/fetcher.go:24-63`) is LOSSY (no mtime restore; dirs get
  |0700) — fine for exec/materialize use, NEVER for cache seeding (see §6.1).

## 9. RunDevelop and the local drivers (jobs-client's "same code paths")

- `RunDevelop` (`runner/develop_linux.go:331-377`): `prepareDevelop` (`develop_linux.go:208-246`) =
  `localBuildFrom` (`runner/localbuild.go:20-44`: `IngestSourceDir` → env subtree →
  `BuildFromTree`; **no `build-from:K` bridge — F IS the identity for local builds**) →
  `driveFStages(f, runFinal=false)` → `assembleBuildSpec` → PTY interactive
  `bash --rcfile /build/.developrc -i` with a recursive host `/dev` bind relaxation
  (`develop_linux.go:262-294`); script written to `/build/build.sh`. Caches mounted warm but
  **never uploaded** (`develop_linux.go:339-343`).
- `developDriver` (`develop_linux.go:39-187`): depth-first ensure of inputs, cycle detection,
  cached-checks via ref existence; imports run with `Subprocess{}` and nil events
  (`develop_linux.go:109`); recipe-declared fetcher builds are driven as ordinary deps
  (`develop_linux.go:103-107`).
- `driveFStages` (`runner/localbuild.go:51-111`): the local F-pipeline — skip stages whose refs
  exist (`build-output:F` / `build-pinned:F` / `build-plugin-resolved:F`), `ensurePinDeps`
  (plugins + resolution deps, `localbuild.go:115-144`), `ensureInputs` from Pinned, then optional
  `RunBuild`. All via `SignerRefWriter` over the local ephemeral/user signer.
- `RunFromSource`/`RunByKey` (`runner/run_linux.go:32-108`) + `runEntrypoint`
  (`run_linux.go:120-173`): resolve artifact via `resolveByKeyArtifact` (`runner/artifact.go:42-111`
  — two-hop with direct `build-output:F`/`build-output-deps:F` fallback for local F), materialize
  run store to disk (never FUSE for exec), `/jobs/shell` symlink, host net + host /dev, execute
  `JOBS.entrypoint` (`runner/entrypoint.go:11-38`).
- `BuildImageByKey` (`runner/image.go:75-160`): pure amber-tree → reproducible OCI tar (epoch
  mtimes, sorted store entries, `/jobs/store/<BOK>` layout, optional shell + `/bin/sh` +
  `/jobs/shell` symlinks).

## 10. Capacity detection

`DetectCapacity` (`runner/capacity.go:59-74`): `detectHostCapacity` (Linux,
`runner/capacity_linux.go:19-59`: cgroup-v2 `cpu.max`/`memory.max`, host fallback
NumCPU/MemTotal) → `applyReserve` (`capacity.go:38-52`: reserve = max(500 mCPU / 512 MiB, 10 %),
floored to one `resources.DefaultBuild` slot) → env overrides `JOBS_RUNNER_CPU`/`JOBS_RUNNER_MEM`
verbatim. Per-job memory limit: offer value else per-kind default (`effectiveMemBytes`,
`runner.go:737-742`); enforced as sandbox `memory.max` (best-effort). For jobs-iroh's c1-m2 sizes:
this maps to Capacity{CPUMilli, MemBytes} advertised at handshake (`runnerlink.go:40,277-281`).

## 11. The amber Store seam (what jobs-iroh re-implements over amber-store-core)

**`amber.Store` interface** (`amber/storeapi.go:23-37`) — the exact method set:
`Ingest(ctx, body io.Reader) (client.Stats, error)` · `Ls(ctx, k, path) ([]client.Entry, error)` ·
`Tar(ctx, k, path) (io.ReadCloser, error)` · `File(ctx, ck) (io.ReadCloser, error)` ·
`PutRef(ctx, reference.Reference)` · `GetRef(ctx, name)` (absent ⇒ `client.ErrRefNotFound`) ·
`ListRefs` · `RemoteList` · `RemotePush` · `RemotePull` (absent remote ref ⇒
`client.ErrRemoteRefNotFound`) · `RemoteLsRefs`. Optional extension `bytesProgressClient`
(`amber/remotesync.go:79-86`): `RemotePullBytes`/`RemotePushBytes` (byte-level watchdog feed).

**Helper API the runner actually calls** (all in `jobs/amber`, to be re-based):
- Ingest side: `IngestFile` (`ingest.go:57`), `IngestDir`/`IngestDirStats` (`ingest.go:66,73`),
  `IngestSourceDir(Stats)` (`.amberignore`, `ingest.go:79,89`), `BuildFile`/`FileKey`
  (`build.go:30,63`), `BuildStoreTree` (`store.go:19`), `BuildFromTree` (`buildfrom.go:21`) — all
  built on `ingestVia` (`ingest.go:22-54`: emit fstree objects → amberpack stream → `st.Ingest`).
  Chunking params MUST match the store's ingest defaults for cross-tool dedup (`build.go:24-26`:
  ByteOpts 32/128/256 KiB, item chunker bit width 7).
- Ref side: `GetKey` (`ref.go:123`), `ListNames` (`ref.go:128`), `ReadKey`/`ReadKeyFresh`
  (`remotesync.go:399-413`), `ResolveBuildOutput(Via)` / `ResolveBuildOutputDeps(Via)`
  (`buildfrom.go:66-82`, `remotesync.go:417-430`), `ResolveBuildArtifact` + `ErrNoArtifact`
  (`buildfrom.go:87-120`), `TreeSubdir` (`buildfrom.go:125-144`), `ToKey` (`ref.go:16`).
- Signing: `Signer` (`ref.go:27-120`: `SignAndPut`, `SignRecord`, `SetRemoteSync`, `SSH()`),
  `LoadSigner`, `NewSigner`; `runner.EphemeralSigner` (`runner/ephemeral.go:19-43`).
- Sync: `RemoteSync` (`remotesync.go:92-359`: `GetKey` pull-on-miss, `GetKeyFresh` remote-first w/
  TTL, `PushAfter`, `Pulled`, `AddProgressListener`, `Enabled`), `DisabledRemoteSync`,
  `NewStallContext`/`ErrStalled` (`stallctx.go`), `Jitter` (`remotesync.go:465`),
  `CentralRemote = "jobs-central"` (`remotesync.go:20`).
- Embedded: `OpenEmbedded`/`EmbeddedConfig{Signer, Grant func, Sync}` (`embedded.go:36-56`),
  `Reset` (in-place wipe preserving identity+remotes, `embedded.go:69-89`), `Identity`
  (`embedded.go:104`), `Underlying().PushTree` (the object-push escape hatch,
  `execcore.go:234,266`), `UpsertCentralRemote` (`centralremote.go:17-26`).

**draganm/amber-store type leaks (the port cut-points)** — every import of
`github.com/draganm/amber-store/...` in the subsystem:
- `key.Key` (32-byte content key; `.String()` hex, `.Type()` node kind used by `BuildStoreTree`,
  `store.go:38`) — pervasive: every driver signature.
- `client.{Stats, Entry, RefInfo, PushStats, PullStats, RemoteInfo, RemoteRefInfo, ErrRefNotFound,
  ErrRemoteRefNotFound}` — the Store interface vocabulary (`storeapi.go`).
- `reference.Reference` (+ `SignaturePayload`/`Encode`) — signed ref records (`ref.go:72-95`).
- `remotesync.{Opts, PushStats, DefaultJobs}` — push options/stats (`refwriter.go:264-281`,
  `execcore.go:234`).
- `fstree` / `chunkers` / `amberpack` / `amberignore` — tree assembly + ingest streaming
  (`amber/build.go`, `amber/ingest.go`, `amber/store.go`, `amber/buildfrom.go`).
- `embedded.Store` + `packstore`/`refstore`/`remotes`/`remoteclient`/`tarexport` —
  `amber.Embedded`'s guts (`embedded.go`).
- `sshsign` + `golang.org/x/crypto/ssh` — ref signing (`ref.go`).
- `tarextract.Extract` — LOSSLESS extraction for cache seeding (`runner/cachedir.go:75`).
- `amberfuse` (jobs' own package over the store): `Mount`, `ResolveDirKey`
  (`fusecaps_linux.go:104`, `buildrun.go:300`) — jobs-iroh can drop FUSE entirely and keep only
  the materialize path (`storemount.go:55-68`), replacing `amberfuse.ResolveDirKey` with
  `resolveSubdirKey`/`TreeSubdir` (pure `st.Ls` navigation, already equivalent).

## 12. Ref name formats (complete inventory produced/consumed by this subsystem)

| Ref | Writer | Notes |
|---|---|---|
| `import-output:<K>` | RunImport | raw fetcher output tree (`importjob.go:225`) |
| `build-from:<K>` | RunBuildFrom | K → F bridge (`buildfrom.go:55`) |
| `build-from-tree:<F>` / `build-from-tree:<tk>` | RunBuildFrom / plugin-resolve+pin | self-ref F-closure carrier (`buildfrom.go:56`, `buildeval.go:244`) |
| `build-plugin-resolved:<F>` | RunPluginResolve | PluginResolved blob (`buildeval.go:91`) |
| `build-pinned:<F>` | RunPin | Pinned blob, Label=display name (`buildeval.go:219`) |
| `build-output-deps:<F>` | RunBuild | flat runtime-closure store tree (`buildrun.go:154`) |
| `build-output:<F>` | RunBuild | `{c/}`; doneness == this ref (`buildrun.go:155`) |
| `build-cache:<id>:<platform>` | RunBuild (success only) | the one MUTABLE result ref (`cachedir_linux.go:65`, `builddef/cache.go:115`) |
| `import:<K>`, `build:<K>` | scheduler/bookkeeping | read by ensure* pulls (`importjob.go:75`, `buildfromenv.go:118`); ExecCore writes them locally when defs travel in-band (`execcore.go:127-139`) |
| `fetcher:<name>:<platform>` | seed only | read remote-FIRST (`fetcher_linux.go:23`) |
| `shell:<platform>` | seed | resolved per attempt (`execcore.go:187`) or per connection (`runner.go:216`) |

## 13. tailbuf / events touchpoints (→ NATS publishes in jobs-iroh)

- `tailbuf.New(4<<10)` stderr tails: `buildexec_linux.go:267`, `importexec_linux.go:85`,
  `plugincaller.go:169`, `executor.go:65` — feed `Outcome.Stderr`. Keep as-is.
- `events.Job` surface consumed (all nil-safe): `Phase(string)`, `Output(stream, phase) io.Writer`
  (full stdout/stderr capture), `Heartbeat(phase, cpuMs, mem, memPeak)` every 5 s
  (`execheartbeat.go:12-58`; phase-only variant for tree extraction spans,
  `execheartbeat.go:67-71`), `Progress("pulling", objs, bytes)` throttled 1 s
  (`pullprogress.go:12-80`), `PushProgress(done,total)` throttled 1 s (`pushprogress.go:13-78`),
  `CacheSeeded`/`CacheFinalized` (`cachedir.go:68,88`, `cachedir_linux.go:46-64`),
  `CAS(...)` (`casreport.go:12`), `Started`/`Finished` (legacy `runJob`).
  jobs-iroh: map 1:1 onto core-NATS subjects per job id; the throttle intervals and the
  final-settled-emit-on-stop patterns are the load-bearing parts.

## 14. Stage-by-stage port checklist (store-seam calls per stage)

Legend: seam calls in `code`; jobs' amber helper names — reimplement over amber-store-core.

**Common infra first**
- [ ] `Store` interface (§11 method set) over amber-store-core; error contracts
      (`ErrRefNotFound`, `ErrRemoteRefNotFound` analogues).
- [ ] Ingest builders: `ingestVia`, `BuildFile`, `BuildDir`, `BuildSourceDir` (.amberignore),
      `BuildStoreTree`, `BuildFromTree` — chunking params must match core's ingest defaults.
- [ ] Ref helpers: `GetKey/ReadKey/ReadKeyFresh`, two-hop resolvers
      (`ResolveBuildOutput/Deps/Artifact` + `ErrNoArtifact`), `TreeSubdir`, `resolveSubtreeKey`.
- [ ] `RefWriter` seam (WriteRef / atomic-batch WriteRefs / RemoteSync-or-equivalent);
      keep pushOutcome's three-way classification (silent-cancel / control-class / retryable).
- [ ] Sync layer or its jobs-iroh replacement (iroh streams): keep pull-on-miss,
      remote-first-for-mutable-refs (+TTL), stall-watchdog deadlines (not wall-clock), jittered
      capped backoff, objects-before-ref ordering.
- [ ] Sandbox package (unchanged semantics): 6-namespace rootless sandbox, cgroup v2 best-effort,
      pivot_root, re-exec `Init()` model; hermetic /dev + /etc writers.

**RunImport**
- [ ] `ReadKey("import:<K>")` (closure pull) → `File(K)` → decode importdef.
- [ ] FetcherDef: `Input.Key()` → `ResolveBuildArtifact` (two-hop + fallback + c/) →
      materialize artifact dir. Named: `ReadKeyFresh("fetcher:<name>:<platform>")` → materialize.
- [ ] Env: `JOBS_FETCH_PARAMS` (params JSON), `JOBS_OUTPUT_DIR`, optional `JOBS_SECRETS_FILE`
      (0600, tag→{scope,secret}).
- [ ] Executor: host-net, no-pivot, userns+mount+pid, memory.max; exit 75 retryable / other hard /
      infra retryable / ctx-cancel silent.
- [ ] `IngestSourceDirStats(outDir)` → `WriteRefs([import-output:<K>])`.

**RunBuildFrom**
- [ ] `ReadKey("build:<K>")` → `File(K)` → decode builddef.
- [ ] Source resolve: `ReadKey("import-output:")` / `ResolveBuildOutputVia`+`c/` /
      tree: `ReadKey("build-from-tree:<tk>")` + `Ls`.
- [ ] `Ls`-walk `def.Dir`; recipe-override normalization (JOIN invariant — identical logic in the
      local path).
- [ ] `BuildFromTree(env, params, platform, override)` → F.
- [ ] Atomic batch: `[build-from:<K>→F, build-from-tree:<F>→F]` — server-side name-gate must
      validate them jointly.

**RunPluginResolve**
- [ ] `ReadKey("build-from-tree:<F>")` → `Tar(F,"")` extract → read params/platform/recipe.
- [ ] EvalPlugins; per plugin+dep: `IngestFile(def)`; `WriteRef("build-from-tree:<tk>")` for
      KindTree sources.
- [ ] Encode PluginResolved (deps nil-normalized) → `IngestFile` →
      `WriteRef("build-plugin-resolved:<F>")`.

**RunPin**
- [ ] `ReadKey("build-plugin-resolved:<F>")` → `File` → decode.
- [ ] Deps: validate names, `resolveInputOutputKey`, `Tar`+extract per dep.
- [ ] Plugins: `ReadKey("import-output:<pk>")` else `ResolveBuildOutputVia`;
      `TreeSubdir(artifact, "fetchers.toml")` → `File` → parse pins.
- [ ] Plugin sandbox: `BuildStoreTree([shell, plugin])` → provision `/jobs/store`; `/jobs/shell`
      symlink; `/jobs/source` + `/jobs/deps/<name>` ro binds; CBOR stdio; exit 75 retryable.
- [ ] EvalBuild → canonicalize Pinned (sorted+deduped inputs/RuntimeDeps/Caches) → `IngestFile`
      each input def + the pinned blob → `WriteRefs([build-pinned:<F> (Label)])`.

**RunBuild**
- [ ] `ReadKey("build-from-tree:<F>")`, `ReadKey("build-pinned:<F>")` → `File` → decode.
- [ ] collectStore (bounded 8): per input `ReadKey`/`ResolveBuildOutputVia`+`c/`;
      closures via `ResolveBuildOutputDepsVia` → `Ls` (names = hex BOKs); JOBS_DEPS map.
- [ ] `BuildStoreTree(BOKs + shell)`; caches: `ReadKey("build-cache:<id>:<platform>")` → `Tar` +
      LOSSLESS extract + pre-age atimes.
- [ ] Sandbox: `/jobs/store` ro (materialize default), `$SRC` = `Tar(F,"env")` writable, `$out`,
      script-from-file, JOBS_DEPS file + ≤64 KiB inline, memory.max, strictatime cache binds,
      net=none.
- [ ] Success: atime-prune caches (skip empty/unchanged) → `IngestDir`; output `T/c` copy →
      `IngestDirStats`; runtime closure = RuntimeDeps→inputs→collectStore→`BuildStoreTree`.
- [ ] Atomic ordered batch: `[cache refs…, build-output-deps:<F>, build-output:<F>]`.

**RunDevelop / local client**
- [ ] `IngestSourceDir` → env subtree → `BuildFromTree` (F = identity, no K bridge).
- [ ] driveFStages with ref-existence skip checks (`GetKey` per stage ref), ensure-loops for
      plugins/deps/inputs, SignerRefWriter-equivalent local publisher.
- [ ] assembleBuildSpec + PTY shell w/ /dev relaxation; caches never uploaded.
- [ ] run/image: `resolveByKeyArtifact` (two-hop + direct fallback), materialized run store,
      `JOBS.entrypoint`, reproducible image tar.

**Capacity / sizing**
- [ ] cgroup-aware detect + reserve + floor + env overrides (map to c1-m2 named sizes);
      per-job memory.max from the placement message with per-kind default fallback.
