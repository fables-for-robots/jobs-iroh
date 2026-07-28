# Cancel Queue Purge Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When a node loses its last interested request (cancel, delete, watcher loss), best-effort delete its queued job message from the JetStream JOBS work queue so undelivered work never executes.

**Architecture:** `enqueueLocked` already receives a `jetstream.PubAck` whose `Sequence` identifies the published job message in the JOBS stream; store it on the node and call `Stream.DeleteMsg(seq)` from `dropInterestLocked`'s eviction loop. Purge is best-effort: any failure leaves today's behavior (job runs, result dropped on arrival). No wire change, no runner change, no ALPN bump.

**Tech Stack:** Go, nats.go JetStream API (`jetstream.Stream.DeleteMsg`), embedded NATS test harness in `sched/sched_test.go`.

**Spec:** `docs/superpowers/specs/2026-07-28-cancel-queue-purge-design.md`

## Global Constraints

- Purge failure must NEVER fail Cancel/Delete/eviction — log at debug, move on.
- Only purge evicted nodes with `queueSeq != 0` AND phase `queued` or `running`.
- Nodes with remaining interest are never evicted, hence never purged (existing `dropInterestLocked` loop guarantees this — do not add a second interest check).
- "Wasteful but never wrong" stays intact: the stale-result drop in `sched/results.go` remains the correctness backstop.
- All commits end with `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`.
- Go toolchain via Nix devShell: prefix build/test commands with `nix develop -c`.

---

### Task 1: Track the publish sequence, purge on eviction (sched package, TDD)

**Files:**
- Modify: `sched/sched_test.go` (helper + rework `TestCancel` ~line 679-698 + two new tests)
- Modify: `sched/sched.go` (Sched struct ~line 79, `New` ~line 131, constructor literal ~line 181)
- Modify: `sched/node.go` (node struct ~line 59, `enqueueLocked` ~line 508, `dropInterestLocked` ~line 609)
- Modify: `sched/submit.go` (`Cancel` doc comment ~line 170)

**Interfaces:**
- Consumes: `jetstream.PubAck.Sequence` (already returned by `PublishMsg`, currently discarded); `jetstream.Stream.DeleteMsg(ctx, seq)`.
- Produces: `node.queueSeq uint64`, `Sched.jobs jetstream.Stream` (internal only — no exported API changes).

- [ ] **Step 1: Add the stream-count test helper**

In `sched/sched_test.go`, after `getRef` (~line 144), add:

```go
// jobsMsgCount returns the JOBS work-queue stream's message count.
func (e *env) jobsMsgCount() uint64 {
	e.t.Helper()
	st, err := e.js.Stream(e.ctx, wire.StreamJobs)
	if err != nil {
		e.t.Fatalf("jobs stream: %v", err)
	}
	info, err := st.Info(e.ctx)
	if err != nil {
		e.t.Fatalf("jobs stream info: %v", err)
	}
	return info.State.Msgs
}
```

- [ ] **Step 2: Rework `TestCancel` to expect the purge**

`TestCancel` currently asserts the OLD contract: after cancel, a fake runner consumes the orphaned job and its result is dropped (lines 679-698, from the comment `// The queued buildfrom job is still in the work queue.` through `assertRefs(...)`).

First, right after the `Watch` setup (`defer stop()`, ~line 651) and BEFORE the `Cancel` call, add a baseline assertion:

```go
	if n := e.jobsMsgCount(); n != 1 {
		t.Fatalf("JOBS msgs after submit = %d, want 1 (queued buildfrom)", n)
	}
```

Then replace the whole old block — starting at the comment `// The queued buildfrom job is still in the work queue.` and ending with `assertRefs(e, map[string]bool{"build-from:" + k.String(): false})` (inclusive; also delete the now-redundant `k, _ := wire.ParseKey(sub.K)` inside it) — with:

```go
	// The queued buildfrom job was purged from the work queue at cancel.
	if n := e.jobsMsgCount(); n != 0 {
		t.Fatalf("JOBS msgs after cancel = %d, want 0 (purged)", n)
	}

	// Backstop unchanged: a late result for an evicted node (e.g. from an
	// attempt that was already in a runner) is dropped and its refs never
	// committed — wasteful, never wrong.
	k, _ := wire.ParseKey(sub.K)
	nodeName := wire.NodeName(wire.KindBuildFrom, k)
	orphanOut := e.ingestFile([]byte("orphan output"))
	res := wire.Result{
		Node:   nodeName,
		Gen:    1,
		Runner: "fake-runner",
		Class:  wire.ClassOK,
		Refs:   []wire.RefProposal{{Name: "build-from:" + k.String(), Key: orphanOut[:]}},
	}
	if _, err := e.js.PublishMsg(e.ctx,
		&nats.Msg{Subject: wire.ResultsSubject(nodeName), Data: wire.MustEncode(res)},
		jetstream.WithMsgID(wire.ResultMsgID(nodeName, res.Gen))); err != nil {
		t.Fatalf("publish orphan result: %v", err)
	}
	time.Sleep(500 * time.Millisecond) // let the (dropped) result flow through
	assertRefs(e, map[string]bool{"build-from:" + k.String(): false})
```

The trailing Delete section of `TestCancel` (from `// Delete removes the request entirely.`) stays untouched.

- [ ] **Step 3: Add the shared-interest and cancel-after-done tests**

Append after `TestCancel`:

```go
// TestCancelSharedInterestKeepsJob: eviction only purges nodes nobody else
// needs — a job message backing a node shared with a live request stays.
func TestCancelSharedInterestKeepsJob(t *testing.T) {
	e := newEnv(t)
	defBytes := e.treeDef("shared-cancel")

	subA, err := e.s.Submit(e.ctx, api.SubmitRequest{Def: defBytes})
	if err != nil {
		t.Fatal(err)
	}
	subB, err := e.s.Submit(e.ctx, api.SubmitRequest{Def: defBytes})
	if err != nil {
		t.Fatal(err)
	}
	if n := e.jobsMsgCount(); n != 1 {
		t.Fatalf("JOBS msgs after two joined submits = %d, want 1", n)
	}

	if err := e.s.Cancel(e.ctx, subA.RequestID); err != nil {
		t.Fatalf("cancel A: %v", err)
	}
	if n := e.jobsMsgCount(); n != 1 {
		t.Fatalf("JOBS msgs after cancelling one of two = %d, want 1 (B still interested)", n)
	}

	if err := e.s.Cancel(e.ctx, subB.RequestID); err != nil {
		t.Fatalf("cancel B: %v", err)
	}
	if n := e.jobsMsgCount(); n != 0 {
		t.Fatalf("JOBS msgs after cancelling both = %d, want 0", n)
	}
}

// TestCancelAfterDone: cancelling a finished request must not error — the
// evicted nodes are done, their queue messages long since acked, and the
// purge must skip them (phase guard).
func TestCancelAfterDone(t *testing.T) {
	e := newEnv(t)
	e.startRunner([]wire.Class{"c0.2-m1", "c1-m1"}, e.standardHandler(builddef.Pinned{}))

	sub, err := e.s.Submit(e.ctx, api.SubmitRequest{Def: e.treeDef("cancel-after-done")})
	if err != nil {
		t.Fatal(err)
	}
	if snap := e.watchTerminal(sub.RequestID); snap.Phase != "done" {
		t.Fatalf("terminal phase = %s, want done", snap.Phase)
	}
	if err := e.s.Cancel(e.ctx, sub.RequestID); err != nil {
		t.Fatalf("cancel after done: %v", err)
	}
	if n := e.jobsMsgCount(); n != 0 {
		t.Fatalf("JOBS msgs = %d, want 0", n)
	}
}
```

- [ ] **Step 4: Run the tests, verify the new assertions fail**

Run: `nix develop -c go test ./sched/ -run 'TestCancel' -v`
Expected: `TestCancel` FAILS at "JOBS msgs after cancel = 1, want 0 (purged)"; `TestCancelSharedInterestKeepsJob` FAILS at "JOBS msgs after cancelling both = 1, want 0"; `TestCancelAfterDone` PASSES (regression guard for the phase check).

- [ ] **Step 5: Implement the purge**

`sched/sched.go` — Sched struct (~line 79): add the stream handle after `js`:

```go
	js    jetstream.JetStream
	jobs  jetstream.Stream // JOBS stream handle (queued-job purge on eviction)
	kv    jetstream.KeyValue
```

`sched/sched.go` — `New` (~line 131): the JOBS `CreateOrUpdateStream` currently discards its result (`if _, err := js.CreateOrUpdateStream(...)`). Capture it:

```go
	jobsStream, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:        wire.StreamJobs,
		Description: "jobs-iroh work queue (one msg per placeable node attempt)",
		Subjects:    []string{wire.SubjectJobsRoot + ".>"},
		Retention:   jetstream.WorkQueuePolicy,
		Duplicates:  10 * time.Minute,
	})
	if err != nil {
		return nil, fmt.Errorf("sched: create %s stream: %w", wire.StreamJobs, err)
	}
```

`sched/sched.go` — constructor literal (~line 181): add `jobs: jobsStream,` after `js: js,`.

`sched/node.go` — node struct (~line 59): add below `runner string`:

```go
	queueSeq   uint64 // JOBS stream seq of the current queued job msg (0 = none)
```

`sched/node.go` — `enqueueLocked` (~line 508): reset the seq with the other attempt state:

```go
	n.gen++
	n.runner = ""
	n.startedAt = time.Time{}
	n.enqueuedAt = time.Now()
	n.queueSeq = 0
```

and capture the ack (currently `_, err = s.js.PublishMsg(...)`):

```go
	ack, err := s.js.PublishMsg(s.ctx, &nats.Msg{Subject: subject, Data: wire.MustEncode(job)},
		jetstream.WithMsgID(wire.JobMsgID(n.name, n.gen)))
```

then set the seq once the publish succeeded (right before `n.phase = wire.PhaseQueued`):

```go
	n.queueSeq = ack.Sequence
	n.phase = wire.PhaseQueued
```

`sched/node.go` — `dropInterestLocked` (~line 609): in the eviction loop, after the `if cur := s.nodes[n.id]; cur != n { continue }` guard, add:

```go
		// Best-effort queue purge: an evicted node's undelivered job message
		// need never run. Deleting a delivered-but-unacked message (running,
		// or queued but already in a runner) stops AckWait redelivery, not
		// the in-flight attempt — its result is dropped on arrival as before.
		if n.queueSeq != 0 && (n.phase == wire.PhaseQueued || n.phase == wire.PhaseRunning) {
			if err := s.jobs.DeleteMsg(s.ctx, n.queueSeq); err != nil {
				s.log.Debug("queued job purge failed", "node", n.name, "seq", n.queueSeq, "error", err)
			}
		}
```

- [ ] **Step 6: Update the two stale comments**

`sched/node.go` — `dropInterestLocked` doc comment currently reads:

```go
// dropInterestLocked removes reqID's interest from the target's closure and
// evicts every node left with zero interest: cancellation and watcher-loss
// cleanup are the same path. Running/queued work is NOT chased — a job whose
// node left the table is dropped when its result arrives ("wasteful but
// never wrong").
```

Replace the last sentence:

```go
// dropInterestLocked removes reqID's interest from the target's closure and
// evicts every node left with zero interest: cancellation and watcher-loss
// cleanup are the same path. An evicted node's queued job message is
// best-effort deleted from the work queue; an attempt already in a runner
// is not chased — it completes and its result is dropped on arrival
// ("wasteful but never wrong").
```

`sched/submit.go` — `Cancel` doc comment currently reads:

```go
// Cancel drops the request's interest: nodes needed by nobody else leave
// the table; in-flight jobs are not chased — their results are dropped on
// arrival (running once too often is wasteful, never wrong). The request
// stays inspectable until Delete.
```

Replace with:

```go
// Cancel drops the request's interest: nodes needed by nobody else leave
// the table and their queued job messages are best-effort purged from the
// work queue. In-flight attempts are not chased — their results are
// dropped on arrival (running once too often is wasteful, never wrong).
// The request stays inspectable until Delete.
```

- [ ] **Step 7: Run the cancel tests, verify they pass**

Run: `nix develop -c go test ./sched/ -run 'TestCancel' -v`
Expected: `TestCancel`, `TestCancelSharedInterestKeepsJob`, `TestCancelAfterDone` all PASS.

- [ ] **Step 8: Run the full sched and serve packages**

Run: `nix develop -c go test ./sched/ ./serve/`
Expected: PASS (serve exercises Cancel over the real API surface).

- [ ] **Step 9: Commit**

```bash
git add sched/sched.go sched/node.go sched/submit.go sched/sched_test.go
git commit -m "$(cat <<'EOF'
Cancel purges queued jobs from the JOBS work queue

The scheduler now remembers each queued job's JetStream sequence (the
PubAck it previously discarded) and best-effort deletes the message when
the node loses its last interested request — cancel, delete and
watcher-loss eviction alike. Undelivered work never runs; an attempt
already in a runner still completes and is dropped on arrival, so
"wasteful but never wrong" is unchanged — there is just less waste.

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: Architecture doc + verification sweep

**Files:**
- Modify: `docs/architecture/architecture.md:315-317` (Node graph paragraph)

**Interfaces:**
- Consumes: Task 1's behavior (queued-job purge on eviction).
- Produces: nothing code-visible; doc consistency per CLAUDE.md ("keep code consistent with it").

- [ ] **Step 1: Update the Node graph cancellation sentence**

In `docs/architecture/architecture.md`, the paragraph ending (lines 315-317):

```
Requests hold *interest* in nodes; cancellation drops interest, and a node
with no interest and no running attempt is dropped from memory. Shared
subtrees survive as long as anyone needs them.
```

becomes:

```
Requests hold *interest* in nodes; cancellation drops interest, and a node
with no interest is dropped from memory, its queued job message best-effort
deleted from the work queue. An attempt already in a runner completes and
its result is dropped on arrival. Shared subtrees survive as long as
anyone needs them.
```

- [ ] **Step 2: Full test suite + vet, both platforms**

Run: `nix develop -c go test ./...`
Expected: PASS.

Run: `nix develop -c env CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go vet ./...`
Expected: clean (no `_linux.go`/`_other.go` pair touched, but the release follows — cheap insurance).

- [ ] **Step 3: Commit**

```bash
git add docs/architecture/architecture.md
git commit -m "$(cat <<'EOF'
architecture.md: cancellation purges queued job messages

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: Release v0.18.0

Behavior change, no wire-contract change (subjects, Job/Result CBOR, ALPNs all untouched) → minor bump. Follows CLAUDE.md's release process verbatim; the Docker image push is part of the release, not optional. The user authorized the release ("implement and release it").

**Files:**
- Modify: `version/version.go` (0.17.1 → 0.18.0)
- Modify: `CHANGELOG.md` (new top entry)

- [ ] **Step 1: Bump version and write the changelog entry**

`version/version.go`: change `const Version = "0.17.1"` to `const Version = "0.18.0"`.

`CHANGELOG.md`: insert at the top (below `# Changelog`):

```markdown
## v0.18.0 — 2026-07-28

- **Cancelling a build now removes its queued jobs from the work queue.**
  Cancellation used to be pure bookkeeping: job messages already published
  to the JOBS stream stayed there, so runners picked up and fully executed
  work nobody wanted (results dropped on arrival). The scheduler now
  remembers each queued job's stream sequence and best-effort deletes the
  message when a node loses its last interested request — cancel, delete
  and watcher-loss eviction alike. Work no runner has picked up never runs.
- Scope: an attempt already delivered to a runner still completes and its
  result is dropped, exactly as before — "wasteful but never wrong" is
  unchanged, there is just less waste. Deleting the message of a running
  attempt also stops the work queue redelivering it to another runner if
  the first dies mid-run. Job messages orphaned by a server restart still
  drain through runners as before.
```

- [ ] **Step 2: Release commit, tag, push, GitHub release**

```bash
git add version/version.go CHANGELOG.md
git commit -m "$(cat <<'EOF'
Release v0.18.0: cancelled builds leave the work queue

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>
EOF
)"
git tag v0.18.0
git push origin main && git push origin v0.18.0
nix develop -c gh release create v0.18.0 --verify-tag --repo jobs-build/jobs-iroh \
  --title "v0.18.0 — cancelled builds leave the work queue" \
  --notes "$(sed -n '/^## v0.18.0/,/^## v0.17.1/p' CHANGELOG.md | sed '1d;$d')"
```

- [ ] **Step 3: Build and push the jobs-registry image (clean tree required)**

```bash
git status --porcelain   # MUST print nothing; abort if dirty
nix develop -c bash -c 'export GOPRIVATE="github.com/jobs-build/*"
  CGO_ENABLED=0 GOARCH=arm64 go build -o deploy/jobs-registry/jobs-registry-arm64 ./cmd/jobs-registry
  CGO_ENABLED=0 GOARCH=amd64 go build -o deploy/jobs-registry/jobs-registry-amd64 ./cmd/jobs-registry'
REV=$(git rev-parse HEAD)
sudo docker --config "$HOME/.docker" buildx build --builder jobs-multi \
  --platform linux/amd64,linux/arm64 --provenance=false --sbom=false \
  --label org.opencontainers.image.version="0.18.0" \
  --label org.opencontainers.image.revision="$REV" \
  --label org.opencontainers.image.source=https://github.com/jobs-build/jobs-iroh \
  --annotation "index:org.opencontainers.image.version=0.18.0" \
  --annotation "index:org.opencontainers.image.revision=$REV" \
  --annotation "index:org.opencontainers.image.source=https://github.com/jobs-build/jobs-iroh" \
  -t "dmilhdef/jobs-registry:v0.18.0" -t dmilhdef/jobs-registry:latest \
  --push deploy/jobs-registry
sudo docker --config "$HOME/.docker" buildx imagetools inspect "dmilhdef/jobs-registry:v0.18.0"
rm -f deploy/jobs-registry/jobs-registry-{amd64,arm64}
```

- [ ] **Step 4: Verify**

`imagetools inspect` output MUST show exactly two platform entries
(linux/amd64, linux/arm64) and no `unknown/unknown` attestation rows.
`gh release view v0.18.0 --repo jobs-build/jobs-iroh` shows the notes.
