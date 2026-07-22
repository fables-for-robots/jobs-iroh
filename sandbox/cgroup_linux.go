//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// CgroupLimits are cgroup v2 resource caps; a zero field means "no limit".
type CgroupLimits struct {
	MemoryMaxBytes  int64
	MemoryHighBytes int64
	PIDsMax         int64
	CPUQuotaUS      int64 // with CPUPeriodUS: "quota period" in cpu.max
	CPUPeriodUS     int64 // default 100000 when CPUQuotaUS>0 and this is 0
}

// cgroupLeafHolderName is the well-known child cgroup a long-running process
// (the runner) parks itself in via EnsureCgroupLeafHolder, leaving its
// original cgroup process-free so that cgroup may distribute domain
// controllers (memory) to job leaf cgroups — cgroup v2 forbids enabling a
// domain controller in the subtree_control of a cgroup with member processes
// (the no-internal-process rule). The name is a reserved sentinel: any
// cgroup with this basename is treated as a holder (delegation is managed
// one level up) — don't run jobs binaries inside an unrelated cgroup that
// happens to use it.
const cgroupLeafHolderName = "jobs-leaf-holder"

// selfCgroupRel is the current process's cgroup v2 path relative to the
// cgroupfs root ("" when the unified hierarchy line is absent).
func selfCgroupRel() string {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if rel, ok := strings.CutPrefix(line, "0::"); ok { // the unified (v2) hierarchy line
			return rel
		}
	}
	return ""
}

// delegatedDirFromRel maps a cgroup v2 rel path to the dir whose
// subtree_control we manage: the cgroup itself, or its PARENT when the
// process is parked in the leaf holder (job cgroups must be siblings of the
// holder — creating them under it would re-trip the no-internal-process rule
// one level down).
func delegatedDirFromRel(rel string) string {
	dir := filepath.Join("/sys/fs/cgroup", rel)
	if filepath.Base(dir) == cgroupLeafHolderName {
		dir = filepath.Dir(dir)
	}
	return dir
}

// canManageCgroup reports whether we may write the cgroup's subtree_control —
// the delegation probe shared by DetectCgroupDelegation and
// EnsureCgroupLeafHolder (they must agree on what counts as delegated).
func canManageCgroup(dir string) bool {
	return unix.Access(filepath.Join(dir, "cgroup.subtree_control"), unix.W_OK) == nil
}

// leafHolderMove creates the leaf-holder child of dir and moves pid into it
// (all threads move together — cgroup.procs migrates whole processes). A
// holder created here is removed again if the migration fails (an empty
// cgroup is the only thing os.Remove can delete — safe by construction).
func leafHolderMove(dir string, pid int) error {
	holder := filepath.Join(dir, cgroupLeafHolderName)
	created := false
	if err := os.Mkdir(holder, 0o755); err != nil {
		if !os.IsExist(err) {
			return fmt.Errorf("mkdir cgroup %s: %w", holder, err)
		}
	} else {
		created = true
	}
	if err := os.WriteFile(filepath.Join(holder, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0); err != nil {
		if created {
			_ = os.Remove(holder)
		}
		return fmt.Errorf("move pid %d into %s: %w", pid, holder, err)
	}
	return nil
}

// EnsureCgroupLeafHolder parks the calling process in the leaf-holder child
// of its own cgroup AND verifies the point of the exercise: that the original
// cgroup now distributes the memory controller ("+memory" is written to its
// subtree_control here, eagerly). Only the calling process is moved — never
// other members of the cgroup — so the verify write fails EBUSY when
// co-resident processes remain, and that failure is REPORTED rather than
// deferred to CreateCgroup's silent best-effort enable. Idempotent (an
// already-parked process re-verifies). Returns (false, nil) — not an error —
// when the technique doesn't apply: no cgroup v2, no writable delegated
// subtree (CreateCgroup degrades the same way), or a non-"domain" cgroup
// (threaded domains reject domain children with EOPNOTSUPP; interactive
// terminal scopes are commonly threaded).
func EnsureCgroupLeafHolder() (parked bool, err error) {
	rel := selfCgroupRel()
	if rel == "" {
		return false, nil
	}
	dir := delegatedDirFromRel(rel)
	if !canManageCgroup(dir) {
		return false, nil
	}
	if typ, terr := os.ReadFile(filepath.Join(dir, "cgroup.type")); terr == nil &&
		strings.TrimSpace(string(typ)) != "domain" {
		return false, nil
	}
	if filepath.Base(rel) != cgroupLeafHolderName {
		if err := leafHolderMove(dir, os.Getpid()); err != nil {
			return false, err
		}
	}
	ctrls, _ := os.ReadFile(filepath.Join(dir, "cgroup.controllers"))
	if !slices.Contains(strings.Fields(string(ctrls)), "memory") {
		return false, fmt.Errorf("memory controller not available in %s", dir)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.subtree_control"), []byte("+memory"), 0); err != nil {
		return false, fmt.Errorf("enable memory distribution in %s (co-resident processes?): %w", dir, err)
	}
	return true, nil
}

// DetectCgroupDelegation returns the current process's delegated cgroup v2 dir
// (one whose cgroup.subtree_control we may write) and its available controllers.
// A process parked in the leaf holder reports the holder's parent. ok=false
// when no writable delegated subtree exists (limits are then skipped).
func DetectCgroupDelegation() (dir string, controllers []string, ok bool) {
	rel := selfCgroupRel()
	if rel == "" {
		return "", nil, false
	}
	dir = delegatedDirFromRel(rel)
	if !canManageCgroup(dir) {
		return "", nil, false
	}
	ctrls, _ := os.ReadFile(filepath.Join(dir, "cgroup.controllers"))
	return dir, strings.Fields(string(ctrls)), true
}

// Cgroup is a created leaf cgroup. A nil *Cgroup is valid and a no-op (the
// best-effort "undelegated" case).
type Cgroup struct{ dir string }

// CreateCgroup makes a leaf cgroup under the delegated parent, enables the
// controllers the limits need (those that are available), writes the limits, and
// returns it. Returns (nil, nil) when no delegated subtree exists — callers treat
// a nil Cgroup as "no limits applied".
func CreateCgroup(name string, lim CgroupLimits) (*Cgroup, error) {
	parent, controllers, ok := DetectCgroupDelegation()
	if !ok {
		return nil, nil
	}
	have := map[string]bool{}
	for _, c := range controllers {
		have[c] = true
	}
	var enable []string
	if (lim.MemoryMaxBytes > 0 || lim.MemoryHighBytes > 0) && have["memory"] {
		enable = append(enable, "+memory")
	}
	if lim.PIDsMax > 0 && have["pids"] {
		enable = append(enable, "+pids")
	}
	if lim.CPUQuotaUS > 0 && have["cpu"] {
		enable = append(enable, "+cpu")
	}
	if len(enable) > 0 {
		// Best-effort: enabling an already-enabled controller is harmless.
		_ = os.WriteFile(filepath.Join(parent, "cgroup.subtree_control"), []byte(strings.Join(enable, " ")), 0)
	}
	dir := filepath.Join(parent, name)
	if err := os.Mkdir(dir, 0o755); err != nil && !os.IsExist(err) {
		return nil, fmt.Errorf("mkdir cgroup %s: %w", dir, err)
	}
	c := &Cgroup{dir: dir}
	if lim.MemoryMaxBytes > 0 && have["memory"] {
		c.write("memory.max", strconv.FormatInt(lim.MemoryMaxBytes, 10))
	}
	if lim.MemoryHighBytes > 0 && have["memory"] {
		c.write("memory.high", strconv.FormatInt(lim.MemoryHighBytes, 10))
	}
	if lim.PIDsMax > 0 && have["pids"] {
		c.write("pids.max", strconv.FormatInt(lim.PIDsMax, 10))
	}
	if lim.CPUQuotaUS > 0 && have["cpu"] {
		period := lim.CPUPeriodUS
		if period <= 0 {
			period = 100000
		}
		c.write("cpu.max", fmt.Sprintf("%d %d", lim.CPUQuotaUS, period))
	}
	return c, nil
}

func (c *Cgroup) write(f, v string) {
	_ = os.WriteFile(filepath.Join(c.dir, f), []byte(v), 0)
}

// Dir is the cgroup path ("" for a nil Cgroup).
func (c *Cgroup) Dir() string {
	if c == nil {
		return ""
	}
	return c.dir
}

// FD opens the cgroup dir for SysProcAttr.CgroupFD (born-in-cgroup at clone).
// ok=false for a nil Cgroup. The caller closes the fd after Start.
func (c *Cgroup) FD() (int, bool) {
	if c == nil {
		return -1, false
	}
	fd, err := unix.Open(c.dir, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, false
	}
	return fd, true
}

// Stats reads the cgroup's current usage (see CgroupStats). ok=false for a
// nil Cgroup (the undelegated best-effort case); otherwise best-effort
// per-field, -1 marking an unreadable one. Valid until the cgroup dir is
// removed (Close) — accounting persists after the member processes exit, so a
// read between process end and Close still reports the settled totals.
func (c *Cgroup) Stats() (CgroupStats, bool) {
	if c == nil {
		return CgroupStats{}, false
	}
	return readCgroupStats(c.dir), true
}

// readCgroupStats parses cpu.stat (usage_usec), memory.current, and
// memory.peak from a cgroup v2 dir; -1 per field when missing or malformed.
func readCgroupStats(dir string) CgroupStats {
	st := CgroupStats{CPUUsec: -1, MemoryBytes: -1, MemoryPeakBytes: -1}
	if b, err := os.ReadFile(filepath.Join(dir, "cpu.stat")); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if rest, ok := strings.CutPrefix(line, "usage_usec "); ok {
				if v, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64); err == nil {
					st.CPUUsec = v
				}
				break
			}
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "memory.current")); err == nil {
		if v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil {
			st.MemoryBytes = v
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "memory.peak")); err == nil {
		if v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil {
			st.MemoryPeakBytes = v
		}
	}
	return st
}

// Add moves an already-running pid into the cgroup (fallback when CgroupFD
// isn't used). No-op for a nil Cgroup.
func (c *Cgroup) Add(pid int) error {
	if c == nil {
		return nil
	}
	return os.WriteFile(filepath.Join(c.dir, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0)
}

// Close removes the cgroup; succeeds only once it has no member processes.
func (c *Cgroup) Close() error {
	if c == nil {
		return nil
	}
	return os.Remove(c.dir)
}
