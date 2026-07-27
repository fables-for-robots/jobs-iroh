package clientcli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jobs-build/jobs-iroh/api"
	"github.com/jobs-build/jobs-iroh/wire"
)

func sampleReply() api.DiagnoseReply {
	return api.DiagnoseReply{
		RequestID: "r1234",
		Phase:     "failed",
		Counts:    wire.Counts{Total: 7, Done: 3, Failed: 1, Waiting: 3},
		Nodes: []api.NodeDiagnosis{{
			Node:     "buildrun_ab12",
			Kind:     wire.KindBuildRun,
			Platform: "linux/amd64",
			Phase:    wire.PhaseFailed,
			Gen:      42,
			Attempts: []api.AttemptReport{
				{FailureRecord: wire.FailureRecord{
					Node: "buildrun_ab12", Gen: 42,
					Origin: wire.FailOriginResult, Disposition: wire.FailDispositionFailed,
					ErrSummary:  "retry budget exhausted: building: gcc: fatal error",
					ConsecRetry: 4,
					RequestIDs:  []string{"r1234"},
					Result: &wire.Result{
						Class: wire.ClassRetryable, Exit: 2, Runner: "runner-a",
						Rusage: wire.Rusage{WallNs: 42_600_000_000, MaxRSS: 512 << 20},
					},
					EnqueuedNs: 1_753_255_000_000_000_000,
					StartedNs:  1_753_255_001_200_000_000,
					FailedNs:   1_753_255_043_800_000_000,
					LogHead:    []byte("compile log head\n"),
					LogGap:     512,
					LogTail:    []byte("gcc: fatal error: no input files\n"),
				}},
				{FailureRecord: wire.FailureRecord{
					Node: "buildrun_ab12", Gen: 41,
					Origin: wire.FailOriginResult, Disposition: wire.FailDispositionRetry,
					ErrSummary: "building: gcc: fatal error", ConsecRetry: 3, BackoffMs: 4000,
					Result:     &wire.Result{Class: wire.ClassRetryable, Exit: 2, Runner: "runner-a"},
					FailedNs:   1_753_254_990_000_000_000,
					LogMissing: true,
				}},
			},
		}},
	}
}

func TestRenderDiagnosis(t *testing.T) {
	var buf bytes.Buffer
	renderDiagnosis(&buf, sampleReply())
	out := buf.String()
	for _, want := range []string{
		"request r1234 · FAILED · 3/7 done, 1 failed",
		"=== node buildrun_ab12 ===",
		"kind buildrun · platform linux/amd64 · current phase failed (gen 42)",
		"attempt gen 42 (newest): class retryable (exit 2) · origin result · FAILED · consecutive retryable 4",
		"runner:   runner-a",
		"error:    retry budget exhausted: building: gcc: fatal error",
		"rusage:   wall 43s · maxRSS 512MiB",
		"requests: r1234",
		"captured output (17B head · 512B omitted · 33B tail):",
		"compile log head",
		"… [512 bytes omitted] …",
		"gcc: fatal error: no input files",
		"--- end of output ---",
		"attempt gen 41: class retryable (exit 2) · origin result · RETRIED (backoff 4s) · consecutive retryable 3",
		"captured output: not available",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderDiagnosisEmpty(t *testing.T) {
	var buf bytes.Buffer
	renderDiagnosis(&buf, api.DiagnoseReply{})
	if !strings.Contains(buf.String(), "no failure records") {
		t.Fatalf("empty reply rendered as %q", buf.String())
	}
}

// TestJSONReportShape: the JSON view carries logs as readable strings (not
// base64) and timestamps as RFC3339 — the paste-into-a-conversation shape.
func TestJSONReportShape(t *testing.T) {
	b, err := json.Marshal(jsonReport(sampleReply()))
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	for _, want := range []string{
		`"requestId":"r1234"`,
		`"logHead":"compile log head\n"`,
		`"logOmittedBytes":512`,
		`"failedAt":"2025-07-23T`,
		`"rusage":{"maxRSSBytes":536870912,"wallMs":42600}`,
		`"class":"retryable"`,
		`"consecutiveRetryable":4`,
		`"logMissing":true`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("json missing %s\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, `"logHead":"Y29tc`) {
		t.Error("log bytes leaked as base64")
	}
}
