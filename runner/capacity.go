package runner

import (
	"log/slog"

	"github.com/fables-for-robots/jobs-iroh/resources"
)

// Capacity headroom reserved for the runner process + sandbox overhead: the
// larger of a flat minimum and a fraction of the detected total.
const (
	cpuReserveMin  = 500 // millicpu
	memReserveMin  = 512 * resources.MiB
	reservePercent = 10 // percent of detected total
)

// capacityFromOverride parses operator-supplied capacity strings
// (JOBS_RUNNER_CPU / JOBS_RUNNER_MEM). An empty or malformed field stays zero,
// leaving that dimension to be filled by auto-detection.
func capacityFromOverride(cpu, mem string) resources.Resources {
	var out resources.Resources
	if cpu != "" {
		if c, err := resources.ParseCPU(cpu); err == nil {
			out.CPUMilli = c
		}
	}
	if mem != "" {
		if m, err := resources.ParseMem(mem); err == nil {
			out.MemBytes = m
		}
	}
	return out
}

// applyReserve subtracts headroom from a detected capacity, then floors the
// result to one default build slot so the runner can always run at least one
// default build (multi-job-runner design).
func applyReserve(d resources.Resources) resources.Resources {
	cpuReserve := max(int64(cpuReserveMin), d.CPUMilli*reservePercent/100)
	memReserve := max(int64(memReserveMin), d.MemBytes*reservePercent/100)
	out := resources.Resources{
		CPUMilli: d.CPUMilli - cpuReserve,
		MemBytes: d.MemBytes - memReserve,
	}
	if out.CPUMilli < resources.DefaultBuild.CPUMilli {
		out.CPUMilli = resources.DefaultBuild.CPUMilli
	}
	if out.MemBytes < resources.DefaultBuild.MemBytes {
		out.MemBytes = resources.DefaultBuild.MemBytes
	}
	return out
}

// DetectCapacity resolves the runner's advertised CPU/RAM: auto-detected
// (cgroup-aware, host fallback) minus a headroom reserve, with any operator
// override (cpuOverride/memOverride) applied verbatim over the detected value
// (multi-job-runner design). Detection failure logs and falls back to one
// default build slot so the runner stays useful.
func DetectCapacity(cpuOverride, memOverride string, log *slog.Logger) resources.Resources {
	if log == nil {
		log = slog.Default()
	}
	cap := applyReserve(detectHostCapacity(log))
	ov := capacityFromOverride(cpuOverride, memOverride)
	if ov.CPUMilli > 0 {
		cap.CPUMilli = ov.CPUMilli
	}
	if ov.MemBytes > 0 {
		cap.MemBytes = ov.MemBytes
	}
	log.Info("runner advertised capacity", "cpuMilli", cap.CPUMilli, "memBytes", cap.MemBytes,
		"cpuOverride", cpuOverride, "memOverride", memOverride)
	return cap
}
