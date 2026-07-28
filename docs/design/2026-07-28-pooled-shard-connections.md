# Pooled shard connections: punch once, reuse across transfers

Date: 2026-07-28
Status: accepted design (pre-implementation)
Scope: one arc — `amberclient` keeps shard connections in a per-Client pool
instead of dialing and tearing down per transfer. Baseline `Conns` total
connections (control + Conns−1 pooled shards) stay open; concurrent
transfers grow the pool toward a `PoolMax` total (default 12); an idle
client shrinks back to baseline. Client-only: no wire changes, no server
changes, no ALPN bump. Runnerd, registryd and clientcli inherit the pool
through `amberclient.Client` unchanged.

## 1. Problem

Since the punched-shard-endpoints arc
(2026-07-28-punched-shard-endpoints.md), every sharded transfer on a
NAT-separated topology pays the punch ramp per shard, per transfer: bind a
full-stack endpoint, connect via relay (~1s), `TAttach`, then ~5s riding
the relay until QNT lands the direct path. `attachExtras` closes every
shard connection when the transfer ends (`amberclient/shard.go`,
`closeExtras`), so the next pull starts the ramp from scratch — a runner
grinding through jobs re-punches the same four paths all day, and the
warmed NAT mappings are thrown away just as they become valuable.

## 2. Design

### 2.1 The protocol already permits reuse

`TAttach` binds a *stream* to a transfer token, not a connection: the
server's attach handler reads one `TAttach` frame per incoming stream and
hands that stream to the gathered transfer. `attachExtras` already opens a
fresh stream per transfer on each shard connection. Reuse is therefore
purely a client-side lifecycle change: keep `{endpoint, conn, identity}`
alive across transfers, open per-transfer streams on them.

### 2.2 Pool structure (`amberclient`)

`Client` gains a `shardPool`:

- Entries are `{ep *iroh.Endpoint, conn *iroh.Conn, id irohkey.EndpointID,
  streams int, lastUsed time}` — `id` is the data-endpoint identity the
  conn authenticated (the server identity `c.id` on the legacy,
  record-less path).
- **Acquire(ctx, k, ports, eps)** returns up to k connections for one
  transfer: live pooled entries whose identity appears in the current
  `DataEndpoints` records first (server restarts rotate identities; an
  entry whose conn context is closed is evicted and redialed), then new
  dials for what is missing, up to the cap. The dial logic is the existing
  `extraConn` punch/legacy machinery, relocated into the pool's dialer.
  Among more entries than k, the k least-loaded (fewest active streams)
  win, so overlapping transfers spread across connections.
- **Release** decrements stream counts and stamps `lastUsed`; connections
  are NOT closed. Streams remain per-transfer and close as today.
- **Sizing.** Shard-conn target = `clamp((Conns−1) × activeTransfers,
  Conns−1, PoolMax−1)`; `activeTransfers` is the acquire/release
  balance. With defaults (Conns 4, PoolMax 12): 3 shard conns at rest —
  4 total connections including control — bursting to 11 (12 total) under
  concurrent transfers. Growth happens at acquire; a transfer that started
  narrow stays narrow (the want loop fixes its channel set at start — the
  NEXT transfer starts wider).
- **Idle scale-down.** When no transfer has been active for `poolIdleTTL`
  (90s; a Client field so tests shorten it), a sweep closes entries back
  to baseline, keeping most-recently-used, at most one per distinct
  identity where possible. `Client.Close` drains the pool.
- **Keepalive is free**: go-iroh applies a QUIC `KeepAlivePeriod`
  (heartbeat interval) to every connection by default, so idle pooled
  conns do not idle-timeout; a conn that dies anyway is caught by the
  liveness check at acquire and transparently replaced.

### 2.3 Interactions

- The relayed-control gate (`transferConns`) still skips extras without
  touching the pool. The sticky demote still latches on genuine
  zero-attach; a demoted client simply never acquires.
- `retrySingle`'s unsharded retry is unaffected (conns==1 skips acquire).
- Old servers (no `DataEndpoints` records) pool identically — entries key
  on the server identity.
- `Options.PoolMax` (0 → 12, clamped ≥ Conns) is the only API addition.
  No new CLI flags: the defaults implement the requested 4→12→4 policy
  everywhere; per-deployment tuning flags can come later.

## 3. Alternatives considered

- **Mid-transfer channel growth** (one transfer widening while running):
  requires want-loop and attach-protocol changes for dynamic channel sets;
  rejected — concurrency-driven sizing captures the workload (runnerd
  slots, registry concurrent pulls) without touching the wire.
- **Static always-12**: standing cost (server + relay state ×every
  client) for no benefit at rest, since the server only exposes ~4 sockets
  by default.
- **Throughput-adaptive sizing**: reacts one transfer late and needs
  saturation heuristics; the concurrency signal is direct and simple.

## 4. Testing

A dialer seam on the pool (the func producing a new shard conn) lets unit
tests count and fake dials: reuse across sequential acquires, growth under
concurrent acquires, least-loaded selection, eviction of dead conns and
stale identities, idle scale-down with a short TTL, drain on Close.
The existing e2e sharded suite exercises acquire/release against a real
server; a sequential-pulls e2e asserts the second transfer opens no new
connections (dial-count seam).

## 5. Out of scope

- CLI flags for pool tuning.
- Mid-transfer channel growth.
- Pooling the control connection (already persistent).
