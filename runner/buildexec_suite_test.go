package runner_test

import (
	"os"
	"testing"

	"github.com/fables-for-robots/jobs-iroh/sandbox"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// TestMain MUST call sandbox.Init() first: NamespaceBuildExecutor (and the
// import CgroupExecutor) re-exec /proc/self/exe, which in tests is this test
// binary. Init() detects the re-exec'd sandbox child (via the
// _JOBS_SANDBOX_CHILD env var), performs the in-namespace setup, execs the
// build command, and never returns. Without this the child would fall through
// to the Go test runner and the build would never run. One TestMain covers
// the whole runner test binary (internal + external test packages).
func TestMain(m *testing.M) {
	sandbox.Init()
	os.Exit(m.Run())
}

func TestBuildExec(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "buildexec suite")
}
