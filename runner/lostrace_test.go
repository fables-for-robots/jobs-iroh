package runner

// Duplicate-execution loser handling (issue #152): a WriteRefs batch the
// server declines as "not assigned" means this job lost its race — the
// outcome must be silence (like a cancel), never a failure. And a refs-ack
// timeout is a CONTROL-PLANE verdict that must not spend the job's retryable
// budget (issue #153). jobs proved this against the WS wsRefWriter; jobs-iroh
// keeps the classification vocabulary (the sentinels + pushOutcome) — the
// remote result publisher of the sched milestone produces the sentinels.

import (
	"errors"
	"fmt"
	"testing"
)

func TestPushOutcome_NotAssignedIsSilence(t *testing.T) {
	err := fmt.Errorf("%w: node import|ab not assigned to runner r-b", errRefsNotAssigned)
	if out := pushOutcome("pushing", err); !out.Cancelled || out.Failed {
		t.Fatalf("pushOutcome = %+v, want silent Cancelled outcome", out)
	}
}

func TestPushOutcome_AckTimeoutIsControlClass(t *testing.T) {
	err := fmt.Errorf("%w: timed out after 2m waiting for the RefsWritten ack", errRefsAckTimeout)
	out := pushOutcome("pushing", err)
	if !out.Failed || out.Class != classControl || out.Cancelled {
		t.Fatalf("pushOutcome = %+v, want failed control-class outcome", out)
	}
}

func TestPushOutcome_OtherStaysRetryable(t *testing.T) {
	err := errors.New("publish failed: server unreachable")
	if out := pushOutcome("pushing", err); !out.Failed || out.Class != "retryable" {
		t.Fatalf("pushOutcome = %+v, want retryable failure", out)
	}
}

// Wrapping must survive the fmt round trips the drivers apply.
func TestLostRace_SurvivesWrapping(t *testing.T) {
	err := fmt.Errorf("run import: %w", fmt.Errorf("%w: node x not assigned to runner y", errRefsNotAssigned))
	if !errors.Is(err, errRefsNotAssigned) {
		t.Fatal("errors.Is lost the sentinel through wrapping")
	}
}
