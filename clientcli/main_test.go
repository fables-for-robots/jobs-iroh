package clientcli

import (
	"os"
	"testing"

	"github.com/jobs-build/jobs-iroh/sandbox"
)

// TestMain MUST call sandbox.Init() first (CLAUDE.md sandbox re-exec rule):
// any test that drives the runner's build sandbox re-execs /proc/self/exe —
// which, under `go test`, is THIS test binary. Init detects the re-exec'd
// child, performs the in-namespace setup, execs the real command, and never
// returns; without it the child would fall through to the test runner. A
// no-op for the plain tests in this package today, mandatory the moment a
// build e2e suite lands here.
func TestMain(m *testing.M) {
	sandbox.Init()
	os.Exit(m.Run())
}
