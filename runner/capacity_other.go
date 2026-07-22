//go:build !linux

package runner

import (
	"log/slog"
	"runtime"

	"github.com/fables-for-robots/jobs-iroh/resources"
)

// detectHostCapacity is the non-Linux fallback (dev hosts): host core count for
// CPU and zero memory (the applyReserve floor then advertises one default build
// slot's worth). Real runners are Linux; operators override via flags/env.
func detectHostCapacity(log *slog.Logger) resources.Resources {
	return resources.Resources{CPUMilli: int64(runtime.NumCPU()) * 1000}
}
