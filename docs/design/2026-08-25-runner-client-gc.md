# Runner- and client-side GC: local store cleanup on every host

Date: 2026-08-25
Status: accepted design (pre-implementation)
Scope: one arc — extend v0.30.0's server GC to the two remaining stores:
the runner's (a pure cache of server refs) and the client's (a flocked
local store holding locally-authoritative build outputs). The server's
sweep machinery is extracted into a shared package all three hosts embed;
a new trees sweep collects the `cache/trees/<key>` materializations that
ref-GC would otherwise orphan.

Prior art: docs/design/2026-08-25-gc-auto-cleanup.md (the server arc) —
`reftrack` tracker, `gc.Collector` wiring, the sweep pipeline (reconcile →
expire → advisory Status → conditional cycle → flush), protected classes,
the build-output family clock+pin mirroring, and the safe-upgrade seeding
rule all ship in v0.30.0 inside `serve/gc.go`. This arc moves the generic
core out of `serve/` and reuses it verbatim.

## 1. Problem

Only the server reclaims disk. The runner's store accumulates every pulled
closure forever; the client's store accumulates every local build, source
push, and pulled output forever; and on both, `cache/trees/<key>` staged
materializations (shell + fetcher toolchain closures, bind-mounted into
sandboxes) are "never evicted" by design — once store GC exists, reaping
the objects behind a tree strands the tree directory permanently. Fetchers
are hermetic: nothing under `cache/` is downloaded state, it is all
derived from store objects or transient `fetcher-*` work dirs.

Decisions taken with the user: client GC triggers automatically after
commands (24h check interval) plus an explicit `jobs-client gc`; retention
defaults are uniform (30 days on all three hosts); the trees sweep is in
scope.

## 2. The shared `gcsweep` package

Extract the host-agnostic core of `serve/gc.go` into `gcsweep/`:

- `gcsweep.New(log, store *amber.Store, opts Options) (*Sweeper, error)`.
  `Options`: `SnapshotPath` (tracker CBOR), `ClosuresDir` (collector
  layout slot), `Retention`, `Rate`, `MinFree`, and optional `CacheDir`
  (enables the trees sweep). The constructor opens the `gc.Collector`
  (`Interval: 0`) and installs both store hooks: the observer
  (`tracker.Touch` on every successful by-name read) and the `PrepareRef`
  guard — every host needs both; the guard is what makes cycles safe
  against concurrent local ref writes.
- `Sweep(ctx, garbage float64, force bool) (Stats, error)` — the v0.30.0
  pipeline unchanged: reconcile (seed unknown at now, drop vanished) →
  expire (protected/pinned skipped, output-before-deps, failed-output
  sibling skip, pin-race re-check) → advisory `Status` → cycle iff
  `force || expired>0 || garbage ≥ 0.5 || free-space pressure` → tracker
  flush → the one `gc sweep` log line — plus the new trees step (§3).
- `Start(ctx, interval)` — the ticker loop (server and runner; the client
  never calls it). `Close()` — stop loop → hold the sweep mutex → close
  collector → flush tracker; runs before the store closes.
- `StatsSnapshot()`, `Entry(name)`, `Pin`, `Unpin` — as today. `Stats` is
  gcsweep's own struct; `serve` maps it onto `api.GCStats`, keeping the
  admin wire shape byte-identical. The existing server GC e2e and admin
  frame tests gate the refactor: `serve/gc.go` shrinks to an adapter
  (hook wiring for amberiroh/sched, api mapping, the test capture seam)
  with zero behavior change.

`reftrack` is reused untouched: protected classes (`shell:`, `fetcher:`,
`seed-src:`), family clock+pin mirroring, safe-upgrade seeding. Pins have
no exposed surface on runner or client — inert machinery, no new
commands.

## 3. Trees sweep

When `Options.CacheDir` is set, `Sweep` ends with:

- For each entry under `<CacheDir>/trees/`: an entry named with the
  `staging-` or `bin-staging-` prefix (the `os.MkdirTemp(trees, ...)` temp
  dirs `stagedTree`/`stagedBinDir` materialize into before their
  atomic rename-into-place, `runner/importexec_linux.go:366,:402`) is
  exempted while young and collected only once its mtime is older than
  24h — same threshold and crash-leftover rationale as `fetcher-*` below.
  Every other entry: parse the directory name as a store key (a trailing
  `.bin` suffix — `stagedBinDir`'s `/bin`-farm companion — is stripped
  first; the remaining hex is the owning key, so the companion lives and
  dies with it); delete the directory when the store no longer holds that
  object (`Has(k)` false) or the name does not parse. The store is the
  truth — no clock, no access tracking needed. Deletion is best-effort
  and logged; a failure retries next sweep.
- For each `fetcher-*` entry directly under `<CacheDir>`: delete when its
  mtime is older than 24h (crash leftovers; live fetcher work dirs are
  torn down by their own cleanup within a job's lifetime).

Safety: a tree in use by a running job cannot be orphaned — the job
touched its refs at ensure time, retention shields the refs, the marked
objects survive the cycle, so `Has(k)` holds. The staged-tree creation
path (rename-into-place, `runner/importexec_linux.go`) stages into a
`staging-`/`bin-staging-` temp dir *inside* `trees/` itself, not outside
it — the trees sweep exempts that prefix while young (see above) rather
than relying on the rename's atomicity alone, so a sweep racing a
concurrent staging leaves the in-flight temp dir alone instead of deleting
it mid-write (which could otherwise let the rename publish an incomplete
tree at the canonical path). Deleting a *complete*, published tree that a
racing job just staged is possible only for a dead key, which a live job
cannot reference.

## 4. Runner integration

`jobs-runner` gains `--gc-retention 720h` (0 disables), `--gc-interval
1h`, `--gc-rate`, `--gc-min-free` (`JOBS_GC_*` env), on by default.
Wiring in `runnerd.Run` after `amber.Open`: construct the Sweeper
(snapshot `<data-dir>/refaccess.cbor`, closures `<data-dir>/store/
closures`, `CacheDir` = the runner's cache dir) and `Start` the loop
**after the boot self-test passes** — the self-test churns temporary refs
and must not race the first sweep. Close order: sweeper before store.

Access model: the store observer alone suffices — every by-name read
(`ensureRef` cache hits, the shell resolve, cache-ref seeding, driver
reads) goes through `GetKey`/`GetRef`. No remote reporting, no amberiroh
hooks (the runner serves no sync protocol), no pins. In-flight jobs are
shielded by the retention argument; local ref writes take the `PrepareRef`
guard. Nothing on the runner is authoritative: any wrong expiry costs one
re-pull. Observability: the shared `gc sweep` log line; no admin surface
(fleet-wide GC stats via heartbeat is explicitly out of scope).

## 5. Client integration

Two triggers:

- **Auto**: every store-touching command, after its main work and while
  the flock is still held, calls `maybeGC`: if `<data-dir>/gc.stamp` is
  older than 24h and retention is non-zero, run `Sweep(force=false)` and
  touch the stamp. Cost on normal commands: one stat. The auto path
  prints one stderr line only when something was reclaimed
  ("gc: expired N refs, freed X"), otherwise silent. `develop` sweeps at
  session end (flock held throughout).
- **Manual**: `jobs-client gc [--garbage F] [--retention D]` — forced
  sweep, printing the same report shape as `admin gc`. The store flock
  makes it wait politely behind a running build.

Retention: default 30d; `JOBS_GC_RETENTION` env overrides (reaches the
auto path without new flags on every command); `--retention` on the `gc`
command for one-offs; 0 disables auto.

Semantics: the client store holds locally-authoritative outputs — a
local-only build's cache exists nowhere else, so expiry means a real
rebuild on next use, not a re-pull. Accepted trade-off (uniform
retention); the envelope is identical: wasteful, never wrong. Bootstrap
seeds stay protected (local builds seed them; `bootstrap.Seed` is
idempotent regardless). Trees + `fetcher-*` sweep runs against
`<data-dir>/cache`.

## 6. Observability

One shared `gc sweep` log line on all hosts (disk, refs, pinned, expired,
live/garbage bytes + percentage, cycle results when run). Client auto
path: the single conditional stderr line. `jobs-client gc`: the full
report. **No new wire or API surface anywhere** — server admin frames
untouched, `api.GCStats` unchanged on the wire.

## 7. Compatibility

Entirely local per host: no protocol changes, any old/new mix of server,
runner, registry, and client interoperates exactly as today. Upgrade
safety inherited from the seeding rule — earliest deletion is one
retention after the upgrade on each host. `--gc-retention 0` (or env)
restores grow-forever per host.

## 8. Testing

- `gcsweep`: the sweep-semantics tests move with the code; the server GC
  e2e + admin tests run unchanged and gate the refactor.
- Trees sweep unit: fabricated `trees/` dir — live key kept, dead key
  removed, junk name removed, young `fetcher-*` kept, old removed.
- Runner e2e (runnerd harness): run a job, age the tracker, sweep; cold
  refs gone, the job's refs + protected classes survive, orphaned trees
  removed.
- Client e2e over a real temp data dir (not the `testStore` bypass — the
  auto trigger lives on the flock/data-dir path): build, backdate stamp +
  tracker, run another command, assert the auto sweep fired once and the
  stamp advanced; plus a direct `jobs-client gc` test.

## 9. Delivery

One implementation plan: gcsweep extraction first (behavior-frozen),
then runner, then client, then the trees sweep and docs. Target release
v0.31.0.
