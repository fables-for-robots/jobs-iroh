//go:build linux

package runner

import (
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/jobs-build/jobs-iroh/resources"
)

// detectHostCapacity reads the runner's raw available CPU/RAM, cgroup-v2-aware
// (the pod's own cgroup limit on Kubernetes), falling back to the host totals
// when the cgroup is unlimited or unreadable. The returned value is the raw
// detection; DetectCapacity applies the headroom reserve.
func detectHostCapacity(log *slog.Logger) resources.Resources {
	return resources.Resources{
		CPUMilli: detectCPUMilli(),
		MemBytes: detectMemBytes(),
	}
}

// detectCPUMilli reads /sys/fs/cgroup/cpu.max ("<quota> <period>"; "max" quota
// means unlimited) and returns quota/period*1000 millicpu, else the host core
// count * 1000.
func detectCPUMilli() int64 {
	host := int64(runtime.NumCPU()) * 1000
	b, err := os.ReadFile("/sys/fs/cgroup/cpu.max")
	if err != nil {
		return host
	}
	fields := strings.Fields(strings.TrimSpace(string(b)))
	if len(fields) != 2 || fields[0] == "max" {
		return host
	}
	quota, err1 := strconv.ParseInt(fields[0], 10, 64)
	period, err2 := strconv.ParseInt(fields[1], 10, 64)
	if err1 != nil || err2 != nil || period <= 0 || quota <= 0 {
		return host
	}
	return quota * 1000 / period
}

// detectMemBytes reads /sys/fs/cgroup/memory.max ("max" means unlimited) and
// returns it, else /proc/meminfo MemTotal.
func detectMemBytes() int64 {
	if b, err := os.ReadFile("/sys/fs/cgroup/memory.max"); err == nil {
		s := strings.TrimSpace(string(b))
		if s != "max" {
			if v, err := strconv.ParseInt(s, 10, 64); err == nil && v > 0 {
				return v
			}
		}
	}
	return memTotalFromMeminfo()
}

// memTotalFromMeminfo parses MemTotal (kB) from /proc/meminfo into bytes; 0 if
// unreadable.
func memTotalFromMeminfo() int64 {
	b, err := os.ReadFile("/proc/meminfo")
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
