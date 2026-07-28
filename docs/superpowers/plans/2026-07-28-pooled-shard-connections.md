# Pooled Shard Connections Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Shard connections live in a per-Client pool — punched once, reused across transfers, grown under concurrency (Conns→PoolMax totals, default 4→12), shrunk after idle.

**Architecture:** A `shardPool` in `amberclient` owns shard-connection lifecycle behind a dialer seam; `attachExtras` acquires/releases instead of dial/close. Entries are keyed by authenticated identity; liveness is checked at acquire; an idle sweep closes back to baseline. Spec: `docs/design/2026-07-28-pooled-shard-connections.md`.

**Tech Stack:** Go, go-iroh (draganm fork). Client-only; no wire/server changes.

## Global Constraints

- Nix devShell for build/test/vet; commit per task with the repo trailers
  (`Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`, `Claude-Session: https://claude.ai/code/session_019UrT7JpW67wgpy5MzzvijD`).
- `Options.PoolMax` counts TOTAL connections (control included), 0 → 12, clamped to ≥ Conns; pool shard capacity is PoolMax−1, baseline Conns−1.
- Pool must be safe for concurrent transfers (Client methods are documented concurrent-safe).
- Darwin cross-vet at the end.

---

### Task 1: `shardPool` with dialer seam

**Files:**
- Create: `amberclient/pool.go`
- Test: `amberclient/pool_internal_test.go` (package `amberclient`)

**Interfaces (produces):**

```go
type shardConn interface {           // *iroh.Conn satisfies this
	Context() context.Context
	OpenStreamConn(ctx context.Context) (net.Conn, error)
	Close() error
}
// dialer: dials shard slot i; returns the conn, its full teardown
// (conn close + endpoint shutdown), and the identity it authenticated.
type poolDialer func(ctx context.Context, i int, ports []uint16, eps []amberiroh.DataEndpointRec) (shardConn, func(), irohkey.EndpointID, error)

type poolEntry struct {
	conn     shardConn
	close    func()
	id       irohkey.EndpointID
	streams  int
	lastUsed time.Time
}
type shardPool struct {
	mu       sync.Mutex
	dial     poolDialer
	log      *slog.Logger
	baseline int           // Conns-1
	max      int           // PoolMax-1
	idleTTL  time.Duration // 90s; tests shorten
	entries  []*poolEntry
	active   int
	sweep    *time.Timer
	closed   bool
}
func newShardPool(dial poolDialer, log *slog.Logger, baseline, max int, idleTTL time.Duration) *shardPool
func (p *shardPool) acquire(ctx context.Context, k int, ports []uint16, eps []amberiroh.DataEndpointRec) ([]*poolEntry, func())
func (p *shardPool) drain()   // Close: shut everything
```

**acquire algorithm:** lock; `active++`; evict entries whose `conn.Context().Err() != nil` (call their close); when `len(eps) > 0`, also evict entries whose id is not among the record ids (server restart rotated identities — their conns are dead or dying). Compute `target = clamp(baseline*active, baseline, max)`; while `len(live) < min(k, target)`, dial (outside the lock, budgeted by ctx) with slot indices chosen to cover record identities with the fewest pooled conns first (ties by index); failed dials log debug and reduce parallelism. Select the k least-loaded entries (streams asc, lastUsed desc tiebreak), `streams++` each, stamp lastUsed, stop the sweep timer. The returned release func: `streams--` each, `active--`, and when `active == 0` (re)arm `sweep = time.AfterFunc(idleTTL, p.scaleDown)`.

**scaleDown:** lock; if `active > 0 || closed` return; sort by lastUsed desc; keep the first `baseline` preferring distinct ids (walk MRU→LRU, keep an entry if its id unseen or quota not yet filled); close the rest.

- [ ] **Step 1: failing tests** — `pool_internal_test.go` with a fake dialer (`stubConn` implementing shardConn over a cancellable context; dial counter):
  - `TestPoolReusesAcrossSequentialAcquires`: acquire(3)+release, acquire(3) again → dial count stays 3, same entry pointers.
  - `TestPoolGrowsUnderConcurrency`: baseline 3, max 11; first acquire(3) → 3 dials; second concurrent acquire(3) (before first release) → target 6, 3 more dials, and the two acquisitions share no entry unless forced (assert least-loaded selection: each entry's streams ≤ 1).
  - `TestPoolCapsAtMax`: max 4; two concurrent acquire(3) → total dials ≤ 4; selection still returns 3 each (entries shared, streams up to 2).
  - `TestPoolEvictsDeadConns`: kill one stub (cancel its context) between acquires → next acquire redials it; dead entry's close called.
  - `TestPoolEvictsRotatedIdentities`: acquire with records A,B,C; next acquire with records D,E,F → all old entries closed, 3 fresh dials.
  - `TestPoolIdleScaleDown`: idleTTL 30ms, max 11; grow to 6, release all, wait 100ms → len(entries)==3, closed ones' close funcs ran; distinct ids preferred.
  - `TestPoolDrain`: drain closes everything; later acquire returns nil entries (closed pool dials nothing).
- [ ] **Step 2: run, verify FAIL** (`undefined: newShardPool`)
- [ ] **Step 3: implement `pool.go`** per the interface block above.
- [ ] **Step 4: run `nix develop -c go test ./amberclient/ -run TestPool -v`, verify PASS**
- [ ] **Step 5: commit** `amberclient: shard connection pool with dialer seam`

### Task 2: wire the pool into Client/attachExtras

**Files:**
- Modify: `amberclient/client.go` (Options.PoolMax + doc, Client.pool field, Dial wiring, Close drains)
- Modify: `amberclient/shard.go` (extraConn → dialShard returning identity+teardown; attachExtras acquires/releases; streams still per-transfer)
- Test: `amberclient/pool_e2e_internal_test.go` (package `amberclient`; minimal serve.Run harness copied from client_test.go's startServerData shape)

**Key edits:**
- `dialShard(ctx, i, ports, eps) (shardConn, func(), irohkey.EndpointID, error)`: current extraConn body; punch branch returns the record id, legacy branch returns `c.id`; teardown = `conn.Close(); ep.Shutdown(...)`.
- `Dial`: `pool: newShardPool(c.dialShard, log, clampConns(o.Conns)-1, clampPoolMax(o.PoolMax, clampConns(o.Conns))-1, 90*time.Second)` (`clampPoolMax(0)→12`, `<Conns→Conns`, `>16… allow up to 17 total? clamp to [Conns,17]` — cap shard conns at 16 to match maxDataConns).
- `attachExtras`: budget as today; `entries, release := c.pool.acquire(actxOuter…)` — acquire once (not per-goroutine), then per entry a goroutine opens stream+TAttach within the budget (unchanged frames); a failed stream-open evicts that entry via its release path and reduces parallelism; zero streams on a live ctx still demotes. Returned closer: close streams (CloseStream), then `release()`. Note: the per-entry `close` stays with the pool — attachExtras never shuts conns down anymore.
- `Client.Close`: `c.pool.drain()` before closing control conn/endpoint.

- [ ] **Step 1: failing e2e test** — internal harness: start `serve.Run` on loopback with `DataEndpoints: 2`, `Dial` with `Conns: 4`; push a small tree, pull it back, then pull again; assert between operations `len(c.pool.entries) == 3` with identical `conn` pointers across the two pulls (no redial), and after `Close` the pool is empty. (Build the tree with the same ingest helpers client_test.go uses; keep the harness minimal.)
- [ ] **Step 2: run, verify FAIL** (`c.pool undefined`)
- [ ] **Step 3: implement the edits above**
- [ ] **Step 4: run `nix develop -c go test ./amberclient/ ./amberiroh/ ./serve/`, verify PASS**
- [ ] **Step 5: commit** `amberclient: transfers acquire shard connections from the pool`

### Task 3: full suite, docs, release v0.20.0

- [ ] **Step 1:** `nix develop -c go test ./...` and darwin cross-vet — both clean.
- [ ] **Step 2: docs** — architecture.md Sync paragraph: after the sharding sentence add: `Shard connections are pooled per client: acquired per transfer (a fresh stream + TAttach each), released open, grown under concurrent transfers (Conns→PoolMax totals, default 4→12) and shrunk back after ~90s idle — so the punch ramp is paid once per connection, not per transfer.` CLAUDE.md amberclient row: append `Shard conns are pooled (punch once, reuse; grow to PoolMax=12 under concurrency, shrink after idle).`
- [ ] **Step 3: commit docs** `architecture.md: pooled shard connections`
- [ ] **Step 4: release** — CHANGELOG v0.20.0 entry (pooled shard connections: what/why/compat — client-only, no wire change), `version/version.go` → 0.20.0, commit `Release v0.20.0: pooled shard connections`, tag, push, `gh release create`, registry image build+push per CLAUDE.md (sudo — user approval), `imagetools inspect` two platforms, rm artifacts.

## Self-review

Spec §2.2 acquire/sizing/scale-down/keepalive → Task 1; §2.1 stream-per-transfer + §2.3 interactions (gate untouched — transferConns unmodified; demote on zero streams kept) → Task 2; §4 dialer-seam tests → Task 1, e2e reuse → Task 2; §5 out-of-scope respected (no flags, no mid-transfer growth). Types consistent: poolDialer ↔ dialShard, shardConn ↔ *iroh.Conn, PoolMax total ↔ max=PoolMax−1.
