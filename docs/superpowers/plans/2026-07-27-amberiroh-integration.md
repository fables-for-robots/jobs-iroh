# amberiroh Integration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fold amber-store-iroh's four library packages into one in-tree
`amberiroh` package, drop the dependency, ship it as jobs-iroh v0.13.0, and
point assimilate at the new release.

**Architecture:** Copy `protocol`, `wantsync`, `server` and `relaymode` into
a single flat `amberiroh` package (namespaces are disjoint — 123 top-level
decls, 0 collisions). Delete the standalone-listener path jobs-iroh cannot
reach, relocate `SendPack` to test scope, then repoint 9 import lines across
7 files. Upstream repo is left untouched.

**Tech Stack:** Go 1.26.5, nix devShell, `golang.org/x/tools/cmd/deadcode`,
`gh` (in devShell), docker + buildx.

## Global Constraints

- Source of truth: `docs/superpowers/specs/2026-07-27-amberiroh-integration-design.md`.
- Run everything through the devShell: `nix develop -c …`, with
  `GOPRIVATE=github.com/jobs-build/*` exported.
- `gh` is **not** on the bare PATH — always `nix develop -c gh …`.
- Every `main()` and sandbox-driving `TestMain` calls `sandbox.Init()` first.
  `amberiroh` drives no sandbox, so it adds no `TestMain`.
- **Identity is untouched.** Do not modify chunker params, canonical-CBOR
  encoding, `cover/`, or `amber.KPVersion`. No `KPVersion` bump.
- **No ALPN fence bump.** `ALPN` and all frame constants move verbatim; the
  wire is unchanged, so `jobs-runner-nats/3.0` stays.
- Upstream `amber-store-iroh` is **read-only** for this work — never commit
  to it.
- `CHANGELOG.md` is stale (last entry v0.8.1, six releases behind). Do not
  add to it; release notes live in the GitHub release, per current practice.

---

### Task 1: Create the merged `amberiroh` package

**Files:**
- Create: `amberiroh/protocol.go`, `amberiroh/pack.go`,
  `amberiroh/wantsync.go`, `amberiroh/server.go`, `amberiroh/relaymode.go`
- Create: `amberiroh/protocol_test.go`, `amberiroh/pack_test.go`,
  `amberiroh/wantsync_test.go`, `amberiroh/loop_test.go`,
  `amberiroh/server_test.go`, `amberiroh/relaymode_test.go`
- Source (read-only): `../amber-store-iroh/{protocol,wantsync,server,relaymode}/`

**Interfaces:**
- Consumes: nothing (first task).
- Produces: package `amberiroh` exporting `ALPN`, `Msg`, `ReadMsg`,
  `WriteMsg`, `RemoteError`, `RemoteFromMsg`, `ErrProtocol`, `RefInfo`,
  `NewPackReader`, `SendPackRecords`, `ChunkSize`, `MaxFrame`, frame-type
  constants (`TPush`, `TPull`, `TWants`, `TData`, `TDataEnd`, `TOK`, `TErr`,
  `TRef`, `TRefs`, `TRefList`, `TAccept`, `TAttach`), error codes
  (`CodeBadRequest`, `CodeInternal`, `CodeUnknownRef`, `CodeCASMismatch`),
  `Send`, `Receive`, `Progress`, `Stats`, `Wants`, `New`, `Server`,
  `FromFlag`. Unexported test helper `sendPack`.

- [ ] **Step 1: Copy the five source files**

```bash
cd /home/dragan/fables-for-robots/jobs-iroh
mkdir -p amberiroh
U=../amber-store-iroh
cp $U/protocol/protocol.go   amberiroh/protocol.go
cp $U/protocol/pack.go       amberiroh/pack.go
cp $U/wantsync/wantsync.go   amberiroh/wantsync.go
cp $U/server/server.go       amberiroh/server.go
cp $U/relaymode/relaymode.go amberiroh/relaymode.go
```

- [ ] **Step 2: Copy the six test files**

```bash
cd /home/dragan/fables-for-robots/jobs-iroh
U=../amber-store-iroh
cp $U/protocol/protocol_test.go  amberiroh/protocol_test.go
cp $U/protocol/pack_test.go      amberiroh/pack_test.go
cp $U/wantsync/wantsync_test.go  amberiroh/wantsync_test.go
cp $U/wantsync/loop_test.go      amberiroh/loop_test.go
cp $U/server/server_test.go      amberiroh/server_test.go
cp $U/relaymode/relaymode_test.go amberiroh/relaymode_test.go
```

- [ ] **Step 3: Rewrite package declarations to `amberiroh`**

Every file (source and test) currently declares `package protocol`,
`package wantsync`, `package server`, or `package relaymode`.

```bash
cd /home/dragan/fables-for-robots/jobs-iroh/amberiroh
sed -i -E 's|^package (protocol|wantsync|server|relaymode)$|package amberiroh|' *.go
grep -h '^package ' *.go | sort -u   # expect exactly: package amberiroh
```

- [ ] **Step 4: Drop the now-intra-package qualifiers and self-imports**

Cross-package references become bare identifiers, and the imports that
provided them disappear.

```bash
cd /home/dragan/fables-for-robots/jobs-iroh/amberiroh
# remove self-imports
sed -i '/"github.com\/jobs-build\/amber-store-iroh\/\(protocol\|wantsync\|relaymode\|server\)"/d' *.go
# drop qualifiers
sed -i -E 's/\bprotocol\.([A-Z])/\1/g; s/\bwantsync\.([A-Z])/\1/g; s/\brelaymode\.([A-Z])/\1/g' *.go
```

Expected: no remaining `amber-store-iroh` import lines
(`grep -c amber-store-iroh *.go` → 0 for every file).

- [ ] **Step 5: Write the single package doc comment**

Replace the four inherited package comments with one. Put this at the top of
`protocol.go` and delete the doc comments above `package amberiroh` in
`pack.go`, `wantsync.go`, `server.go` and `relaymode.go`.

```go
// Package amberiroh moves amber store content between peers over iroh QUIC.
//
// It is the transport half of the store: amber-store-core holds the
// content-addressed objects, this package ships them. One bidirectional QUIC
// stream carries one operation as length-prefixed CBOR frames, with
// amberpack payloads chunked into TData frames.
//
// The have/want loop (Send, Receive) transfers objects below a root: the
// receiver announces which keys it is missing, the sender answers each round
// with an amberpack of exactly those objects. Server answers push, pull and
// ref-list operations against a store directory; jobs-server mounts it on the
// jobs-runner-amber/1.0 and jobs-amber-admin/1.0 ALPNs.
//
// Vendored from github.com/jobs-build/amber-store-iroh, which jobs-iroh was
// the only external consumer of. This copy is authoritative; upstream retains
// its own copy for the amber and amber-serve CLIs.
package amberiroh
```

- [ ] **Step 6: Delete the unreachable listener path**

`Server.Serve` (accept loop) and `Server.serveConn` (its per-connection
helper) served upstream's `amber-serve` binary. jobs-iroh mounts the handler
through its own iroh Router, so both are unreachable. Delete both functions
from `amberiroh/server.go`, including their doc comments.

Then remove any import left unused by their deletion (likely `sync` and/or
`time` — let the compiler tell you in Step 9).

- [ ] **Step 7: Relocate `SendPack` to test scope**

Cut `func SendPack(w io.Writer, objs iter.Seq2[fstree.Object, error]) error`
(and its doc comment) out of `amberiroh/pack.go` and paste it into
`amberiroh/pack_test.go`, renamed to `sendPack`.

**Keep `chunkWriter` in `pack.go`** — it is shared with `SendPackRecords`,
the zero-copy push path `amberclient` uses.

Then update the five call sites, all now in-package:

```bash
cd /home/dragan/fables-for-robots/jobs-iroh/amberiroh
sed -i -E 's/\bSendPack\(/sendPack(/g' pack_test.go server_test.go loop_test.go
# SendPackRecords must NOT be renamed - verify:
grep -n 'sendPackRecords' *.go   # expect: no output
```

- [ ] **Step 8: Format**

```bash
cd /home/dragan/fables-for-robots/jobs-iroh
nix develop -c gofmt -w amberiroh/
```

- [ ] **Step 9: Build and run the inherited suite**

Run:
```bash
cd /home/dragan/fables-for-robots/jobs-iroh
nix develop -c bash -c 'export GOPRIVATE="github.com/jobs-build/*"; \
  go build ./amberiroh/... && go vet ./amberiroh/... && go test ./amberiroh/...'
```
Expected: `ok github.com/jobs-build/jobs-iroh/amberiroh`. Every upstream
assertion passes unchanged — this is the oracle proving the merge preserved
behaviour. If anything fails, the merge is wrong; do not adjust assertions to
make them pass.

- [ ] **Step 10: Commit**

```bash
cd /home/dragan/fables-for-robots/jobs-iroh
git add amberiroh/
git commit -m "amberiroh: vendor amber-store-iroh as one in-tree package

Merge protocol, wantsync, server and relaymode into a single package
(123 top-level decls, 0 collisions). Drop Server.Serve/serveConn - the
standalone-listener path only upstream's amber-serve used. Relocate
SendPack to test scope: unreachable from the binaries but backing five
test call sites, so it becomes an unexported helper rather than being
deleted with its coverage.

All six upstream test files move with assertions unchanged."
```

---

### Task 2: Repoint jobs-iroh and drop the dependency

**Files:**
- Modify: `amberclient/client.go` (import line 14-15; 22 `protocol.` + 1 `wantsync.` refs)
- Modify: `amberclient/shard.go` (import line 12-13; 8 `protocol.` + 4 `wantsync.` refs)
- Modify: `runnerd/sync.go` (import line 7; 2 `protocol.` refs)
- Modify: `registryd/sync.go` (import line 9; 2 `protocol.` refs)
- Modify: `serve/serve.go` (import line 13, aliased `ambserver`; 2 `ambserver.` refs)
- Modify: `serve/announce.go` (import line 9; 1 `relaymode.` ref)
- Modify: `serve/serve_test.go` (import line 7; 4 `protocol.` refs)
- Modify: `go.mod`, `go.sum`

**Interfaces:**
- Consumes: package `amberiroh` from Task 1 — all symbols listed there.
- Produces: a `go.mod` with no `amber-store-iroh` requirement.

- [ ] **Step 1: Rewrite imports and qualifiers**

```bash
cd /home/dragan/fables-for-robots/jobs-iroh
FILES="amberclient/client.go amberclient/shard.go runnerd/sync.go \
registryd/sync.go serve/serve.go serve/announce.go serve/serve_test.go"

# collapse the four imports into one (dedup handled by gofmt + manual check)
sed -i -E 's|^(\s*)(ambserver )?"github.com/jobs-build/amber-store-iroh/(protocol|wantsync|server|relaymode)"$|\1"github.com/jobs-build/jobs-iroh/amberiroh"|' $FILES

# qualifiers -> amberiroh.
sed -i -E 's/\bprotocol\.([A-Z])/amberiroh.\1/g; s/\bwantsync\.([A-Z])/amberiroh.\1/g; s/\bambserver\.([A-Z])/amberiroh.\1/g; s/\brelaymode\.([A-Z])/amberiroh.\1/g' $FILES
```

- [ ] **Step 2: De-duplicate the import line in the two-import files**

`amberclient/client.go` and `amberclient/shard.go` each imported *two*
packages and now have the `amberiroh` line twice. Delete the duplicate in
each file by hand (keep one).

Verify:
```bash
cd /home/dragan/fables-for-robots/jobs-iroh
for f in amberclient/client.go amberclient/shard.go; do
  echo -n "$f: "; grep -c 'jobs-iroh/amberiroh' $f
done   # expect 1 each
```

- [ ] **Step 3: Drop the dependency and tidy**

```bash
cd /home/dragan/fables-for-robots/jobs-iroh
nix develop -c bash -c 'export GOPRIVATE="github.com/jobs-build/*"; \
  gofmt -w . && go mod edit -droprequire=github.com/jobs-build/amber-store-iroh && go mod tidy'
```

- [ ] **Step 4: Assert the dependency is gone**

Run:
```bash
cd /home/dragan/fables-for-robots/jobs-iroh
grep -c 'amber-store-iroh' go.mod go.sum
```
Expected: `go.mod:0` and `go.sum:0`. A non-zero count means something still
imports it and `go mod tidy` put it back — the goal has silently failed.

- [ ] **Step 5: Build, vet, full test suite**

Run:
```bash
cd /home/dragan/fables-for-robots/jobs-iroh
nix develop -c bash -c 'export GOPRIVATE="github.com/jobs-build/*"; \
  go build ./... && go vet ./... && gofmt -l . && go test ./...'
```
Expected: build and vet clean, `gofmt -l` prints nothing, all 31 packages
(30 existing + `amberiroh`) report `ok`.

- [ ] **Step 6: Confirm no dead code remains**

Run:
```bash
cd /home/dragan/fables-for-robots/jobs-iroh
nix develop -c bash -c 'export GOPRIVATE="github.com/jobs-build/*"; \
  go run golang.org/x/tools/cmd/deadcode@latest -filter amberiroh ./cmd/...'
```
Expected: no output (previously reported `SendPack`, `Server.Serve`,
`Server.serveConn`).

- [ ] **Step 7: Commit**

```bash
cd /home/dragan/fables-for-robots/jobs-iroh
git add -A
git commit -m "Drop the amber-store-iroh dependency

Repoint the 9 import lines across 7 files onto the in-tree amberiroh
package. go.mod loses github.com/jobs-build/amber-store-iroh and gains
nothing - amber-store-core, tmc/go-iroh and fxamacker/cbor were already
direct dependencies.

deadcode reports amberiroh clean."
```

---

### Task 3: Update the docs

**Files:**
- Modify: `CLAUDE.md` (package map table; `amberclient/` row)
- Modify: `README.md` (only if it names amber-store-iroh)

**Interfaces:**
- Consumes: the `amberiroh` package from Task 1.
- Produces: nothing code-facing.

- [ ] **Step 1: Add the package-map row**

In `CLAUDE.md`, insert after the `amberclient/` row:

```markdown
| `amberiroh/` | Store sync over iroh QUIC — vendored from amber-store-iroh (jobs-iroh was its only consumer). Wire protocol (length-prefixed CBOR frames, amberpack payloads chunked into `TData`), the have/want transfer loop, and the `Server` that `serve/` mounts on `jobs-runner-amber/1.0` + `jobs-amber-admin/1.0`. Upstream keeps its own copy for the `amber`/`amber-serve` CLIs; **this copy is authoritative and the two can drift silently.** |
```

- [ ] **Step 2: Check README for stale mentions**

Run:
```bash
cd /home/dragan/fables-for-robots/jobs-iroh
grep -n 'amber-store-iroh' README.md CLAUDE.md
```
Update any line that describes amber-store-iroh as a dependency. Leave
`docs/design/`, `docs/research/` and `docs/superpowers/` alone — historical
record.

- [ ] **Step 3: Commit**

```bash
cd /home/dragan/fables-for-robots/jobs-iroh
git add CLAUDE.md README.md
git commit -m "docs: amberiroh package map entry, drop amber-store-iroh as a dependency"
```

---

### Task 4: Release jobs-iroh v0.13.0

**Files:**
- Modify: `version/version.go` (line 6)

**Interfaces:**
- Consumes: Tasks 1-3 landed on `main`.
- Produces: tag `v0.13.0`, a GitHub release, and a resolvable
  `github.com/jobs-build/jobs-iroh@v0.13.0`.

- [ ] **Step 1: Bump the version constant**

`version/version.go` line 6: `const Version = "0.12.0"` → `const Version = "0.13.0"`.

This is the **only** change in the release commit.

- [ ] **Step 2: Verify the binaries report it**

Run:
```bash
cd /home/dragan/fables-for-robots/jobs-iroh
nix develop -c bash -c 'export GOPRIVATE="github.com/jobs-build/*"; \
  go build ./... && go run ./cmd/jobs-server --version'
```
Expected: version string contains `0.13.0`.

- [ ] **Step 3: Commit, tag, push**

```bash
cd /home/dragan/fables-for-robots/jobs-iroh
git add version/version.go
git commit -m "Release v0.13.0: jobs-build org, amberiroh in-tree"
git tag v0.13.0
git push origin main
git push origin v0.13.0
```

- [ ] **Step 4: Create the GitHub release**

Body uses bold lead sentences in prose, matching prior releases.

```bash
cd /home/dragan/fables-for-robots/jobs-iroh
nix develop -c gh release create v0.13.0 --verify-tag \
  --repo jobs-build/jobs-iroh \
  --title "v0.13.0 — jobs-build org, amberiroh in-tree" \
  --notes "$(cat <<'EOF'
**The module moved to `github.com/jobs-build/jobs-iroh`.** The repo now lives in the `jobs-build` org, and the module path changed with it. Update imports and `GOPRIVATE=github.com/jobs-build/*`. Tags `v0.1.0`–`v0.12.0` are **not** resolvable under the new path — those commits declare the old module path — so v0.13.0 is the first release consumable as `jobs-build/jobs-iroh`. GitHub redirects keep plain `git` clones of the old URL working.

**amber-store-iroh is now in-tree as `amberiroh/`.** jobs-iroh was its only external consumer, and two of the five server ALPNs (`jobs-runner-amber/1.0`, `jobs-amber-admin/1.0`) are served directly by its `Server` — so the split bought no reuse while costing a cross-repo release round-trip on every protocol change. Its four packages are merged into one in-tree package and the dependency is gone. `amber-store-core` remains external.

- **Nothing on the wire changed.** The ALPN and every frame constant moved verbatim, so a 0.13 server and a 0.12 runner still interoperate on the amber ALPNs. No `jobs-runner-nats` fence bump, no `KPVersion` bump — **every cached pin, KP tree and build output stays valid.**
- **Dead code removed:** the standalone-listener path (`Server.Serve`/`serveConn`) that only upstream's `amber-serve` CLI used.

Design: `docs/superpowers/specs/2026-07-27-amberiroh-integration-design.md`
EOF
)"
```

- [ ] **Step 5: Verify the module resolves under the tag**

Run:
```bash
D=$(mktemp -d); cd /home/dragan/fables-for-robots/jobs-iroh
nix develop -c bash -c "cd $D && export GOPRIVATE='github.com/jobs-build/*' \
  && export GOMODCACHE=$D/mc && go mod init t >/dev/null \
  && go get github.com/jobs-build/jobs-iroh@v0.13.0 && echo TAG_RESOLVES"
chmod -R u+w $D && rm -rf $D
```
Expected: `TAG_RESOLVES`. This is the check that the pre-move tag problem is
actually fixed.

---

### Task 5: Publish the jobs-registry image

**Files:** none tracked (binaries are gitignored).

**Interfaces:**
- Consumes: tag `v0.13.0` from Task 4.
- Produces: `dmilhdef/jobs-registry:v0.13.0` and `:latest` on Docker Hub.

**Note:** this pushes publicly and needs `sudo`. Confirm with the user before
running Step 2.

- [ ] **Step 1: Build both arch binaries from a clean tree**

A dirty tree flips Go's `vcs.modified` stamp, so verify cleanliness first.

```bash
cd /home/dragan/fables-for-robots/jobs-iroh
git status --porcelain    # must be empty
nix develop -c bash -c 'export GOPRIVATE="github.com/jobs-build/*"
  CGO_ENABLED=0 GOARCH=arm64 go build -o deploy/jobs-registry/jobs-registry-arm64 ./cmd/jobs-registry
  CGO_ENABLED=0 GOARCH=amd64 go build -o deploy/jobs-registry/jobs-registry-amd64 ./cmd/jobs-registry'
ls -l deploy/jobs-registry/jobs-registry-*
```

- [ ] **Step 2: Buildx multi-arch push**

```bash
cd /home/dragan/fables-for-robots/jobs-iroh
REV=$(git rev-parse HEAD)
sudo docker --config /home/dragan/.docker buildx build \
  --builder jobs-multi \
  --platform linux/amd64,linux/arm64 \
  --provenance=false --sbom=false \
  --label org.opencontainers.image.version=0.13.0 \
  --label org.opencontainers.image.revision=$REV \
  --label org.opencontainers.image.source=https://github.com/jobs-build/jobs-iroh \
  --annotation "index:org.opencontainers.image.version=0.13.0" \
  --annotation "index:org.opencontainers.image.revision=$REV" \
  --annotation "index:org.opencontainers.image.source=https://github.com/jobs-build/jobs-iroh" \
  -t dmilhdef/jobs-registry:v0.13.0 \
  -t dmilhdef/jobs-registry:latest \
  --push deploy/jobs-registry
```

Note the `image.source` label now points at the **new** org — the v0.12.0
image still advertises `fables-for-robots`.

- [ ] **Step 3: Verify the manifest list and clean up**

```bash
sudo docker --config /home/dragan/.docker buildx imagetools inspect dmilhdef/jobs-registry:v0.13.0
rm -f /home/dragan/fables-for-robots/jobs-iroh/deploy/jobs-registry/jobs-registry-{amd64,arm64}
```
Expected: exactly two platform entries (`linux/amd64`, `linux/arm64`), no
`unknown/unknown` attestation rows.

---

### Task 6: Update and release assimilate v0.2.3

**Files:**
- Modify: `../assimilate/go.mod`, `../assimilate/go.sum`
- Modify: `../assimilate/package.nix` (line 5 version, line 9 vendorHash)

**Interfaces:**
- Consumes: `github.com/jobs-build/jobs-iroh@v0.13.0` from Task 4.
- Produces: assimilate tag `v0.2.3` + GitHub release.

Version choice: `v0.2.2` was itself a dependency bump ("Upgraded `jobs-iroh`
to v0.11.0"), so a patch bump matches convention.

- [ ] **Step 1: Repin to the tagged release**

assimilate currently points at a jobs-iroh pseudo-version; move it to the tag.

```bash
cd /home/dragan/fables-for-robots/assimilate
nix develop -c bash -c 'export GOPRIVATE="github.com/jobs-build/*"; \
  go get github.com/jobs-build/jobs-iroh@v0.13.0 && go mod tidy'
grep jobs-iroh go.mod
```
Expected: `github.com/jobs-build/jobs-iroh v0.13.0` (no `-0.2026…` suffix).

- [ ] **Step 2: Confirm amber-store-iroh left the graph**

Run:
```bash
cd /home/dragan/fables-for-robots/assimilate
grep -c 'amber-store-iroh' go.mod
```
Expected: `0`. It was an indirect dep via jobs-iroh; Task 2 removed it, so it
should drop out here too.

- [ ] **Step 3: Build, vet, test**

```bash
cd /home/dragan/fables-for-robots/assimilate
nix develop -c bash -c 'export GOPRIVATE="github.com/jobs-build/*"; \
  gofmt -l . && go build ./... && go vet ./... && go test ./...'
```
Expected: `gofmt -l` silent, all 10 packages `ok`.

- [ ] **Step 4: Bump version and recompute vendorHash**

`package.nix` line 5: `version = "0.2.2";` → `version = "0.2.3";`

The go.mod change invalidates the pinned `vendorHash`. Get the new one:
```bash
cd /home/dragan/fables-for-robots/assimilate
nix build .#assimilate -L 2>&1 | grep -A1 'specified:'
```
Copy the `got:` value into `package.nix` line 9, then re-run to confirm:
```bash
nix build .#assimilate -L && ls -l result/bin/assimilate && rm -f result
```
Expected: builds clean. Skipping this breaks the CI `nix build` job.

- [ ] **Step 5: Commit, tag, push**

```bash
cd /home/dragan/fables-for-robots/assimilate
git add -A
git commit -m "Release v0.2.3: jobs-iroh v0.13.0

Move off the pseudo-version onto the first jobs-iroh release cut under
the jobs-build org. amber-store-iroh leaves the dependency graph -
jobs-iroh vendored it in-tree.

vendorHash recomputed for the changed go.mod/go.sum."
git tag v0.2.3
git push origin main
git push origin v0.2.3
```

- [ ] **Step 6: GitHub release**

```bash
cd /home/dragan/fables-for-robots/assimilate
nix develop -c gh release create v0.2.3 --verify-tag \
  --repo jobs-build/assimilate \
  --title "v0.2.3" \
  --notes "## What's new

- Upgraded \`jobs-iroh\` to v0.13.0 — the first release under the \`jobs-build\` org.
- \`amber-store-iroh\` drops out of the dependency graph; jobs-iroh now vendors it in-tree."
```

---

## Self-Review

**Spec coverage.** Merge into one package → Task 1 Steps 1-5. Prune
`Serve`/`serveConn` → Task 1 Step 6. Relocate `SendPack`, keep `chunkWriter`
→ Task 1 Step 7. Nine import lines / seven files → Task 2 Steps 1-2. Drop
dependency → Task 2 Steps 3-4. Inherited suite as oracle → Task 1 Step 9.
Full suite green → Task 2 Step 5. `deadcode` clean → Task 2 Step 6. `go mod
tidy` as assertion → Task 2 Step 4. CLAUDE.md follow-up → Task 3. Upstream
untouched → Global Constraints. No `KPVersion`/ALPN bump → Global
Constraints, restated in the release notes.

**Placeholders.** None: every step carries the actual command, path, or code.

**Type consistency.** Symbol names in Task 2's qualifier rewrite match Task
1's Produces block. `SendPackRecords` is explicitly guarded against the
`SendPack`→`sendPack` rename (Task 1 Step 7). The `ambserver` alias is
handled where it exists (`serve/serve.go` only).

**Open item carried from design:** amber-store-core and amber-store-iroh stay
untagged and are consumed by pseudo-version — unchanged by this plan, and
amber-store-iroh is no longer consumed by jobs-iroh at all.
