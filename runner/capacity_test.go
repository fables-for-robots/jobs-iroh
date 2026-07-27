package runner

import (
	"testing"

	"github.com/jobs-build/jobs-iroh/resources"
)

func TestCapacityOverrideWins(t *testing.T) {
	got := capacityFromOverride("2", "4Gi")
	if got.CPUMilli != 2000 || got.MemBytes != 4<<30 {
		t.Fatalf("override: %+v", got)
	}
	// Partial + malformed fields stay zero (caller fills them from detection).
	if g := capacityFromOverride("", "bad"); g.CPUMilli != 0 || g.MemBytes != 0 {
		t.Fatalf("partial/bad: %+v", g)
	}
}

func TestReserveFloorsToDefaultBuild(t *testing.T) {
	// Detected below one default build slot ⇒ floor up to DefaultBuild so the
	// runner can always run at least one default build.
	got := applyReserve(resources.Resources{CPUMilli: 300, MemBytes: 256 << 20})
	if got.CPUMilli < resources.DefaultBuild.CPUMilli || got.MemBytes < resources.DefaultBuild.MemBytes {
		t.Fatalf("floor: %+v", got)
	}
}

func TestReserveSubtractsHeadroom(t *testing.T) {
	// 8 CPU / 16Gi: reserve = 10% (above the 500m / 512Mi minimums).
	got := applyReserve(resources.Resources{CPUMilli: 8000, MemBytes: 16 << 30})
	if got.CPUMilli != 8000-800 {
		t.Fatalf("cpu reserve: got %d want %d", got.CPUMilli, 8000-800)
	}
	if got.MemBytes != (16<<30)-(16<<30)/10 {
		t.Fatalf("mem reserve: got %d want %d", got.MemBytes, (16<<30)-(16<<30)/10)
	}
}
