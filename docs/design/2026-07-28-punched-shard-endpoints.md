# Punched shard endpoints: sharded store transfers through NAT

Date: 2026-07-28
Status: accepted design (pre-implementation)
Scope: one arc — the server's dedicated data endpoints become first-class,
independently punchable iroh endpoints (own keys, relay home connection, QUIC
address discovery), advertised in-band on `TAccept`/`TRef`, so a sharding
client's extra connections hole-punch to them exactly the way the control
connection already does. Client-side, shard dials trade their bare bind for
the control dial's full stack, gated on the control path being direct. One
new wire field, no ALPN bump, no go-iroh fork changes expected.

## 1. Problem

A runner on a public IP (Hetzner) pulling from a server behind NAT moves
1 GB in >10 minutes (~1.5 MB/s) over a **direct** path. The runner log shows
why:

```
09:50:32 WARN server connection goes through a relay …    link=store
09:50:37 INFO server connection is direct  addr=ip:31.165.170.159:40968
09:51:10 INFO amberclient: sharded transfers disabled for this connection
              reason="no shard attached within budget"
```

The v0.17.x go-iroh fork fixes work — the control connection punches to
direct in ~5s. But every shard attach fails, the client demotes itself
(sticky, `amberclient/shard.go:167-169`), and the whole transfer rides one
QUIC connection — whose single UDP socket loop is the throughput cap that
sharding exists to bypass (`amberclient/shard.go:3-9`).

The shard attaches fail structurally, not incidentally:

- The control connection punches because it dials with the full stack —
  relay enabled, `WithNetReport` (QAD), seeded local candidates
  (`amberclient/dial.go:67-83,116-143`) — which is what feeds QNT punch
  coordination on both sides.
- A shard endpoint binds **bare** (`amberclient/shard.go:54-61`): no relay,
  no net report, no seeded candidates. Its dial is a blind UDP shot.
- Shot one, the server's dedicated data ports: kernel-assigned
  (`serve/serve.go:225`, bind port 0), so the NAT has no mapping at all.
- Shot two, the control candidates: the punched mapping `:40968` exists, but
  a port-restricted NAT filters by four-tuple — the shard's fresh source
  port was never punched for, so the packet is dropped.

Zero attaches inside the 3s budget → demote. Nothing retries until the
control connection itself is re-dialed.

## 2. Design

### 2.1 Server: data endpoints become first-class iroh endpoints

Today the data endpoints share the server's secret key and exist only as
ports (`serve/serve.go:216-235`). Instead, each binds with:

- its **own generated secret key** — a data endpoint gets its own identity;
- the same relay mode as the main endpoint (the `--relay` flag through
  `amberiroh.FromFlag`, default map otherwise);
- `iroh.WithNetReport()` — QAD against the relays learns the endpoint's
  public NAT mapping and keeps it alive;
- the existing `--bind` host handling (port 0 stays).

Each data endpoint therefore holds a relay home connection and knows its
observed public address — it is reachable via relay and punchable via QNT,
by exactly the machinery the fork just fixed for the main endpoint. Data
endpoints do NOT announce (no pkarr, no mDNS); their reachability travels
in-band (§2.2). Relay connect remains best-effort, as for the main endpoint:
a server on an offline LAN still starts, and its records then carry only
direct addresses — the LAN fast path is unaffected.

Identity note: shard dials now authenticate a data endpoint's own ID rather
than the server's. Trust is delegated: the IDs arrive on the control
connection, which authenticated the server; the shard handshake then proves
possession of the advertised key. (This is the same trust shape as the
transfer token itself.)

`serve` wires the addresses with a snapshot closure instead of a static
list: `amberSrv.SetDataEndpoints(func() []DataEndpointRec)` (the wire type,
§2.2) reading each
endpoint's live `Addr()` (go-iroh `iroh/endpoint.go:927`) at frame-build
time, so a `TAccept` written before the relay lands carries what exists then
(direct addrs), and later frames pick up the relay and QAD candidates as
they appear. `SetDataPorts` stays (`amberiroh/server.go:135-137`): the
dedicated-port fast path remains the LAN/public-server route.

`attachWait` grows 5s → 10s (`amberiroh/server.go:132`). `gather`
early-exits once all promised shards attach (`amberiroh/server.go:88-106`,
`for len(out) < n`), so the bump costs nothing when attaches are fast — it
only extends patience for punching stragglers.

### 2.2 Wire: one new Msg field, no ALPN bump

```go
// on TAccept (amberiroh/server.go:314) and sharded-pull TRef (…:376-381),
// alongside DataPorts
DataEndpoints []DataEndpointRec `cbor:"16,keyasint,omitempty"`

type DataEndpointRec struct {
        ID    []byte   `cbor:"0,keyasint"` // 32-byte endpoint ID
        Addrs []string `cbor:"1,keyasint"` // TransportAddr strings: "ip:host:port", "relay:url"
}
```

Compatibility falls out of the CBOR modes already in use
(`amberiroh/protocol.go:104-108`): `cbor.DecOptions{}` ignores unknown map
keys, so an old client skips field 16 and keeps direct-dialing `DataPorts`;
`omitempty` means an old server simply omits it and a new client falls back
to today's path. Every old/new pairing stays *correct* (at worst
single-connection slow), so no ALPN fence is needed — the fence rule
(CLAUDE.md invariants) is for wrong results, not slow ones.

**Field presence is the capability signal.** A client that sees
`DataEndpoints` knows the server gathers for 10s and may spend an 8s attach
budget; absence means an old server's 5s window and today's 3s budget
(`amberclient/shard.go:38-46`).

### 2.3 Client: shard dials get the control dial's stack

With `DataEndpoints` present, `extraConn` (`amberclient/shard.go:54-82`)
changes to:

1. Bind the shard endpoint full-stack: fresh key, relay mode,
   `WithNetReport()`, `seedLocalCandidates` — plus the pinned bind address
   as today. (mDNS and pkarr are not needed: candidates are provided.)
2. One `raceConnect` (`amberclient/dial.go:174-224`) per shard against the
   union of `{ip:controlIP:dataPort}` and the advertised `Addrs`,
   authenticating the **data endpoint's ID**. On a LAN the ip candidate wins
   in milliseconds; through NAT the relay candidate wins in ~1s, `TAttach`
   rides it immediately (well inside the budget), and QNT punches the
   connection to direct seconds into the transfer — mid-transfer path
   migration, same as the control connection's upgrade. The union race
   replaces the sequential dedicated-dial sub-budget
   (`dedicatedDialTimeout`, `amberclient/shard.go:45`), which survives only
   on the old-server path.

Without `DataEndpoints` (old server), the current code path runs unchanged:
bare endpoint, server ID, dedicated-port then control-candidate dials, 3s
budget, sticky demote.

The per-shard round-robin over `ports[i%len]` becomes per-shard over the
`DataEndpoints` entries (falling back to index-matching `DataPorts` when the
record lacks addresses), preserving the spread across server sockets.

### 2.4 Gate and demote semantics

- **Gate on control path.** Before attaching extras, consult `c.Path()`
  (`amberclient/path.go:50-55`). If the control path is currently *relayed*,
  skip extras for this transfer — logged once per connection, **no demote**.
  When relay-bound, extra relay connections move no additional bytes; and
  the path commonly upgrades moments later, so the next transfer retries.
- **Sticky demote stays** for genuine zero-attach on a live context
  (`amberclient/shard.go:165-169`), old and new path alike. Under the new
  path it now means LAN, relay, and punch all failed within 8s — rare, and
  re-dialing the control connection resets it, as today.

Consumers inherit the fix with no interface change: runnerd's sync client,
`remote-build --conns`, registry `--sync-conns`, and a NAT'd `jobs-client`
(whose shard endpoints, carrying net report, gain QAD candidates for the
both-sides-NAT punch, mirroring `amberclient/dial.go:71-77`).

## 3. Alternatives considered

- **Same-key data endpoints + QAD-advertised addresses + hand-rolled
  simultaneous-open punch** coordinated over the control stream (a
  `TAttachIntent` frame; the server's data endpoints fire short-timeout
  dials at the client's announced shard addresses while the client dials
  in). Direct from the first byte and keeps the single server identity — but
  it is a novel punch protocol whose races and timeouts this repo would own,
  it fails hard (no relay fallback) on symmetric NATs where QAD's mapped
  port is wrong, a NAT'd client needs its own per-socket QAD choreography,
  and the NAT-keepalive probes cost about as much relay chatter as home
  connections. Rejected in favor of riding the battle-tested QNT machinery.
- **Main-socket-only punching** (shards punch to the main endpoint, data
  endpoints stay unreachable): smallest change, but all shards then share
  the server's single socket loop — it abandons the server-side spread that
  is the point of the data endpoints. Rejected as scope, though its
  mechanics (full-stack shard dials) are subsumed by this design.
- **Pinned data-endpoint ports + router port-forwarding** (`--data-port-base`):
  works, but pushes per-deployment router configuration onto every NAT'd
  server. May still land later as an operator escape hatch; this design
  removes the need.

## 4. Risks and accepted trade-offs

- **Straggler shards.** Want rounds barrier on the slowest channel
  (`amberiroh/wantsync.go:170-320`): a shard stuck on the relay while its
  siblings punched becomes the per-round drag. The gate makes this rare
  (control proved the pair punchable), and the 30s read watchdog
  (`amberclient/shard.go:177-202`) still catches parked channels. Future
  option if it bites: deal wants by observed per-channel throughput instead
  of round-robin.
- **Standing relay connections.** N+1 per server (4 at the default
  `--data-endpoints 3`). Idle relay home connections are cheap keepalives;
  accepted.
- **First seconds of a transfer ride the relay** on punched shards. For the
  multi-hundred-MB transfers sharding targets, the ~5s ramp is noise.

## 5. Testing

- `amberiroh`: `Msg` round-trip with `DataEndpoints`; forward/backward
  compat (new frame decoded by a field-16-less struct ignores it; absent
  field yields nil).
- `amberclient`: candidate assembly (dedicated + advertised union, ID
  selection old vs new server); budget selection by field presence; gate
  behavior on a relayed control path (unit-level, against `pathOf` inputs);
  demote untouched by gate skips.
- `serve`: data endpoints bind distinct keys; the snapshot closure reflects
  live `Addr()` content; ports and records stay index-aligned.
- End-to-end: sharded pull through the existing serve/loop test harnesses on
  localhost (punch mechanics themselves are go-iroh's, regression-tested by
  the fork's `iroh/upgrade_test.go` in-process-relay test).
- Darwin cross-vet per CLAUDE.md (no `_linux.go` twins expected, but the
  rule stands).

## 6. Out of scope

- Persistent (cross-transfer) shard connections — today's per-transfer
  attach lifecycle stays; punching adds ~1s setup per shard per transfer,
  amortized over the large transfers sharding exists for.
- Throughput-weighted want dealing (§4).
- `--data-port-base` operator escape hatch.
- Any go-iroh fork change: the design deliberately uses only existing
  public API (`Bind` options, `Addr()`, `Connect`, QNT/punch internals
  untouched).
