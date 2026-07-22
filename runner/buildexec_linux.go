//go:build linux

package runner

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/fables-for-robots/jobs-iroh/amber"
	"github.com/fables-for-robots/jobs-iroh/sandbox"
	"github.com/fables-for-robots/jobs-iroh/tailbuf"
)

// NamespaceBuildExecutor runs a hermetic build in a rootless namespace sandbox.
//
// It assembles a new root on the host, stages the assembled content-addressed
// store (deps + shell) to disk and binds it read-only at /jobs/store, extracts
// the source subtree writable as $SRC, creates a writable $out, then re-execs
// into User+Mount+PID+Net+UTS+IPC namespaces (Net ⇒ net=none) with a
// best-effort cgroup, pivot_roots into the new root, and runs the recipe script
// under the imported bash. All staging happens in THIS (parent) process before
// sandbox.Run; the re-exec'd child inherits the pre-established mounts.
type NamespaceBuildExecutor struct{}

var _ BuildExecutor = NamespaceBuildExecutor{}

// assembled is the result of assembleSandbox: a ready-to-run sandbox.Config plus
// the resources the caller must release (cgroup, work tree).
type assembled struct {
	cfg    sandbox.Config
	cg     *sandbox.Cgroup
	work   string
	outDir string
}

// assembleSandbox builds the hermetic build sandbox for spec and command: it
// creates the work tree, stages the store read-only, extracts $SRC, makes a
// writable $out, creates a best-effort cgroup, and returns the assembled
// sandbox.Config (shared base mounts + extraMounts + the store bind, build env,
// namespaces, cgroup, NewRoot). The caller sets cfg.Stdout/Stderr/Stdin or
// cfg.Tty, runs it, and then closes a.cg and removes a.work. On any error here
// the cgroup is closed and the work tree removed.
func assembleSandbox(ctx context.Context, st *amber.Store, spec BuildSpec, command []string, extraMounts []sandbox.Mount) (a assembled, err error) {
	a.work, err = os.MkdirTemp("", "jobs-build-")
	if err != nil {
		return a, fmt.Errorf("create build work dir: %w", err)
	}
	newRoot := filepath.Join(a.work, "root")

	ok := false
	defer func() {
		if !ok {
			if a.cg != nil {
				_ = a.cg.Close()
			}
			_ = os.RemoveAll(a.work)
		}
	}()

	// Provision the assembled content-addressed store (deps + shell, each at
	// /jobs/store/<BOK>, build.md §6) read-only: materialized to disk under the
	// work tree and bound in read-only (jobs-iroh is materialize-only).
	spec.Events.Phase("materializing")
	// Liveness ticks while the store + source extract — a big store tree is
	// otherwise event-silent until "building" (issue #101). Covers the whole
	// assembly; the executor's "building" heartbeat takes over after return.
	defer startPhaseHeartbeat(spec.Events, "materializing", execHeartbeatInterval)()
	storeBinds, err := provisionStore(ctx, st, spec.StoreKey, newRoot, a.work)
	if err != nil {
		return a, err
	}

	srcDir := filepath.Join(newRoot, sandboxSrcDir)
	if err = os.MkdirAll(srcDir, 0o755); err != nil {
		return a, fmt.Errorf("mkdir $SRC: %w", err)
	}
	rc, terr := st.Tar(ctx, spec.SourceKey, spec.SourceDir)
	if terr != nil {
		return a, fmt.Errorf("tar source %s/%s: %w", spec.SourceKey, spec.SourceDir, terr)
	}
	if err = extractTar(rc, srcDir); err != nil {
		rc.Close()
		return a, fmt.Errorf("extract source: %w", err)
	}
	rc.Close()

	a.outDir = filepath.Join(newRoot, sandboxOutDir)
	if err = os.MkdirAll(a.outDir, 0o755); err != nil {
		return a, fmt.Errorf("mkdir $out: %w", err)
	}

	// The full deps map + the build script, file-carried (see the consts —
	// both can exceed MAX_ARG_STRLEN on many-input builds).
	if err = os.WriteFile(filepath.Join(newRoot, sandboxJobsDepsFile), []byte(encodeJobsDeps(spec.JobsDeps)), 0o644); err != nil {
		return a, fmt.Errorf("write jobs-deps file: %w", err)
	}
	if err = os.WriteFile(filepath.Join(newRoot, sandboxScriptFile), []byte(spec.Script), 0o755); err != nil {
		return a, fmt.Errorf("write script file: %w", err)
	}

	cgName := fmt.Sprintf("jobs-build-%d-%d", os.Getpid(), time.Now().UnixNano())
	a.cg, err = sandbox.CreateCgroup(cgName, sandbox.CgroupLimits{
		MemoryMaxBytes: spec.MemoryMaxBytes,
		PIDsMax:        spec.PIDsMax,
	})
	if err != nil {
		return a, fmt.Errorf("create cgroup: %w", err)
	}

	// A minimal, hermetic /dev (standard pseudo-device nodes + std fd symlinks) so
	// build tools that require /dev/null etc. — bash redirects, the Go toolchain's
	// telemetry/buildID, CGO-free Go reading /dev/urandom — work without exposing
	// the host's real devices.
	devMounts, err := hermeticDevMounts(newRoot)
	if err != nil {
		return a, err
	}

	// Minimal /etc/hosts + empty resolv.conf so resolver paths fail fast
	// instead of hanging forever under net=none (issue #99).
	if err = writeHermeticEtc(newRoot); err != nil {
		return a, err
	}

	// Persistent build caches: each seeded host dir is rw-bound at its declared
	// path; strictatime records access for the post-build prune (design §5.2).
	cacheMounts := make([]sandbox.Mount, 0, len(spec.Caches))
	for _, cm := range spec.Caches {
		cacheMounts = append(cacheMounts, sandbox.Mount{
			Source: cm.HostDir, Target: cm.Path, Strictatime: true,
		})
	}

	a.cfg = sandbox.Config{
		Command: command,
		Env:     buildEnv(spec),
		NewRoot: newRoot,
		Mounts: append(append(append(append([]sandbox.Mount{
			// The vendored shell is a fully STATIC userland (build.md §8, §14), so
			// no /nix/store bind is needed — the sandbox is hermetic.
			// Writable scratch /tmp (the rest of the root is staged read-only).
			{Source: "tmpfs", Target: "/tmp", FSType: "tmpfs"},
		}, devMounts...), cacheMounts...), extraMounts...), storeBinds...),
		Namespaces: sandbox.Namespaces{
			User: true, Mount: true, PID: true, Net: true, UTS: true, IPC: true,
		},
		Cgroup: a.cg,
	}
	ok = true
	return a, nil
}

// hermeticDevMounts provisions a minimal /dev for the hermetic build sandbox.
// The standard stateless pseudo-device nodes are bind-mounted individually from
// the host (they are identical on every system and leak no host state — unlike a
// full host /dev bind, which would expose real disks and break hermeticity), and
// /dev/fd + /dev/std{in,out,err} are symlinked to /proc/self/fd. Without this a
// build has no /dev/null, so bash redirects and the Go toolchain fail. Mirrors
// what the nix build sandbox provides. newRoot must already exist on disk.
func hermeticDevMounts(newRoot string) ([]sandbox.Mount, error) {
	devDir := filepath.Join(newRoot, "dev")
	if err := os.MkdirAll(devDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir /dev: %w", err)
	}
	for link, target := range map[string]string{
		"fd":     "/proc/self/fd",
		"stdin":  "/proc/self/fd/0",
		"stdout": "/proc/self/fd/1",
		"stderr": "/proc/self/fd/2",
	} {
		if err := os.Symlink(target, filepath.Join(devDir, link)); err != nil && !os.IsExist(err) {
			return nil, fmt.Errorf("symlink /dev/%s: %w", link, err)
		}
	}
	var mounts []sandbox.Mount
	for _, dev := range []string{"null", "zero", "full", "random", "urandom"} {
		src := "/dev/" + dev
		if _, err := os.Stat(src); err != nil {
			continue // host lacks it (not expected on Linux) — skip rather than fail the build
		}
		mounts = append(mounts, sandbox.Mount{Source: src, Target: "/dev/" + dev})
	}
	return mounts, nil
}

func (NamespaceBuildExecutor) RunBuild(ctx context.Context, st *amber.Store, spec BuildSpec) (BuildResult, error) {
	a, err := assembleSandbox(ctx, st, spec,
		[]string{storeShellDir(spec.ShellBOK) + "/bin/bash", "-e", sandboxScriptFile}, nil)
	if err != nil {
		return BuildResult{ExitCode: -1}, err
	}
	keepWork := false
	defer func() {
		if a.cg != nil {
			_ = a.cg.Close()
		}
		if !keepWork {
			_ = os.RemoveAll(a.work)
		}
	}()

	tail := tailbuf.New(4 << 10)
	var stdout io.Writer = os.Stderr // build stdout is diagnostic; tee to our stderr stream
	if spec.StdoutSink != nil {
		stdout = io.MultiWriter(os.Stderr, spec.StdoutSink)
	}
	var stderr io.Writer = tail
	if spec.StderrSink != nil {
		stderr = io.MultiWriter(tail, spec.StderrSink)
	}
	a.cfg.Stdout = stdout
	a.cfg.Stderr = stderr

	spec.Events.Phase("building")
	// Liveness + cgroup usage while the script runs; stop before the deferred
	// cg.Close (accounting persists until rmdir, so the final settled read is
	// valid after the process exits).
	stopHB := startExecHeartbeat(spec.Events, "building", execHeartbeatInterval, a.cg.Stats)
	code, runErr := sandbox.Run(ctx, a.cfg)
	stopHB()
	if runErr != nil {
		return BuildResult{ExitCode: code, StderrTail: tail.String()}, fmt.Errorf("run build sandbox: %w", runErr)
	}
	keepWork = true
	return BuildResult{ExitCode: code, StderrTail: tail.String(), OutDir: a.outDir}, nil
}

// buildEnv merges spec.Env over the sandbox-provided variables. spec.Env is
// applied first; the structural defaults (SRC/out/PATH/HOME/JOBS_DEPS) then
// overwrite, so the build always sees the real sandbox layout, never a stale
// spec value.
func buildEnv(spec BuildSpec) []string {
	shellDir := storeShellDir(spec.ShellBOK)
	merged := map[string]string{}
	for k, v := range spec.Env {
		merged[k] = v
	}
	// Structural variables are authoritative — the recipe's $SRC/$out/PATH must
	// point at the sandbox layout, never at a stale spec value.
	merged["SRC"] = sandboxSrcDir
	merged["out"] = sandboxOutDir
	merged["HOME"] = sandboxHomeDir
	merged["PATH"] = shellDir + "/bin"
	// The vendored userland is statically linked — no LD_LIBRARY_PATH needed.
	// JOBS_DEPS maps each input's recipe name to its /jobs/store/<BOK> path; the
	// build script resolves dependency paths from it (build.md §3.1). The full
	// map always rides in JOBS_DEPS_FILE (written by assembleSandbox); the
	// inline env var is dropped for big maps (E2BIG — see sandboxJobsDepsFile).
	if deps := encodeJobsDeps(spec.JobsDeps); len(deps) <= maxInlineJobsDeps {
		merged["JOBS_DEPS"] = deps
	}
	merged["JOBS_DEPS_FILE"] = sandboxJobsDepsFile

	out := make([]string, 0, len(merged))
	for k, v := range merged {
		out = append(out, k+"="+v)
	}
	return out
}
