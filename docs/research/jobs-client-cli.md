# jobs end-user CLI subsystem map (`internal/jobscli` + `cmd/`)

Source: `~/fables-for-robots/jobs` (read-only survey, 2026-07-22, post actor-scheduler cutover).
All paths below are relative to that repo unless absolute. This maps what **jobs-iroh's `jobs-client`
must replicate**: local build (`run`), `develop`, `image`, `remote-build`, `status`, plus the store
layout, the local scheduler, and the remote flow. Port cut-points (draganm/amber-store leakage) are
flagged `⚠ CUT`.

---

## 1. Binaries and app wiring

- `cmd/jobs/main.go:15-24` — end-user binary. **`sandbox.Init()` MUST run before the CLI parser**
  (`cmd/jobs/main.go:19`): local build commands re-exec `/proc/self/exe` to enter the namespace
  sandbox; `Init` detects the re-exec'd child and never returns. jobs-iroh's `jobs-client` needs the
  identical first-line call (and any test main driving builds needs it too).
- `internal/jobscli/app.go:58-77` — `ClientApp()` assembles commands:
  `submit, submit-build, status, cancel, delete, remote-build, develop, run, image, auth` +
  `engineOperatorCommands()` (`issue-runner-token`, `revoke-runner-token`, `list-runner-tokens`,
  `clear-store`; `tokens.go:23-30`). Bare `jobs` prints help.
- `app.go:30-53` — `ServerApp`/`RunnerApp`: bare root Action runs engine/runner; global flags are
  deliberately **not** `Required` (urfave/cli validates required globals before subcommand dispatch,
  breaking `jobs-server issue-runner-token`), so the action self-validates
  (`engine.go:102-118`, `runner.go:48-58`). Same pattern applies to any jobs-iroh single-binary +
  subcommands design.
- Logging: interactive CLI keeps human text; service binaries install a JSON slog default via
  `serviceLogger()` (`logging.go:13-17`). Signal handling: every long action wraps
  `signal.NotifyContext(SIGINT, SIGTERM)` via `signalCtx` (`app.go:20-22`).

---

## 2. Command surface + flags

### jobs run (`local.go:146-242`)
`jobs run [build-K] [-- args...]`
Flags (`local.go:152-163`): `--data-dir` (env `JOBS_DATA_DIR`, default `defaultDataDir()`),
`--source` (build this tree then run it), `--dir` (build root inside source), `--build-file`
(recipe path relative to dir, default BUILD.jobs), `--platform` (env `JOBS_PLATFORM`, default
`runner.Platform()` = `GOOS/GOARCH`, `runner/fetcher.go:14`), `--shell-ref` (env `JOBS_SHELL_REF`,
default `shell:<platform>`), `--signing-key` (env `JOBS_SIGNING_KEY`, default ephemeral),
`--user` (signer identity, default "run"), `--tags-file` (JSON `{tag:{scope,secret}}`),
`--param k=v` (repeatable).
Two modes:
- **source mode** (`local.go:181-206`): open store EX → `prepareSourceBuild` (params via
  `importdef.CanonicalParams`, secrets, signer; `local.go:247-277`) → `localBootstrap` (seed) →
  `runner.RunFromSource(...)`. Non-zero entrypoint exit propagates via `cli.Exit("", code)`
  (`local.go:203`, comment at 237-239: urfave runs the action's defers first).
- **by-key mode** (`local.go:210-241`): positional hex K → `key.Parse` ⚠ CUT → seed first
  (`local.go:221-230`: a store populated only by remote-build pulls has the output but no
  `shell:<platform>`) → `runner.RunByKey`.

### jobs develop (`local.go:36-110`)
Same flag set minus output-related ones (`local.go:41-52`; `--user` default "develop").
Action: params → tag secrets → `openClientStore(EX)` → signer (`loadOrEphemeralSigner`,
`local.go:283-292`: real key file or `runner.EphemeralSigner`) → `localBootstrap` →
`runner.RunDevelop` with `runner.DevelopConfig{SourceDir, Dir, BuildFile, Platform, Params,
ShellRef, CacheDir: cs.CacheDir, Secrets}` (`local.go:100-109`). **Holds the store flock for the
whole interactive session.**

### jobs image (`local.go:311-413`)
`jobs image -o <tar|-> [--tag ref] [--no-shell] [--source dir | build-K]`
Flags (`local.go:317-331`): everything `run` has plus `--output/-o` (**Required**; `-` = stdout but
refuses a TTY, `local.go:349-352`), `--tag` (tarball manifest ref; default derived), `--no-shell`
(skip baking shell + `/bin/sh` + `/jobs/shell`). `--user` default "image".
Source mode → `runner.BuildImageFromSource` (`local.go:377-387`); by-key mode → seed →
`runner.BuildImageByKey` (`local.go:406-412`). Output-file close error is captured only when the
build succeeded (`local.go:359-364`).

### jobs remote-build (`remotebuild.go:36-52`)
Flags: `--engine-url` (env `JOBS_ENGINE_URL`, default `http://localhost:8080`), `--data-dir`,
`--source-dir` (**Required**, `.amberignore` honored), `--dir`, `--build-file`, `--platform`
(default host platform), `--param` (repeatable). Full flow in §8.

### jobs status (`client.go:101-135`)
Flags: `--engine-url`, `--target` (job identity K; omit = all targets). Action: GET
`/status[?target=K]`, raw SSE `data:` lines printed verbatim (`client.go:114-135`). It decodes
nothing — the payloads are `httpapi.NodeStatus` JSON (§9). **Note:** the actor-scheduler HTTP API
does **not** mount `/status` (`sched/httpapi/httpapi.go:80-97` routes only `/submit`,
`/submit-build`, `/builds…`, `/requests/{id}/cancel`, `/api/runners`, `/healthz`) — `jobs status`
against a current server 404s; only the `/builds/{id}` view family is really served. jobs-iroh
should design status around the request-scoped snapshot stream, not the engine-era `/status` SSE.

### jobs submit (`client.go:26-92`)
Flags: `--engine-url`, `--fetcher` (**Required**), `--platform` (**Required, no host default** —
deliberate: a raw import targets the fleet; a silent host-arch pin would strand it,
`client.go:34-38`), `--params-file` | `--param`, `--required-tag`. POST `/submit` with
`httpapi.SubmitRequest{Fetcher, Params, RequiredTags, Platform}` (`client.go:67-70`,
`sched/httpapi/httpapi.go:31-36`); prints `SubmitResponse.K`.

### jobs submit-build (`client.go:146-223`)
Flags: `--engine-url`, `--source-fetcher` (**Required**), `--source-param`, `--source-required-tag`,
`--dir`, `--build-file`, `--platform` (default host), `--param`. Body built as a raw map (source
spec type was unexported engine-side, `client.go:188-200`): `{source:{fetcher,params,requiredTags},
dir, buildFile, platform, params}` → POST `/submit-build`; prints K.

### jobs cancel / delete (`cancel.go:21-45`, `delete.go:20-54`)
`cancel <request-id>` → POST `/requests/{id}/cancel`, decodes `{status}` ∈
`cancelled|already-cancelled|already-done|already-failed` (`cancel.go:51-76`).
`delete <request-id>` → DELETE `/builds/{id}` — the build view + logs vanish (memory-only server
state by design, `delete.go:17-19`).

### jobs auth login|logout|status (`auth.go:19-241`)
Device-code flow: POST `/auth/device/start` (anonymous; 409 = auth not enabled) → print/open
`verifyUrl` + `userCode` (best-effort `xdg-open`, `--no-browser` opts out) → poll
`/auth/device/poll` every `interval` (default 2s); 202=pending, 200=token, 410=expired
(`auth.go:64-148`). Token stored per **normalized engine URL** in `<data-dir>/credentials.json`
(`credentials.go:22-52`; 0600, temp+rename, `credentials.go:57-82`). `auth status` checks
`GET /auth/session` (`auth.go:165-199`); `logout` best-effort POSTs `/auth/logout` then forgets
locally (`auth.go:222-237`).

### Operator commands (`tokens.go`, `clearstore.go`)
All plain HTTP against the client API via `doEngine` (admin-gated once auth is armed):
`issue-runner-token NAME` → POST `/runner-tokens`, token printed once (`tokens.go:51-82`);
`revoke-runner-token NAME` → DELETE `/runner-tokens/{name}` expecting 204 (`tokens.go:103-123`);
`list-runner-tokens` → GET `/runner-tokens`, rows `httpapi.RunnerTokenInfo{Name, CreatedAt,
Revoked}` (`tokens.go:143-169`, `enginewire.go:69-73`). `clear-store --admin-token` → POST `/wipe`
with interactive type-the-URL confirmation, 5-minute client timeout, per-component report decode
(`clearstore.go:41-111`).

### Shared HTTP plumbing
`doEngine(req, dataDir, engineURL)` (`credentials.go:96-109`): attaches stored bearer if any and
maps 401 → `"not authenticated — run 'jobs auth login --engine-url …'"`. Every engine-facing call in
the CLI funnels through it.

---

## 3. clientstore.go — embedded store under --data-dir

- `defaultDataDir()` (`clientstore.go:24-33`): `$XDG_DATA_HOME/jobs` → `~/.local/share/jobs` →
  `.jobs-data` fallback.
- Layout: `<data-dir>/store` (embedded amber), `<data-dir>/cache` (fetcher cache),
  `<data-dir>/store.lock` (flock), `<data-dir>/credentials.json`.
- `openClientStore(dataDir, mode, transport)` (`clientstore.go:51-79`): mk cache dir → **flock
  first** → mk store dir → `amber.OpenEmbedded(storeDir, EmbeddedConfig{Sync: true, Grant:
  grants.Provider(), Signer: transport})`. End-user commands pass `transport=nil` → the store's own
  auto-generated transport identity is used (`amber/embedded.go:38-44`). Returns `clientStore{Store,
  Embedded, CacheDir, Grants *runner.GrantHolder, dataDir, release, closeFn}`
  (`clientstore.go:41-49`). Test seam: package var `testStore` bypasses everything
  (`clientstore.go:57-59`, `engine.go:18`).
- Flock (`storelock.go:25-49`): `flock(<data-dir>/store.lock)`, **EX for every store-opening
  command in v1** (SH exists but unused, `storelock.go:12-22`); try `LOCK_NB` first, print
  `"waiting for another jobs process to release the store…"` then block; release = unlock+close;
  kill-safe (lock dies with process). remote-build releases it while the remote build runs (§8).
- `ConnectCentral(ctx, engineURL)` (`clientstore.go:90-103`): keyless handshake —
  `fetchClientGrant` POSTs the store's transport pubkey (`cs.Embedded.Identity()`,
  `amber/embedded.go:104-106`) to `POST /client-grants` (`clientstore.go:121-143`), decodes
  `httpapi.ClientGrantResponse{CentralURL, CentralServerKey, Grant, ExpiresAt}`
  (`enginewire.go:17-22`) → `amber.UpsertCentralRemote(embedded, url, serverKeyWire)` registers the
  `jobs-central` remote (`amber/centralremote.go:17`, const `amber/remotesync.go:20`) →
  `cs.Grants.Set(grant)` makes the grant the per-request credential
  (`runner/refwriter.go:119-134`: `GrantHolder` is an atomic pointer whose `Provider()` feeds
  `amber.EmbeddedConfig.Grant`). Server side mints it at `schedserver.go:256-289` (engine-signed
  short-TTL `read`+`push-objects` grant bound to that pubkey; `--central-public-url` override at
  `schedserver.go:272-275`).
- `PushSourceTree(ctx, k, progress)` (`clientstore.go:109-118`): `Embedded.Underlying().PushTree
  (ctx, amber.CentralRemote, k, remotesync.Opts{Progress})` ⚠ CUT — objects only, **no ref**; the
  server publishes the only ref. Idempotent/incremental; returns `remotesync.PushStats` ⚠ CUT.

⚠ CUT summary for this file: `key.Key`, `remotesync.{PushStats,Opts}`, `Embedded.Underlying()`
escape hatch, SSH-wire pubkey identity, the grant byte-blob format. jobs-iroh re-bases all of these
on amber-store-core; the *shape* (flock + store/ + cache/ + credentials.json + grant-holder) ports
verbatim.

---

## 4. The LOCAL build path (no engine) — the model scheduler

The CLI's local orchestrator lives in **package `runner`**, shared with the fleet runner's stage
drivers. This is the piece jobs-iroh reuses for both `jobs-client` local builds and (potentially)
the server scheduler.

### Entry points
- `runner.RunDevelop(ctx, st, signer, DevelopConfig)` (`runner/develop_linux.go:331-377`)
- `runner.RunFromSource(ctx, st, signer, DevelopConfig, extraArgs, RunIO) (int, error)`
  (`runner/run_linux.go:32-44`)
- `runner.RunByKey(ctx, st, k, platform, shellRef, extraArgs, RunIO)` (`runner/run_linux.go:96-108`)
- `runner.BuildImageFromSource` (`runner/image_source_linux.go:19-37`) /
  `runner.BuildImageByKey` (`runner/image.go:75-92`)
- Non-Linux stubs return `ErrUnsupported` (`runner/develop_other.go:24-26`,
  `runner/run_other.go`, `runner/image_other.go`); `BuildImageByKey` itself is cross-platform (pure
  amber reads, `runner/image.go:71-75`).

### DevelopConfig (`runner/develop_linux.go:26-35`)
`{SourceDir, Dir, BuildFile, Platform, Params (canonical CBOR), ShellRef, CacheDir, Secrets
map[string]TagSecret}` — `TagSecret{Scope, Secret json.RawMessage}` (`runner/importjob.go:24-27`).

### Step 1 — compute F: `localBuildFrom` (`runner/localbuild.go:20-44`)
`amber.IngestSourceDir(srcDir)` (`.amberignore` honored, `amber/ingest.go:85-92`) →
`resolveSubtreeKey(source, cfg.Dir)` (`runner/buildfrom.go:155-168`) →
`resolveRecipeOverride(env, buildFile, nil)` (`runner/buildfrom.go:73-92`: effective recipe =
inline override else `env/<buildFile>` (must exist) else `env/BUILD.jobs`; spliced only when it
**differs** from `env/BUILD.jobs` — the omission is what makes equivalent builds join) →
`amber.BuildFromTree(env, params, platform, override)` (`amber/buildfrom.go:21-61`: F-tree =
`{env/ (by key), params, platform[, BUILD.jobs]}`, deterministic). **A local source has no
submission K; F is the identity** — but the F-tree is byte-identical to the engine's for the same
environment, so local and remote builds join in the shared CAS (`runner/localbuild.go:14-19,33-34`).

### Step 2 — the recursive driver: `developDriver` (`runner/develop_linux.go:37-48`)
State: `{ctx, st, signer, rw RefWriter, brc BuildRunCfg, secrets, visited map[string]bool,
inProgress map[string]bool}`. `rw = NewSignerRefWriter(signer, st)`
(`runner/refwriter.go:74-76`) — local refs are self-signed (`Signer.SignAndPut`,
`amber/ref.go:71-100`); the six stage drivers only ever talk to the `RefWriter` seam
(`runner/refwriter.go:46-64`), which is exactly where the fleet runner swaps in the
engine-acked `wsRefWriter` (`runner/refwriter.go:203+`). **jobs-iroh: the same seam maps to
"publish over NATS + wait for server ack" vs "sign locally".**

Methods (depth-first, memoized by node string, cycle-detected by `inProgress`):
- `ensureInput(in builddef.Input, p)` (`develop_linux.go:52-77`): ingest the definition bytes so
  they are readable by key; dispatch on `Kind` — `KindImport` → decode + `ensureImport`;
  `KindBuild` → `ensureBuild`; `KindTree` → no-op (already-present content).
- `ensureImport(k, idef, p)` (`develop_linux.go:79-117`): done iff `import-output:K` exists
  (`amber.GetKey`); else, if the def carries `FetcherDef`, **first drive the fetcher's build as an
  ordinary `KindBuild` input** (`develop_linux.go:103-107`, recipe-declared-fetchers), then
  `RunImport(ctx, st, rw, Subprocess{}, brc.CacheDir, k, secrets, nil)`
  (`runner/importjob.go:87`). Progress label = `"fetch <fetcher> k=v …"`
  (`develop_linux.go:123-142`).
- `ensureBuild(k, p)` (`develop_linux.go:144-187`): done iff two-hop
  `amber.ResolveBuildOutput(K)` (`amber/buildfrom.go:66-72`: `build-from:K → F →
  build-output:F`); else read+decode `build:K`'s definition, recurse into `def.Source`, run
  `RunBuildFrom(ctx, st, rw, brc, k)` (`runner/buildfrom.go:19-61` — publishes
  `build-from:K→F` **and** self-referential `build-from-tree:F→F` as ONE WriteRefs batch,
  `buildfrom.go:47-59`), then `driveFStages(F, runFinal=true, …)`.

### Step 3 — the F-keyed pipeline: `driveFStages(f, runFinal, p)` (`runner/localbuild.go:51-111`)
The four-stage state machine, with **join checks** (skip any stage whose result ref exists):
1. Full join: if `runFinal` and `build-output:F` exists → done ("cached").
2. If `build-pinned:F` exists → skip plugin-resolve + pin.
3. Else: if no `build-plugin-resolved:F` → `RunPluginResolve(ctx, st, rw, brc, f)`
   (`runner/buildeval.go:35`); then `ensurePinDeps(f)` (`localbuild.go:115-144`): decode
   `builddef.PluginResolved`, `ensureInput` every plugin **and** every resolution dep; then
   `RunPin(ctx, st, rw, brc, f)` (`runner/buildeval.go:100`).
4. `ensureInputs(f)` (`localbuild.go:147-169`): decode `build-pinned:F` (`builddef.DecodePinned`),
   `ensureInput` each `pinned.Inputs` (recursion re-enters ensureImport/ensureBuild).
5. If `runFinal`: `RunBuild(ctx, st, rw, brc, NamespaceBuildExecutor{}, f)`
   (`runner/buildrun.go:81-165`) → publishes `build-output-deps:F` **before** `build-output:F`
   in one ordered batch (`buildrun.go:146-160`: doneness is derived from `build-output:F` alone,
   so a crash never leaves a done-looking build without its runtime closure).
`runFinal=false` is `jobs develop` (stop at pin); `true` is `jobs run`/`jobs image`.

Stage drivers all share signature `func(ctx, st amber.Store, rw RefWriter, brc BuildRunCfg, …)
Outcome`; `BuildRunCfg{Platform, ShellKey, CacheDir, MemoryMaxBytes, Events}`
(`runner/buildeval.go:19-30`); `Outcome{OutputKey, Decline, DeclineReason, Cancelled, Failed,
Class(hard|retryable|control), ExitCode, Phase, Stderr}` (`runner/importjob.go:30-40`), converted
to errors locally by `outcomeErr` (`develop_linux.go:190-201`).

Progress: `runner.Progress` (`runner/progress.go:14-55`) — `Start(label) → done(err)` prints
`→/✓/✗ label elapsed`, `Cached(label)` prints `✓ … (cached)`, `Sub()` indents nested dep builds.
Written to stderr; nil-safe.

**This driver IS the local scheduler**: depth-first, sequential, cache-join everywhere, dedup by
node id, cycle-detected. It is deliberately isomorphic to the engine's unfold: the engine
discovers the same edges (import→fetcher-build, build→source, pin→plugins+deps, build→inputs)
concurrently instead of recursively. A jobs-iroh server scheduler can reuse the edge-discovery
logic (ensurePinDeps/ensureInputs decode steps) with a parallel executor.

### Bootstrap floor
`localBootstrap → localSeed → bootstrap.Seed(ctx, st, signer, bootstrap.SeededPlatforms(), log)`
(`local.go:117-130`), non-fatal warning on failure; publishes `shell:<p>` +
`fetcher:{tarball+https,hostmusl,github}:<p>` refs from the go:embed'd seed. **Every store-opening
local command seeds first** — including by-key run/image (`local.go:221-230, 402-411`).

---

## 5. `jobs run` of built outputs (executing the artifact)

- `resolveByKeyArtifact(ctx, st, rs, k, platform, shellRef, needShell)`
  (`runner/artifact.go:42-111`) → `resolvedArtifact{bokSelf, shellKey, depBOKs, ep}`
  (`artifact.go:19-24`). Resolution order (serves both engine-built K and local F):
  two-hop `build-from:K → F → build-output:F` via `amber.ResolveBuildArtifact`
  (`amber/buildfrom.go:95-120`) with fallback to direct `build-output:K` (local builds have no
  bridge), then the `c/` subtree descent (missing `c/` = `ErrNoArtifact`, hard). Entrypoint =
  `c/JOBS.entrypoint` read by streaming the tree tar (`artifact.go:75-79,136-156`). Runtime
  closure = `build-output-deps` via the bridge, falling back to direct
  `build-output-deps:F` (`artifact.go:84-96`), then `Ls` its entries into dep BOKs.
- `Entrypoint{Command (required), Args, Env}` from JSON `JOBS.entrypoint`
  (`runner/entrypoint.go:11-38`). Relative command resolves against the artifact root; absolute is
  verbatim.
- `runEntrypoint` (`runner/run_linux.go:120-173`): build a run-store union tree
  (`amber.BuildStoreTree(deps + self + shell)`, `amber/store.go:19`) → **materialize to real disk**
  by extracting the store tar under a temp `root/jobs/store` (`run_linux.go:126-140`; FUSE exec is
  unreliable) → compat symlink `/jobs/shell → /jobs/store/<shellBOK>` (`run_linux.go:142-146`) →
  argv = entrypoint command (rooted at `/jobs/store/<bokSelf>` when relative) + ep.Args + extraArgs
  → `sandbox.Run` with `Namespaces{User,Mount,PID,UTS,IPC: true, Net: false}` — **Net=false means
  SHARE the host network** (a server binds host localhost); mounts: tmpfs `/tmp`, recursive-bind
  host `/dev`; cwd = artifact root (`run_linux.go:148-172`). Deliberately NOT hermetic.
- Env (`run_linux.go:179-192`): base `PATH=<self>/bin:<shell>/bin`, `HOME=/tmp`, overlaid by
  entrypoint env (wins). No `JOBS_DEPS` — baked-in `/jobs/store/<BOK>` paths resolve via the
  mounted closure.
- `RunFromSource` = `prepareSourceArtifact` (build via driveFStages(runFinal=true),
  `run_linux.go:53-90`) + the same runEntrypoint; exit code returned, not an error.

---

## 6. develop mode (sandbox shell)

- `prepareDevelop` (`runner/develop_linux.go:208-246`): resolve `shell:<platform>` key (hard error
  with "seed missing — restart to re-seed" if absent) → `localBuildFrom` → `driveFStages(f,
  runFinal=false)` → `assembleBuildSpec(ctx, st, nil, brc, f)` (`runner/buildrun.go:25-75`: decode
  `build-pinned:F`, collect the `/jobs/store` union + `JOBS_DEPS` map, `$SRC` = F-tree `env/`,
  script from `Pinned.Script`, seeded caches).
- `RunDevelop` (`develop_linux.go:331-377`): print cache-mount notices (declared caches mount rw
  warm but are **never uploaded** on exit, `develop_linux.go:338-343`) → `printScript`
  (`develop_linux.go:298-326`: `$SRC`/`$out`, sorted JOBS_DEPS, env, the full build script) →
  `pty.Open()` → host stdin raw-mode (best-effort) → SIGWINCH → size propagation → stdio ↔ PTY
  copy loops → command `[/jobs/store/<shellBOK>/bin/bash --rcfile /build/.developrc -i]`.
- `developRun` (`develop_linux.go:262-294`): `assembleSandbox` with an extra recursive host `/dev`
  bind (develop-only relaxation), writes `/build/build.sh` (the script, 0755) and
  `/build/.developrc` (prompt `(jobs develop) \w \$` + reminder lines, `develop_linux.go:250-253`)
  into the sandbox, wires the PTY as controlling terminal, `sandbox.Run`.

---

## 7. image production (go-containerregistry)

All in `runner/image.go` (+ `image_source_linux.go`). Deps ⚠ external:
`github.com/google/go-containerregistry/pkg/{name,v1,v1/empty,v1/mutate,v1/tarball}`
(`image.go:15-19`).

- Reproducibility: fixed epoch `time.Unix(0,0)` stamped on every layer entry + config
  (`image.go:24`); env sorted (`image.go:56`); store BOKs sorted+deduped (`image.go:289-300`);
  layer opener re-streams a deterministic tar (`image.go:166-175` — read twice: diffID +
  compressed blob).
- `assembleImage(st, selfBOK, depBOKs, shellBOK, ep, platform, includeShell)`
  (`image.go:126-160`): single layer over `empty.Image`; config: OS/Arch from `platform`
  (`os/arch` split validated), `Entrypoint = imageEntrypointArgv` (command `path.Join("/", cmd)` —
  relative rooted at image root; `image.go:29-35`), `Cmd = nil`, `Env = imageEnv`
  (`PATH=/bin[:shell/bin]`, `HOME=/tmp`, entrypoint env wins; `image.go:40-58`),
  `WorkingDir = "/"`.
- Layer content `writeStoreTar` (`image.go:182-231`): (1) artifact tree at image **root**;
  (2) each dep (+ shell when included) at `/jobs/store/<BOK>`; (3) empty `/tmp`;
  (4) with shell: symlinks `/bin/sh → <shellRoot>/bin/sh` and `/jobs/shell → <shellRoot>`.
  Entries normalized: uid/gid 0, fixed mtime; symlink targets preserved (`image.go:237-277`).
- Tarball write `writeImageTar` (`image.go:106-118`): `name.NewTag(tag,
  name.WithDefaultRegistry(""))` — **empty default registry keeps bare `name:tag` verbatim**
  (no docker.io/library normalization) — then `tarball.Write` (docker-load-able, RepoTags set).
- Default tags: by-key `jobs-<K[:12]>:latest` (`image.go:96-101`); source mode
  `lower(basename(SourceDir)):latest` (`image_source_linux.go:33-35`).
- `--no-shell` ⇒ `IncludeShell=false` ⇒ `needShell=false` in artifact resolution — a store with no
  shell ref (remote-build-pull-only) still images (`runner/artifact.go:38-41`).

---

## 8. remote-build flow (the jobs-iroh client↔server model)

`remoteBuildConfig.run` (`remotebuild.go:54-183`) — four phases:

1. **Ingest+push** (`remotebuild.go:185-204`; store open, EX lock): `openClientStore` →
   `ConnectCentral` (**handshake FIRST so an unreachable/central-less engine fails before any
   work**, comment `remotebuild.go:70-71`) → `amber.IngestSourceDir(sourceDir)` → T →
   `cs.PushSourceTree(T, progress)` (objects only, no ref). Progress: `xferProgress` over the
   `liveView` (`liveview.go:128-193`: TTY in-place `[push] n/m objects`; non-TTY quarter lines;
   collapse to `[push] N objects · bytes · dur`). Store then **closed — the flock is not held
   during the remote build** (`remotebuild.go:80-81`).
2. **Submit** (`remotebuild.go:82-91`, `submitTreeBuild` at 256-284): POST `/submit-build` with
   `{source:{tree:T.hex}, dir, buildFile, platform, params}` → `SubmitResponse{K, RequestID,
   EventsURL}` (`httpapi.go:63-69`). Server side (`httpapi.go:197-220`): signs+publishes
   `build-from-tree:T → T` (engine-signed; **422 = client's upload incomplete**), makes a
   `KindTree` source input, spawns the request grain. Prints K + platform.
3. **Watch** (`remotebuild.go:93-146`): its own context tree — SIGTERM cancels `watchParent`,
   SIGINT is diverted to `intCh` to drive the cancel prompt (`remotebuild.go:103-107`). Loop:
   `watchOnce` (`remotebuild.go:292-301`) probes `isSchedServer` (GET `/builds/{id}` == 200,
   `schedwatch.go:36-49`) → `watchSched` (§9), else legacy `watchTarget` on `/status?target=K`.
   On stream error: real error / SIGTERM / non-TTY → `detachExit` ("the build continues remotely;
   re-attach with `jobs status --target K`", `remotebuild.go:307-309`); interactive SIGINT →
   `promptYesNo` (`cancelprompt.go:16-44`; second Ctrl-C = no) → yes ⇒ `cancelRequestHTTP` then
   **re-enter the watch until the terminal cancelled event** (`remotebuild.go:138-145`).
   Terminal mapping (`remotebuild.go:148-162`): `CANCELLED` → exit 1; any non-DONE terminal
   (`Failed`, `FailedUpstream`, `InfraFailed`) → "build FAILED" exit 1 (the target usually derives
   FAILED_UPSTREAM, not Failed — comment 150-156).
4. **Pull** (`remotebuild.go:164-182`, `pullOutput` at 206-241): store **reopened, fresh
   handshake** (the first grant may have expired during the build) — on `watchParent` (SIGTERM
   only) deliberately not the phase-1 ctx (a spent SIGINT must not fail the pull; comment
   167-173). `amber.NewRemoteSync(st, CentralRemote)` → `rs.ResolveBuildOutput(K)` (pull-on-miss
   two-hop) and `rs.ResolveBuildOutputDeps(K)` (runtime closure; only a warning if missing).
   Failure exits with "build succeeded … re-run remote-build to retry the pull". Prints
   `output: <key>` and `done — run it with: jobs run <K>`.

**jobs-iroh mapping:** phase 1 = push over `jobs-amber-admin`/`jobs-runner-amber`-style CAS sync;
phase 2/3 = submit+snapshot-watch over `jobs-build/1.0`; phase 4 = pull over the same CAS ALPN.
The grant handshake disappears if iroh node identity is the credential, but keep the "handshake
fails fast before ingest" ordering and the re-handshake-before-pull semantics (grant/session
expiry across a long build).

---

## 9. status rendering + wire types

Two watch generations coexist in the CLI:

### (a) sched snapshot watch — current server (`schedwatch.go`)
- `watchSched` (`schedwatch.go:54-109`): GET `/builds/{id}/events` SSE; each `data:` is a
  `schedSnapshot{Phase, Cursor, Nodes []schedNodeSnap{Node, Phase, Class, Summary, Runner, Name}}`
  — **declared locally so the CLI needs no sched import** (`schedwatch.go:17-32`; mirrors
  `sched/msg.Snapshot`). Server implementation: cursor-driven poll of the build grain, 250ms
  (`httpapi.go:333-372`).
- Terminal request phases: `done` / `failed` / `cancelled` → `state.Done` / `state.Failed` +
  first hard failure's `Summary` (`schedFailureSummary`, `schedwatch.go:147-154`) /
  pseudo-status `CANCELLED` (`eventswatch.go:16`).
- Node phases folded for display: `done`; `running|publishing` count as running (display name =
  `Name` or `kind:hex8`, `schedwatch.go:157-163`); `failed|failed-upstream`; else waiting
  (`schedwatch.go:112-144`). TTY: two-line block `build: PHASE · dur` + `progress: d/t done, r
  running, w waiting[, f failed] · names…` via `liveView`.

### (b) engine-era `/status` SSE — legacy fallback (`remotebuild.go:315-386`, types in
`sched/httpapi/enginewire.go`)
- `NodeStatus{Node, Status state.Status, Err, Progress *Progress, Cancelled}`
  (`enginewire.go:29-38`); `Progress{Total, Done, Running, Failed, Waiting, RunningNodes
  []RunningNode, QueuedNodes []QueuedNode}` (`enginewire.go:81-93`);
  `RunningNode{Node, Label, Platform, Runner, ElapsedMs}` — **ElapsedMs computed engine-side so
  clock skew can't render nonsense** (`enginewire.go:43-49`); `QueuedNode{Node, Label, Platform,
  CPUMilli, MemBytes, Runner, FreeCPUMilli, FreeMemBytes, Running}` — capacity-starvation report
  (`enginewire.go:55-65`).
- Terminal statuses watched: `state.Done|Failed|InfraFailed|FailedUpstream` + `Cancelled` flag
  (`remotebuild.go:372-380`). TTY block `buildBlock` (`remotebuild.go:413-449`): header
  `[build <platform>] d/t done · r running[ · f failed] · dur`, ≤8 running rows (`maxRunningRows`,
  `remotebuild.go:390`) with `label · arch · runner · dur`, queued rows via `queued.go:81-98`
  (`◷ label` + `queued: needs 9.7 GB / 1 cpu, runner has … free (running: …)`,
  `queued.go:36-55`). Non-TTY: change-gated `status:`/`progress:`/`running:`/`queued:` lines
  (`remotebuild.go:345-370`).
- **The sched server does not mount `/status`** (`httpapi.go:80-97`) — this path is dead against a
  current server; `jobs status` (client.go) prints raw NodeStatus JSON only against an old engine.
- `eventswatch.go` holds a third, richer renderer (`watchNode` fold with output tails, phases,
  heartbeat cpu/mem, stalled detection after 60s silence — `eventswatch.go:20-59,249-346`) that
  belonged to the deleted collector tail; only its helpers (`statusCancelled`, block layout
  helpers) are still load-bearing. Treat it as the design reference for what a rich jobs-iroh
  live view should show: per-node output tail (4 lines / 4KB, `eventswatch.go:87-99`), phase +
  object/byte progress placeholders (`eventswatch.go:71-83`), staleness (`watchStaleAfter`,
  `eventswatch.go:53`), queued/cancelled rows, render throttle 100ms (`eventswatch.go:121`).
- `liveView` (`liveview.go:21-107`): single in-place ANSI block on stderr (cursor-up +
  clear-to-end, width-truncated lines so wrapping never breaks the math), non-TTY degrades to
  append-only, no alt-screen so an interrupt can't wedge the terminal. Port as-is.

### `state.Status` vocabulary (leaf dep `state/`)
`Done, Failed, InfraFailed, FailedUpstream, Waiting/Running…` + CLI pseudo-status
`CANCELLED` (`eventswatch.go:12-16`). jobs-iroh should collapse this to the sched-snapshot phase
vocabulary (`done/failed/cancelled` request-level; node `waiting/running/publishing/done/failed/
failed-upstream/cancelled`).

---

## 10. Server/runner boot (context for the client's counterparties)

- `jobs-server` root action `runSched` (`schedserver.go:34-169`): embedded amber (+`Sync:true`,
  signer = the one long-lived key), pebble for **auth + runner tokens only**, `bootstrap.Seed`
  before serving, goakt cluster, gateway (runner handshakes + grants), `httpapi.API` behind the
  auth `Protect` gate, plus mounted `/runner-tokens*` (`schedserver.go:194-252`), `/client-grants`
  (`schedserver.go:256-289`), `/wipe` (`schedserver.go:296-339`: central wipe → local
  `Embedded.Reset` → epoch bump → **process exit for a clean re-seed**).
- `jobs-runner` root action `runRunnerlink` (`schedrunner.go:23-72`): flags in `runner.go:32-46`
  (`--engine-url`, `--token`/`JOBS_RUNNER_TOKEN`, `--name`, `--data-dir`, `--tags-file`, `--tag`,
  `--shell-ref`, `--cpu`/`--mem` capacity overrides, `--cgroup-leaf-holder`). Capacity autodetect
  `runner.DetectCapacity` — the c1-m2-style sizing input for jobs-iroh runners.

---

## 11. draganm/amber-store leakage — the port cut-points

Everything below must re-base onto amber-store-core:

| Leak | Where (non-test) | Notes |
|---|---|---|
| `key.Key` (32-byte content key) + `key.Parse` | pervasive: `jobscli/local.go:13`, `jobscli/clientstore.go:14`, `jobscli/remotebuild.go:18`, ~20 runner files (`runner/artifact.go:11`, `runner/localbuild.go:9`, …) | THE identity type; every K/F/BOK. |
| `remotesync.{PushStats, Opts, Progress}` | `jobscli/clientstore.go:15,109-118`, `runner/refwriter.go:13,263-282`, `runner/pushprogress.go` | push/pull progress + stats plumbed into xferProgress. |
| `client.{Stats, Entry, RefInfo, Remote*, PushStats, PullStats, ErrRefNotFound}` + `reference.Reference` | `amber/storeapi.go:23-37` (the `amber.Store` interface itself), `amber/ref.go`, `amber/remotesync.go` | the whole Store seam is spelled in daemon-client vocabulary. |
| `embedded.Store` via `amber.Embedded` (`OpenEmbedded`, `Underlying().PushTree`, `Identity()`, `Reset`, `RemoteWipe`) | `amber/embedded.go:30-106`; used by `jobscli/clientstore.go:70,113`, `schedserver.go:52,309-325` | jobs-iroh: replace with amber-store-core embedded API; `Underlying()` escape hatch must get a first-class PushTree. |
| `fstree`/`amberpack` ingest pipeline (`ingestVia`, `BuildFile`, `BuildDir`, `BuildSourceDir`, `DirBuilder`) | `amber/ingest.go:8-11,22-54`, `amber/buildfrom.go:10,39-59`, `amber/store.go` | `.amberignore` semantics live in `BuildSourceDir`. |
| `sshsign` + SSH-wire identities + signed refs | `amber/ref.go:27-121`, grant blobs (`enginewire.go:17-22`), `Embedded.Identity()` | if iroh node keys replace SSH transport identity, the grant handshake + ref-signature scheme is the biggest semantic re-design. |
| `tarextract` | `runner/cachedir.go` | cache tree materialization. |

The `amber/` package (Store interface + helpers `GetKey/ReadKey/IngestSourceDir/BuildFromTree/
ResolveBuildOutput/ResolveBuildArtifact/BuildStoreTree/RemoteSync/Signer`) is the designed seam:
port that package, and jobscli + runner come along with only import renames — except grant/ref
signing (above) and any Store-interface reshaping.

---

## 12. Semantics checklist for jobs-iroh's jobs-client

1. `sandbox.Init()` before argv parsing (`cmd/jobs/main.go:19`).
2. Data dir: `store/` + `cache/` + `store.lock` (EX flock, blocking with notice) +
   `credentials.json`; default `~/.local/share/jobs` (`clientstore.go:24-33`, `storelock.go`).
3. Seed the embedded store on every local command, including by-key run/image
   (`local.go:117-130,221-230`).
4. Local identity: ephemeral signer unless `--signing-key`; local store does no key authorization
   (`runner/ephemeral.go:19-43`, `local.go:283-292`).
5. Local F must equal server F for the same env (cache join): same IngestSourceDir + subtree +
   recipe-override normalization + BuildFromTree (`runner/localbuild.go:33-34`,
   `runner/buildfrom.go:63-92`).
6. Result-ref ordering invariants: `build-from:K` + `build-from-tree:F` one batch
   (`buildfrom.go:47-59`); `build-output-deps:F` before `build-output:F` (`buildrun.go:146-160`);
   objects always pushed before refs.
7. Exit-code fidelity: entrypoint exit → `cli.Exit("", code)` after defers (`local.go:236-241`).
8. Remote flow: handshake→ingest→push (lock held) / submit / watch (lock released, own signal
   tree, cancel prompt, detach message) / re-handshake→pull (`remotebuild.go:54-183`).
9. Status: request-scoped snapshot SSE is the only live surface a new server serves
   (`httpapi.go:80-97`); keep the TTY liveView block + non-TTY change-lines duality.
10. Image: reproducible (epoch 0, sorted env/keys), artifact at root + deps under
    `/jobs/store/<BOK>`, `--no-shell` skips the shell requirement entirely
    (`runner/image.go:24,126-231`, `runner/artifact.go:38-41`).
