//go:build linux

package runner

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fables-for-robots/jobs-iroh/sandbox"
	"github.com/fables-for-robots/jobs-iroh/tailbuf"
)

// CgroupExecutor runs a fetcher's ./fetch inside a best-effort cgroup
// (memory + pids limits) and light namespaces (User, Mount, PID), but
// WITHOUT CLONE_NEWNET — the host network is kept, because imports are
// network-capable (import.md §1).
//
// NewRoot is "" so childSetup skips pivot_root; the fetcher sees the real
// host filesystem and can write directly to the OutputDir/SecretsFile paths.
// With Mount:true + NewRoot:"", childSetup rprivate's / and mounts a fresh
// /proc in the private mount namespace.
type CgroupExecutor struct {
	MemoryMaxBytes int64
	PIDsMax        int64
}

var _ Executor = CgroupExecutor{}

// defaultImportExecutor is the production import executor: CgroupExecutor
// (light namespaces + best-effort cgroup + fetching heartbeats) with the
// job's resolved memory limit, when this host can create user namespaces.
// Without userns the sandbox re-exec cannot work at all, so degrade to the
// plain Subprocess (announced once) rather than failing every import.
func defaultImportExecutor(memMaxBytes int64) Executor {
	usernsProbe.Do(func() {
		usernsOK = usernsAvailableFn()
		if !usernsOK {
			fmt.Fprintln(os.Stderr, "jobs: user namespaces unavailable; running imports without cgroup confinement")
		}
	})
	if !usernsOK {
		return Subprocess{}
	}
	return CgroupExecutor{MemoryMaxBytes: memMaxBytes}
}

var (
	usernsProbe       sync.Once
	usernsOK          bool
	usernsAvailableFn = sandbox.UserNSAvailable // test seam
)

func (e CgroupExecutor) Run(ctx context.Context, spec ExecSpec) (ExecResult, error) {
	// Best-effort cgroup; nil means no delegation — the process runs without caps.
	cgName := fmt.Sprintf("jobs-import-%d-%d", os.Getpid(), time.Now().UnixNano())
	cg, err := sandbox.CreateCgroup(cgName, sandbox.CgroupLimits{
		MemoryMaxBytes: e.MemoryMaxBytes,
		PIDsMax:        e.PIDsMax,
	})
	if err != nil {
		return ExecResult{}, fmt.Errorf("create import cgroup: %w", err)
	}
	defer func() {
		if cg != nil {
			_ = cg.Close()
		}
	}()

	// Build env: start from os.Environ() (like Subprocess) then overlay spec.Env.
	env := os.Environ()
	for k, v := range spec.Env {
		env = append(env, k+"="+v)
	}

	tail := tailbuf.New(4 << 10)
	var stderr io.Writer = tail
	if spec.StderrSink != nil {
		stderr = io.MultiWriter(tail, spec.StderrSink)
	}

	cfg := sandbox.Config{
		// Absolute path to the fetcher entrypoint — robust regardless of Dir.
		Command: []string{filepath.Join(spec.FetcherDir, "fetch")},
		Dir:     spec.FetcherDir,
		Env:     env,
		// NewRoot:"" → no pivot_root; fetcher runs on the host filesystem.
		NewRoot: "",
		// Net:false → NO CLONE_NEWNET → host network is kept (imports need network).
		// User+Mount+PID: lightweight isolation; Mount:true + PID:true + NewRoot:""
		// causes childSetup to mount a fresh /proc in the private mount ns.
		Namespaces: sandbox.Namespaces{
			User:  true,
			Mount: true,
			PID:   true,
			Net:   false,
		},
		Cgroup: cg,
		Stdout: spec.StdoutSink, // nil = discard, as before
		Stderr: stderr,
	}

	// Liveness + cgroup usage while ./fetch runs (a large download is
	// otherwise silent); stop before the deferred cg.Close.
	stopHB := startExecHeartbeat(spec.Events, "fetching", execHeartbeatInterval, cg.Stats)
	code, runErr := sandbox.Run(ctx, cfg)
	stopHB()
	if runErr != nil {
		// Infrastructure failure: the process did not run to completion.
		return ExecResult{}, fmt.Errorf("run import sandbox: %w", runErr)
	}
	// sandbox.Run returns (exitCode, nil) for any command exit (zero or non-zero).
	return ExecResult{ExitCode: code, StderrTail: tail.String()}, nil
}
