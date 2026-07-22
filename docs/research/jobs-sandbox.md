# jobs sandbox layer — subsystem map for the jobs-iroh port

Scope: `jobs/sandbox` (rootless namespace jail + cgroup v2) and `jobs/amberfuse` (read-only FUSE
tree mount), plus the runner-side code that *assembles* sandboxes (the consumers that define the
mount contract: `/jobs/store`, `/jobs/deps`, `/build`, `/jobs/shell`). All paths relative to
`/home/dragan/fables-for-robots/jobs` unless absolute.

Headline: **the `sandbox` package is 100% store-agnostic — verified** (imports are stdlib +
`golang.org/x/sys/unix` only; the sole "amber" occurrence is a comment, `sandbox/reexec_linux.go:56`).
It ports to jobs-iroh verbatim, module path change only. All draganm/amber-store coupling lives in
`amberfuse` and in the runner's assembly code.

---

## 1. The re-exec model

### 1.1 Why re-exec

Rootless namespaces need the child to be *born* with `CLONE_NEWUSER|...` and uid/gid maps written
by the parent; Go cannot fork() safely, so the sandbox re-execs `/proc/self/exe` and dispatches on
an env sentinel.

- Sentinel env var: `_JOBS_SANDBOX_CHILD` (`sandbox/sandbox_linux.go:20`). Value is either:
  - the JSON-marshalled `Config`,
  - `@<path>` — config spilled to a temp file when marshaled size > `maxInlineConfig` = 64 KiB
    (`sandbox_linux.go:25`, spill logic `sandbox_linux.go:116-131`; child reads it back at
    `reexec_linux.go:30-39`). Rationale: kernel MAX_ARG_STRLEN ≈128 KiB per env string → E2BIG.
    Real-world trigger: a GitLab-monolith build with ~850 store mounts (`sandbox/spill_linux_test.go:19-23`).
  - `"probe"` — a `UserNSAvailable` probe child that just exits 0 (`reexec_linux.go:27-29`).
- Child argv[0] is `"jobs-sandbox-child"` (`sandbox_linux.go:172`); the probe uses
  `"jobs-sandbox-probe"` (`sandbox_linux.go:230`).

### 1.2 `Init()` — must be first in main()

`sandbox.Init()` (`sandbox/reexec_linux.go:22-83`):
1. Returns immediately if `_JOBS_SANDBOX_CHILD` unset (normal process).
2. `probe` → `os.Exit(0)`.
3. Reads/unmarshals config (exit 127 on failure), runs `childSetup` (exit 126 on failure).
4. Execs `cfg.Command[0]` via `unix.Exec` in a retry loop — **never returns**.

It MUST run before any flag parsing because the re-exec'd child is the *same binary* — under
`go test` it is the test binary, and without `Init()` first the child would fall into the test
runner (hang / bogus test re-entry). Consequences:

- Every binary `main()` calls it first: `cmd/jobs/main.go:19`, `cmd/jobs-server/main.go:18`,
  `cmd/jobs-console/main.go:17` (comment at `cmd/jobs/main.go:16` explains).
- Every test binary whose package drives the sandbox needs a `TestMain` calling it first:
  `sandbox/sandbox_suite_test.go:12-15`, `runner/buildexec_suite_test.go:12-19`,
  `internal/jobscli/build_e2e_suite_test.go:14-27`, `sched/session/main_test.go:14`,
  `sched/httpapi/main_test.go:13`.
- **jobs-iroh rule**: jobs-server, jobs-runner, jobs-client mains and every sandbox-driving test
  suite must carry the same call. This is the single most common way to break the port.

### 1.3 The execve retry loop (`reexec_linux.go:53-82`)

- **EINTR: retry forever.** execve can return EINTR when a signal (notably Go runtime SIGURG
  async-preemption) interrupts the kernel mid-load while faulting in pages from an interruptible
  filesystem (amberfuse open/read). execve has no side effects on failure → safe infinite retry.
  Pairs with amberfuse returning EINTR (not EIO) on a cancelled FUSE request context
  (`amberfuse/amberfuse_linux.go:268-277`). Keep this even in a FUSE-less jobs-iroh — SIGURG can
  interrupt execve from any slow storage.
- **ETXTBSY: bounded retry** — 10 attempts, 5 ms doubling to a 200 ms cap (`reexec_linux.go:65-79`).
  Cause: a concurrently forked child of a multi-slot runner (between its clone and its own execve)
  inherits the write fd of a just-extracted target binary (golang/go#22315); the writer is always
  moribund (`sandbox/etxtbsy_linux_test.go:17-21`). **Required for any multi-slot runner** — jobs-iroh
  runners are multi-slot, so keep.
- Exit codes: 127 = bad config / exec failure, 126 = childSetup failure.

### 1.4 `Run()` — parent side (`sandbox_linux.go:106-163`)

- Marshals config, spills if oversized, appends sentinel to `os.Environ()`.
- `buildChildCmd` (`sandbox_linux.go:168-204`): `exec.CommandContext(ctx, "/proc/self/exe")`,
  `SysProcAttr`:
  - `Cloneflags` from `Namespaces` (`cloneflags`, `sandbox_linux.go:70-91`),
  - `Pdeathsig: SIGKILL` (child dies with parent),
  - `GidMappingsEnableSetgroups: false`,
  - when `Namespaces.User`: uid/gid maps `{ContainerID: 0, HostID: getuid/getgid, Size: 1}`
    (`sandbox_linux.go:187-190`) — root inside == invoking user outside,
  - `UseCgroupFD`/`CgroupFD` when a cgroup fd exists (born-in-cgroup, clone3 CLONE_INTO_CGROUP),
  - `Setsid+Setctty` when `cfg.Tty != nil` (`sandbox_linux.go:198-201`).
- Return contract: `(exitCode, nil)` for any command exit incl. non-zero; non-nil error only for
  setup/infra failure (`sandbox_linux.go:93-96`).
- `UserNSAvailable()` (`sandbox_linux.go:228-239`): forks the probe child with only CLONE_NEWUSER +
  same maps; used by every self-skipping test and by the import executor's degrade path.
- Non-Linux stub: `Run` returns `ErrUnsupported`, `Init()` no-op, `UserNSAvailable()` false
  (`sandbox/sandbox_other.go:43-51`).

## 2. The request struct

`Config` (`sandbox/sandbox_linux.go:52-68`):

| Field | Semantics |
|---|---|
| `Command []string` | argv; `Command[0]` exec'd absolute inside the (possibly pivoted) root |
| `Env []string` | full child env (no inheritance unless caller includes os.Environ) |
| `Dir string` | chdir after pivot ("" ⇒ "/") |
| `NewRoot string` | non-empty ⇒ pivot_root into it; "" ⇒ no pivot (host fs visible) |
| `Mounts []Mount` | applied inside the new mount ns, targets relative to NewRoot |
| `Namespaces` | `{User, Mount, PID, Net, UTS, IPC bool}` — **`Net:true` = CLONE_NEWNET = net=NONE inside**; `Net:false` = host network. Inverted vs intuition — a port hazard. |
| `Cgroup *Cgroup` | `json:"-"` parent-side; nil valid (= no limits) |
| `Stdout/Stderr/Stdin` | `json:"-"` parent-side wiring on the exec.Cmd |
| `Tty *os.File` | `json:"-"` PTY slave; becomes stdin/out/err + controlling terminal (develop) |

`Mount` (`sandbox_linux.go:35-42`): `{Source, Target, FSType, ReadOnly, Strictatime, Flags}`.
`FSType==""` ⇒ bind (MS_BIND|MS_REC); else filesystem type ("tmpfs", "proc"). `Strictatime` is a
best-effort second remount so cache-mount atimes record even on noatime hosts (used by the
build-cache prune; EPERM ignored — `reexec_linux.go:173-182`).

## 3. Namespace setup — `childSetup` (`sandbox/reexec_linux.go:85-135`)

Critical ordering (documented at `reexec_linux.go:85-90`):

1. `mount("", "/", MS_REC|MS_PRIVATE)` — stop propagation to host (`:93-97`).
2. Bind NewRoot onto itself (pivot_root requires a mount point; works for empty and populated
   dirs) (`:98-104`).
3. Apply `cfg.Mounts` in order via `applyMount` (`:105-109`).
4. **Fresh procfs BEFORE pivot** — after the old root is detached the kernel refuses a new procfs
   mount in a userns (EPERM). Mounted at `NewRoot/proc` (or `/proc` when NewRoot=="") with
   MS_NOSUID|MS_NODEV|MS_NOEXEC, only when `Namespaces.PID` (`:110-123`).
5. `pivot(newRoot)`: chdir; `PivotRoot(".", ".")`; `Unmount(".", MNT_DETACH)`; chdir "/"
   (`reexec_linux.go:200-211`).
6. chdir `cfg.Dir` (`:129-133`).

`applyMount` (`reexec_linux.go:142-196`):
- Bind: stat source; dir source ⇒ MkdirAll target, file source ⇒ create empty file target (so
  single device nodes bind — `/dev/null` etc.) (`:145-162`).
- `MS_BIND|MS_REC|Flags` initial mount; **ReadOnly needs a second `MS_REMOUNT|MS_BIND|MS_RDONLY|MS_REC`
  pass** (kernel ignores RDONLY on the first bind) (`:163-172`).
- Strictatime: third best-effort remount, never fails the build (`:173-182`).
- Non-bind: MkdirAll + `mount(Source, target, FSType, flags)` with RDONLY folded into flags (`:185-195`).

Proven behavior (`sandbox/jail_linux_test.go`): net=none blocks TCP (`:100-105`), RO binds return
EROFS (`:107-112`), child is PID 1 in a fresh procfs after pivot (`:114-125`). Also: tmpfs as the
new root itself (`{Source:"", Target:"/", FSType:"tmpfs"}`, `:55`) works. `tty_linux_test.go:17`
proves `Tty` gives a real controlling terminal (`test -t 0`).

## 4. cgroup v2 (`sandbox/cgroup_linux.go`) — best-effort by contract

- `CgroupLimits{MemoryMaxBytes, MemoryHighBytes, PIDsMax, CPUQuotaUS, CPUPeriodUS}` — zero = no
  limit (`cgroup_linux.go:17-23`).
- `CreateCgroup(name, lim)` (`:159-204`): finds the delegated parent via `DetectCgroupDelegation`;
  **returns `(nil, nil)` when no delegation** — a nil `*Cgroup` is a valid no-op handle everywhere
  (Add/Close/FD/Stats all nil-safe). Enables only needed+available controllers in parent
  `subtree_control` (best-effort), mkdirs the leaf, writes `memory.max`/`memory.high`/`pids.max`/
  `cpu.max` (period default 100000) — each write best-effort (`(c).write` swallows errors, `:206-208`).
- **Placement**: first attempt born-in-cgroup (`UseCgroupFD`, `Cgroup.FD()` opens O_PATH dir fd,
  `:220-229`). If `cmd.Start` fails with EOPNOTSUPP/EINVAL/EBUSY/EPERM (`isCgroupCloneUnsupported`,
  `sandbox_linux.go:211-223` — e.g. domain-threaded delegated scope, no clone3 support), retry
  without the fd and best-effort `Cgroup.Add(pid)` (`sandbox_linux.go:140-153`). If even Add fails,
  the build runs uncapped — never a hard failure (`sandbox_linux.go:98-105`).
- `Stats()` (`:236-268`): `cpu.stat usage_usec` (cumulative, incl. exited children),
  `memory.current`, `memory.peak` (kernels ≥5.19); -1 per unreadable field
  (`sandbox/cgroupstats.go:3-15`). Accounting persists after process exit until `Close()` (rmdir),
  so the settled read after the child dies is valid — the runner exploits this
  (`runner/buildexec_linux.go:280-286`).

### 4.1 Leaf holder — making memory.max real on k8s

cgroup v2 no-internal-process rule: a cgroup with member processes cannot enable a domain
controller (memory) in `subtree_control`. On k8s the runner is pid 1 *inside* the container scope,
so job leaf cgroups would silently get no memory controller.

- `EnsureCgroupLeafHolder()` (`cgroup_linux.go:106-132`): moves **only the calling process** into a
  `jobs-leaf-holder` child (`cgroupLeafHolderName`, `:34`), then eagerly writes `+memory` to the
  original cgroup's `subtree_control` and **reports** failure (EBUSY ⇒ co-resident processes)
  instead of deferring to CreateCgroup's silent best-effort. Returns `(false, nil)` — not an error —
  when the technique doesn't apply: no v2 line in `/proc/self/cgroup` (`selfCgroupRel`, `:38-49`),
  no writable `cgroup.subtree_control` (`canManageCgroup`, `:67-69`), or a non-"domain"
  `cgroup.type` (threaded terminal scopes) (`:115-118`). Idempotent.
- `DetectCgroupDelegation()` (`:138-149`): the delegated dir + available controllers; a parked
  process maps back to the **holder's parent** (`delegatedDirFromRel`, `:56-62` — job cgroups must
  be siblings of the holder, not children).
- Called at runner startup behind `cfg.CgroupLeafHolder` (`runner/runner.go:177-185`) and in the CLI
  runner wiring (`internal/jobscli/schedrunner.go:32`). jobs-iroh's runner must do the same or job
  `memory.max` + mem heartbeats silently degrade on k8s.
- Leaf cgroup names: `jobs-build-<pid>-<unixnano>` (`runner/buildexec_linux.go:157`),
  `jobs-import-<pid>-<unixnano>` (`runner/importexec_linux.go:61`). A `node → cgroup dir` registry
  (`runner/procs.go:16-30`) feeds live process snapshots (issue #97) — optional for jobs-iroh's
  admin TUI.

## 5. Sandbox shapes per job kind (the runner-side assembly contract)

### 5.1 Hermetic build — `assembleSandbox` (`runner/buildexec_linux.go:92-212`)

Fixed in-sandbox paths (`buildexec_linux.go:40-56`):
`/jobs/store` (assembled CAS union, RO), `/build/src` = `$SRC` (writable extracted source),
`/build/out` = `$out`, `/build` = `$HOME`, `/build/.jobs-deps.json` = `JOBS_DEPS_FILE`,
`/build/.jobs-script.sh` (script always file-carried — both can exceed MAX_ARG_STRLEN).

Assembly steps:
1. Work dir `jobs-build-*`; `newRoot = work/root`.
2. `provisionStore` puts the store tree at `newRoot/jobs/store` — FUSE mount(s) or materialize +
   RO bind (see §6) (`:123-127`).
3. Source subtree `st.Tar(SourceKey, SourceDir)` extracted **writable** into `$SRC` (`:129-141`).
4. `$out` created empty (`:143-146`).
5. JOBS_DEPS JSON + script written as files (`:150-155`).
6. Best-effort cgroup `{MemoryMaxBytes, PIDsMax}` (`:157-167`).
7. Hermetic `/dev`: bind-mounts of exactly `null,zero,full,random,urandom` (individually — never
   the whole host /dev, which would leak disks) + symlinks `fd,stdin,stdout,stderr → /proc/self/fd*`
   (`hermeticDevMounts`, `:221-245`). Missing host node ⇒ skip, don't fail.
8. Hermetic `/etc`: `hosts` (localhost v4+v6) + **empty** `resolv.conf` as plain files, so resolver
   paths fail fast under net=none instead of hanging forever (issue #99; a Rails boot once parked
   on a UDP socket at zero CPU) (`runner/hermeticetc.go:26-39`).
9. Persistent cache mounts: rw binds at declared paths with `Strictatime:true` (atime feeds the
   post-build prune) (`:186-191`).
10. Final config (`:193-209`): mounts = `tmpfs /tmp` + devMounts + cacheMounts + extraMounts +
    storeBinds; **Namespaces all six true** (`Net:true` ⇒ net=none); `NewRoot=newRoot`.

Command: `<store shell>/bin/bash -e /build/.jobs-script.sh` where shell dir =
`/jobs/store/<ShellBOK>` (`storeShellDir`, `:71-73`; RunBuild `:248-249`).

Env (`buildEnv`, `:297-324`): spec.Env first, then structural overwrite: `SRC`, `out`,
`HOME=/build`, `PATH=<shellDir>/bin` (static userland, no LD_LIBRARY_PATH); `JOBS_DEPS` inline only
when ≤64 KiB (`maxInlineJobsDeps`, `:60`), `JOBS_DEPS_FILE` always. `encodeJobsDeps` is
deterministic JSON (sorted keys, `runner/buildexec.go:21-31`).

Spec type: `BuildSpec` (`runner/buildexec.go:45-73`) — `StoreKey`, `ShellBOK` (both
`amber-store/key.Key` — **port cut-point**), `JobsDeps map[string]string`, `SourceKey`,
`SourceDir`, `Env`, `Script`, `MemoryMaxBytes`, `PIDsMax`, `Caches`, sinks, `Node`, `Events`.
Result: `BuildResult{ExitCode, StderrTail, OutDir}` — caller ingests OutDir (`buildexec.go:75-88`).

Cleanup ordering: FUSE mounts closed **LIFO before** `os.RemoveAll(work)` (a busy FUSE mount fails
the removal), then cgroup Close (`:100-111`, `:253-265`).

### 5.2 Import / fetcher — `CgroupExecutor` (`runner/importexec_linux.go:27-123`)

- `NewRoot: ""` — **no pivot**; the fetcher sees the host fs and writes directly to
  OutputDir/SecretsFile paths (`:96-97`).
- Namespaces `{User, Mount, PID}` only; **`Net:false` ⇒ host network kept** (imports are the only
  network-capable node) (`:98-106`). Mount:true+PID:true+NewRoot:"" still gets rprivate / + fresh
  /proc.
- Env = `os.Environ()` overlaid with spec.Env (`:79-83`) — deliberately not hermetic.
- Degrade path: without userns, fall back to plain `Subprocess{}` once-announced instead of failing
  every import (`defaultImportExecutor`, `:40-51`).
- Command: `<fetcherDir>/fetch`, Dir = fetcherDir (`:92-94`).

### 5.3 Plugin resolve — `SandboxedPluginCaller` (`runner/plugincaller.go:73-222`)

Hermetic like a build (all six namespaces, net=none, `:197-199`) but CBOR-over-stdio:
- Store = union of shell + plugin artifact via `amber.BuildStoreTree` (`:145`), provisioned by the
  same `provisionStore` (`:149`).
- `/jobs/shell → /jobs/store/<shellBOK>` compat **symlink** (not mount) so fixed
  `#!/jobs/shell/bin/bash` shebangs resolve (`:154-159`); paths const at `:33-37`.
- Source tree RO bind at `/jobs/source`, path carried in the CBOR request (`:185-190`).
- Resolution deps RO binds at `/jobs/deps/<name>` (const `pluginDepsDir`,
  `runner/depsmount.go:16`; sorted for determinism, `:97-113`; materialized host-side by
  `materializeDeps`, `depsmount.go:23-70`).
- `tmpfs /tmp`, `HOME=/tmp`, `PATH=<shell>/bin` (`:175-196`); stdin = CBOR request, stdout = CBOR
  response (never log-captured), exit 75 = retryable (`:200-221`).

### 5.4 `jobs run` entrypoint (`runner/run_linux.go:120-173`) — deliberately NOT hermetic

- Store **always materialized** to disk, never FUSE — exec'ing a binary from amberfuse is
  unreliable (`:116-119`, `:128-140`). Same `/jobs/store/<BOK>` layout so baked-in paths resolve.
- `/jobs/shell` symlink again (`:142-145`).
- `Net:false` (host network — a server binds host localhost), host `/dev` recursive bind, tmpfs
  /tmp; User/Mount/PID/UTS/IPC still on (`:156-171`).

### 5.5 `jobs develop` (`runner/develop_linux.go:262-294`, `:331-377`)

`assembleSandbox` + one extra mount: host `/dev` recursive bind (develop-only relaxation, `:263-265`).
PTY: `pty.Open()`, `cfg.Tty = slave` ⇒ Setsid+Setctty in the child; script + rcfile written into
`newRoot/build` (`:279-286`); command `bash --rcfile /build/.developrc -i` (`:373`).

## 6. Store provisioning: materialize (default) vs FUSE

- Mode switch `JOBS_STORE_MOUNT` = `materialize` (default, incl. unset/unrecognized) | `auto`
  (probe, prefer FUSE, fall back) | `fuse` (forced, failure fatal) —
  `resolveStoreMode` (`runner/storemount.go:37-49`). Default is materialize because **FUSE proved
  flaky under concurrent load** (comment `:32-36`; CLAUDE.md corroborates).
- `materializeStore` (`storemount.go:55-68`): `st.Tar(storeKey, "")` + `extractTar` to
  `work/store`, then RO bind `{Source: staging, Target: "/jobs/store", ReadOnly: true}`
  (`runner/fusecaps_linux.go:119-123`).
- `provisionStore` (`fusecaps_linux.go:90-124`): returns `([]*amberfuse.Mounted, []sandbox.Mount)`
  — exactly one of the two is non-empty. FUSE options when used: `DirectMount:true`,
  `CacheDir=workDir` (backing files torn down with the work tree), `CacheFiles=4096`
  (`fuseIdleFileCap`, `buildexec_linux.go:62-67`).
- FUSE probe (`fusecaps_linux.go:36-79`): a **real trial mount** of an empty ingested tree
  (`/dev/fuse` existing doesn't prove rootless DirectMount works); success cached per-process,
  failure deliberately NOT cached (transient store-not-ready must not latch).
- Key architectural fact: **FUSE servers run in the parent runner process**; the mounts pre-exist
  the clone, the child inherits them and pivot_roots over them — the child never needs store
  access (`buildexec_linux.go:29-33`).
- Fetcher artifacts: `ResolveFetcherArtifact` (`runner/fetcher_linux.go:50-92`) — same
  mode switch; materialize by default since `./fetch` is exec'd from the dir.

## 7. amberfuse — API and caching

### 7.1 API (all `amberfuse/amberfuse_linux.go`)

- `Mount(ctx, src Source, rootKey key.Key, mountpoint string, opts Options) (*Mounted, error)`
  (`:71-113`). `Mounted.Close()` unmounts + closes all cached fds (`:116-122`);
  `LazyUnmount()` = `fusermount3/fusermount -u -z` fallback `unix.Unmount(MNT_DETACH)` (`:128-141`).
- `Source` interface (`:65-68`): `File(ctx, ck key.Key) (io.ReadCloser, error)` +
  `Ls(ctx, k key.Key, path string) ([]client.Entry, error)` — satisfied by both amber backends.
- `Options` (`:31-55`): `MaxFileSize` (open > cap ⇒ EFBIG), `CacheBytes`, `CacheFiles`,
  `CacheDir` (backing files; must be real disk, not tmpfs, for page-cache reclaim),
  `IdleTTL` (default 60 s, `:27-28`), `Debug`, `DirectMount`.
- `ResolveDirKey(ctx, src, root, subpath)` — path walk by key (`:144-146`, `:357-387`).
- Immutability exploited: attr/entry kernel timeouts = 1 year (`:100-105`); dir listings memoized
  forever (`dirCache`, `:151-180`).
- Read-only enforced at Open: any non-O_RDONLY accmode ⇒ EROFS — the backing file is shared by
  content across paths, a write would corrupt all of them (`:253-259`).
- FUSE passthrough: backing fd handed to the kernel (`PassthroughFd`, `:295-297`) when the kernel
  advertises CAP_PASSTHROUGH *and* the process holds CAP_SYS_ADMIN (`:111`, `hasCapSysAdmin`
  `:334-344`); `Read` is the fallback (counted in `fallbackReads`, `:299-302`).
- **EINTR contract**: a failed open with a cancelled request ctx returns EINTR, not EIO (`:268-277`)
  — the other half of the sandbox execve-EINTR retry.

### 7.2 Backing cache (`amberfuse_linux.go:389-589`, `backingfile_linux.go`)

- Each opened file is fully reconstructed into an **unlinked on-disk temp file** —
  `O_TMPFILE` preferred, fallback create+unlink (`backingfile_linux.go:61-77`); disk-backed (not
  memfd) so pages are reclaimable under memory pressure (`:14-25`). Truncate-to-size first for a
  fast ENOSPC (`:34-40`).
- Dedup by content key; **single-flight** concurrent opens of the same content (`inflight`,
  `acquire` loop `:444-484`).
- Idle handling: refs==0 ⇒ push on LRU + per-file TTL timer (`release`, `:511-525`); re-open pulls
  it back (`reuseLocked`, `:504-509`); TTL drop guarded against races by identity check
  (`dropIfIdle`, `:546-555`); `CacheBytes`/`CacheFiles` caps evict LRU-tail (`evictLocked`,
  `:561-574`).

### 7.3 draganm/amber-store coupling in amberfuse (port cut-points)

- `key.Key` — 32-byte key that also encodes `Type()` (Blob/FileNode/DirLeaf/DirNode) and
  `Length()`; amberfuse relies on `ck.Length()` for file size *before fetching*
  (`amberfuse_linux.go:265`, `:493-499`) and on `Type()` in `resolveDirKey` (`:363`).
- `client.Entry` — dir entry `{Name, Mode, UID, GID, Key(hex string), Size, MtimeNs, LinkTarget,
  Rdev}` consumed in `fillAttr` (`:310-332`), Lookup/Readdir, and the `Source.Ls` signature.
- Re-basing on amber-store-core requires equivalents (typed key with embedded length, entry
  metadata) or a thin adapter interface — or dropping FUSE entirely (§8).
- Note the wider seam: `amber.Store` (`amber/storeapi.go:23-37`) is the `*client.Client` method
  subset in the client package's vocabulary (`client.Stats`, `reference.Reference`, remote-sync
  methods). jobs-iroh will re-cut this seam against amber-store-core; the sandbox package itself
  needs none of it.

## 8. Can jobs-iroh skip FUSE entirely? — Yes

- Materialize is already the **default** on every store-mount consumer: build/develop sandbox
  (`provisionStore`), plugin-resolve (same), fetchers (`ResolveFetcherArtifact`), and `jobs run`
  *always* materializes (`run_linux.go:116-119`). `go test ./...` and all examples run FUSE-free
  today (CLAUDE.md: "Builds don't use FUSE by default"; only `amberfuse/*` tests and one
  provisionStore FUSE-mode test gate on `/dev/fuse`).
- The go-build example even *stages the toolchain out of the store into /build* because the Go
  toolchain hammered FUSE; under materialize that staging is a plain disk copy — examples are
  compatible by construction.
- What is lost by dropping FUSE: laziness only. Materialize extracts the whole `/jobs/store`
  closure per job (disk + startup latency on big closures; the "materializing" phase heartbeat
  exists precisely because this takes real time, `buildexec_linux.go:118-122`). No semantic
  difference — same paths, same RO enforcement (bind + remount-ro instead of EROFS-at-open).
- Contained refactor: `provisionStore` collapses to materialize+bind (drop the
  `[]*amberfuse.Mounted` return and the LIFO unmount loops); delete `amberfuse/`,
  `fusecaps_linux.go` probe machinery, `fuseIdleFileCap`, `JOBS_STORE_MOUNT` (or keep as a no-op
  for compat). Keep the EINTR/ETXTBSY execve retries regardless.
- Optional future win under materialize: a shared per-runner CAS extraction cache keyed by BOK
  (today each job extracts its own staging copy under its work dir).

## 9. Copy verbatim vs re-base vs drop

**Copy verbatim (store-free, proven):**
- `sandbox/sandbox_linux.go`, `sandbox/reexec_linux.go`, `sandbox/cgroup_linux.go`,
  `sandbox/cgroupstats.go`, `sandbox/sandbox_other.go`, `sandbox/cgroup_other.go` — module path
  change only. Bring the tests: `jail_linux_test.go`, `spill_linux_test.go`,
  `etxtbsy_linux_test.go`, `tty_linux_test.go`, `cgroup_linux_test.go`,
  `cgroup_leafholder_internal_test.go`, `cgroupstats_internal_test.go`, `sandbox_suite_test.go`
  (the jail test binds `/nix` — generalize to a host-userland lookup if jobs-iroh CI isn't NixOS).
- `runner/hermeticetc.go` (store-free), the hermetic-/dev logic (`hermeticDevMounts`), the
  `encodeJobsDeps` determinism rule, the fixed path constants, and the cleanup ordering.

**Copy with re-base onto amber-store-core (the assembly layer):**
- `runner/buildexec.go`+`buildexec_linux.go` (BuildSpec carries `key.Key`; `st.Tar` for source
  extract), `runner/storemount.go`, `runner/fusecaps_linux.go` (or its materialize-only residue),
  `runner/plugincaller.go` (`amber.BuildStoreTree`, `key.Key`), `runner/importexec_linux.go`
  (already nearly store-free — only spec plumbing), `runner/depsmount.go`, `runner/run_linux.go`,
  `runner/develop_linux.go`. Coupling is uniformly: `key.Key` identifiers + `Store.Tar/File/Ls` +
  `amber.BuildStoreTree`/`IngestDir`.

**Droppable:**
- All of `amberfuse/` if materialize-only (§8). If kept, it needs the amber-store-core key/entry
  adapter (§7.3).
- FUSE probe/caps machinery, `JOBS_STORE_MOUNT`, `logStoreProvisioning`.
- `runner/procs.go` job-cgroup registry — keep only if the admin TUI wants live process snapshots.
- WS-era: nothing in the sandbox layer touches `wire`/WebSocket; the old WS runner loop is outside
  this subsystem (already unreferenced by binaries per CLAUDE.md). No WS bits to port here.

**Non-negotiable invariants to preserve:**
1. `sandbox.Init()` first line of every main and sandbox-driving TestMain.
2. proc-before-pivot mount ordering; remount-ro second pass.
3. `Net:true == net-none` semantics (or rename the field during the port to `NetNone` to kill the trap).
4. EINTR-forever + ETXTBSY-bounded execve retries.
5. Cgroups best-effort at every layer (nil Cgroup, clone-into fallback, silent limit writes);
   leaf-holder parking at runner startup for k8s memory limits.
6. Config spill at 64 KiB; JOBS_DEPS file-carry; script file-carry (MAX_ARG_STRLEN).
7. Hermetic /etc + minimal /dev in every net=none sandbox.
8. FUSE-mounts/materialized-staging owned by the parent; child inherits via pre-clone mounts;
   LIFO close before work-tree removal.
