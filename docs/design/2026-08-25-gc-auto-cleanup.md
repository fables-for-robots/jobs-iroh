# GC and auto-cleanup: access-tracked ref expiry + mark-sweep cycles

Date: 2026-08-25
Status: accepted design (pre-implementation)
Scope: one arc — the server tracks when every reference was last used
(remote pulls, runner-reported per-job read lists, its own doneness/cache
reads), expires refs unused for a configurable retention (default 30 days),
and drives amber-store-core's new mark-and-sweep collector to reclaim the
disk. Registry-served images are pinned so they never expire. An hourly job
does the expiry, decides whether a GC cycle is worth running, and logs disk
usage, ref counts, and the garbage percentage.

Prior art: amber-store-core's `gc` package (merged 2026-08-25, a2ff135) — a
bitmap mark-and-sweep collector over the packstore's footer indexes.
`gc.Open(dir, objects, refs, opts)` returns a `Collector`; every reference
PUT must go through `PrepareRef(root) → commit/abort` (the write barrier
that keeps a mid-mark publication alive); `Run(ctx, garbage)` executes one
cycle; `Status(ctx)` scores every pack against an advisory mark and reports
live/garbage bytes. jobs-iroh pins a pre-GC revision, so a dependency bump
is part of this arc.

## 1. Problem

The server store only grows. Doneness = ref existence, so nothing is ever
deleted; `registryd`'s package doc explicitly defers store GC upstream. The
missing pieces are (a) a notion of which refs are still *used* — reads, not
just writes, and including cache hits that move no data — and (b) wiring
the collector's hooks and cycles into `serve/`.

Safety envelope, from the invariants: deleting an output ref silently
un-memoizes the build — the next submit rebuilds it. "Running twice is
wasteful but never wrong." Expiry can therefore only cost time, never
correctness — provided (1) bootstrap seeds and in-flight publications are
protected, and (2) `PrepareRef` guards every ref PUT so a concurrent sweep
cannot reap a just-walked closure.

## 2. What counts as an access

Every read of a ref by name resets its clock (decision: all reads, not just
data movement):

1. **Server-side reads** — `amber.Store.GetKey`/`GetRef` invoke an optional
   `Observer` hook (set once at open). This covers sched doneness checks at
   node creation, `computePullRefs`, and the kp back-fills — so a fully
   cached build refreshes its whole ref family even though nothing is
   pulled.
2. **Remote pulls** — `amberiroh.Server` gets an `OnPull(name)` callback in
   `handlePull`; `handlePush` touches the pushed name. Covers runner pulls,
   registry pulls, client source push and pull-home.
3. **Runner job reports** — `wire.Result` grows `ReadRefs []string`
   (additive CBOR, `omitempty`). The runner records every name that passes
   through `ensureRef` while driving the job — i.e. the ensured `PullRefs`
   it was handed — and reports the list on every result regardless of
   outcome; `sched.handleResult` forwards it to the tracker before any
   commit logic. Warm runners (local closure, no pull) are exactly the
   blind spot this closes.
   > **Amendment (as built):** the out-of-band `shell:<platform>` ensure
   > inside the runner's `buildRunCfg` (pull-on-miss, per job) is
   > deliberately **not** recorded in `ReadRefs` — `shell:` is a protected
   > class (§3) that expires on nobody's clock, so reporting it would only
   > add noise. `ReadRefs` covers the job's `PullRefs` family (including
   > `build-cache:`), which is what actually needs its clock reset.
4. **Registry pin-asserts** — `TPin` (§4): both an access and a pin.

## 3. The tracker (`reftrack/`)

New package owning `map[refName] → {FirstSeen, LastAccess, Pinned}`:

- `Touch(name)` / `TouchAll(names)` / `Pin(name)` / `Unpin(name)` /
  `Snapshot()` / `Expired(retention, now) []string` / `Load(path)` /
  `Flush(path)`. Mutex-protected map writes — cheap enough for the
  `GetKey` hot path.
- Persistence: `<data-dir>/refaccess.cbor`, loaded at boot, written by
  atomic rename (temp file + rename) on every hourly sweep, on pin
  changes, and on shutdown. A corrupt or missing snapshot starts empty —
  never fatal. Crash-loss window ≤ 1 h of touches; failure direction is
  benign (a ref looks staler than it is; worst case an early rebuild).
- **Safe-upgrade rule**: a name with no entry gets
  `FirstSeen = LastAccess = now` on first touch or first enumeration
  (the sweep seeds unknown names from `ListRefs`). Never `CreatedAt` — an
  upgraded server must not mass-delete on day one; earliest possible
  deletion is `retention` after the upgrade.
- Entries whose refs no longer exist are dropped at sweep time.

**Protected classes** (never expire regardless of clock): `shell:`,
`fetcher:`, `seed-src:` (bootstrap seeds), and pinned entries.
`client-push/` / `runner-push/` scratch refs follow the normal retention
(the server already deletes them after submit/commit; expiry collects
crash orphans).

**Family rule**: `build-output:X` and `build-output-deps:X` share one clock
— touching either touches both; expiry deletes output before deps,
mirroring the "deps strictly before output" write invariant, so no
consumer ever observes deps-implied-by-output missing.

> **Amendment (added during review):** the family rule extends to pin
> state, not just the clock — `Pin`/`Unpin` mirror onto the tracked
> sibling exactly like `Touch` does. Pinning `build-output:X` alone would
> leave `build-output-deps:X` expirable, letting a sweep delete the deps
> while the output survives — the same inversion of "deps strictly before
> output" that the clock-sharing rule exists to prevent, just reached via
> the pin flag instead of the access clock.

## 4. Registry pinning (`TPin`)

New message in the vendored `amberiroh` protocol: `TPin` with
`Names []string` in the existing CBOR `Msg` envelope; reply `TDone` /
`TError`. The server pins each name that exists in the refstore;
nonexistent names are ignored (the registry may assert ahead of a
re-resolve). `amberclient` grows `Pin(ctx, names)` on the control stream.

Registry behavior: after every successful `resolveBuild`, and whenever a
manifest is served from the warm cache, assert `TPin` for the refs backing
the image — `build-from:<K>`, `build-output:<F>`, `build-output-deps:<F>`,
`build:<K>`, `shell:<platform>` — coalesced to at most one assert per ref
per hour (small in-process `map[string]time.Time`, same pattern as the
blobCache touch). Best-effort: a failed assert is logged and retried on
the next serve, never blocks a pull. Self-healing: as long as an image is
served, its pins reappear within an hour even after a server access-DB
loss.

Compatibility: an old server errors on `TPin` → the registry logs once and
stops asserting. An old registry never pins → its images age out
(pre-feature status quo).

## 5. Collector wiring and the hourly sweep

`serve.Run` opens a `gc.Collector` right after `amber.Open`
(`Interval: 0` — the server drives cycles itself; `Grace`/`Garbage` keep
upstream defaults; `Rate`/`MinFree` from flags). Close order: collector
before store (waits out a running cycle); tracker flush on ctx cancel.

**PrepareRef at both chokepoints.** A small nil-safe `RefGuard` interface
(`PrepareRef(root) (commit, abort func(), err error)`) is injected into
`amber.Store` (used by `PutRef`) and `amberiroh.Server` (used by
`handlePush`, which writes to the refstore directly). Nil guard = current
behavior, so `jobs-runner`'s and `jobs-registry`'s private stores (no
collector) and existing tests run unchanged. Local client builds keep
flock-based single-process safety and get no GC — out of scope.

**`gcLoop`** goroutine in `serve.Run`, ticking at `--gc-interval`
(default 1 h), each tick serialized with manual `admin gc` by a mutex:

1. **Seed & prune**: `ListRefs`; seed unknown names (`FirstSeen = now`);
   drop tracker entries for vanished refs.
2. **Expire**: names with `LastAccess < now − retention`, minus protected
   classes and pins; delete via `amber.DeleteRef`, output before deps.
   Each deletion logged at debug with its last-access age.
3. **GC decide & run**: always run `Status(ctx)` (advisory mark walk —
   the unavoidable price of the garbage-% stat; concurrent with builds,
   ingests ride the write barrier; upstream benched 150 GiB). Then run a
   full `Run(ctx, -1)` cycle iff refs were expired this tick, garbage
   fraction ≥ 0.5, or the free-space floor is breached (the collector's
   own pack-by-pack policy line then applies).
4. **Persist & report**: flush the tracker; log the stats line — store
   disk usage (`dirSize` walk), ref count, pinned count, refs expired
   this tick, live bytes, garbage bytes and percentage, and, when a cycle
   ran, packs reaped / bytes freed / duration.

Every failure inside a tick (Status, Run, delete, flush) is logged and
abandoned until the next tick — the sweep never takes the server down;
`Status.LastError` surfaces in stats.

## 6. Admin surface

- **`admin stats`** — `api.StatsReply` grows optional `GC *GCStats`
  (additive CBOR): retention, last-sweep time, refs expired (last sweep +
  since boot), pinned count, live bytes, garbage bytes, garbage %, last
  cycle {time, packs reaped, bytes freed, duration}, last error.
  Populated from tracker + collector cached state — **no mark walk on the
  stats path** (numbers are "as of the last tick"). CLI and TUI stats
  render the block when present; old servers omit it.
- **`admin refs`** — reply entries grow `LastAccess` (unix seconds, 0 =
  unknown) and `Pinned bool` from the tracker snapshot. CLI adds
  age-formatted and `pin` columns (`-` when unknown); TUI refs view
  likewise.
- **`admin gc [--garbage 0.4]`** — new frame `TGC`: runs the full sweep
  tick immediately with the cycle forced (optional garbage-line override
  → `Run(ctx, garbage)`); returns the `GCStats` shape plus the cycle
  result. A cycle already in flight returns "cycle already running".
  CLI prints the report, exits non-zero on cycle error.
- **`admin pin <ref>` / `admin unpin <ref>`** — frames `TPinRef` /
  `TUnpinRef`. Pin requires the ref to exist; unpin always succeeds and
  only clears the flag — the ref then lives by its access clock (a
  registry still serving it re-asserts within the hour, which is correct:
  unpin doesn't fight active use). Reply echoes the resulting entry.
  These ride `jobs-admin/1.0`; the registry's `TPin` stays on the amber
  ALPN.

## 7. Config

`cmd/jobs-server` flags: `--gc-retention 720h` (`JOBS_GC_RETENTION`; 0
disables expiry *and* cycles), `--gc-interval 1h`, `--gc-rate 0` (copier
bytes/s cap → `gc.Options.Rate`), `--gc-min-free 0` (free-space floor
bytes; 0 = collector's 5 % default). Grace and the garbage line keep
upstream defaults — not exposed until someone needs them. The sweep's
compaction holds ref publication (build commits, pushes) for its duration,
so a low `--gc-rate` cap lengthens that stall.

## 8. Compatibility

- Old runner → no `ReadRefs`; its warm-cache reads are invisible; worst
  case an expiry-triggered rebuild. Safe → **no `jobs-runner-nats` ALPN
  bump** (the fence is for wrong results only).
- Old registry → never pins; images age out (status quo ante).
- New registry / old server → `TPin` errors once; registry disables
  asserting.
- Old client → unchanged; new stats/refs fields are additive CBOR both
  directions.
- Dependency bump: amber-store-core → a2ff135 (mark-sweep merge).
  Direct packstore/refstore use is confined to `amber/` and `amberiroh/`;
  the bump is additive (new `gc` package, new packstore methods).
  Verified by the full test run + the macOS cross-vet.

## 9. Testing

- `reftrack`: touch/expire/pin/protected classes/family rule, load–flush
  round-trip, corrupt-snapshot recovery (start empty).
- `amber`: RefGuard commit on success, abort on refstore error, nil guard
  unchanged.
- `amberiroh`: `TPin` round-trip; `OnPull` observation; push guard.
- `sched`: result carrying `ReadRefs` touches the tracker.
- `serve` end-to-end: build → age the clock → tick → expired refs gone,
  pinned/protected survive, packs reaped, doneness triggers a clean
  rebuild.
  > **Amendment (as built):** no injected `now` test hook — the harness
  > configures a real but tiny `GCRetention` (500ms), sleeps past it, and
  > calls `gcRunner.Sweep` directly with its `force` argument set so the
  > cycle always runs regardless of the garbage-fraction/free-space
  > gate. Simpler than threading a clock seam through the collector and
  > tracker for one test file, and it still exercises the real
  > retention-comparison and delete path end to end.
- `registryd`: resolve → pin asserted → sweep spares the image family.
- CLI/TUI rendering stays presentational (no logic worth testing beyond
  formatting helpers already covered).
