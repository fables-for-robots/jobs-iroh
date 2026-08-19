# Remote-build TUI: navigable build graph with per-node output

Date: 2026-08-19
Status: accepted design (pre-implementation)
Scope: a full-screen bubbletea view for `remote-build` and `watch` on a TTY —
a navigable build-graph tree (logical rows, per-node state/durations/cached)
with a live output pane per selected node. One additive wire change
(`NodeSnap.Deps`/`.Cached`); no ALPN bump, no identity impact. The current
block view and non-TTY change-lines remain as fallbacks.

## 1. Wire + server (additive, no ALPN bump)

`api.NodeSnap` gains two optional fields:

```go
Deps   []string `cbor:"deps,omitempty"`   // dep node names within the closure
Cached bool     `cbor:"cached,omitempty"` // fast-pathed done at creation (output ref existed)
```

- `sched.node` gains an explicit `cached bool`, set ONLY in `require()`'s
  doneness fast-path (`sched/node.go:148`). Explicit, never inferred from
  zero timestamps: `buildvalue` never gets `startedAt`, so inference would
  mark every done buildvalue cached. Crash-recovery resubmits and KP-memo
  buildrun hits go through the same fast-path and correctly read as cached.
- `assembleLocked` fills `Deps` (sorted node names, edges within the walked
  closure by construction) and copies `cached`.
- Compat: old client + new server ignores unknown CBOR fields. New client +
  old server sees no `Deps` on any node → the client falls back to the
  existing block view with a one-line notice. No flat-mode TUI layout.

## 2. Tree folding (pure function, client-side)

A **logical row** = a `buildvalue` or `import` node. From the graph
(`unfoldLocked`'s per-kind table):

- Root: the unique in-degree-0 node of the closure (the target; nothing in
  its own closure can depend on it — that would be a cycle).
- A buildvalue's **chain** = its deps of kinds
  `buildfrom`/`pluginresolve`/`pin`/`buildrun`. Chains appear progressively
  (`advanceBuildValueLocked` registers stages as reached) — the fold
  tolerates partial chains. Chain nodes can be shared (`buildrun` is the
  cross-context KP memo); they are display-folded per row.
- A row's **children** = the `buildvalue`/`import` nodes among its chain
  nodes' deps; an import row's child is its fetcher buildvalue.
- Row state, folded from the chain (first match wins):
  1. any chain node failed → **failed** (failing stage + its errSummary)
  2. row's own node failed/cancelled/upstream → that verdict
  3. any chain node running/publishing → **running**, stage shown as
     `eval`/`resolve`/`pin`/`build` (buildfrom/pluginresolve/pin/buildrun),
     with that node's elapsed
  4. any chain node queued → **queued**
  5. row node done → **done**: duration = buildrun's `ElapsedMs`;
     **(cached)** when `row.Cached || buildrun.Cached`
  6. else **waiting**
- DAG→tree: shared subtrees repeat under every parent (join = get-or-create
  means real sharing). Expansion state is **per path** (names joined
  root→row): default expanded, except cached-done subtrees (collapsed);
  user toggles override defaults and stick for the session.
- Row **log node** (what the output pane shows), by priority: failed chain
  node → running chain node → buildrun → the import node itself.
  `buildvalue` is server-internal and never has output.
- Elapsed extrapolation: running rows display
  `ElapsedMs + (now − snapshot arrival)` on a 1s tick — a client-clock
  delta, immune to server clock skew (same reason `ElapsedMs` is
  server-computed).

## 3. TUI component (`tui/` package)

New files `tui/buildtree.go` (folding) and `tui/buildview.go` (model), plus
an exported standalone entry:

```go
func RunBuildWatch(ctx context.Context, s BuildStreams, opts BuildWatchOptions) (BuildOutcome, error)

type BuildStreams interface {
    OpenWatch(ctx context.Context) (next func() (api.Snapshot, error), stop func(), err error)
    OpenLogs(ctx context.Context, node string) (api.LogView, func() (wire.LogChunk, error), func(), error)
    Cancel(ctx context.Context) error
}
```

`clientcli` implements `BuildStreams` over its `apiConn`
(`openRequest`/`openLogs`/`TCancel` already have the right shapes). The
admin TUI's detail view can adopt the component later — out of scope.

- Program: alt-screen, rendered to **stderr** (stdout stays
  machine-readable), input from stdin.
- Header: request id, phase, counts, total elapsed. Footer: key help +
  stream health (retrying note on a dropped stream).
- Left pane (~40% width, min 30 cols): the tree. Right pane: selected row's
  output — stored head/gap/tail first, then live follow chunks. Gen-guarded
  like `logs.go` (stale-attempt chunks skipped, retry marker on gen bump).
  Follow mode disengages on scroll-up, `G` re-engages (admin-TUI logs
  behavior). Output buffer capped at 512 KiB (drop oldest).
- Keys: `↑`/`↓` move over visible rows, `←`/`→`/`enter` collapse/expand,
  `o` toggle maximized output pane, `pgup`/`pgdn` scroll output, `G`
  follow, `q` detach, `c`/`ctrl-c` → y/n confirm → cancel.
- Selection change closes the old log stream and opens the new one,
  seq-guarded against stale messages (the `tui/builds.go` pattern). Watch
  and log streams retry on the 1s tick — transport errors only, server
  answers (not-found) are terminal.
- Never block in `Update` — all I/O in `tea.Cmd` goroutines (tui/ rule).

## 4. Command integration

Activation matrix (both `remote-build` and `watch`): the TUI runs when
stdin AND stderr are TTYs and `--no-tui` is absent. `--no-tui` on a TTY →
today's block view. Non-TTY → today's change-lines. `watchview.go`,
`logtracker.go` stay for the fallback paths; `--no-logs` keeps meaning
only there (TUI logs are on-demand).

Exit flow, `remote-build`:

- terminal **done** → TUI auto-exits; pull-home + `build:`/`output:`
  stdout lines exactly as today; exit 0.
- terminal **failed** → TUI stays for inspection; on `q`: print
  failureSummary + re-attach/diagnose hints (not the full failure logs —
  they were inspectable in the TUI); exit 1.
- terminal **cancelled** (own cancel or external) → auto-exit, "build
  cancelled", exit 130.
- `q` mid-build → **detach**: build keeps running server-side; re-attach
  hint on stderr; no pull; exit 0.
- In TUI mode the outer SIGINT handler is NOT installed — bubbletea owns
  ctrl-c (confirm-cancel). SIGTERM ends the program ≙ detach.

`watch` mirrors everything minus pull-home and the submit phases.

## 5. Testing

- Folding: table tests over synthetic `[]api.NodeSnap` — chains, shared
  deps (join), partial chains, cached collapse default, import→fetcher
  edge, root derivation, no-deps (old server) detection.
- Rendering: pure row/pane formatters asserted as strings
  (`watchview_test`/`format_test` pattern).
- Model: update-loop tests for key routing, watch/log seq guards, gen
  guards, terminal-snapshot exit behavior.
- Server: `assembleLocked` asserts `Deps` + `Cached` (extend sched tests).
- CLAUDE.md gates: darwin cross-vet, full `go test ./...`.
