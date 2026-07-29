# Node labels: names instead of CAS keys in build progress

Date: 2026-07-29
Status: accepted design (pre-implementation)
Scope: one arc — human-readable, display-only labels for graph nodes in
`build` / `remote-build` / `watch` / `status` / `diagnose` / TUI output.
Labels never enter identity (nothing is added to `Canonical()`, no
`KPVersion` bump, no ALPN fence); every wire addition is an optional CBOR
field, so old/new peers interoperate showing today's keys.

## 1. Sources of names (all exist today, all dropped)

- `builddef.PinnedInput.Name` (`builddef/refs.go:61`) — recipe dep names —
  stripped by `unfoldLocked` (`sched/node.go:272`) and by the local
  driver's `ensureInputs` (`runner/localbuild.go:256-258`).
- `PluginResolved.Plugins`/`.Deps` map keys — flattened away by
  `sortedInputs` (`sched/node.go:669`) and dropped by `ensurePinDeps`.
- `Definition.Dir` — a buildfrom's own human context.
- `importdef` already renders `fetch <fetcher> <params>`
  (`importFetchLabel`, `runner/develop_linux.go:128`) on the local path;
  the server side shows raw keys.
- The root target: the client resolves and prints `context: <root>
  (dir …)` at submit time but sends no name.

## 2. Server (sched)

`node` gains `label string`, display-only. Assignment rules, first
non-empty wins (joins keep first-seen):

1. Edge names: `require`/`requireInputLocked` grow a label parameter;
   unfold passes `PinnedInput.Name`, the plugin/dep map keys (via a
   name-preserving `sortedNamedInputs`), and `"fetcher <name>"` for a
   recipe-declared fetcher's build def.
2. Self-derivation (in `unfoldLocked`, where the def is already decoded):
   a label-less buildfrom takes `def.Dir`; a label-less import takes
   `fetch <fetcher>`.
3. Root: `api.SubmitRequest.Label` (new, omitempty) — the client sends the
   resolved context dir (or the source root's base name). `Submit` stamps
   it on the request's target chain.

`api.NodeSnap.Label` (new, omitempty) carries it out in snapshots — built
from in-memory nodes (`sched/submit.go:373`), so restarts simply re-derive
at re-unfold. Failure records gain the label at write time so `diagnose`
names the failed node after retries and restarts.

## 3. Client rendering

`shortNode` gains a labeled form: with a label, `<label> (<kind>:<key8>)`;
without, exactly today's `<kind>:<key8>`. Applied to the live progress
running/failed lists, the one-line status output, the `--logs` line prefix
(label replaces the node name), and diagnose reports. The label lookup
comes from the latest snapshot (the client keeps node→label from the
watch stream).

## 4. Local build (runner/developDriver)

`ensureInput`/`ensureBuild` grow a name parameter threaded from
`ensureInputs` (`pi.Name`) and `ensurePinDeps` (map keys);
`ensureBuild`'s step label becomes `build <name>` (fallback: today's
short key form). Imports keep `importFetchLabel`. `develop_other.go` has
no twin of these funcs; darwin cross-vet guards regardless.

## 5. Testing

Unit: `sortedNamedInputs` order+names; label-assignment rules on the sched
node (first-wins, self-derivation); labeled `shortNode` rendering; local
driver label threading. The existing sched/serve/clientcli suites pin
compatibility (absent labels render as before).

## 6. Out of scope

- Persisting labels beyond failure records.
- Renaming NATS subjects or status-KV keys (node identity strings stay).
- TUI-specific layout work beyond rendering the label where node names
  show today.
