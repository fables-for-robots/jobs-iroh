//go:build linux

package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// childEnv carries the JSON-marshalled Config to the re-exec'd child. The
// sentinel value "probe" marks a UserNSAvailable probe child.
const childEnv = "_JOBS_SANDBOX_CHILD"

// maxInlineConfig is the largest marshaled Config passed inline in childEnv;
// anything bigger is spilled to a temp file (kernel MAX_ARG_STRLEN is ~128KiB
// per env string — stay well under it, os.Environ rides along too).
const maxInlineConfig = 64 << 10

// ErrUnsupported is returned by Run on platforms without the rootless jail.
var ErrUnsupported = errors.New("sandbox: unsupported on this platform")

// Mount is a mount to set up inside the new root. FSType=="" means a bind mount
// of Source onto Target; otherwise FSType is a filesystem type ("tmpfs",
// "proc", ...). Target is relative to NewRoot (a leading slash is fine).
// Strictatime (binds only) remounts with MS_STRICTATIME so file access is
// recorded even on relatime/noatime hosts — best-effort (see applyMount).
type Mount struct {
	Source      string
	Target      string
	FSType      string
	ReadOnly    bool
	Strictatime bool
	Flags       uintptr
}

// Namespaces selects which namespaces the child is cloned into. User must be set
// for the rootless case (it grants capabilities over the others).
type Namespaces struct{ User, Mount, PID, Net, UTS, IPC bool }

// Config fully describes one sandboxed command invocation. The Cgroup and the
// std streams are parent-side wiring (the cgroup is applied at clone via
// CgroupFD; the streams are set on the child exec.Cmd) and are not part of the
// JSON the child receives — hence `json:"-"`.
type Config struct {
	Command    []string
	Env        []string
	Dir        string // working dir inside the new root ("" => "/")
	NewRoot    string // if non-empty, pivot_root into it
	Mounts     []Mount
	Namespaces Namespaces
	Cgroup     *Cgroup   `json:"-"`
	Stdout     io.Writer `json:"-"`
	Stderr     io.Writer `json:"-"`
	Stdin      io.Reader `json:"-"`
	// Tty, when non-nil, is the slave end of a PTY. The child uses it as
	// stdin/stdout/stderr and makes it its controlling terminal (Setsid +
	// Setctty), so an interactive shell gets a real terminal. It overrides
	// Stdout/Stderr/Stdin. Parent-side wiring → not part of the child JSON.
	Tty *os.File `json:"-"`
}

func cloneflags(ns Namespaces) uintptr {
	var f uintptr
	if ns.User {
		f |= unix.CLONE_NEWUSER
	}
	if ns.Mount {
		f |= unix.CLONE_NEWNS
	}
	if ns.PID {
		f |= unix.CLONE_NEWPID
	}
	if ns.Net {
		f |= unix.CLONE_NEWNET
	}
	if ns.UTS {
		f |= unix.CLONE_NEWUTS
	}
	if ns.IPC {
		f |= unix.CLONE_NEWIPC
	}
	return f
}

// Run re-execs /proc/self/exe into the configured namespaces; the child (Init)
// applies the mounts, pivot_roots into NewRoot, mounts a fresh /proc, and execs
// cfg.Command. The parent waits and returns the command's exit code. err is
// non-nil only for setup/infra failure; a non-zero command exit is (code, nil).
//
// Cgroup placement is best-effort. When a cgroup is supplied, Run first tries to
// have the child born directly into it (clone3 CLONE_INTO_CGROUP via
// SysProcAttr.UseCgroupFD). On kernels/cgroup-layouts where that clone is
// rejected (e.g. a "domain threaded" delegated scope, or no clone3-into-cgroup
// support), Run falls back to starting the child without the cgroup FD and then
// moving it in via cgroup.procs (Cgroup.Add) — and if even that move fails, the
// build still runs, just without the resource caps. This matches the cgroup v2
// "best-effort" contract (limits where the kernel allows, never a hard failure).
func Run(ctx context.Context, cfg Config) (int, error) {
	payload, err := json.Marshal(cfg)
	if err != nil {
		return -1, fmt.Errorf("marshal sandbox config: %w", err)
	}
	// A config with many store mounts / a big env can exceed the kernel's
	// MAX_ARG_STRLEN (~128KiB) cap on a single env string, failing the re-exec
	// with E2BIG. Spill such payloads to a temp file and pass an "@path"
	// reference instead; Init reads it back before any mount changes. The file
	// outlives the child's read because Run waits on the child below.
	payloadStr := string(payload)
	if len(payloadStr) > maxInlineConfig {
		f, err := os.CreateTemp("", "jobs-sandbox-cfg-*.json")
		if err != nil {
			return -1, fmt.Errorf("spill sandbox config: %w", err)
		}
		defer os.Remove(f.Name())
		if _, err := f.Write(payload); err != nil {
			f.Close()
			return -1, fmt.Errorf("spill sandbox config: %w", err)
		}
		if err := f.Close(); err != nil {
			return -1, fmt.Errorf("spill sandbox config: %w", err)
		}
		payloadStr = "@" + f.Name()
	}
	env := append(os.Environ(), childEnv+"="+payloadStr)

	// First attempt: born-in-cgroup at clone (UseCgroupFD) when a cgroup exists.
	cmd, closeFD := buildChildCmd(ctx, cfg, env, true)
	startErr := cmd.Start()
	if closeFD != nil {
		closeFD()
	}
	if startErr != nil {
		// If the cgroup clone-into is what failed, retry without it (best-effort).
		if _, hasCgroup := cfg.Cgroup.FD(); hasCgroup && isCgroupCloneUnsupported(startErr) {
			cmd, _ = buildChildCmd(ctx, cfg, env, false)
			if err := cmd.Start(); err != nil {
				return -1, fmt.Errorf("start sandbox child (no cgroup fd): %w", err)
			}
			// Best-effort: move the started child into the cgroup. A failure here
			// (e.g. threaded-domain scope) leaves it running without caps.
			_ = cfg.Cgroup.Add(cmd.Process.Pid)
		} else {
			return -1, fmt.Errorf("start sandbox child: %w", startErr)
		}
	}
	werr := cmd.Wait()
	if werr == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(werr, &ee) {
		return ee.ExitCode(), nil
	}
	return -1, werr
}

// buildChildCmd constructs the re-exec command and its SysProcAttr. When
// withCgroupFD is true and cfg.Cgroup has an fd, the child is set to be born in
// that cgroup; the returned closeFD must be called after Start to release the fd.
func buildChildCmd(ctx context.Context, cfg Config, env []string, withCgroupFD bool) (cmd *exec.Cmd, closeFD func()) {
	cmd = exec.CommandContext(ctx, "/proc/self/exe")
	// argv[0] is the sentinel; the child never reaches Go test-flag parsing
	// because Init() execs before m.Run().
	cmd.Args = []string{"jobs-sandbox-child"}
	cmd.Env = env
	cmd.Stdout, cmd.Stderr, cmd.Stdin = cfg.Stdout, cfg.Stderr, cfg.Stdin
	if cfg.Tty != nil {
		// An interactive shell wants a controlling terminal: route all three std
		// streams to the PTY slave and make it the controlling tty (Setctty uses
		// fd 0 by default, which is the slave because Stdin is set to it).
		cmd.Stdin, cmd.Stdout, cmd.Stderr = cfg.Tty, cfg.Tty, cfg.Tty
	}

	sp := &syscall.SysProcAttr{
		Cloneflags:                 cloneflags(cfg.Namespaces),
		Pdeathsig:                  syscall.SIGKILL,
		GidMappingsEnableSetgroups: false,
	}
	if cfg.Namespaces.User {
		sp.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}}
		sp.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}
	}
	if withCgroupFD {
		if fd, ok := cfg.Cgroup.FD(); ok {
			sp.UseCgroupFD = true
			sp.CgroupFD = fd
			closeFD = func() { unix.Close(fd) }
		}
	}
	if cfg.Tty != nil {
		sp.Setsid = true
		sp.Setctty = true
	}
	cmd.SysProcAttr = sp
	return cmd, closeFD
}

// isCgroupCloneUnsupported reports whether a cmd.Start failure looks like the
// clone3 CLONE_INTO_CGROUP request being unsupported/rejected, as opposed to a
// genuine inability to fork-exec. These errnos are what the kernel returns when
// the target cgroup is an invalid clone destination (no clone3-into-cgroup, or a
// threaded/internal-process layout): EOPNOTSUPP/ENOTSUP, EINVAL, EBUSY, EPERM.
func isCgroupCloneUnsupported(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	switch errno {
	case unix.EOPNOTSUPP, unix.EINVAL, unix.EBUSY, unix.EPERM:
		// (EOPNOTSUPP == ENOTSUP on Linux.)
		return true
	default:
		return false
	}
}

// UserNSAvailable reports whether an unprivileged user namespace can be created
// in this environment. It forks a probe child that only unshares CLONE_NEWUSER
// (with the same uid/gid maps Run uses) and reports whether it exited 0.
func UserNSAvailable() bool {
	cmd := exec.Command("/proc/self/exe")
	cmd.Args = []string{"jobs-sandbox-probe"}
	cmd.Env = append(os.Environ(), childEnv+"=probe")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 unix.CLONE_NEWUSER,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		GidMappingsEnableSetgroups: false,
	}
	return cmd.Run() == nil
}
