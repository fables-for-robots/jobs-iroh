//go:build linux

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestDelegatedDirFromRel: the /proc/self/cgroup v2 rel path maps to the
// cgroup dir whose subtree_control we manage — the cgroup itself, or its
// PARENT when the process has parked itself in the leaf-holder child (job
// cgroups must be siblings of the holder, not children of it, or the
// no-internal-process rule bites again one level down).
func TestDelegatedDirFromRel(t *testing.T) {
	for _, tc := range []struct{ rel, want string }{
		{"/foo/bar", "/sys/fs/cgroup/foo/bar"},
		{"/foo/bar/" + cgroupLeafHolderName, "/sys/fs/cgroup/foo/bar"},
		{"/", "/sys/fs/cgroup"},
		{"/" + cgroupLeafHolderName, "/sys/fs/cgroup"},
	} {
		if got := delegatedDirFromRel(tc.rel); got != tc.want {
			t.Errorf("delegatedDirFromRel(%q) = %q, want %q", tc.rel, got, tc.want)
		}
	}
}

// TestEnsureCgroupLeafHolderNonDomainScope: on a non-"domain" cgroup
// (interactive terminal scopes are commonly "domain threaded", where a
// domain child would be created "domain invalid" and reject migration with
// EOPNOTSUPP) the parker declines quietly — (false, nil) — and leaves no
// stray holder behind. Runs natively in such scopes; skips elsewhere.
func TestEnsureCgroupLeafHolderNonDomainScope(t *testing.T) {
	rel := selfCgroupRel()
	if rel == "" {
		t.Skip("no cgroup v2")
	}
	dir := delegatedDirFromRel(rel)
	if !canManageCgroup(dir) {
		t.Skip("no delegated cgroup v2 subtree")
	}
	typ, err := os.ReadFile(filepath.Join(dir, "cgroup.type"))
	if err != nil || strings.TrimSpace(string(typ)) == "domain" {
		t.Skip("current cgroup is a plain domain cgroup (or type unreadable); nothing to demonstrate")
	}

	holder := filepath.Join(dir, cgroupLeafHolderName)
	_ = os.Remove(holder) // clear a stray from earlier builds of this code
	hadHolder := false
	if _, err := os.Stat(holder); err == nil {
		hadHolder = true // non-empty leftover we can't remove; don't assert absence below
	}

	parked, err := EnsureCgroupLeafHolder()
	if err != nil || parked {
		t.Fatalf("EnsureCgroupLeafHolder on %q scope: parked=%v err=%v, want false/nil", strings.TrimSpace(string(typ)), parked, err)
	}
	if !hadHolder {
		if _, err := os.Stat(holder); !os.IsNotExist(err) {
			t.Fatalf("stray holder cgroup left behind at %s", holder)
		}
	}
}

// The two tests below PERMANENTLY MUTATE the surrounding delegated scope:
// they park every process of the scope (including the go test tooling) into
// the leaf holder and enable +memory in the scope's subtree_control, which
// then refuses direct process attachment (the very rule under test). They
// therefore require an explicit opt-in AND a dedicated scope:
//
//	systemd-run --user --scope -p Delegate=yes \
//	  --setenv=JOBS_TEST_CGROUP_LEAFHOLDER=1 -- go test ./sandbox/ -count=1
const leafHolderTestEnv = "JOBS_TEST_CGROUP_LEAFHOLDER"

// parkTestScope opts in, checks the environment can host the demonstration
// (delegated + memory controller + plain domain), and empties the scope of
// direct processes: parks the test binary via the real API, sweeps wrapper
// processes (go test, the devShell) after it, then verifies via
// EnsureCgroupLeafHolder's parked=true — which asserts +memory distribution
// succeeded. Returns the scope dir.
func parkTestScope(t *testing.T) string {
	t.Helper()
	if os.Getenv(leafHolderTestEnv) != "1" {
		t.Skipf("set %s=1 (in a DEDICATED delegated scope — this test mutates it) to run", leafHolderTestEnv)
	}
	parent, ctrls, ok := DetectCgroupDelegation()
	if !ok {
		t.Skip("no delegated cgroup v2 subtree")
	}
	if !slices.Contains(ctrls, "memory") {
		t.Skip("memory controller not delegated")
	}
	skipUnlessDomainCgroup(t, parent)

	if _, err := EnsureCgroupLeafHolder(); err != nil {
		// Expected with wrapper processes still in the scope (EBUSY on the
		// verify write): self is parked now; sweep the rest and re-verify.
		t.Logf("first park reported: %v (sweeping co-resident processes)", err)
	}
	parkAllScopeProcs(t, parent)
	parked, err := EnsureCgroupLeafHolder()
	if err != nil || !parked {
		t.Skipf("cannot empty and verify scope %s: parked=%v err=%v", parent, parked, err)
	}
	return parent
}

// TestEnsureCgroupLeafHolder: parked=true is VERIFIED success — the process
// sits in the leaf holder and the scope's subtree_control now distributes
// the memory controller. Idempotent, and DetectCgroupDelegation still
// reports the scope (the holder's parent), so job cgroups become siblings
// of the holder.
func TestEnsureCgroupLeafHolder(t *testing.T) {
	before, _, ok := DetectCgroupDelegation()
	if !ok {
		t.Skip("no delegated cgroup v2 subtree")
	}
	scope := parkTestScope(t)

	rel := selfCgroupRel()
	if filepath.Base(rel) != cgroupLeafHolderName {
		t.Fatalf("process not in leaf holder: rel = %q", rel)
	}
	st, err := os.ReadFile(filepath.Join(scope, "cgroup.subtree_control"))
	if err != nil || !slices.Contains(strings.Fields(string(st)), "memory") {
		t.Fatalf("scope subtree_control does not distribute memory (got %q, err %v)", st, err)
	}

	// Idempotent: a second call re-verifies and still reports parked.
	parked, err := EnsureCgroupLeafHolder()
	if err != nil || !parked {
		t.Fatalf("second call: parked=%v err=%v, want true/nil", parked, err)
	}

	after, _, ok := DetectCgroupDelegation()
	if !ok {
		t.Fatal("delegation lost after leaf-holder move")
	}
	if after != before || after != scope {
		t.Fatalf("delegated dir changed by leaf-holder move: before=%q after=%q scope=%q", before, after, scope)
	}
}

// TestLeafHolderEnablesMemoryDelegation: a cgroup holding a process cannot
// distribute the memory controller to children (cgroup v2's
// no-internal-process rule — the root cause of memory-blind job cgroups on
// k8s, where the runner sits directly in the container scope). Parking the
// process in the leaf-holder child empties the fixture scope, after which
// +memory distribution succeeds and a sibling job cgroup gets
// memory.current (and memory.peak on kernels >= 5.19).
func TestLeafHolderEnablesMemoryDelegation(t *testing.T) {
	parent := parkTestScope(t)

	// Fabricate a scope with one member process — the shape of the runner's
	// container cgroup on k8s.
	scope := filepath.Join(parent, fmt.Sprintf("leafholder-test-%d", os.Getpid()))
	if err := os.Mkdir(scope, 0o755); err != nil {
		t.Fatalf("mkdir scope: %v", err)
	}
	t.Cleanup(func() { removeCgroupTree(t, scope) })

	cmd := exec.Command("cat")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() {
		stdin.Close()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	pid := cmd.Process.Pid
	if err := os.WriteFile(filepath.Join(scope, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0); err != nil {
		t.Fatalf("move helper into scope: %v", err)
	}

	// Precondition — the bug's mechanism: with an internal process, enabling
	// the memory controller for children must fail with EBUSY (the
	// no-internal-process rule).
	subtree := filepath.Join(scope, "cgroup.subtree_control")
	err = os.WriteFile(subtree, []byte("+memory"), 0)
	if err == nil {
		t.Skip("kernel allowed domain distribution with internal processes; leaf holder not demonstrable here")
	}
	if !errors.Is(err, unix.EBUSY) {
		t.Fatalf("+memory with internal process: got %v, want EBUSY", err)
	}

	if err := leafHolderMove(scope, pid); err != nil {
		t.Fatalf("leafHolderMove: %v", err)
	}

	holderProcs, err := os.ReadFile(filepath.Join(scope, cgroupLeafHolderName, "cgroup.procs"))
	if err != nil {
		t.Fatalf("read holder cgroup.procs: %v", err)
	}
	if !slices.Contains(strings.Fields(string(holderProcs)), strconv.Itoa(pid)) {
		t.Fatalf("helper pid %d not in holder (procs: %q)", pid, holderProcs)
	}
	scopeProcs, err := os.ReadFile(filepath.Join(scope, "cgroup.procs"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(scopeProcs)) != "" {
		t.Fatalf("scope still has direct processes: %q", scopeProcs)
	}

	// The point of the exercise: memory distribution now works, and a sibling
	// job cgroup gets memory accounting files.
	if err := os.WriteFile(subtree, []byte("+memory"), 0); err != nil {
		t.Fatalf("+memory after leaf-holder move: %v", err)
	}
	job := filepath.Join(scope, "job")
	if err := os.Mkdir(job, 0o755); err != nil {
		t.Fatalf("mkdir job cgroup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(job, "memory.current")); err != nil {
		t.Fatalf("job cgroup has no memory.current: %v", err)
	}
	// memory.peak needs kernel >= 5.19; production treats its absence as
	// "unknown" (-1), so tolerate it here too.
	if _, err := os.Stat(filepath.Join(job, "memory.peak")); err != nil {
		t.Logf("no memory.peak (kernel < 5.19?): %v — peak metering will be unavailable", err)
	}
}

// parkAllScopeProcs best-effort moves every direct member process of the
// delegated scope into its leaf holder (the test binary's wrapper processes —
// `go test`, the devShell — when the suite runs under a dedicated scope).
// Migration within the delegated scope preserves every limit; processes
// don't observe it. Test-only: the production API moves only the calling
// process.
func parkAllScopeProcs(t *testing.T, scope string) {
	t.Helper()
	holder := filepath.Join(scope, cgroupLeafHolderName)
	for range 10 {
		procs, err := os.ReadFile(filepath.Join(scope, "cgroup.procs"))
		if err != nil || strings.TrimSpace(string(procs)) == "" {
			return
		}
		for _, pid := range strings.Fields(string(procs)) {
			_ = os.WriteFile(filepath.Join(holder, "cgroup.procs"), []byte(pid), 0)
		}
	}
}

// skipUnlessDomainCgroup skips when dir isn't a plain "domain" cgroup: under
// a threaded domain, child cgroups are "domain invalid" and reject process
// migration outright (EOPNOTSUPP), so the leaf-holder technique — and this
// test — cannot work there. (Some interactive session scopes are threaded;
// run the suite under `systemd-run --user --scope -p Delegate=yes` for a
// clean domain scope.)
func skipUnlessDomainCgroup(t *testing.T, dir string) {
	t.Helper()
	typ, err := os.ReadFile(filepath.Join(dir, "cgroup.type"))
	if err != nil {
		t.Skipf("cannot read cgroup.type of %s: %v", dir, err)
	}
	if strings.TrimSpace(string(typ)) != "domain" {
		t.Skipf("cgroup %s is %q, not a plain domain cgroup", dir, strings.TrimSpace(string(typ)))
	}
}

// removeCgroupTree removes a small test cgroup subtree (children first);
// member processes must already be gone. Retries briefly — a just-killed
// process can keep its cgroup busy for a moment.
func removeCgroupTree(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() {
			removeCgroupTree(t, filepath.Join(dir, e.Name()))
		}
	}
	for i := 0; i < 50; i++ {
		if err := os.Remove(dir); err == nil || os.IsNotExist(err) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("could not remove test cgroup %s", dir)
}
