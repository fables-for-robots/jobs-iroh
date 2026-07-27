# cwd-centric build source — `--source` becomes optional

**Date:** 2026-07-27
**Status:** design, approved for planning

## Goal

Make `jobs-client` build commands work from where you are standing. Typing
`jobs-client build` with no paths at all inside a checked-out repository
should build the thing you are in, with the whole repository as the ingest
context — the same `(root, dir)` pair you would get by naming them by hand.

## Motivation

Every source-building command requires `--source` today (`local.go:39`,
`remote.go:89`), and the value is almost always `.` or a path the user just
`cd`'d out of. The context machinery already does the interesting half of the
job: `resolveContextRoot` (`contextroot.go:24`) re-anchors `--source` to the
git toplevel and rewrites `dir` relative to it, which is what makes sibling
references (`../lib`, `//lib/common`) resolvable. What is missing is the
trivial half — inferring `--source` itself.

The result is a CLI that knows how to find the repository but not the build.

## Scope

**In scope.** An optional `--source` on `build`, `run`, `develop`,
`remote-build` and `image`, defaulting to a bounded upward search from the
cwd; a pure-Go repo-root finder replacing the `git rev-parse` subprocess; a
one-line report of what got resolved; a new mode discriminator for `image`.

**Out of scope.** `--server` stays `Required` on `remote-build` (the
`JOBS_SERVER` env var already covers the ergonomic case). No positional form
(`jobs-client build ./svc`) — `run` spends positionals on entrypoint args and
`image` on the build key, so a positional source would be inconsistent on
exactly the commands that matter most. No config-file discovery.

## Non-goals — identity

**Identity does not move.** The same `(root, dir)` pair still produces the
same `F`; this changes only which pair the CLI picks when the user names
none. No `amber.KPVersion` bump, no `jobs-runner-nats` ALPN bump, no
canonical-CBOR change. Every cached pin, KP tree and build output stays
valid, and the local↔remote `F` join is preserved because both paths call
the same two functions in the same order.

## Design

### 1. `repoRoot` — one pure repo-root finder

`resolveContextRoot` is the only place in the tree that shells out to git
(`contextroot.go:38`). The new default needs the repo root too, as the
ceiling for its upward search. Two independent detections could disagree —
git worktrees, submodules, `GIT_DIR`, `GIT_CEILING_DIRECTORIES`, and symlinked
paths (git returns the physical path, `filepath.Abs` does not resolve
symlinks) all produce divergence — so there is exactly one finder:

```go
// repoRoot walks up from dir looking for a .git entry (directory OR file —
// worktrees and submodules use a .git file). Returns "" if none found.
func repoRoot(dir string) string
```

`resolveContextRoot` drops `exec.Command` and calls it. The two steps then
agree by construction.

Behavior deltas from dropping `git rev-parse --show-toplevel`:

- `GIT_CEILING_DIRECTORIES` and `GIT_DIR` stop being honored. Neither has a
  legitimate use here — the detection picks which tree gets ingested, and an
  env var silently changing the ingest root is a misfeature.
- Bare repos: `rev-parse --show-toplevel` fails there, and the `.git` walk
  finds nothing inside one either. Same outcome, still the `root = source`
  fallback.
- Submodules and worktrees: a `.git` **file** sits at the root, so the walk
  must accept a file as well as a directory. Same answer git gives.
- No `git` binary required — matters for the Nix devShell and sandboxed CI.

The existing comment's promise gets stronger, not weaker: *identity never
depends on git itself; the detection only chooses which tree gets ingested.*

### 2. `defaultSource` — the bounded upward search

A pure function that fills in an omitted `--source`, running **before**
`resolveContextRoot`:

```go
// defaultSource fills in an omitted --source from the cwd.
func defaultSource(cwd, dir, sourceRoot, buildFile string, noRepoRoot bool) (string, error)
```

It takes `cwd` as a parameter rather than calling `os.Getwd()`, so it is
directly drivable from tests. Callers pass `os.Getwd()`.

When `--source` is empty:

1. **`dir != ""` → return `cwd`, no search.** The user already named the
   build root; searching for a recipe would contradict them. Composition
   proceeds exactly as today.
2. **Compute the ceiling:** `--source-root` if given; else `cwd` if
   `--no-repo-root`; else `repoRoot(cwd)`; else (no repo) `cwd`.
3. **Probe from `cwd` upward to the ceiling, inclusive.** First hit wins:
   `<candidate>/<recipe>` exists, where `recipe` is `--build-file` when set
   and `BUILD.jobs` otherwise. `--build-file` is documented as a *path*
   relative to `dir`, so a value with slashes (`recipes/app.jobs`) joins
   correctly.
4. **No hit → error** naming the recipe, the cwd, the ceiling, and
   `--source`.

Then `resolveContextRoot(found, dir, sourceRoot, noRepoRoot)` runs unchanged
and yields `(root, dir)`. `repoRoot` from the found candidate returns the same
toplevel it returned from the cwd (the candidate is at or above the cwd and at
or below the root), so the two steps cannot disagree.

Worked examples:

```
cd ~/repo/services/api/internal/db && jobs-client build
  no BUILD.jobs here, walk up…            ceiling = ~/repo
  found ~/repo/services/api/BUILD.jobs
  ingest root: ~/repo        dir: services/api

cd ~/scratch && jobs-client build          # not a repo
  ceiling = ~/scratch (cwd only)
  error: no BUILD.jobs in /home/x/scratch (pass --source <dir>)

cd ~/repo/services && jobs-client build --dir api
  --dir given → no search
  ingest root: ~/repo        dir: services/api
```

### 3. Command surface

**`local.go`** — `build`, `run` and `develop` share `localConfig.flags()`.
`--source` drops `Required: true`; its usage becomes *"source directory to
ingest as the build source (default: nearest ancestor of the cwd holding
BUILD.jobs, up to the repo root)"*. `resolveContext()` gains the
`defaultSource` pre-step.

**`remote.go`** — identical change to the flag and to the inline resolve at
`remote.go:130`. Local and remote keep calling the same two functions in the
same order; divergence would silently kill the `F` join.

**`image.go`** — `--source` is already optional, but the mode discriminator
is `cfg.source != ""` (`image.go:94`), which stops working once source is
always populated. The **positional build key** becomes the discriminator:

| invocation | mode |
|---|---|
| `image -o x.tar` | source mode, cwd-resolved |
| `image -o x.tar --source ./svc` | source mode, explicit |
| `image -o x.tar <K>` | by-key mode |
| `image -o x.tar --source ./svc <K>` | **error** (today: silently ignores `<K>`) |

The "provide `--source` or a build key" error (`image.go:128`) disappears.

### 4. The context line

A shared helper prints one line to **stderr**, through the command's
`liveView` so it interleaves cleanly with the in-place progress block,
immediately after resolution and before ingest:

```
context: /home/x/repo  (dir services/api, recipe BUILD.jobs)
```

Always both fields — `dir .` when the build root is the context root, and the
*effective* recipe. Printed whether or not `--source` was given: the ingest
root can be an entire repository, and that is worth making visible uniformly
rather than only in the inferred case. stdout stays machine-readable (keys,
tables).

Wired into all five commands. It replaces `remote-build`'s existing
`lv.Println("ingesting " + cfg.source)` (`remote.go:135`), which said a
strictly weaker version of the same thing. `develop` has no live view today;
it gets `cliLiveView(c)` for this one line, printed before the PTY takes over.

## Testing

Dropping the git subprocess makes the resolver testable with plain
`os.MkdirAll` — no `git init`, no skip-if-git-absent. New
`clientcli/contextroot_test.go`, table-driven over temp trees:

- recipe in cwd; recipe in an ancestor inside the repo; recipe **only above**
  the repo root → error (the ceiling is inclusive and hard)
- repo root itself holds the recipe, cwd several levels below
- non-repo tree: recipe in cwd → ok; recipe in an ancestor → error
- `--dir` given → cwd, no search, *even when cwd has no recipe*
- `--source-root` as the ceiling, both above and below the repo root
- `--no-repo-root` → cwd only
- custom `--build-file`, including a path with a slash
- `.git` as a **file** (worktree/submodule) recognized as a root
- the not-found error names the recipe, the cwd and the ceiling

Plus `resolveContextRoot` coverage it never had behind `exec`: root
detection, and the escaping-source error. `commands_test.go` gains assertions
that `--source` is not `Required` on the four commands; `image` gets a test
for `--source` + positional `K` → error.

Gate: `nix develop -c go test ./...` green.

## Docs

`CLAUDE.md` (command synopses + the `clientcli/` package-map row),
`README.md`, `CHANGELOG.md`. `docs/design/2026-07-26-sibling-sources.md` §11.1
stays untouched — `docs/design/*` is a historical record, and this is a CLI
layer on top of its rule, documented here instead.
