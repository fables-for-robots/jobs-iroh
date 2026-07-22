// Package runner holds the JOBS stage drivers and their execution machinery:
// it resolves and runs fetchers for imports, assembles and runs the hermetic
// build sandbox, ingests outputs into amber, and publishes result refs
// through the RefWriter seam. Ported from jobs with the store seam swapped to
// jobs-iroh's *amber.Store (single local store — no remote sync, no signing)
// and FUSE dropped (materialize-only store provisioning).
package runner

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/fables-for-robots/jobs-iroh/events"
	"github.com/fables-for-robots/jobs-iroh/tailbuf"
)

// ExecSpec describes one fetcher invocation.
type ExecSpec struct {
	FetcherDir  string            // CWD; entrypoint ./fetch lives here
	OutputDir   string            // writable; fetcher writes results here
	Env         map[string]string // JOBS_FETCH_PARAMS / JOBS_OUTPUT_DIR / JOBS_SECRETS_FILE
	SecretsFile string            // path, or "" when no requiredTags
	// StdoutSink/StderrSink, when non-nil, additionally receive the process's
	// full stdout/stderr (build-events output capture). Stderr keeps the 4KB
	// tail for the failure path regardless.
	StdoutSink io.Writer
	StderrSink io.Writer
	// Events (nil-safe) receives the executor's exec.heartbeat liveness/usage
	// events while ./fetch runs (Linux CgroupExecutor only).
	Events *events.Job
	// Node, when set (the scheduler path), identifies the job for
	// observability. Empty on the local path.
	Node string
}

// ExecResult is the outcome of running ./fetch.
type ExecResult struct {
	ExitCode   int
	StderrTail string
}

// Executor runs a fetcher. On Linux with user namespaces the default is
// CgroupExecutor (light namespaces + best-effort cgroup); the cross-platform
// Subprocess is the fallback and the explicit test/develop seam (see
// defaultImportExecutor).
type Executor interface {
	Run(ctx context.Context, spec ExecSpec) (ExecResult, error)
}

// Subprocess runs the fetcher's ./fetch as a plain child process — no namespace
// isolation (imports are network-capable anyway). A non-zero exit is reported in
// ExecResult, not as a Go error; a Go error means the process could not run
// (infrastructure failure, including ctx cancellation).
type Subprocess struct{}

func (Subprocess) Run(ctx context.Context, spec ExecSpec) (ExecResult, error) {
	env := os.Environ()
	for k, v := range spec.Env {
		env = append(env, k+"="+v)
	}

	tail := tailbuf.New(4 << 10)
	var stderr io.Writer = tail
	if spec.StderrSink != nil {
		stderr = io.MultiWriter(tail, spec.StderrSink)
	}

	// exec can fail ETXTBSY when a concurrently forked child of this process
	// (another job slot's fork, between its clone and execve) still holds the
	// write fd of the just-extracted fetch binary (golang/go#22315). That
	// writer is always moribund — it vanishes as soon as the other child
	// execs — so retry briefly instead of failing the import. A failed Start
	// has no side effects, so re-creating the Cmd per attempt is safe.
	delay := 5 * time.Millisecond
	for attempt := 0; ; attempt++ {
		cmd := exec.CommandContext(ctx, "./fetch")
		cmd.Dir = spec.FetcherDir
		cmd.Env = env
		cmd.Stdout = spec.StdoutSink // nil = discard, as before
		cmd.Stderr = stderr

		err := cmd.Run()
		if err == nil {
			return ExecResult{ExitCode: 0, StderrTail: tail.String()}, nil
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ExecResult{ExitCode: ee.ExitCode(), StderrTail: tail.String()}, nil
		}
		if errors.Is(err, syscall.ETXTBSY) && attempt < 7 && ctx.Err() == nil {
			time.Sleep(delay)
			delay *= 2
			continue
		}
		return ExecResult{ExitCode: -1, StderrTail: tail.String()}, err
	}
}
