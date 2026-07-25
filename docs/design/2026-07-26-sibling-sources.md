# Sibling sources: monorepo-aware builds

Date: 2026-07-26
Status: accepted design (pre-implementation)
Scope: one arc — context widening + covered closure + KP-keyed buildrun (core),
generated sources, and sibling builds. Ecosystem plugins: Go, Rust.

This design was produced from a full subsystem survey (sched, pin/plugins,
identity/store, serve/gate, recipe, local pipeline), a cross-ecosystem
requirements survey, and an adversarial review pass; the review findings are
folded in as spec requirements, marked **[INV]** where they are correctness
invariants rather than preferences.

## 1. Problem and reframing

A `BUILD.jobs` in a monorepo subdirectory cannot reference source outside its
own directory: Go `replace ../lib` directives, cargo path-deps, workspace
manifests, Maven parent poms, shared proto dirs, and symlinks into sibling
directories are all inexpressible. `source.read("../x")` silently collapses
into the env tree (`runner/source.go:61-63`), `subbuild` rejects `..`
(`recipe/subbuild.go:70-86`), and the F-tree simply does not contain siblings.

The system already has early cutoff — at `dir` granularity:

- K already hashes {whole source tree, dir, params, platform, build-file}
  (`clientcli/remote.go:33-54`); the whole tree is ingested and pushed on
  every path today (`remote.go:119,144`, `runner/localbuild.go:25`).
- buildfrom narrows to the `dir` subtree (`runner/buildfrom.go:32`); F hashes
  only {env/, params, platform, override}. Changes outside `dir` re-run only
  buildfrom (cheap, pure store computation).

In this design's vocabulary, today's K already plays the widened-F role and
today's F is "KP with covered-set = {dir}". The change is to **move the
narrowing from buildfrom (static, dir-only) to pin (dynamic,
closure-aware)**: F widens to the whole source context; pin computes the
covered closure (declared + plugin-discovered paths + symlink chasing +
generated files); buildrun is re-keyed by KP = key of {pinned job, platform,
closure-algorithm version, covered subset}, restoring cutoff at exactly the
granularity the build depends on.

Upstream anticipated this and deferred it twice
(2026-06-25-subbuild-descendant-inputs-design.md §2/§11: "weakens F dedup …
reopens dependency cycles"; 2026-06-25-build-from-content-addressing-design.md
§14). The KP layer answers the F-dedup objection; §8's engine-level cycle
detection answers the cycle objection now that sibling builds are in scope.

## 2. Decisions of record

| Decision | Choice |
|---|---|
| Activation | **Always-on for subdir builds**: every definition with `dir != ""` gets widened-context semantics, marked structurally in the definition (§3.2). Root builds (`dir == ""`) are byte-for-byte unchanged. One-time re-key of all subdir builds, accepted. |
| Covered-tree timestamps | **Normalized to the ZIP epoch** — 1980-01-01T00:00:00Z (Unix 315532800). uid/gid zeroed, modes preserved. KP is a pure content+mode hash. |
| Phasing | **One arc**: core + generated sources + sibling builds designed and implemented together. |
| Plugins | **Go** (extend goplugin: replace directives + go.work) and **Rust** (new cargo plugin: workspace membership, path deps, reduced manifests + pruned Cargo.lock). |
| Declaration surface | `sources = [...]` on the `build()` return; `//`-rooted paths; plugins feed it (§5.2). |
| Sibling builds | `subbuild("//path")` — same context tree, different dir; scheduler cycle detection (§8). |
| Symlink policy | Chased links that do not resolve in-root: keep + warn. Declared sources missing: pin-time hard fail. Links escaping the context root: hard fail with per-recipe escape hatch (§5.4). |
| Sandbox | CWD = `$SRC` = `/build/src/<dir>`; new `$SRC_ROOT` = `/build/src`. Existing recipes keep their meaning (§9). |

## 3. Identity

### 3.1 Context

The **context** is the source tree the definition already carries: the pushed
tree for tree-sourced builds, the import-output tree for import-sourced
builds. The **context root** is that tree's root. For local commands the
client defaults the ingest root to the git repository root (§11.1); for
fetched repos it is the fetched tree root. Identity never depends on whether
the source was a git repo — the git-root detection is client-side sugar that
changes only *which tree gets ingested*.

### 3.2 The widened F and the definition marker

For `dir != ""` definitions, the build-from tree becomes:

```
{ env/      — the WHOLE context tree, spliced by key (entry name unchanged)
  dir       — file containing the dir string
  params    — as today
  platform  — as today
  [BUILD.jobs] — override, resolved against env/<dir>/ (was env/) }
```

For `dir == ""` definitions nothing changes: no `dir` entry, `env/` is the
whole tree already — F is byte-identical to today.

**The semantics are marked in the definition, not inferred.** Every def
constructor (clientcli `treeDefinition`, `recipe/input.go` `bld`/`subbuild`)
emits a new field `ctx: 2` (canonical CBOR, omitempty) whenever `dir != ""`.
Consequences:

- Subdir builds re-key once (new K). Root builds keep their K.
- A `ctx: 2` def is never confused with a legacy narrow def under the same K,
  so stale `build-from:K → F_narrow` bindings cannot poison widened builds.
- **[INV] Old servers reject `ctx: 2` defs loudly — verified.** The submit
  path's canonicality re-check (`sched/submit.go:47-59`) decodes into the
  typed struct, re-encodes only the known fields via `Canonical()`
  (`builddef/definition.go:70-80`), and byte-compares — a def carrying `ctx`
  re-encodes without it, differs, and is bounced with `badRequest` before
  any ingest. Two implementation consequences: (a) `Definition.Canonical()`
  builds its output by explicit field copy — it MUST gain the `ctx` copy, or
  new servers strip the field and reject every widened def themselves;
  (b) the rejection text ("definition is not canonical CBOR") is misleading
  for a valid newer-schema def — release-note it.
- **[INV] Old runners must be fenced at the fleet level.** Definitions
  travel in-band for K-kind jobs (`sched/node.go:471-472`) and hash-verify
  byte-verbatim (`runnerd/job.go:110-118`), so an old runner decodes a
  `ctx: 2` def with unknown-field-ignoring `DecodeDefinition`, silently
  drops `ctx`, and computes a NARROW F under the new K — and the gate's
  buildfrom row is name-keyed, so `build-from:K → F_narrow` would commit
  and poison the binding. New-driver strictness cannot fix old binaries,
  and runners pull work straight off JetStream queues, so a hello-field
  handshake could be ignored by exactly the binaries it must stop. The
  fence is the connection itself: the runner-NATS ALPN is bumped
  (`jobs-runner-nats/1.0` → `/2.0`), so an old runner fails loudly at dial
  time. Bump it again whenever a change would make an old runner produce
  wrong results rather than clean errors. New runner drivers additionally
  hard-fail on `ctx` values they do not implement (forward-compat for
  `ctx: 3+`).

`resolveRecipeOverride` re-anchors: effective recipe = inline `buildJobs`,
else `env/<dir>/<buildFile>`, else `env/<dir>/BUILD.jobs`; spliced as
top-level `BUILD.jobs` only when it differs from `env/<dir>/BUILD.jobs`
(join-preserving omission, as today). Shared verbatim between server and
local paths — the local/remote F-equality invariant is preserved because
there is exactly one ingest and trees travel by key.

### 3.3 KP

KP is the root key of a deterministic store tree assembled
`BuildFromTree`-style (fixed synthetic modes for the synthetic entries,
bytewise-sorted, pinned chunkers):

```
{ job.cbor  — the canonical Pinned bytes (the build-pinned blob, verbatim)
  platform  — file containing the placement platform string
  v         — file containing the closure-algorithm version (ASCII integer)
  src/      — the covered tree (§6) }
```

- **[INV] `platform` is mandatory.** `Pinned` has no platform field; the
  shell resolves at pull time by node platform. Without this entry a
  pure-script recipe pins byte-identically on amd64 and arm64, collides on
  one KP, and cross-platform memo hits serve wrong binaries — and the
  scheduler's `nodeID{kind,key}` would collapse both platforms onto one node.
  Platform ∈ F is a deliberate upstream safety property; KP keeps it.
- **[INV] `v` is the closure/prune algorithm version; it starts at 2,
  matching `ctx: 2`.** Derived bindings are forever (pin never re-runs once
  `build-pinned:F` exists); without a version a walker bug would survive its
  own fix in every memoized binding. Bump `v` on any semantic change to
  §5/§6; the `pin-cover` ref name carries the same version
  (`pin-cover/2:F`), so stale bindings are superseded, not trusted.
- Params correctly do NOT appear: they exist only at eval time; the buildrun
  sandbox receives only pinned Env/Script + inputs + shell. Param and
  recipe-comment changes that do not alter the pinned job join. Resources
  remain inside `job.cbor` (no regression; the `PinnedResources` doc-comment
  "never affects build identity or joins" must be updated — under KP it
  does).
- Cache *ids* ride in `job.cbor`; cache *state* never enters KP (caches are
  declared non-hermetic accelerators, unchanged).

`Pinned` gains three omitempty fields (old pinned blobs stay byte-identical):

```go
Sources   []string          `cbor:"sources,omitempty"`   // sorted, root-relative covered paths
Dir       string            `cbor:"dir,omitempty"`       // the build dir (sandbox CWD, §9)
Generated map[string][]byte `cbor:"generated,omitempty"` // root-relative path -> synthesized content (§7)
```

## 4. Ref namespaces (new)

| Ref | Written by | Points at |
|---|---|---|
| `pin-cover/2:F` | server, at pin commit or on demand (§6.3) | KP |
| `kp-tree/<KP>` | server, same | KP (name-carrier so runners pull the closure by name; `f-tree/` precedent) |
| `build-pinned:<KP>` | server, same | the pinned blob (so runner drivers and unfold keep reading `build-pinned:<nodekey>` unchanged) |
| `build-output-deps:<KP>`, then `build-output:<KP>` | server commit of a gated buildrun batch | the deps/output trees; `build-output:<KP>` is **the doneness marker and the memo, one and the same**. **[INV] deps strictly before output** — the runner already proposes in this order and the server preserves payload order; a crash between them the other way round would memoize a closure-free build that nothing self-heals (§10.2 rule 2 applies to this pair first) |
| `build-output:F`, `build-output-deps:F` | server aliases (§10.2) | same keys as the KP refs |

The gate needs no new rows for server-authored namespaces (default-deny
covers them); the buildrun row re-points to KP automatically because the
allow-table is keyed by the node's own key.

## 5. The covered closure (pin time)

### 5.1 Seeds

Always: `dir` itself and the effective recipe (override included). Plus
recipe-declared `sources`, plus plugin-contributed paths.

### 5.2 Declaration

`build()` may return `sources = ["//lib/common", "//pom.xml", ...]`:
root-relative `//` paths, files or directories. `../x` is accepted as sugar
and normalized against `dir` before validation — no recipe-layer validator
learns upward traversal, and all CAS path operations stay descend-only
(root-relative resolution happens before any tree op). The `//` prefix is
surface syntax only: `Pinned.Sources` and `Pinned.Generated` store plain
root-relative paths with the prefix stripped. Plugins return plain path
strings anywhere in their CBOR response (they pass through the rehydrator
untouched); the recipe forwards them into `sources`.

### 5.3 Mechanical expansion (core walker, always on)

Normalize, dedup, collapse nested paths (a path inside another covered path
is dropped). Validate: declared paths must exist in the context tree —
missing declared paths are a pin-time hard failure with the path named.

### 5.4 Symlinks

The walk over the covered set chases symlinks **component-wise, in-store**
(entries carry `LinkTarget`; symlink entries are keyless leaves):

- Resolution is per component with a loop budget of 40 link traversals per
  path (the Linux ELOOP convention): a path `a/b/c` where `b` is itself a
  symlink must resolve `b` first; **every intermediate symlink encountered
  joins the closure**, plus final targets, transitively. (Whole-target
  lexical cleaning is wrong: it either hard-fails paths that work at runtime
  or silently drops real inputs.)
- Targets resolving inside the root: added to the closure.
- Targets that do not resolve in-root (dangling — `.amberignore` legitimately
  manufactures these by dropping ignored targets of checked-in links): the
  link is kept as-is, a warning is emitted, no target is added. This matches
  today's sandbox semantics.
- Targets escaping the context root (absolute, or `..` past root): pin-time
  hard failure, with a per-recipe escape hatch
  (`sources_allow_escaping = ["path", ...]`); an allowed escaping link is
  kept verbatim and materializes as a dangling link in the sandbox.
- Directory symlinks to ancestors are legal; the closure degrades toward the
  root — warn when a chased target covers more than a quarter of the context
  tree's entries (constant, implementation-tunable; warning only, never a
  failure).

Non-UTF-8 path names: `Sources` is canonical-CBOR text; a chased path with
invalid UTF-8 fails pin with a clear error (fail-closed, documented).

## 6. The covered tree and KP derivation

### 6.1 PruneTree

New amber-seam op: `PruneTree(ctx, root key.Key, keep []path) (key.Key, error)`.
Organizes `keep` into a trie and recurses via `CollectEntries` +
`DirBuilder`, keeping covered entries and descending into partially-covered
directories.

**[INV] Normalization requires re-emitting every directory object in the
covered closure** — entry metadata (mode/uid/gid/mtime) lives in the parent
directory object, so "keep fully-covered subtrees by original key" and
"normalize metadata" are mutually exclusive. PruneTree re-emits all covered
directory objects with uid/gid = 0 and mtime = the ZIP epoch
(315532800 · 10⁹ ns), preserving modes; file *content* objects are shared by
key untouched. Cost is O(directories in the closure); the normalized tree is
deterministic and dedups across pins. Do not implement the cheap
spine-only variant — it silently reintroduces mtime-busted memos.

### 6.2 Assembly

covered = PruneTree(context, closure) overlaid with `Pinned.Generated`
entries (§7; generated content replaces or adds files at their root-relative
paths, synthetic mode 0444, ZIP-epoch mtime). KP tree = `{job.cbor, platform,
v, src/<covered>}` per §3.3.

**[INV] KP derivation is identity-critical shared code.** The server
(pin commit) and the local pipeline (`driveFStages`) must run the *same*
walk + normalize + prune + assemble implementation — pin it like the chunker
params, with a cross-path golden test (same inputs ⇒ same KP on both paths).
Divergence is benign for correctness (each side checks its own KP; misses
only) but silently kills the promised local↔remote memo join.

### 6.3 Server-side derivation, trust, and crash recovery

The server derives KP itself at pin commit (after gating `build-pinned:F`),
then writes `kp-tree/<KP>`, `build-pinned:<KP>`, `pin-cover/2:F` — matching
the gate discipline (runner-computed keys are never trusted; a hostile pin
runner forging `Sources` only changes its own pinned bytes and lands on its
own KP, never colliding with an honest one).

**[INV] Derived refs re-derive on demand.** Pin's doneness ref is
`build-pinned:F`; a crash between it and the derived refs must not wedge:
whenever `build-pinned:F` exists but `pin-cover/2:F` (or `kp-tree/<KP>`, or
`build-pinned:<KP>`) is missing, the consumer (`advanceBuildValueLocked`,
PullRefs computation) triggers re-derivation — a pure function of the pinned
blob and the store — instead of failing. The existing `f-tree/<F>` write has
the same latent crash window today (buildfrom done, f-tree missing ⇒
pluginresolve pulls hard-fail after retry budget, permanently); fix it with
the same on-demand pattern while in the area.

Serialization note: `commit` runs off the scheduler lock but *on* the single
ordered results-consumer goroutine. A very large monorepo pin commit (walk +
prune) stalls all result processing for its duration. Implementation must
measure this; if it bites, move derivation to a worker with the doneness
ordering preserved (derived refs before the result is acked), or parallelize
commits per node.

## 7. Generated sources

Plugins (and recipes) may synthesize files into the covered tree:
`build()` may return `generated = {"//Cargo.lock": bytes, ...}`; plugin
responses may carry the same map for the recipe to forward. Content lands in
`Pinned.Generated` (inline bytes, canonical CBOR — deterministic by
construction) and is overlaid onto the covered tree at §6.2.

Uses:

- **Lockfile pruning** (the cargo/pnpm requirement): the plugin parses the
  real workspace lockfile from `/jobs/source`, computes the member's
  reachable subgraph (resolution-aware, never textual: workspace-unified
  resolution means an unrelated member's bump can legitimately change your
  resolved versions — the slice catches exactly that), serializes it
  deterministically (sorted), and returns it as the generated lockfile.
  Unrelated dep churn now leaves the generated file byte-identical → KP
  unchanged → memo hit.
- **Reduced workspace manifests**: cargo loads *every* member manifest at
  workspace parse (and wants entry files); pnpm/npm validate lockfile
  importers against present packages. The plugin synthesizes a reduced root
  manifest (members = the covered set) so the pruned tree is a coherent
  little world.
- **VCS-derived versions**: setuptools-scm-style version stamping — resolve
  to a literal at pin time instead of shipping `.git`.

Plugin output determinism is identity-critical (same manifest+lockfile input
⇒ byte-identical generated file) — same discipline as goplugin's sorted
go.sum parse. Size: inline bytes in Pinned are capped at 1 MiB total,
pin-time error above — this cap is the contract of this arc (store
indirection for oversized generated content is a recorded non-goal, §13).

## 8. Sibling builds

`subbuild(dir)` accepts `//`-rooted paths. A `subbuild("//lib/x")` in a
widened build constructs a build input over the **same context tree** with
`dir = "lib/x"` and `ctx: 2`: K_sib = hash{context tree, sibling dir,
params, platform, build-file, ctx}. Consequences, all falling out of
existing machinery:

- Two consumers in the same monorepo commit produce the same K_sib → the
  same buildvalue node → the sibling builds once per commit, not per
  consumer (in-flight join included).
- Across commits, K_sib changes but the sibling's own KP joins — the sibling
  rebuilds only when *its* covered closure changes. Per-sibling cutoff, the
  artifact-sharing answer for built-dist ecosystems (JS/TS) and expensive
  compiled siblings.
- Strict-descendant `subbuild` keeps its current validation and stays
  cycle-free by construction; only `//`-paths engage the new machinery.

**[INV] Cycle detection.** Root-relative subbuilds can express cycles
(A subbuilds B, B subbuilds A), which manifest as a waits-for cycle among
buildvalue nodes (A's buildrun waits on bv_KB; B's buildrun waits on bv_KA)
— a silent mutual-wait today. At `requireInputLocked` time, walk the
dependent ancestry of the required node: if the requiring chain already
contains the node being required, fail hard with the full K-chain (rendered
as context-relative dirs) in the error. The graph is in-memory with parent
edges; the check is a bounded DFS at node-creation time only.

Diagnose/FailureRecord: records gain an optional `for_f` **list** (a KP node
can serve many Fs; interest snapshots already cover the by-request path).

## 9. Sandbox contract

- The buildrun executor extracts the KP tree's `src/` whole (writable), with
  repo-relative layout preserved — layout is identity-critical (proto
  imports, Go import paths, relative path-deps encode it).
- `sandbox.Config.Dir = /build/src/<dir>` (mechanism already exists and is
  applied post-pivot; `run` and plugin callers use it today).
- Env: `$SRC = /build/src/<dir>` (existing recipes keep their meaning),
  `$SRC_ROOT = /build/src`, everything else unchanged (`$out`, `JOBS_DEPS`,
  file-carried script/deps).
- `develop` shares `assembleSandbox` and inherits CWD + layout; note it now
  materializes the covered tree (not the whole monorepo — develop runs after
  pin, so the KP tree exists locally).
- `run` (entrypoint execution) is untouched — it consumes only the artifact.
- Eval stages: `loadBuildFromEnv` currently tars the whole F-tree to disk per
  stage; with whole-context F-trees this is O(monorepo) twice per build.
  Switch to lazy subpath materialization (`st.Tar(key, subpath)`,
  `TreeSubdir`) for the recipe-eval side; the plugin sandbox binds the whole
  context at `/jobs/source` read-only (that visibility is the feature), with
  a new omitempty `dir` field in `pluginRequest` so plugins know where the
  consumer package lives (resolution-deps §3.3 precedent for request
  evolution).
- `source.read`/`exists` stay rooted at the build dir; escape hardening
  stays. Recipe-side sibling reads are not added — discovery belongs to
  plugins (whole-tree mount) and build-time access to `$SRC_ROOT`.

## 10. Scheduler integration

### 10.1 KP re-key

`advanceBuildValueLocked` gains `bv.kp/kpResolved`, resolved from
`pin-cover/2:F` after pin reports done (re-derive on demand per §6.3) — the
exact `bv.f/fResolved` pattern used for the K→F handoff. The buildrun node
is required with key = KP. `require`'s doneness fast-path on
`build-output:<nodekey>` then *is* the memo: a second F whose monorepo
changed outside the covered subset resolves the same KP and joins the same
node in flight, or finds it done. `buildrun_<KP>` is wire-grammatical
(`ParseNodeName` accepts any canonical hex key; every consumer echoes node
names, never reconstructs them).

PullRefs for buildrun: `kp-tree/<KP>` + `build-pinned:<KP>` + shell + input
outputs + caches — buildrun runners pull only the covered subset, never the
monorepo. pluginresolve/pin keep pulling `f-tree/<F>` (whole context — the
feature). The gate's declared-cache check and unfold read
`build-pinned:<nodekey>` unchanged thanks to the `build-pinned:<KP>` alias.

### 10.2 F-aliases **[INV]**

The K→F→output two-hop is load-bearing in six verified consumers (buildvalue
doneness, pullrefs input resolution,
`ResolveBuildOutput`/`ResolveBuildOutputDeps`/`ResolveBuildArtifact`,
client pullHome, registry, run/image). The server writes `build-output:F`
and `build-output-deps:F` as aliases of the KP refs, under these rules:

1. **All waiter Fs.** Many buildvalues with different Fs can wait on one
   `buildrun_<KP>`; the commit spec snapshots the dependent bvs' resolved Fs
   under the lock at result time. Fs that join later get their aliases from
   the advance-walk helper (rule 4).
2. **Deps strictly before output — for the KP pair at buildrun commit AND
   for every F-alias pair.** The reverse order with a crash between yields a
   done-looking build without its runtime closure — silently wrong artifacts
   downstream (pullHome and the registry tolerate a missing deps ref as
   "closure-free build"), and nothing self-heals because doneness already
   holds. The KP pair matters first: `build-output:<KP>` alone is the memo,
   so a closure-free KP state would be replicated into every future F-alias.
3. **Aliases durable before the buildvalue transitions to done.** pullHome
   hard-fails on a missing `build-output:F`; if `nodeDoneLocked(bv)` (and
   its terminal watch snapshot) precedes the alias writes, a successful
   remote-build reports failure. Write the aliases synchronously under
   `s.mu` (precedent: the scheduler already performs KV puts and store reads
   under the lock) — never fast-path a memo hit to done with pending alias
   writes.
4. **One shared idempotent helper**, invoked at buildrun commit, on memo
   hit, and on every advance-to-done where an F-alias is missing — the same
   helper is the crash-heal for a server that died between the KP refs and
   the aliases. The local pipeline mirrors it in `driveFStages` (compute KP
   after pin, check `build-output:KP`, on hit write both aliases
   deps-first via `LocalRefWriter`, report the build stage as cached);
   `pullHome` additionally pulls the KP refs so later local builds memo-join.

### 10.3 Failure semantics

Unchanged: sticky FAILED is memory-only; failed-upstream is derived
per-request from dep phases and works for KP nodes as-is. Retry classes,
budgets, and FAILURES records key off the (opaque) node name. Known
cosmetic wrinkle: the buildrun row's key prefix no longer visually matches
pin's F prefix in watch/TUI; the `for_f` list on FailureRecord (§8) is the
human bridge.

## 11. Client

### 11.1 Git-root context

For `build`/`run`/`develop`/`remote-build`/`image --source`, when the source
dir sits inside a git repository, the client defaults the ingest root to
`git rev-parse --show-toplevel` and sets `dir` to the source dir's path
relative to it. `--source-root <dir>` overrides; `--no-repo-root` disables.
**The rule is identical for local and remote paths** — divergence would
silently kill the local↔remote F join.

**[INV] `.git` is excluded from ingest unconditionally** — implemented as a
source-ingest rule in the amber seam (`BuildSourceDir` skips `.git`, dir or
gitfile, at every level: VCS metadata is never build source). Without this,
`.git` — including packfiles and `config`, which can carry credentials — is
pushed to the server and bind-mounted into every plugin sandbox, and F churns
on every git command (`git status` rewrites `.git/index`, and amber hashes
mtimes). `.amberignore` at the repo root remains the scoping tool. (A
`--gitignore` convenience flag was considered and deferred: folding
`.gitignore` chains into ingest needs upstream amberignore support; half
implementations would silently diverge from git semantics.)

Behavior note for release notes: existing `--source <subdir>` invocations
inside a git repo change identity (new K via new root + dir) and ingest the
whole repo client-side. This is the accepted always-on re-key; the flags
above are the escape hatches.

### 11.2 Local pipeline

`localBuildFrom` widens identically (ingest whole root, keep `dir` in the
F-tree, `ctx: 2`). `driveFStages` gains the KP memo step per §10.2(4).
`develop` stops after pin as today and sandboxes the covered tree.

## 12. Plugins

### 12.1 Go (extend goplugin)

Recipe passes `go_mod = source.read("go.mod")` alongside `go_sum`. The
plugin parses `replace` directives — main-module-only semantics work in our
favor: the consumer's `go.mod` enumerates its entire local closure — and
walks `/jobs/source` for the sibling modules' `go.mod`s (their `require`
sets feed module resolution; path-replaced modules have no go.sum entries
and must be pinned by our covered tree instead). Returns
`{imports: [...], sources: ["//lib/common", ...]}`. `go.work`: cover the
file plus each `use`d module's `go.mod` (single-file covers). A new external
require in a sibling flows through the existing go.sum parse unchanged.

### 12.2 Rust (new cargo plugin)

Parses the workspace root `Cargo.toml`, member manifests, and `Cargo.lock`
from `/jobs/source`. Returns: covered paths (the member's path-dep closure —
manifests + `src/` + `build.rs` per dep, transitively; ancestor
`.cargo/config.toml` and `rust-toolchain.toml` files — upward-walked config
changes build output and must be covered or explicitly neutralized),
generated sources (reduced `[workspace]` manifest listing only covered
members; pruned `Cargo.lock` = the member's reachable subgraph), and the
usual fetch imports for registry deps (vendored via the store, `--offline`).
Note honestly: ambient-config coverage is a per-ecosystem checklist, not a
property the engine can guarantee — an uncovered optional config file
changes behavior silently in any content-addressed system.

## 13. Costs accepted / non-goals

- **Eval-stage churn**: pluginresolve + pin re-run on every context change
  for every subdir build (pin runs plugin sandboxes). Mitigation deferred:
  re-key eval stages by their true input subsets (same memo pattern one
  stage earlier).
- **Transfer**: eval-stage runners pull the whole monorepo (chunk-dedup
  softens; warm runners cheap); buildrun pulls only the KP tree. `watch` +
  pull from a third machine pulls the `build-from:K` closure = whole context.
- **Lost**: the standalone-repo ↔ monorepo F-join bonus for subdir builds
  (covered trees embed root-relative layout).
- **Not addressed**: repo-level affected-target discovery ("which builds does
  this commit touch") — CI submits everything and lets memos hit, paying
  eval churn per build per commit.
- **JS/pnpm**: engine-ready after this arc (generated sources + sibling
  builds are exactly what pnpm needs), but no JS plugin is in scope.
- **Oversized generated sources**: the 1 MiB inline cap in `Pinned.Generated`
  is the contract; store indirection for larger synthesized content is out
  of this arc.

## 14. Test plan (sketch)

- Golden KP test: same context + closure through the server path and the
  local path ⇒ identical KP (cross-implementation, pinned like chunker
  params).
- Crash-window tests: kill between every adjacent pair of ref writes in pin
  commit and buildrun commit; assert re-derivation/alias-heal, never wedge;
  assert deps-before-output ordering observable at every intermediate state
  for BOTH the KP pair and every F-alias pair.
- Platform test: identical pure-script recipe on two platforms ⇒ distinct
  KPs, distinct nodes.
- mtime test: touch/re-checkout inside the covered set with identical bytes
  ⇒ same KP; byte change ⇒ new KP. Outside the covered set ⇒ same KP, memo
  hit, aliases written for the new F.
- Symlink suite: chased chains with intermediate links, cycles, dangling
  in-root (warn, keep), escaping (fail, hatch), dir-symlink to ancestor.
- Cycle test: `//a` ↔ `//b` mutual subbuilds ⇒ hard fail naming the chain.
- e2e: Go two-module repo with replace (new require in sibling propagates);
  cargo workspace member with path dep + pruned lock (unrelated member bump
  ⇒ memo hit; shared-dep unification change ⇒ correct rebuild).
