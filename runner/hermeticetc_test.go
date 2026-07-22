package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteHermeticEtc pins issue #99's file contract: /etc/hosts answers
// localhost (v4+v6) and /etc/resolv.conf exists EMPTY so resolver paths fail
// fast under net=none instead of hanging forever. The in-sandbox behavior is
// exercised by the NamespaceBuildExecutor suite (buildexec_linux_test.go).
func TestWriteHermeticEtc(t *testing.T) {
	root := t.TempDir()
	if err := writeHermeticEtc(root); err != nil {
		t.Fatal(err)
	}
	hosts, err := os.ReadFile(filepath.Join(root, "etc", "hosts"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(hosts), "127.0.0.1 localhost") || !strings.Contains(string(hosts), "::1 localhost") {
		t.Fatalf("hosts = %q, want localhost v4+v6", hosts)
	}
	fi, err := os.Stat(filepath.Join(root, "etc", "resolv.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 0 {
		t.Fatalf("resolv.conf must be EMPTY (fail-fast lookups), got %d bytes", fi.Size())
	}
	// Idempotent: re-running over an existing /etc succeeds.
	if err := writeHermeticEtc(root); err != nil {
		t.Fatal(err)
	}
}
