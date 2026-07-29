# Node Labels Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build progress shows recipe names (`apk_acl_libs`, `apps/web`, `fetch alpine …`) instead of bare CAS keys, in local build, remote-build/watch, status, logs prefixes, and diagnose.

**Architecture:** Display-only labels: sched nodes learn names from the edges that create them (PinnedInput.Name, plugin/dep map keys) or self-derive (Dir, fetcher), the root from a new optional `SubmitRequest.Label`; `NodeSnap.Label` (optional CBOR) carries them to clients; the local developDriver threads the same names into its Progress step labels. Spec: `docs/design/2026-07-29-node-labels.md`.

## Global Constraints

- Nothing enters `Definition.Canonical()` or any identity; all wire fields optional CBOR; no ALPN bump.
- Nix devShell for build/test; darwin cross-vet at the end; repo commit trailers.

---

### Task 1: wire fields + sched labels

**Files:** `api/api.go` (SubmitRequest.Label, NodeSnap.Label), `sched/node.go` (node.label, require/requireInputLocked label param, unfold sites, `sortedNamedInputs`), `sched/submit.go` (root label stamp, snapshot Label), `sched/failures.go` (+ its api record type) — test via `sched/` suite additions.

- [ ] Failing test: sched harness — submit with Label, drive to a snapshot, assert the target node's Label; unit test `sortedNamedInputs` (plugins first, name-sorted, names preserved); label first-wins on join.
- [ ] Implement; run `nix develop -c go test ./sched/ ./api/...`.
- [ ] Commit: `sched: nodes carry display labels from recipe names`

### Task 2: client rendering + submit label

**Files:** `clientcli/remote.go` (send Label = resolved dir or base(root); labeled shortNode helper), `clientcli/watchview.go`, `clientcli/logtracker.go`, `clientcli/diagnose.go` — snapshot-fed node→label lookup.

- [ ] Failing test: labeled rendering helper (`label (kind:key8)`, fallback unchanged); logtracker prefix uses label when known.
- [ ] Implement; run `nix develop -c go test ./clientcli/`.
- [ ] Commit: `clientcli: render node labels in progress, logs and diagnose`

### Task 3: local driver names

**Files:** `runner/localbuild.go` (`ensurePinDeps`/`ensureInputs` pass names), `runner/develop_linux.go` (`ensureInput`/`ensureBuild`/`ensureImport` name param; `build <name>` labels).

- [ ] Failing test: extend the localbuild deps test to assert a named step label appears (or a focused unit on the label choice).
- [ ] Implement; run `nix develop -c go test ./runner/ -run 'Local|Develop'` then full runner suite.
- [ ] Commit: `runner: local build steps show recipe names`

### Task 4: verify, docs, release v0.22.0

- [ ] Full suite + darwin vet; deploy server+runner to the live pair, run a build, eyeball labeled output via tmux capture.
- [ ] architecture.md + CLAUDE.md one-liners; CHANGELOG; version bump; tag; gh release; image push.

## Self-review
Spec §2→Task 1, §3→Task 2, §4→Task 3, §5 covered per-task, §6 respected. Names consistent: `sortedNamedInputs`, `SubmitRequest.Label`, `NodeSnap.Label`.
