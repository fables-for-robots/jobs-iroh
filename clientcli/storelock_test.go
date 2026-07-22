package clientcli

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestStoreLock_ExclusiveBlocksExclusive(t *testing.T) {
	dir := t.TempDir()
	rel1, err := acquireStoreLock(dir, lockExclusive)
	if err != nil {
		t.Fatal(err)
	}
	// A second EX acquisition must not be immediately available: probe with a
	// non-blocking flock on a separate fd (flock contends across separate
	// opens even within one process).
	if tryLock(t, dir, unix.LOCK_EX) {
		t.Fatal("second exclusive lock acquired while first held")
	}
	rel1()
	if !tryLock(t, dir, unix.LOCK_EX) {
		t.Fatal("exclusive lock not acquirable after release")
	}
}

func TestStoreLock_SharedCoexists(t *testing.T) {
	dir := t.TempDir()
	rel1, err := acquireStoreLock(dir, lockShared)
	if err != nil {
		t.Fatal(err)
	}
	defer rel1()
	rel2, err := acquireStoreLock(dir, lockShared)
	if err != nil {
		t.Fatal("second shared lock failed while first held")
	}
	rel2()
	if tryLock(t, dir, unix.LOCK_EX) {
		t.Fatal("exclusive acquired while shared held")
	}
}

// tryLock: open <dir>/store.lock and attempt a non-blocking flock; true on success (then unlock+close).
func tryLock(t *testing.T, dir string, how int) bool {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, "store.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), how|unix.LOCK_NB); err != nil {
		return false
	}
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return true
}
