package sandbox

// CgroupStats is a point-in-time usage reading of a leaf cgroup: CPUUsec is
// the CUMULATIVE CPU time (µs) the cgroup's whole process tree has consumed
// since creation (cgroup v2 cpu.stat usage_usec — includes exited children),
// MemoryBytes its CURRENT memory usage (memory.current), MemoryPeakBytes its
// PEAK memory usage since creation (memory.peak, the kernel high-water mark —
// kernels ≥5.19). -1 means that field is unavailable (file missing/unreadable
// — per-field, since cpu.stat exists regardless of controller enablement
// while memory.current/memory.peak need the memory controller delegated).
type CgroupStats struct {
	CPUUsec         int64
	MemoryBytes     int64
	MemoryPeakBytes int64
}
