//go:build !linux

package runner

import "context"

// CgroupExecutor is the cgroup-confined import executor. On non-Linux
// platforms there is no user-namespace or cgroup v2 support, so this
// falls back to the plain Subprocess executor (no cgroup confinement).
//
// MemoryMaxBytes and PIDsMax are accepted but ignored on non-Linux.
type CgroupExecutor struct {
	MemoryMaxBytes int64
	PIDsMax        int64
}

var _ Executor = CgroupExecutor{}

// defaultImportExecutor: no namespace sandbox off Linux — imports run as a
// plain subprocess, exactly as before.
func defaultImportExecutor(int64) Executor { return Subprocess{} }

// Run delegates to Subprocess on non-Linux (no cgroup/namespace support).
func (e CgroupExecutor) Run(ctx context.Context, spec ExecSpec) (ExecResult, error) {
	return Subprocess{}.Run(ctx, spec)
}
