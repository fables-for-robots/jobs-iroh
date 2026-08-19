package sched

// No-runners policy tests (issue #8): submit-time rejection when the fleet
// has no live runner for the build's platform, the cached-doneness
// exemption, and the mid-build watchdog that fails a request after a
// continuous runnerless stretch.

import (
	"strings"
	"testing"
	"time"

	"github.com/jobs-build/jobs-iroh/api"
	"github.com/jobs-build/jobs-iroh/builddef"
	"github.com/jobs-build/jobs-iroh/wire"
)

// TestSubmitRejectedWithoutRunners: a build needing runner work on a
// platform with no live runner is rejected outright, leaving no request
// behind.
func TestSubmitRejectedWithoutRunners(t *testing.T) {
	e := newEnv(t)
	_, err := e.s.Submit(e.ctx, api.SubmitRequest{Def: e.treeDef("no-runners")})
	if err == nil {
		t.Fatal("submit succeeded with an empty fleet")
	}
	if code := ErrorCode(err); code != api.CodeUnavailable {
		t.Fatalf("error code = %q, want %q (err: %v)", code, api.CodeUnavailable, err)
	}
	if !strings.Contains(err.Error(), testPlatform) {
		t.Fatalf("rejection does not name the platform: %v", err)
	}
	if got := e.s.Requests(); len(got) != 0 {
		t.Fatalf("rejected submit left %d request(s) behind", len(got))
	}
}

// TestSubmitRejectedStaleRunner: a runner past the liveness window does not
// count — same rejection as an empty fleet.
func TestSubmitRejectedStaleRunner(t *testing.T) {
	e := newEnv(t)
	e.hello(testPlatform)
	e.staleFleet()
	_, err := e.s.Submit(e.ctx, api.SubmitRequest{Def: e.treeDef("stale-runner")})
	if code := ErrorCode(err); err == nil || code != api.CodeUnavailable {
		t.Fatalf("want %q rejection, got err=%v", api.CodeUnavailable, err)
	}
}

// TestSubmitRejectedWrongPlatform: a live runner for ANOTHER platform does
// not satisfy the gate.
func TestSubmitRejectedWrongPlatform(t *testing.T) {
	e := newEnv(t)
	e.hello("linux/arm64")
	_, err := e.s.Submit(e.ctx, api.SubmitRequest{Def: e.treeDef("wrong-arch")})
	if code := ErrorCode(err); err == nil || code != api.CodeUnavailable {
		t.Fatalf("want %q rejection, got err=%v", api.CodeUnavailable, err)
	}
}

// TestSubmitCachedDoneWithoutRunners: the doneness fast-path is exempt — a
// closure that joins fully done needs no fleet and still completes.
func TestSubmitCachedDoneWithoutRunners(t *testing.T) {
	e := newEnv(t)
	def := e.treeDef("cached-then-runnerless")

	// First run with a scripted runner drives the build to done.
	e.startRunner([]wire.Class{"c0.2-m1", "c1-m1"}, e.standardHandler(builddef.Pinned{}))
	sub, err := e.s.Submit(e.ctx, api.SubmitRequest{Def: def})
	if err != nil {
		t.Fatalf("submit with runner: %v", err)
	}
	if snap := e.watchTerminal(sub.RequestID); snap.Phase != "done" {
		t.Fatalf("first build phase = %s, want done", snap.Phase)
	}

	// Fleet gone: the resubmit joins everything done and must be accepted.
	e.staleFleet()
	sub2, err := e.s.Submit(e.ctx, api.SubmitRequest{Def: def})
	if err != nil {
		t.Fatalf("cached resubmit rejected: %v", err)
	}
	if snap := e.watchTerminal(sub2.RequestID); snap.Phase != "done" {
		t.Fatalf("cached resubmit phase = %s, want done", snap.Phase)
	}
}

// TestNoRunnerWatchdogFailsBuild: an accepted request whose platform loses
// its last live runner fails after the (shrunken) watchdog deadline, with
// the reason on Snapshot.Error, and its queued job messages purged.
func TestNoRunnerWatchdogFailsBuild(t *testing.T) {
	e := newEnv(t)
	e.s.mu.Lock()
	e.s.noRunnerAfter = 50 * time.Millisecond
	e.s.noRunnerCheck = 10 * time.Millisecond
	e.s.mu.Unlock()

	// A live runner admits the submit; nothing consumes the lanes, so the
	// buildfrom job stays queued.
	e.hello(testPlatform)
	sub, err := e.s.Submit(e.ctx, api.SubmitRequest{Def: e.treeDef("watchdog")})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	e.staleFleet()
	snap := e.watchTerminal(sub.RequestID)
	if snap.Phase != "failed" {
		t.Fatalf("terminal phase = %s, want failed", snap.Phase)
	}
	if !strings.Contains(snap.Error, "no live runners for platform "+testPlatform) {
		t.Fatalf("Snapshot.Error = %q, want the no-runners reason", snap.Error)
	}
	// Interest dropped like Cancel: the queued job message is purged.
	deadline := time.Now().Add(5 * time.Second)
	for e.jobsMsgCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("queued jobs not purged: %d left", e.jobsMsgCount())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestNoRunnerWatchdogResetsOnReturn: a runner reappearing before the
// deadline resets the stall clock — the request stays running.
func TestNoRunnerWatchdogResetsOnReturn(t *testing.T) {
	e := newEnv(t)
	e.s.mu.Lock()
	e.s.noRunnerAfter = 300 * time.Millisecond
	e.s.noRunnerCheck = 10 * time.Millisecond
	e.s.mu.Unlock()

	e.hello(testPlatform)
	sub, err := e.s.Submit(e.ctx, api.SubmitRequest{Def: e.treeDef("watchdog-reset")})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	// Stall briefly, then return before the deadline; hold live past the
	// original deadline and verify the request never failed.
	e.staleFleet()
	time.Sleep(100 * time.Millisecond)
	e.hello(testPlatform)
	time.Sleep(400 * time.Millisecond)
	e.s.mu.Lock()
	r := e.s.requests[sub.RequestID]
	reason := ""
	if r != nil {
		reason = r.failReason
	}
	e.s.mu.Unlock()
	if r == nil {
		t.Fatal("request vanished")
	}
	if reason != "" {
		t.Fatalf("watchdog fired despite the runner returning: %q", reason)
	}
}
