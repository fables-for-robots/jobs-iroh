//go:build linux

package runnerd

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jobs-build/jobs-iroh/amber"
	"github.com/jobs-build/jobs-iroh/runner"
	"github.com/jobs-build/jobs-iroh/sandbox"
)

// TestMain MUST call sandbox.Init() first: bootSelfTest drives the real
// namespace build sandbox, which re-execs /proc/self/exe — in tests, this
// test binary (the runner suite's exact arrangement).
func TestMain(m *testing.M) {
	sandbox.Init()
	os.Exit(m.Run())
}

// TestBootSelfTest_PassesAndCleansUp runs the real boot self-test twice
// against one fresh store: it must pass on a sandbox-capable host (the second
// run proving the shell-seed join and the nonce'd F still exec — not join —
// the build), leave no build-* refs behind, and never publish
// shell:<platform>, which must stay pull-from-server on runners.
func TestBootSelfTest_PassesAndCleansUp(t *testing.T) {
	if !sandbox.UserNSAvailable() {
		t.Skip("user namespaces unavailable")
	}
	ctx := context.Background()
	st, err := amber.Open(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	platform := runner.Platform()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if err := bootSelfTest(ctx, st, platform, t.TempDir(), log); err != nil {
		t.Fatalf("first bootSelfTest: %v", err)
	}
	if err := bootSelfTest(ctx, st, platform, t.TempDir(), log); err != nil {
		t.Fatalf("second bootSelfTest: %v", err)
	}

	refs, err := st.ListRefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range refs {
		if strings.HasPrefix(r.Name, "build-") {
			t.Errorf("leftover build ref %s", r.Name)
		}
		if r.Name == "shell:"+platform {
			t.Errorf("self-test published shell:%s — the real shell ref must stay pull-from-server", platform)
		}
	}
}
