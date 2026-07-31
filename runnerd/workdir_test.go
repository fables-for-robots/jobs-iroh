package runnerd

// Work trees must live on the data dir's filesystem, not the OS temp dir:
// /tmp is a RAM-backed tmpfs on many hosts (NixOS sizes it at 50% of RAM),
// where one multi-GB build tree starves the builds' own memory and dies
// with ENOSPC long before the disk fills. And a killed attempt leaks its
// tree — the boot sweep is the backstop that reclaims it.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitWorkDirSweepsAndRedirectsTemp(t *testing.T) {
	t.Setenv("TMPDIR", os.Getenv("TMPDIR")) // restore after the test

	dataDir := t.TempDir()
	stale := filepath.Join(dataDir, "work", "jobs-build-stale")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatalf("plant stale tree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stale, "leak"), []byte("x"), 0o600); err != nil {
		t.Fatalf("plant stale file: %v", err)
	}

	work, err := initWorkDir(dataDir)
	if err != nil {
		t.Fatalf("initWorkDir: %v", err)
	}
	if want := filepath.Join(dataDir, "work"); work != want {
		t.Fatalf("work dir = %q, want %q", work, want)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale tree survived the boot sweep (stat err %v)", err)
	}

	// Every anonymous MkdirTemp in the drivers must now land inside work.
	probe, err := os.MkdirTemp("", "probe-")
	if err != nil {
		t.Fatalf("probe MkdirTemp: %v", err)
	}
	defer os.RemoveAll(probe)
	if !strings.HasPrefix(probe, work+string(os.PathSeparator)) {
		t.Fatalf("MkdirTemp landed at %q, want inside %q", probe, work)
	}
}
