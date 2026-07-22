package clientcli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/fables-for-robots/jobs-iroh/amber"
)

// testStore, when non-nil, bypasses the data-dir open entirely (no flock, no
// on-disk store): the test harness pre-opens one amber.Open(t.TempDir())
// store and shares it across in-process CLI invocations.
var testStore *amber.Store

// defaultDataDir is the client data dir when neither --data-dir nor
// JOBS_DATA_DIR is set: $XDG_DATA_HOME/jobs-iroh, else
// ~/.local/share/jobs-iroh.
func defaultDataDir() string {
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "jobs-iroh")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".jobs-iroh-data"
	}
	return filepath.Join(home, ".local", "share", "jobs-iroh")
}

// clientStore is the end-user CLI's store access: an embedded
// amber-store-core store under <data-dir>/store plus the fetcher cache
// <data-dir>/cache, serialized against concurrent jobs-client processes by
// acquireStoreLock. Single-store world: no remotes, no grants, no transport
// identity — jobs' Embedded/GrantHolder plumbing has no counterpart here.
type clientStore struct {
	Store    *amber.Store
	CacheDir string
	dataDir  string
	release  func()
	closeFn  func() error
}

// openClientStore locks the data dir (flock FIRST — the embedded packstore's
// own flock is non-blocking and would fail instead of waiting) and opens the
// embedded store. Callers must defer Close.
func openClientStore(dataDir string, mode lockMode) (*clientStore, error) {
	cacheDir := filepath.Join(dataDir, "cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}
	if testStore != nil {
		return &clientStore{Store: testStore, CacheDir: cacheDir, dataDir: dataDir,
			release: func() {}, closeFn: func() error { return nil }}, nil
	}
	release, err := acquireStoreLock(dataDir, mode)
	if err != nil {
		return nil, err
	}
	st, err := amber.Open(filepath.Join(dataDir, "store"))
	if err != nil {
		release()
		return nil, fmt.Errorf("open embedded store: %w", err)
	}
	return &clientStore{Store: st, CacheDir: cacheDir, dataDir: dataDir,
		release: release, closeFn: st.Close}, nil
}

// Close closes the store, then releases the flock.
func (cs *clientStore) Close() {
	_ = cs.closeFn()
	cs.release()
}
