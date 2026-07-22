//go:build linux

package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadCgroupStats: the stat reader parses cgroup v2 cpu.stat usage_usec +
// memory.current from a cgroup dir, reporting -1 per field when a file is
// missing or malformed (per-field best-effort — cpu.stat exists regardless of
// controller enablement, memory.current only with the memory controller).
func TestReadCgroupStats(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Neither file: both unknown.
	st := readCgroupStats(dir)
	if st.CPUUsec != -1 || st.MemoryBytes != -1 {
		t.Fatalf("empty dir: got %+v, want -1/-1", st)
	}

	write("cpu.stat", "usage_usec 42000000\nuser_usec 30000000\nsystem_usec 12000000\n")
	write("memory.current", "1288490189\n")
	st = readCgroupStats(dir)
	if st.CPUUsec != 42000000 {
		t.Fatalf("CPUUsec = %d, want 42000000", st.CPUUsec)
	}
	if st.MemoryBytes != 1288490189 {
		t.Fatalf("MemoryBytes = %d, want 1288490189", st.MemoryBytes)
	}

	// Malformed content: unknown, not zero.
	write("cpu.stat", "nonsense\n")
	write("memory.current", "not-a-number\n")
	st = readCgroupStats(dir)
	if st.CPUUsec != -1 || st.MemoryBytes != -1 {
		t.Fatalf("malformed: got %+v, want -1/-1", st)
	}
}

// TestCgroupStatsNil: Stats on a nil Cgroup (the undelegated best-effort case)
// reports ok=false.
func TestCgroupStatsNil(t *testing.T) {
	var c *Cgroup
	if _, ok := c.Stats(); ok {
		t.Fatal("nil Cgroup must report ok=false")
	}
}
