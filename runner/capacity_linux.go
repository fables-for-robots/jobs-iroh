//go:build linux

package runner

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/jobs-build/jobs-iroh/resources"
)

// Where the cgroup v2 hierarchy is mounted and where the kernel tells a
// process which cgroup it is in. Variables so tests can point them at a
// fake tree.
var (
	cgroupMount    = "/sys/fs/cgroup"
	procSelfCgroup = "/proc/self/cgroup"
	procMeminfo    = "/proc/meminfo"
)

// detectHostCapacity reads the runner's raw available CPU/RAM, cgroup-v2-aware,
// falling back to the host totals when no cgroup on the path limits it. The
// returned value is the raw detection; DetectCapacity applies the headroom
// reserve.
//
// cgroup-aware means: starting from the process's OWN cgroup (per
// /proc/self/cgroup) and walking up to the mount root, the tightest
// memory.max and cpu.max found apply. Reading only the mount root is wrong
// in both directions a container can be set up: with a private cgroup
// namespace the root IS the container, fine — but in the host namespace (a
// privileged Kubernetes pod, for one) the root is the machine, while the
// pod's limit sits on an ancestor of the process's cgroup several levels
// down; and once the runner has parked itself in its leaf-holder child
// cgroup, its own cgroup is unlimited while the limit lives one level up.
func detectHostCapacity(log *slog.Logger) resources.Resources {
	return resources.Resources{
		CPUMilli: detectCPUMilli(),
		MemBytes: detectMemBytes(),
	}
}

// cgroupPathChain lists the process's cgroup directory and every ancestor up
// to and including the mount root, nearest first. Empty when /proc/self/cgroup
// has no v2 entry (cgroup v1 host, or no cgroupfs at all).
func cgroupPathChain() []string {
	b, err := os.ReadFile(procSelfCgroup)
	if err != nil {
		return nil
	}
	rel := ""
	for _, line := range strings.Split(string(b), "\n") {
		// v2 entry: "0::/kubepods.slice/.../cri-containerd-<id>.scope"
		if strings.HasPrefix(line, "0::") {
			rel = strings.TrimPrefix(line, "0::")
			break
		}
	}
	if rel == "" {
		return nil
	}
	rel = strings.TrimSuffix(strings.TrimSpace(rel), "/")
	dir := filepath.Clean(filepath.Join(cgroupMount, rel))
	root := filepath.Clean(cgroupMount)
	var chain []string
	for {
		chain = append(chain, dir)
		if dir == root || !strings.HasPrefix(dir, root) {
			break
		}
		dir = filepath.Dir(dir)
	}
	if chain[len(chain)-1] != root {
		chain = append(chain, root)
	}
	return chain
}

// detectCPUMilli returns the tightest cpu.max quota on the cgroup chain
// ("<quota> <period>"; "max" quota means unlimited) as millicpu, else the host
// core count * 1000.
func detectCPUMilli() int64 {
	host := int64(runtime.NumCPU()) * 1000
	best := int64(0)
	for _, dir := range cgroupPathChain() {
		b, err := os.ReadFile(filepath.Join(dir, "cpu.max"))
		if err != nil {
			continue
		}
		fields := strings.Fields(strings.TrimSpace(string(b)))
		if len(fields) != 2 || fields[0] == "max" {
			continue
		}
		quota, err1 := strconv.ParseInt(fields[0], 10, 64)
		period, err2 := strconv.ParseInt(fields[1], 10, 64)
		if err1 != nil || err2 != nil || period <= 0 || quota <= 0 {
			continue
		}
		if m := quota * 1000 / period; best == 0 || m < best {
			best = m
		}
	}
	if best > 0 && best < host {
		return best
	}
	return host
}

// detectMemBytes returns the tightest memory.max on the cgroup chain ("max"
// means unlimited), else /proc/meminfo MemTotal.
func detectMemBytes() int64 {
	host := memTotalFromMeminfo()
	best := int64(0)
	for _, dir := range cgroupPathChain() {
		b, err := os.ReadFile(filepath.Join(dir, "memory.max"))
		if err != nil {
			continue
		}
		s := strings.TrimSpace(string(b))
		if s == "max" {
			continue
		}
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil || v <= 0 {
			continue
		}
		if best == 0 || v < best {
			best = v
		}
	}
	if best > 0 && (host == 0 || best < host) {
		return best
	}
	return host
}

// memTotalFromMeminfo parses MemTotal (kB) from /proc/meminfo into bytes; 0 if
// unreadable.
func memTotalFromMeminfo() int64 {
	b, err := os.ReadFile(procMeminfo)
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}
