# Source closure: precise build-input enumeration

Date: 2026-07-27
Status: accepted design (pre-implementation)
Scope: one arc — a `closure = [...]` declaration on the `build()` return that
names the **complete** cover of the source context (files and directories),
a pure-Go closure computation in goplugin (gosha-style transitive import
walk), and the root-build gate lift so single-module repos benefit too.

Prior art: github.com/draganm/gosha computes a Go module's SHA by loading the
transitive package closure (`packages.Load` with
`NeedDeps|NeedImports|NeedFiles|NeedEmbedFiles`) and hashing every input file
per package — GoFiles + EmbedFiles + OtherFiles (.c/.h/.s/…) + IgnoredFiles
(build-tag-excluded, the cross-compile safety net). This design ports the
*mechanism* (transitive closure of real build inputs) onto the shipped
sibling-sources machinery (2026-07-26-sibling-sources.md), which already
carries declared covers through `Pinned.Sources` → `PruneTree` → KP.

## 1. Problem and reframing

Sibling sources restored early cutoff at *sibling-directory* granularity, but
two blind spots remain:

- **The consumer dir is always wholesale-covered.** `cover.Walk` seeds `dir`
  unconditionally (`cover/cover.go:71-78`), so editing a README, an unused
  `cmd/`, or an unimported package inside the consumer's own directory
  invalidates KP and rebuilds.
- **Root builds (`dir == ""`) have no cover at all.** `sources=` is
  hard-rejected there (`recipe/recipe.go:363-366`), the pin walk never runs
  (`runner/buildeval.go:182` gates on a non-zero ContextKey), and
  `cover.Derive` falls through to `NormalizeTree` over the whole tree
  (`cover/cover.go:268`) — every file in the repo is identity.

The fix is not new identity machinery — `Pinned` covers, `PruneTree`,
KP-keyed buildrun, and re-derivable refs all shipped in the sibling-sources
arc. The fix is a declaration that means "this list is the *whole* cover"
(no implicit dir seed), plus a computation that produces that list correctly
for Go: the transitive closure of locally-resolvable imports, at package-dir
granularity, so `//go:embed` targets, cgo `.c`/`.h`, assembly `.s`/`.syso`,
and `testdata/` ride along for free (embed patterns cannot escape the
package directory — Go rejects `..` and absolute patterns — and covered
directories are recursive subtrees per §6.1 of sibling-sources).

## 2. Decisions of record

| Decision | Choice |
|---|---|
| Goal | **Rebuild precision only**: closure narrows KP identity and the buildrun `$SRC`. Ingest/upload (context tree, F) unchanged. |
| Surface | New optional `closure = [...]` key on the `build()` return — a **complete cover**, mutually exclusive with `sources=`. Dedicated `Pinned.Closure` field. |
| Dir seed | **Dropped** when `closure` is declared: what you list is what you get. Workdir coverage is validated at pin time (§5.3). |
| Root builds | **Gate lifted for `closure`**: allowed with `dir == ""`; the walk runs against the build-root tree. `sources`/`generated` keep their widened-only gate, except `generated` is also allowed alongside `closure`. |
| Granularity | **Package-dir level**: the computed closure lists reached package directories (recursive covers) + per-module manifests, not individual files. File-level exactness (gosha-exact) is a possible later tightening (§10). |
| Computation | **goplugin, pure Go, in the hermetic plugin sandbox** — no Go toolchain, no network. `go/parser` in `ImportsOnly` mode over *every* `.go` file (including build-tag-ignored and `_test.go`), local module map from `go.mod` `module`/`replace` + `go.work` `use` directives. |
| Identity | **No `amber.KPVersion` bump** — derivation on existing `Pinned` values is byte-identical (§7.1). |
| Fencing | **Runner ALPN bump `jobs-runner-nats/2.0 → 3.0`** — old recipe decoders silently ignore unknown `build()` attrs, which would fork `pin-cover/<v>:F` content (§7.2). |

## 3. Declaration surface

`build()` may return:

```python
closure = ["//lib/common", "//cmd/foo", "//go.mod", "//go.sum"]
```

- Entries are files or directories; a directory covers its whole subtree
  (shipped prune semantics, sibling-sources §5.3/§6.1).
- Path syntax is identical to `sources=`: `//x` root-relative, plain/`../x`
  resolved against `dir`, absolute rejected, resolving-to-root rejected —
  reuse `NormalizeSourcePath` (`recipe/sources.go:22-45`).
- `closure` + `sources` together is an eval-time error: `closure` subsumes
  `sources` (a complete cover has nothing to be additive to).
- `sources_allow_escaping` applies to closure walks identically (escaping
  symlink policy is unchanged).
- `generated = {...}` overlays on top of the covered tree exactly as today,
  and is allowed wherever `closure` is (including root builds).
- Struct form only (like `caches`/`sources`); the legacy 4-tuple cannot
  carry it.

Hand-written closures are first-class: a recipe may list paths itself with
no plugin involved. The typical Go recipe delegates:

```python
resp = plugin("go", go_sum=source.read("go.sum"),
              go_mod=source.read("go.mod"),
              go_closure=["."])
def build():
    return struct(..., inputs=resp_inputs, closure=resp["closure"])
```

## 4. Identity carrier

`builddef.Pinned` (`builddef/refs.go:78-88`) gains one field:

```go
Closure []string `cbor:"closure,omitempty"` // complete cover; excludes Sources
```

- Canonicalized like `Sources`: sorted, deduped (`CanonicalSources`,
  `builddef/canon.go:36-50`), root-relative plain paths (no `//` prefix in
  storage — surface syntax only, sibling-sources §5.2).
- **[INV]** `Closure` and `Sources` are mutually exclusive in a valid
  `Pinned`; producers enforce it at eval, consumers (`cover.Derive`) branch
  on `Closure` first.
- `Definition` is untouched — no `Canonical()` change, no submit-check
  change, no K re-key, no ctx bump. Root builds keep `Ctx = 0`.

## 5. Pin-stage walk

### 5.1 Seeds

`cover.Walk` gains a closure mode: when the eval declared a closure, seeds
are the declared closure paths **only** — `dir` is not auto-seeded. All other
walk behavior is identical (sibling-sources §5.3–§5.4): missing declared
path ⇒ hard error; component-wise in-store symlink chase with the 40-link
budget; dangling chased targets keep-and-warn; escapes fail unless listed in
`sources_allow_escaping`. The expanded result is baked into `Pinned.Closure`
(never `Sources`).

### 5.2 Root builds

For `dir == ""` defs the widened context does not exist (`ContextKey` is
zero); the walk runs against the build-root tree (`SourceContentKey`) with
`dir=""`. Server-side (`sched/kp.go` `deriveKP`) and local-pipeline
(`runner/localbuild.go` `developDriver.deriveKP`) derivations use the F-tree
`env/` as the context root exactly as they do for widened builds — for root
builds `env/` *is* the build root, so the same code path serves both.
The eval gate at `recipe/recipe.go:363-366` changes from "sources/generated
require a widened context" to: `closure` allowed always; `generated` and
`sources_allow_escaping` allowed with widened context **or** a declared
closure; `sources` alone keeps the widened-only rule.

### 5.3 Workdir validation **[INV]**

After expansion, at least one covered path must sit at, under, or **above**
`Pinned.Dir` (a covered ancestor materializes the workdir too — covers are
recursive), else the pruned tree would not contain the sandbox workdir and
buildrun would fail at `cd`. This is a **pin-time hard error**:

```
closure does not cover the build dir "cmd/foo" — the sandbox workdir would not exist
```

(For root builds `dir == ""` and the pruned root always exists — the check
is trivially satisfied.)

## 6. Derivation

`cover.Derive` (`cover/cover.go:259-280`) grows one branch, strictly first:

```go
switch {
case len(p.Closure) > 0: covered, err = st.PruneTree(ctx, contextRoot, p.Closure)
case len(p.Sources) > 0: covered, err = st.PruneTree(ctx, contextRoot, p.Sources)  // unchanged
default:                 covered, err = st.NormalizeTree(ctx, contextRoot)         // unchanged
}
```

Generated overlay and `BuildKPTree` are untouched. All three call sites
(server pin-commit `sched/kp.go:59`, local pipeline
`runner/localbuild.go:170`, runner self-test) inherit the branch through the
shared function — the §6.2 sibling-sources invariant (one derivation
implementation) is preserved, and the cross-path golden test extends to a
closure-carrying `Pinned`.

## 7. Identity, compatibility, fencing

### 7.1 No KPVersion bump

`cover.Derive` on any *existing* `Pinned` value (no `Closure` field) is
byte-identical to today, so every cached `pin-cover/2:F`, `kp-tree/<KP>`,
`build-pinned:<KP>`, and `build-output:<KP>` remains valid. `KPVersion`
measures the derivation function's semantics *on the same inputs*; a branch
that only fires on a new input field does not qualify. Bumping would force
fleet-wide re-pin churn with zero correctness gain. (Future changes to how a
*closure* itself is pruned or walked DO bump `v` per the standing rule.)

### 7.2 Runner ALPN bump `jobs-runner-nats/2.0 → 3.0` **[INV]**

The `build()` decoder treats unknown struct attrs as absent
(`recipe/recipe.go:415-425` pattern), so an **old runner** evaluating a
closure-declaring recipe would silently pin with whole-tree/dir-seed
semantics and write `pin-cover/2:F` with *different content* than a new
runner — same ref name, nondeterministic KP depending on who pins first.
That is "wrong results rather than clean errors": bump the ALPN. The
mismatch fences both directions:

- old runner ↔ new server: cannot connect — no silent mis-pins;
- new runner ↔ old server: cannot connect — an old server's `cover.Derive`
  would skip the unknown `closure` CBOR field and derive a different KP than
  the pinning runner.

### 7.3 Fully-old fleet

An old server + old runner receiving a closure recipe silently ignores the
attr and builds with whole-tree cover: outputs are correct (a superset
`$SRC`), hermeticity is weaker, the memo key differs — accepted degradation,
documented. The *plugin* path degrades louder: an old goplugin has no
`go_closure` kwarg, its response carries no `closure` key, and the recipe's
`resp["closure"]` access fails eval visibly.

## 8. goplugin closure computation

New kwarg `go_closure = [<entry-dir>, ...]` (dir-relative package dirs of
the build's entry points, e.g. `["."]` or `["./cmd/foo"]`). Works with or
without siblings: a consumer-only module simply has an empty sibling map.
Algorithm — pure Go, filesystem-only, deterministic:

1. **Local module map.** The consumer's manifest arrives as kwarg bytes
   exactly as today (`go_mod`, optionally `go_work`): `module` path +
   `replace`/`use` relative targets (existing `relDirectives`,
   `plugins/goplugin/gomod.go:17`). Each *sibling's* `go.mod` is read from
   the mounted source tree (the plugin sandbox mounts the whole context
   read-only) to learn its module path → map module path → root-relative
   dir.
2. **Import extraction.** For each package dir on the frontier, glob `*.go`
   and parse each file with `go/parser` `ImportsOnly` — **every** file,
   including build-tag-ignored and `_test.go` files. Deliberately not
   `go/build.ImportDir`: it applies host GOOS/GOARCH/tags, and the closure
   must be platform-independent (gosha includes `IgnoredFiles` for the same
   reason).
3. **Resolution.** Longest-prefix match of each import path against the
   local module map decides alone — so dot-less local module paths (legal
   for replace-only modules) resolve locally. Every unmatched import is
   skipped: stdlib (incl. the cgo pseudo-import `"C"`) and external modules
   (the go.sum fetcher's territory) alike. There is deliberately no
   "unresolvable" error class: the walker cannot distinguish external-shaped
   from misspelled imports without resolving `go.sum` semantics, and a
   genuinely missing package fails the sandbox build loudly.
4. **Output.** Sorted, `//`-rooted, nested paths collapsed (a covered
   ancestor subsumes descendants): reached package dirs + each involved
   module's `go.mod` and `go.sum` (when present) + `go.work`/`go.work.sum`
   at the consumer dir (when a go.work was passed — dir-relative like its
   `use` targets). A reached package at a MODULE ROOT cannot be covered as
   a dir (that would swallow the whole module; the context root cannot be
   covered at all): its regular files are enumerated non-recursively, its
   `//go:embed` patterns are glob-resolved, and its `testdata/` is covered
   when present. The residual limitation: other non-embed subdirectory
   inputs of a module-root package (e.g. cgo `#include` subdirs) must be
   hand-added to the closure. The pinned job itself rides in the KP tree as
   `job.cbor`, never as a source file.
5. **Response.** The monorepo-mode map (`plugins/goplugin/main.go:84-88`)
   gains a `closure` key next to `modules`/`sources`; the recipe forwards it
   verbatim into `build() closure=`. Without the kwarg every existing
   response shape is byte-identical.

Determinism **[INV]**: same mounted tree ⇒ same list (sorted walks, sorted
output, no host-environment inputs) — the plugin-determinism requirement of
sibling-sources §7 applies unchanged.

## 9. Error surface

All failures land at eval/pin, never at buildrun:

| Condition | Failure |
|---|---|
| `closure` + `sources` both declared | eval error naming both keys |
| declared closure path missing from context | pin hard error (existing walk path) |
| expanded closure covers nothing at/under/above `dir` | pin error (§5.3 message) |
| goplugin: empty `go_closure` entry list | plugin hard error |
| goplugin: entry dir missing / not a package (no `.go` files) | plugin hard error |
| goplugin: `go.mod` without a module directive; sibling replace/use target without a readable `go.mod` | plugin hard error naming it |
| goplugin: embed pattern matching nothing | plugin hard error (under-covering is the one forbidden failure mode) |
| goplugin: unmatched import | skipped, never an error (§8.3) |

Vendored module trees (`vendor/`): out of scope — a vendored import resolves
like any local path only if the author lists `vendor/` by hand; the plugin
neither reads nor special-cases it. Documented limitation.

## 10. Non-goals

- **Ingest/upload narrowing.** The whole context is still ingested and
  pushed; F still hashes the whole `env/`. Closure affects KP and `$SRC`
  only.
- **File-level exactness.** Package dirs are recursive covers; a README
  *inside* a covered package dir still rebuilds. gosha-exact file
  enumeration (GoFiles/EmbedFiles/OtherFiles classification + embed-glob
  resolution) is a compatible later tightening: same `Pinned.Closure`
  carrier, only the plugin's output gets finer.
- **Rust/cargo closure.** The cargo plugin keeps its sibling-dir behavior;
  a cargo `closure` mode is a separate arc.
- **`.gitignore` semantics.** `.amberignore` remains the ingest-scoping
  tool (sibling-sources §11.1).

## 11. Testing

- `cover/`: closure-seed walk (no dir seed); symlink chase + escape under
  closure mode; §5.3 workdir validation (positive, negative, root-build
  trivial case); golden cross-path test — server `deriveKP` and local
  pipeline produce identical KP for a closure build.
- `recipe/`: `closure=` decode (struct form); mutual-exclusion error;
  root-build acceptance (gate lift) + `generated`-with-closure acceptance;
  path normalization reuse (`//x`, `../x`, absolute rejected).
- `plugins/goplugin`: fixture monorepo — consumer + two siblings, an
  unreached sibling package, a build-tag-ignored file importing an
  otherwise-unreached package, a `_test.go`-only import, an embed
  directory, `.c`/`.s` files in a covered package — exact expected closure
  list; stdlib/external skip; nested-collapse; response byte-compat without
  the kwarg.
- `runner/` e2e (pattern: `runner/monorepo_linux_test.go`):
  (a) widened build with closure — edit an uncovered file in the consumer
  dir ⇒ KP memo hit; edit a covered sibling package ⇒ rebuild;
  (b) root build with closure over `cmd/foo` — edit `cmd/bar` ⇒ memo hit;
  (c) `$SRC` contains exactly the closure (embed/c/asm/testdata present,
  uncovered dirs absent); (d) workdir-coverage pin failure surfaces as a
  clean diagnostic.
- `sched/`: KP resolve/re-derive crash windows for a closure-carrying
  `Pinned` (absence after done re-derives, never fails).

## 12. Doc & invariant updates

- `docs/architecture/architecture.md`: stage-pipeline and ref-namespace
  tables (already stale re: KP) get the closure branch noted when touched.
- `CLAUDE.md` sibling-sources invariant block: add `closure` (complete
  cover, mutual exclusion, workdir validation), note the root-build gate
  lift, and record the ALPN bump to `/3.0`.
