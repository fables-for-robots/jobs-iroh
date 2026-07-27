# amberiroh — folding amber-store-iroh into jobs-iroh

**Date:** 2026-07-27
**Status:** design, approved for planning

## Goal

Absorb the four amber-store-iroh library packages into a single in-tree
`amberiroh` package and drop `github.com/jobs-build/amber-store-iroh` from
`go.mod`. jobs-iroh stops depending on the repo it was the only external
consumer of.

## Motivation

jobs-iroh is the sole external consumer of amber-store-iroh's library
packages — `assimilate` never imported them (it only ever saw them as an
indirect dependency through jobs-iroh). The split therefore buys no reuse,
while costing a cross-repo release dance: every protocol change needs an
upstream commit, a pseudo-version bump, and a jobs-iroh `go get` before it
can be tested end to end.

Two of the five ALPNs a jobs-server endpoint speaks — `jobs-runner-amber/1.0`
and `jobs-amber-admin/1.0` — are served directly by upstream's
`server.Server`. That code is not a peripheral utility; it is part of the
server's public surface, and it belongs in the same tree as the server.

## Scope

**In scope.** Copy `protocol`, `wantsync`, `server` and `relaymode` into one
`amberiroh` package; prune what jobs-iroh cannot reach; repoint the 9 import
lines; drop the dependency.

**Out of scope.** The upstream repo is left untouched and keeps building its
`amber` and `amber-serve` CLIs from its own copy. `amberclient/` is not
folded in — it holds jobs-iroh-specific sharding (`--sync-conns`) that is not
part of the vendored protocol. `amber-store-core` remains an external
dependency.

## End state

```
jobs-iroh/
├── amber/          # store seam over amber-store-core (unchanged)
├── amberclient/    # sharded sync client (imports change only)
├── amberiroh/      # <- this spec
│   ├── protocol.go
│   ├── pack.go
│   ├── wantsync.go
│   ├── server.go
│   ├── relaymode.go
│   └── *_test.go   # 6 files, all in-package
└── natsiroh/       # NATS-over-iroh tunnel (unrelated, unchanged)
```

`go.mod` loses `github.com/jobs-build/amber-store-iroh`. Nothing is added:
the four packages import only `amber-store-core`, `tmc/go-iroh` and
`fxamacker/cbor`, all already direct dependencies. **The dependency graph
shrinks by exactly one module and gains nothing.**

## Why one package rather than four

The four upstream packages have **disjoint namespaces**: 123 top-level
declarations across them, **zero collisions**, verified with an AST scan over
both source and test files. There is no `TestMain` in any of them and no
filename clashes. Merging is therefore mechanical rather than a rename
exercise.

The cost is that verbatim directory-diffing against upstream stops working as
a drift check. This is accepted (see Risks).

## What moves

| Source | Destination | Treatment |
|---|---|---|
| `protocol/protocol.go` | `amberiroh/protocol.go` | verbatim |
| `protocol/pack.go` | `amberiroh/pack.go` | verbatim **minus `SendPack`** |
| `wantsync/wantsync.go` | `amberiroh/wantsync.go` | verbatim |
| `server/server.go` | `amberiroh/server.go` | verbatim **minus `Serve`, `serveConn`** |
| `relaymode/relaymode.go` | `amberiroh/relaymode.go` | verbatim |
| `protocol/{protocol,pack}_test.go` | `amberiroh/` | verbatim + `sendPack` helper |
| `wantsync/{wantsync,loop}_test.go` | `amberiroh/` | verbatim |
| `server/server_test.go` | `amberiroh/` | verbatim |
| `relaymode/relaymode_test.go` | `amberiroh/` | verbatim |

Mechanical edits: four `package` declarations become `package amberiroh`; the
four package doc comments are synthesized into one; intra-package qualifiers
are dropped (`protocol.Msg` → `Msg`, `wantsync.Send` → `Send`,
`ambserver.New` → `New`).

## What is pruned

Reachability was measured with `golang.org/x/tools/cmd/deadcode` from
jobs-iroh's four `cmd/` mains, not by inspection. It reports exactly three
unreachable functions.

**Deleted outright — 54 LOC, no test references:**

- `Server.Serve` (34 LOC) — accept loop over an `iroh.Endpoint`.
- `Server.serveConn` (20 LOC) — its per-connection helper.

These are the standalone-listener path that only upstream's `amber-serve`
binary used. jobs-iroh mounts the handler through its own iroh Router
(`serve/serve.go:299`, `amberConnHandler`), so this path is unreachable **by
construction**, not by accident. Removing it also removes the only reason
`amberiroh` would need to own an endpoint lifecycle.

**Relocated to test scope — `SendPack` (23 LOC):**

`deadcode` flags `SendPack` as unreachable from the binaries, but it is not
unused: five call sites across `pack_test.go`, `server_test.go` and
`loop_test.go` construct wire payloads with it, including the multi-round
transfer loop test. Deleting it outright would take real coverage of code
being **kept**.

Because every upstream test is in-package (`package protocol`, not
`package protocol_test`), the merge makes them all `package amberiroh`
internal tests. `SendPack` therefore moves into a `_test.go` file as
unexported `sendPack`. This satisfies the goal — it leaves the production
binary and `deadcode` reports clean — at zero coverage cost.

`chunkWriter` **stays in production code**: it is shared with
`SendPackRecords`, the zero-copy push path `amberclient` actually uses.

Net: production code drops 77 LOC (54 deleted, 23 moved to tests), from
~1,257 to ~1,180 across 5 source files. Test code grows by the relocated
helper, ~1,419 to ~1,442 LOC across 6 files.

## Call-site changes

Nine import lines across seven files, all in jobs-iroh:

| File | Currently imports | Becomes |
|---|---|---|
| `amberclient/client.go` | `protocol`, `wantsync` | `amberiroh` |
| `amberclient/shard.go` | `protocol`, `wantsync` | `amberiroh` |
| `runnerd/sync.go` | `protocol` | `amberiroh` |
| `registryd/sync.go` | `protocol` | `amberiroh` |
| `serve/serve.go` | `server` (as `ambserver`) | `amberiroh` |
| `serve/announce.go` | `relaymode` | `amberiroh` |
| `serve/serve_test.go` | `protocol` | `amberiroh` |

Call sites change qualifier only — `protocol.WriteMsg` → `amberiroh.WriteMsg`,
`ambserver.New` → `amberiroh.New`, `relaymode.FromFlag` →
`amberiroh.FromFlag`. No signatures change.

## Testing

1. **The inherited suite is the oracle.** All 6 upstream test files move with
   their assertions unchanged and must pass as-is — the only edits are the
   `package` line, dropped qualifiers, and `SendPack` → `sendPack`. Passing
   otherwise-verbatim tests is what proves the transformation preserved
   behaviour; nothing else gives that guarantee as cheaply.
2. **Full jobs-iroh suite stays green** — 30 packages including the real
   sandbox and iroh-networking end-to-end tests. This is currently passing,
   so any regression is attributable.
3. **`deadcode` re-run** over `./cmd/...` reports no unreachable functions in
   `amberiroh`.
4. **`go mod tidy` is a check, not a step**: if anything still references
   amber-store-iroh, tidy puts it back and the dependency-removal goal has
   silently failed. Assert its absence from `go.mod` explicitly.

## Risks

**Drift — accepted.** Two copies of a wire protocol now exist, both speaking
the same `protocol.ALPN`, with no automated reconciliation. Collapsing the
package boundaries means directory-diffing against upstream no longer works,
so divergence will be silent. The mitigation is ownership, not tooling:
jobs-iroh's copy is authoritative, and upstream's is a CLI-only fork. If the
CLIs are ever needed again, take them from upstream rather than re-syncing
the library.

**Identity — none.** No chunker parameters, canonical-CBOR encoding, or
`cover`/`KPVersion` logic is touched. Build keys are unaffected.

**Wire compatibility — none.** `ALPN` and every frame constant move
verbatim. No `jobs-runner-nats` fence bump is required; a 0.13 server and a
0.12 runner remain compatible on the amber ALPNs.

**`--relay` flag surface — unchanged.** `relaymode.FromFlag` moves verbatim,
so `jobs-server --relay` keeps its current parsing.

## Follow-ups (not in this change)

- CLAUDE.md package map gains an `amberiroh/` row; the `amber-store-iroh`
  mention in the `amberclient/` row is dropped.
- README's dependency list, if it names amber-store-iroh.
