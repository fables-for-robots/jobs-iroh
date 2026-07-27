//go:build linux

package runner

import (
	"testing"

	"github.com/jobs-build/jobs-iroh/sandbox"
)

// The production runner must confine imports: the default import executor is
// CgroupExecutor (namespaces + best-effort cgroup + fetching heartbeats),
// carrying the job's resolved memory limit — Subprocess is only the fallback
// for hosts without user namespaces (and the explicit test/develop seam).
func TestDefaultImportExecutorIsCgroupConfined(t *testing.T) {
	if !sandbox.UserNSAvailable() {
		t.Skip("user namespaces unavailable")
	}
	ex := defaultImportExecutor(512 << 20)
	cg, ok := ex.(CgroupExecutor)
	if !ok {
		t.Fatalf("default executor = %T, want CgroupExecutor", ex)
	}
	if cg.MemoryMaxBytes != 512<<20 {
		t.Fatalf("MemoryMaxBytes = %d, want %d (the job's resolved limit)", cg.MemoryMaxBytes, 512<<20)
	}
}
