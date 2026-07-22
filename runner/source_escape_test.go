package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDiskSource_SymlinkEscapeRejected: a symlink inside a materialized tree
// pointing outside it (a foreign resolution dep is untrusted content, and
// dep.read runs on the runner HOST) must not be followed by Read/Exists.
func TestDiskSource_SymlinkEscapeRejected(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("host-secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	if err := os.Symlink(secret, filepath.Join(root, "go.sum")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "dir")); err != nil {
		t.Fatal(err)
	}
	// A benign in-tree relative symlink must keep working.
	if err := os.WriteFile(filepath.Join(root, "real.txt"), []byte("in-tree"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.txt", filepath.Join(root, "alias.txt")); err != nil {
		t.Fatal(err)
	}

	s := diskSource{root: root}
	if _, err := s.Read("go.sum"); err == nil || !strings.Contains(err.Error(), "outside the tree") {
		t.Fatalf("Read through escaping symlink: want outside-the-tree error, got %v", err)
	}
	if s.Exists("go.sum") {
		t.Fatal("Exists must not follow an escaping symlink")
	}
	if _, err := s.Read("dir/secret.txt"); err == nil || !strings.Contains(err.Error(), "outside the tree") {
		t.Fatalf("Read through escaping dir symlink: want outside-the-tree error, got %v", err)
	}
	got, err := s.Read("alias.txt")
	if err != nil || string(got) != "in-tree" {
		t.Fatalf("in-tree relative symlink must still resolve: %q %v", got, err)
	}
}
