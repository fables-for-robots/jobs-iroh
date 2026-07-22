package clientcli

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// lockMode selects the advisory flock mode serializing jobs-client processes
// on one data dir. The embedded store is single-owner; EX is the v1 default
// for every command that opens it. SH exists for future read-only downgrades
// once concurrent read-only opens are verified safe. The packstore takes its
// own NON-BLOCKING exclusive flock on open, so this lock is what makes a
// second jobs-client process wait politely instead of failing with
// "already open".
type lockMode int

const (
	lockShared    lockMode = unix.LOCK_SH
	lockExclusive lockMode = unix.LOCK_EX
)

// acquireStoreLock flocks <dataDir>/store.lock, blocking until available
// (with a stderr notice when it has to wait). The returned release unlocks
// and closes; the lock also dies with the process (kill-safe, no cleanup).
func acquireStoreLock(dataDir string, mode lockMode) (release func(), err error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	path := filepath.Join(dataDir, "store.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open store lock: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), int(mode)|unix.LOCK_NB); err != nil {
		if err != unix.EWOULDBLOCK {
			f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		fmt.Fprintln(os.Stderr, "waiting for another jobs-client process to release the store…")
		if err := unix.Flock(int(f.Fd()), int(mode)); err != nil {
			f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
	}
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
	}, nil
}
