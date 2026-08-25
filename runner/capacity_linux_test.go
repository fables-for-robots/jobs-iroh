//go:build linux

package runner

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeCgroups builds a cgroup v2 tree under a temp dir and points the detector
// at it: files is {relative path: content}; self is the process's cgroup
// (the /proc/self/cgroup v2 entry).
func fakeCgroups(t *testing.T, self string, files map[string]string, memTotalKB int64) {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	proc := filepath.Join(t.TempDir(), "cgroup")
	if err := os.WriteFile(proc, []byte("0::"+self+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	meminfo := filepath.Join(t.TempDir(), "meminfo")
	if err := os.WriteFile(meminfo, []byte("MemTotal:       "+itoa(memTotalKB)+" kB\nMemFree: 1 kB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldMount, oldProc, oldMem := cgroupMount, procSelfCgroup, procMeminfo
	cgroupMount, procSelfCgroup, procMeminfo = root, proc, meminfo
	t.Cleanup(func() { cgroupMount, procSelfCgroup, procMeminfo = oldMount, oldProc, oldMem })
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}

const gib = int64(1) << 30

// The Kubernetes case that motivated the walk: the container runs in the host
// cgroup namespace, /proc/self/cgroup names the container scope several levels
// below the root, the pod's limit sits on an ancestor, the root is unlimited
// — and the runner has parked itself in an (unlimited) leaf-holder child.
func TestDetectCapacity_WalksUpToThePodLimit(t *testing.T) {
	fakeCgroups(t, "/kubepods.slice/pod1.slice/cri-containerd-abc.scope/jobs-leaf", map[string]string{
		"memory.max":                           "max",
		"cpu.max":                              "max 100000",
		"kubepods.slice/memory.max":            "max",
		"kubepods.slice/pod1.slice/memory.max": itoa(32 * gib),
		"kubepods.slice/pod1.slice/cpu.max":    "max 100000",
		"kubepods.slice/pod1.slice/cri-containerd-abc.scope/memory.max":           itoa(32 * gib),
		"kubepods.slice/pod1.slice/cri-containerd-abc.scope/cpu.max":              "max 100000",
		"kubepods.slice/pod1.slice/cri-containerd-abc.scope/jobs-leaf/memory.max": "max",
	}, 125*1024*1024) // 125 GiB host
	if got := detectMemBytes(); got != 32*gib {
		t.Fatalf("mem: got %d want %d (the pod limit, not the host)", got, 32*gib)
	}
	// No CPU limit anywhere ⇒ host cores.
	if got := detectCPUMilli(); got <= 0 || got%1000 != 0 {
		t.Fatalf("cpu: got %d want host cores*1000", got)
	}
}

// Private cgroup namespace: /proc/self/cgroup says "/" and the root IS the
// container — the old single-file read, still correct.
func TestDetectCapacity_PrivateNamespaceRoot(t *testing.T) {
	fakeCgroups(t, "/", map[string]string{
		"memory.max": itoa(8 * gib),
		"cpu.max":    "400000 100000",
	}, 125*1024*1024)
	if got := detectMemBytes(); got != 8*gib {
		t.Fatalf("mem: got %d want %d", got, 8*gib)
	}
	if got := detectCPUMilli(); got != 4000 {
		t.Fatalf("cpu: got %d want 4000", got)
	}
}

// The tightest limit on the chain wins, whichever level carries it.
func TestDetectCapacity_TightestOnChainWins(t *testing.T) {
	fakeCgroups(t, "/a/b", map[string]string{
		"memory.max":     itoa(64 * gib),
		"a/memory.max":   itoa(16 * gib),
		"a/b/memory.max": itoa(40 * gib),
		"cpu.max":        "1600000 100000",
		"a/cpu.max":      "max 100000",
		"a/b/cpu.max":    "200000 100000",
	}, 125*1024*1024)
	if got := detectMemBytes(); got != 16*gib {
		t.Fatalf("mem: got %d want %d", got, 16*gib)
	}
	if got := detectCPUMilli(); got != 2000 {
		t.Fatalf("cpu: got %d want 2000", got)
	}
}

// No cgroup v2 at all ⇒ host totals.
func TestDetectCapacity_NoCgroupFallsBackToHost(t *testing.T) {
	fakeCgroups(t, "/", map[string]string{}, 4*1024*1024)
	procSelfCgroup = filepath.Join(t.TempDir(), "missing")
	if got := detectMemBytes(); got != 4*gib {
		t.Fatalf("mem: got %d want MemTotal %d", got, 4*gib)
	}
}

// Explicit caps apply verbatim over the detection (no reserve).
func TestDetectCapacity_OverridesApplyVerbatim(t *testing.T) {
	fakeCgroups(t, "/", map[string]string{"memory.max": itoa(64 * gib), "cpu.max": "max 100000"}, 125*1024*1024)
	got := DetectCapacity("6", "28Gi", nil)
	if got.CPUMilli != 6000 || got.MemBytes != 28*gib {
		t.Fatalf("override: %+v", got)
	}
	got = DetectCapacity("", "28Gi", nil)
	if got.MemBytes != 28*gib || got.CPUMilli <= 0 {
		t.Fatalf("partial override: %+v", got)
	}
}
