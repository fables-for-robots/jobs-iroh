package clientcli

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/urfave/cli/v2"

	"github.com/jobs-build/jobs-iroh/api"
	"github.com/jobs-build/jobs-iroh/wire"
)

// diagnoseCmd fetches the durable failure trail of a request or node and
// renders the full debugging story: per attempt the verdict (origin, class,
// exit, disposition), the runner, the error summary, timing, rusage, any
// refused ref batch, and the captured build output — everything a human (or
// an LLM the report is pasted to) needs to pinpoint the failure without
// access to the server.
func diagnoseCmd() *cli.Command {
	return &cli.Command{
		Name:  "diagnose",
		Usage: "full failure report for a build request or node (survives retries and server restarts)",
		Flags: append(serverFlags(),
			&cli.StringFlag{Name: "request", Usage: "request ID printed at submit time"},
			&cli.StringFlag{Name: "node", Usage: "node name (<kind>_<hexkey>) from status/watch output"},
			&cli.IntFlag{Name: "attempts", Usage: "failed attempts per node, newest first (server default 4)"},
			&cli.BoolFlag{Name: "json", Usage: "emit the report as one JSON object on stdout"},
			&cli.StringFlag{Name: "logs-dir", Usage: "additionally write each attempt's captured output verbatim to <dir>/<node>.<gen>.{head,tail}"},
		),
		Action: func(c *cli.Context) error {
			reqID, node := c.String("request"), c.String("node")
			if (reqID == "") == (node == "") {
				return cli.Exit("exactly one of --request and --node is required", 2)
			}
			ctx, stop := signalCtx(c.Context)
			defer stop()
			ac, err := dialAPI(ctx, c.String("server"), c.StringSlice("addr"), alpnAdmin)
			if err != nil {
				return err
			}
			defer ac.Close()

			var reply api.DiagnoseReply
			err = ac.call(ctx, api.TDiagnose, api.DiagnoseRequest{
				RequestID:   reqID,
				Node:        node,
				MaxAttempts: c.Int("attempts"),
			}, api.TDiagnoseReply, &reply)
			if err != nil {
				return err
			}

			if dir := c.String("logs-dir"); dir != "" {
				if err := writeAttemptLogs(dir, reply); err != nil {
					return err
				}
			}
			if c.Bool("json") {
				enc := json.NewEncoder(c.App.Writer)
				enc.SetIndent("", "  ")
				return enc.Encode(jsonReport(reply))
			}
			renderDiagnosis(c.App.Writer, reply)
			return nil
		},
	}
}

// --- human rendering ---

// renderDiagnosis writes the plain-text failure report: a request header,
// then per node the attempt trail newest-first with verdict, attribution,
// timing, rusage and the captured output between explicit markers.
func renderDiagnosis(w io.Writer, reply api.DiagnoseReply) {
	if reply.RequestID != "" {
		c := reply.Counts
		fmt.Fprintf(w, "request %s · %s · %d/%d done, %d failed, %d running, %d waiting\n",
			reply.RequestID, strings.ToUpper(orUnknown(reply.Phase)),
			c.Done, c.Total, c.Failed, c.Running, c.Waiting)
	}
	if reply.Truncated {
		fmt.Fprintln(w, "(more failing nodes than one report carries — re-run with --node for the rest)")
	}
	if len(reply.Nodes) == 0 {
		fmt.Fprintln(w, "no failure records")
		return
	}
	for _, nd := range reply.Nodes {
		fmt.Fprintf(w, "\n=== node %s ===\n", nd.Node)
		fmt.Fprintf(w, "kind %s · platform %s", nd.Kind, orUnknown(nd.Platform))
		if nd.Phase != "" {
			fmt.Fprintf(w, " · current phase %s (gen %d)", nd.Phase, nd.Gen)
		}
		fmt.Fprintln(w)
		if len(nd.Attempts) == 0 {
			fmt.Fprintln(w, "no recorded attempts")
			continue
		}
		for i, a := range nd.Attempts {
			renderAttempt(w, i == 0, a)
		}
	}
}

// renderAttempt writes one attempt block. The first (newest) attempt is the
// story; the older ones show how the budget burned.
func renderAttempt(w io.Writer, newest bool, a api.AttemptReport) {
	tag := ""
	if newest {
		tag = " (newest)"
	}
	fmt.Fprintf(w, "\n--- attempt gen %d%s: %s ---\n", a.Gen, tag, attemptVerdict(a))
	if r := a.Result; r != nil && r.Runner != "" {
		fmt.Fprintf(w, "runner:   %s\n", r.Runner)
	}
	if a.ErrSummary != "" {
		fmt.Fprintf(w, "error:    %s\n", a.ErrSummary)
	}
	if line := attemptTiming(a); line != "" {
		fmt.Fprintf(w, "timing:   %s\n", line)
	}
	if r := a.Result; r != nil {
		if line := rusageLine(r.Rusage); line != "" {
			fmt.Fprintf(w, "rusage:   %s\n", line)
		}
		// A refused batch (gate/commit) is diagnostic gold: exactly which
		// names/keys the runner proposed.
		if len(r.Refs) > 0 && (a.Origin == wire.FailOriginGate || a.Origin == wire.FailOriginCommit) {
			fmt.Fprintln(w, "proposed refs:")
			for _, ref := range r.Refs {
				fmt.Fprintf(w, "  %s → %s\n", ref.Name, hex.EncodeToString(ref.Key))
			}
		}
	}
	if len(a.RequestIDs) > 0 {
		fmt.Fprintf(w, "requests: %s\n", strings.Join(a.RequestIDs, ", "))
	}

	switch {
	case a.LogMissing:
		fmt.Fprintln(w, "captured output: not available (evicted or the attempt produced none)")
	case len(a.LogHead) == 0 && len(a.LogTail) == 0:
		fmt.Fprintln(w, "captured output: empty")
	default:
		note := ""
		if a.LogTruncated {
			note = ", truncated"
		}
		fmt.Fprintf(w, "captured output (%dB head · %dB omitted · %dB tail%s):\n",
			len(a.LogHead), a.LogGap, len(a.LogTail), note)
		w.Write(a.LogHead)
		if a.LogGap > 0 {
			fmt.Fprintf(w, "\n… [%d bytes omitted] …\n", a.LogGap)
		}
		w.Write(a.LogTail)
		if n := len(a.LogTail); n > 0 && a.LogTail[n-1] != '\n' {
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w, "--- end of output ---")
	}
}

// attemptVerdict compresses one attempt's fate into the block header:
// what the runner said, who decided, and what the scheduler did about it.
func attemptVerdict(a api.AttemptReport) string {
	var b strings.Builder
	if r := a.Result; r != nil {
		b.WriteString("class " + r.Class)
		if r.Exit != 0 {
			fmt.Fprintf(&b, " (exit %d)", r.Exit)
		}
	} else {
		b.WriteString("server-side failure")
	}
	fmt.Fprintf(&b, " · origin %s · ", a.Origin)
	if a.Disposition == wire.FailDispositionRetry {
		fmt.Fprintf(&b, "RETRIED (backoff %s)", formatDur(time.Duration(a.BackoffMs)*time.Millisecond))
	} else {
		b.WriteString("FAILED")
	}
	if a.ConsecRetry > 0 {
		fmt.Fprintf(&b, " · consecutive retryable %d", a.ConsecRetry)
	}
	if a.ConsecCtrl > 0 {
		fmt.Fprintf(&b, " · consecutive control %d", a.ConsecCtrl)
	}
	return b.String()
}

// attemptTiming renders the attempt's absolute anchor plus the queued/ran
// spans (server clocks only, like every elapsed the scheduler reports).
func attemptTiming(a api.AttemptReport) string {
	if a.FailedNs == 0 {
		return ""
	}
	parts := []string{"failed " + time.Unix(0, a.FailedNs).UTC().Format(time.RFC3339)}
	if a.StartedNs > 0 && a.FailedNs > a.StartedNs {
		parts = append(parts, "ran "+formatDur(time.Duration(a.FailedNs-a.StartedNs)))
	}
	if a.EnqueuedNs > 0 && a.StartedNs > a.EnqueuedNs {
		parts = append(parts, "queued "+formatDur(time.Duration(a.StartedNs-a.EnqueuedNs)))
	}
	return strings.Join(parts, " · ")
}

// rusageLine renders the runner's resource report, skipping zero fields.
func rusageLine(ru wire.Rusage) string {
	var parts []string
	if ru.WallNs > 0 {
		parts = append(parts, "wall "+formatDur(time.Duration(ru.WallNs)))
	}
	if ru.CPUNs > 0 {
		parts = append(parts, "cpu "+formatDur(time.Duration(ru.CPUNs)))
	}
	if ru.MaxRSS > 0 {
		parts = append(parts, fmt.Sprintf("maxRSS %dMiB", ru.MaxRSS>>20))
	}
	if ru.Stored > 0 {
		parts = append(parts, fmt.Sprintf("stored %dB", ru.Stored))
	}
	return strings.Join(parts, " · ")
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// --- JSON rendering ---

// jsonReport mirrors the reply with log bytes as sanitized strings (base64
// blobs are useless to paste into a conversation) and nanosecond anchors as
// RFC3339 — the agent-facing shape.
func jsonReport(reply api.DiagnoseReply) map[string]any {
	out := map[string]any{}
	if reply.RequestID != "" {
		out["requestId"] = reply.RequestID
		out["phase"] = reply.Phase
		out["counts"] = map[string]int{
			"total": reply.Counts.Total, "done": reply.Counts.Done,
			"running": reply.Counts.Running, "failed": reply.Counts.Failed,
			"waiting": reply.Counts.Waiting,
		}
	}
	if reply.Truncated {
		out["truncated"] = true
	}
	nodes := make([]map[string]any, 0, len(reply.Nodes))
	for _, nd := range reply.Nodes {
		jn := map[string]any{
			"node":     nd.Node,
			"kind":     nd.Kind,
			"platform": nd.Platform,
		}
		if nd.Phase != "" {
			jn["currentPhase"] = nd.Phase
			jn["currentGen"] = nd.Gen
		}
		attempts := make([]map[string]any, 0, len(nd.Attempts))
		for _, a := range nd.Attempts {
			attempts = append(attempts, jsonAttempt(a))
		}
		jn["attempts"] = attempts
		nodes = append(nodes, jn)
	}
	out["nodes"] = nodes
	return out
}

func jsonAttempt(a api.AttemptReport) map[string]any {
	ja := map[string]any{
		"gen":         a.Gen,
		"origin":      a.Origin,
		"disposition": a.Disposition,
		"errSummary":  a.ErrSummary,
	}
	if r := a.Result; r != nil {
		ja["class"] = r.Class
		ja["exit"] = r.Exit
		ja["runner"] = r.Runner
		ru := map[string]int64{}
		if r.Rusage.WallNs > 0 {
			ru["wallMs"] = r.Rusage.WallNs / 1e6
		}
		if r.Rusage.CPUNs > 0 {
			ru["cpuMs"] = r.Rusage.CPUNs / 1e6
		}
		if r.Rusage.MaxRSS > 0 {
			ru["maxRSSBytes"] = r.Rusage.MaxRSS
		}
		if len(ru) > 0 {
			ja["rusage"] = ru
		}
		if len(r.Refs) > 0 {
			refs := make([]map[string]string, 0, len(r.Refs))
			for _, ref := range r.Refs {
				refs = append(refs, map[string]string{"name": ref.Name, "key": hex.EncodeToString(ref.Key)})
			}
			ja["proposedRefs"] = refs
		}
	}
	if a.ConsecRetry > 0 {
		ja["consecutiveRetryable"] = a.ConsecRetry
	}
	if a.ConsecCtrl > 0 {
		ja["consecutiveControl"] = a.ConsecCtrl
	}
	if a.BackoffMs > 0 {
		ja["backoffMs"] = a.BackoffMs
	}
	if len(a.RequestIDs) > 0 {
		ja["requestIds"] = a.RequestIDs
	}
	for _, ts := range []struct {
		key string
		ns  int64
	}{{"enqueuedAt", a.EnqueuedNs}, {"startedAt", a.StartedNs}, {"failedAt", a.FailedNs}} {
		if ts.ns > 0 {
			ja[ts.key] = time.Unix(0, ts.ns).UTC().Format(time.RFC3339Nano)
		}
	}
	switch {
	case a.LogMissing:
		ja["logMissing"] = true
	default:
		ja["logHead"] = strings.ToValidUTF8(string(a.LogHead), "�")
		ja["logOmittedBytes"] = a.LogGap
		ja["logTail"] = strings.ToValidUTF8(string(a.LogTail), "�")
		if a.LogTruncated {
			ja["logTruncated"] = true
		}
	}
	return ja
}

// writeAttemptLogs dumps each attempt's captured output verbatim for
// byte-exact inspection.
func writeAttemptLogs(dir string, reply api.DiagnoseReply) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, nd := range reply.Nodes {
		for _, a := range nd.Attempts {
			base := filepath.Join(dir, fmt.Sprintf("%s.%d", nd.Node, a.Gen))
			for suffix, data := range map[string][]byte{".head": a.LogHead, ".tail": a.LogTail} {
				if len(data) == 0 {
					continue
				}
				if err := os.WriteFile(base+suffix, data, 0o644); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
