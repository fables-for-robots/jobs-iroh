package runnerd

import (
	"context"
	"testing"
	"time"

	"github.com/fables-for-robots/jobs-iroh/resources"
)

func res(cpu, memGiB int64) resources.Resources {
	return resources.Resources{CPUMilli: cpu, MemBytes: memGiB << 30}
}

func TestAdmissionAccounting(t *testing.T) {
	a := newAdmission(res(2000, 4), 0)

	if !a.TryAcquire("job/a", res(1000, 2)) {
		t.Fatal("first acquire should fit")
	}
	if !a.TryAcquire("job/b", res(1000, 2)) {
		t.Fatal("second acquire should exactly fill capacity")
	}
	if a.TryAcquire("job/c", res(200, 1)) {
		t.Fatal("third acquire must not fit (both dimensions full)")
	}
	free, inflight := a.Snapshot()
	if free.CPUMilli != 0 || free.MemBytes != 0 {
		t.Fatalf("free = %+v, want zero", free)
	}
	if inflight != 2 {
		t.Fatalf("inflight = %d, want 2", inflight)
	}

	a.Release("job/a")
	if !a.TryAcquire("job/c", res(200, 1)) {
		t.Fatal("acquire after release should fit")
	}
	free, _ = a.Snapshot()
	if free.CPUMilli != 800 || free.MemBytes != 1<<30 {
		t.Fatalf("free = %+v, want 800m/1GiB", free)
	}
}

func TestAdmissionDimensionsIndependent(t *testing.T) {
	a := newAdmission(res(4000, 4), 0)
	if !a.TryAcquire("job/a", res(1000, 4)) {
		t.Fatal("first acquire should fit")
	}
	// CPU would fit, memory would not.
	if a.TryAcquire("job/b", res(1000, 1)) {
		t.Fatal("memory-exceeding acquire must fail even with CPU free")
	}
}

func TestAdmissionDuplicateID(t *testing.T) {
	a := newAdmission(res(4000, 16), 0)
	if !a.TryAcquire("job/a", res(200, 1)) {
		t.Fatal("acquire failed")
	}
	if a.TryAcquire("job/a", res(200, 1)) {
		t.Fatal("duplicate id must be rejected")
	}
}

func TestAdmissionRename(t *testing.T) {
	a := newAdmission(res(4000, 16), 0)
	if !a.TryAcquire("lane/c1-m1", res(1000, 1)) {
		t.Fatal("lane acquire failed")
	}
	if !a.Rename("lane/c1-m1", "job/n1") {
		t.Fatal("rename lane -> job failed")
	}
	if a.Rename("lane/c1-m1", "job/n2") {
		t.Fatal("rename of a no-longer-held id must fail")
	}
	if !a.TryAcquire("lane/c1-m1", res(1000, 1)) {
		t.Fatal("lane re-acquire after rename failed")
	}
	// Duplicate-node guard: renaming onto a running job's id must fail and
	// keep the source reservation held.
	if a.Rename("lane/c1-m1", "job/n1") {
		t.Fatal("rename onto a running node must fail")
	}
	free, inflight := a.Snapshot()
	if inflight != 1 {
		t.Fatalf("inflight = %d, want 1", inflight)
	}
	if free.CPUMilli != 2000 {
		t.Fatalf("free cpu = %d, want 2000 (two 1000m holds)", free.CPUMilli)
	}
}

func TestAdmissionSlotsCap(t *testing.T) {
	a := newAdmission(res(4000, 16), 2)
	if !a.TryAcquire("job/a", res(200, 1)) || !a.TryAcquire("job/b", res(200, 1)) {
		t.Fatal("first two acquires should fit")
	}
	if a.TryAcquire("job/c", res(200, 1)) {
		t.Fatal("slots cap must reject the third hold despite free resources")
	}
	a.Release("job/a")
	if !a.TryAcquire("job/c", res(200, 1)) {
		t.Fatal("acquire after release should fit under the cap")
	}
}

func TestAdmissionAcquireBlocksUntilRelease(t *testing.T) {
	a := newAdmission(res(1000, 1), 0)
	if !a.TryAcquire("job/a", res(1000, 1)) {
		t.Fatal("acquire failed")
	}

	acquired := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		acquired <- a.Acquire(ctx, "job/b", res(1000, 1))
	}()

	select {
	case err := <-acquired:
		t.Fatalf("Acquire returned early: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	a.Release("job/a")
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatalf("Acquire after release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Acquire did not wake on release")
	}
}

func TestAdmissionAcquireHonorsContext(t *testing.T) {
	a := newAdmission(res(1000, 1), 0)
	if !a.TryAcquire("job/a", res(1000, 1)) {
		t.Fatal("acquire failed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Acquire(ctx, "job/b", res(1000, 1)); err == nil {
		t.Fatal("Acquire must fail on a done context")
	}
}
