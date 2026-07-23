//go:build linux

package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Init runs the in-namespace child logic when this process was re-exec'd by Run,
// then execs the target command and never returns. In a normal (non-child)
// process it returns immediately. It MUST be called first in main() and in the
// test TestMain, before any flag parsing, because the re-exec'd child reaches
// Init via /proc/self/exe with argv[0] == the sentinel.
func Init() {
	v := os.Getenv(childEnv)
	if v == "" {
		return
	}
	if v == "probe" { // UserNSAvailable probe child: just confirm we got here.
		os.Exit(0)
	}
	if strings.HasPrefix(v, "@") {
		// Oversized config spilled to a file by Run (E2BIG guard). Read it
		// back before childSetup changes any mounts.
		b, err := os.ReadFile(v[1:])
		if err != nil {
			fmt.Fprintln(os.Stderr, "sandbox child: read spilled config:", err)
			os.Exit(127)
		}
		v = string(b)
	}
	var cfg Config
	if err := json.Unmarshal([]byte(v), &cfg); err != nil {
		fmt.Fprintln(os.Stderr, "sandbox child: bad config:", err)
		os.Exit(127)
	}
	if err := childSetup(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "sandbox child setup:", err)
		os.Exit(126)
	}
	if len(cfg.Command) == 0 {
		fmt.Fprintln(os.Stderr, "sandbox child: empty command")
		os.Exit(127)
	}
	// execve can return EINTR when a signal interrupts the kernel mid-load — in
	// particular the Go runtime's SIGURG async-preemption, which fires more often
	// the busier the process is, while the kernel is faulting in the binary's
	// pages from the amberfuse mount (whose open/read is interruptible). execve
	// has no side effects on failure, so retry until it takes rather than dying
	// with a spurious "interrupted system call".
	//
	// It can also return ETXTBSY: a concurrently forked child of the parent
	// runner (another job slot, between its clone and execve) can still hold
	// the write fd of the just-extracted target binary (golang/go#22315). That
	// writer is always moribund, so retry briefly — bounded, unlike EINTR —
	// rather than failing the job.
	etxtbsyDelay := 5 * time.Millisecond
	etxtbsyLeft := 10
	for {
		err := unix.Exec(cfg.Command[0], cfg.Command, cfg.Env)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.ETXTBSY) && etxtbsyLeft > 0 {
			etxtbsyLeft--
			time.Sleep(etxtbsyDelay)
			if etxtbsyDelay < 200*time.Millisecond {
				etxtbsyDelay *= 2
			}
			continue
		}
		fmt.Fprintln(os.Stderr, "sandbox child exec:", err)
		os.Exit(127)
	}
}

// childSetup performs the in-namespace mount/pivot setup. Critical ordering on
// this kernel: a fresh procfs must be mounted into NewRoot/proc *before*
// pivot_root, because once the old root (and its procfs) is detached the kernel
// refuses to mount a new procfs in a user namespace (EPERM). So the order is:
// make-rprivate -> tmpfs/bind NewRoot mountpoint -> apply Mounts -> fresh /proc
// into NewRoot -> pivot_root -> detach old root -> chdir.
func childSetup(cfg Config) error {
	// Stop mount propagation to the host (we are in a new mount ns).
	if cfg.Namespaces.Mount {
		if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
			return fmt.Errorf("make-rprivate /: %w", err)
		}
	}
	if cfg.NewRoot != "" {
		// new_root must be a mount point. Bind it onto itself so pivot_root
		// accepts it (works for both an empty dir and a pre-populated one).
		if err := unix.Mount(cfg.NewRoot, cfg.NewRoot, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
			return fmt.Errorf("bind new root: %w", err)
		}
	}
	for _, m := range cfg.Mounts {
		if err := applyMount(cfg.NewRoot, m); err != nil {
			return err
		}
	}
	// Fresh /proc for the PID namespace. Mount it BEFORE pivot (see func doc).
	// When NewRoot is set, mount into NewRoot/proc; otherwise mount at /proc.
	if cfg.Namespaces.PID {
		procTarget := "/proc"
		if cfg.NewRoot != "" {
			procTarget = filepath.Join(cfg.NewRoot, "proc")
		}
		if err := os.MkdirAll(procTarget, 0o555); err != nil {
			return fmt.Errorf("mkdir %s: %w", procTarget, err)
		}
		if err := unix.Mount("proc", procTarget, "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
			return fmt.Errorf("mount %s: %w", procTarget, err)
		}
	}
	if cfg.NewRoot != "" {
		if err := pivot(cfg.NewRoot); err != nil {
			return err
		}
	}
	if cfg.Dir != "" {
		if err := os.Chdir(cfg.Dir); err != nil {
			return fmt.Errorf("chdir %s: %w", cfg.Dir, err)
		}
	}
	return nil
}

// applyMount sets up a single mount under newRoot. For binds the read-only flag
// requires a second MS_REMOUNT pass (the kernel ignores MS_RDONLY on the initial
// bind). Mount targets are created in newRoot first: a bind whose source is a
// file (e.g. /dev/null) gets a file target, otherwise a directory target — so
// individual device nodes can be bound, not just whole directories.
func applyMount(newRoot string, m Mount) error {
	target := filepath.Join(newRoot, m.Target)
	if m.FSType == "" { // bind
		fi, err := os.Stat(m.Source)
		if err != nil {
			return fmt.Errorf("stat bind source %s: %w", m.Source, err)
		}
		if fi.IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("mkdir mount target %s: %w", target, err)
			}
		} else {
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("mkdir mount target parent %s: %w", filepath.Dir(target), err)
			}
			f, err := os.OpenFile(target, os.O_CREATE, 0o644)
			if err != nil {
				return fmt.Errorf("create mount target %s: %w", target, err)
			}
			f.Close()
		}
		if err := unix.Mount(m.Source, target, "", unix.MS_BIND|unix.MS_REC|m.Flags, ""); err != nil {
			return fmt.Errorf("bind %s->%s: %w", m.Source, target, err)
		}
		// Per-mount-point flags (ro, strictatime) require a second
		// MS_REMOUNT|MS_BIND pass — the kernel ignores them on the initial
		// bind. That pass must REPEAT the flags the mount already carries: in
		// a user namespace every flag inherited from the parent namespace is
		// locked (can_change_locked_flags), and a remount that omits a locked
		// flag — or the mount's exact atime mode — asks the kernel to clear
		// it and fails with EPERM. The bind inherited these flags from its
		// source, so repeating them changes nothing; omitting them broke
		// remount-ro whenever the bind source sat on a nosuid/nodev/noatime
		// mount (e.g. TMPDIR on a hardened or nix-shell /tmp).
		if m.ReadOnly || m.Strictatime {
			inherited, err := inheritedMountFlags(target)
			if err != nil {
				return fmt.Errorf("statfs %s: %w", target, err)
			}
			if m.ReadOnly {
				flags := unix.MS_REMOUNT | unix.MS_BIND | unix.MS_RDONLY | unix.MS_REC | m.Flags | inherited
				if err := unix.Mount("", target, "", flags, ""); err != nil {
					return fmt.Errorf("remount-ro %s: %w", target, err)
				}
			}
			if m.Strictatime {
				// Strictatime IS a requested atime change, so the inherited
				// atime mode is stripped, not repeated.
				const atimeMask = unix.MS_NOATIME | unix.MS_NODIRATIME | unix.MS_RELATIME | unix.MS_STRICTATIME
				flags := unix.MS_REMOUNT | unix.MS_BIND | unix.MS_REC | unix.MS_STRICTATIME | m.Flags | (inherited &^ atimeMask)
				if m.ReadOnly {
					flags |= unix.MS_RDONLY
				}
				// Best-effort: pre-aged epoch atimes already refresh under default
				// relatime; strictatime only hardens the noatime-host edge, and a
				// locked atime mode inside the userns must not fail the build.
				_ = unix.Mount("", target, "", flags, "")
			}
		}
		return nil
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return fmt.Errorf("mkdir mount target %s: %w", target, err)
	}
	flags := m.Flags
	if m.ReadOnly {
		flags |= unix.MS_RDONLY
	}
	if err := unix.Mount(m.Source, target, m.FSType, flags, ""); err != nil {
		return fmt.Errorf("mount %s %s: %w", m.FSType, target, err)
	}
	return nil
}

// inheritedMountFlags translates target's current per-mount flags (statfs
// f_flags) into their MS_* equivalents so a MS_REMOUNT|MS_BIND pass can
// repeat them (see applyMount — a user namespace may never clear a locked
// flag). A mount carrying no atime bit at all is strictatime, and that too
// must be explicit: an atime-flag-less remount requests the kernel's
// relatime DEFAULT, which mismatches a locked strictatime mode.
func inheritedMountFlags(target string) (uintptr, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(target, &st); err != nil {
		return 0, err
	}
	fl := uintptr(0)
	for _, f := range [...]struct{ st, ms uintptr }{
		{unix.ST_RDONLY, unix.MS_RDONLY},
		{unix.ST_NOSUID, unix.MS_NOSUID},
		{unix.ST_NODEV, unix.MS_NODEV},
		{unix.ST_NOEXEC, unix.MS_NOEXEC},
		{unix.ST_NOATIME, unix.MS_NOATIME},
		{unix.ST_NODIRATIME, unix.MS_NODIRATIME},
		{unix.ST_RELATIME, unix.MS_RELATIME},
	} {
		if uintptr(st.Flags)&f.st != 0 {
			fl |= f.ms
		}
	}
	if fl&(unix.MS_NOATIME|unix.MS_RELATIME) == 0 {
		fl |= unix.MS_STRICTATIME
	}
	return fl, nil
}

// pivot performs the pivot_root dance: chdir into newRoot, pivot_root(".","."),
// detach the old root (now overmounted at "."), and chdir to the new "/".
func pivot(newRoot string) error {
	if err := os.Chdir(newRoot); err != nil {
		return fmt.Errorf("chdir new root %s: %w", newRoot, err)
	}
	if err := unix.PivotRoot(".", "."); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := unix.Unmount(".", unix.MNT_DETACH); err != nil {
		return fmt.Errorf("detach oldroot: %w", err)
	}
	return os.Chdir("/")
}
