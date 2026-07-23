package sandbox_test

import (
	"os"
	"testing"

	"github.com/fables-for-robots/jobs-iroh/sandbox"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// testHelpers are subprocess entry points for specs that must run test code
// INSIDE a sandbox namespace (registered by init() in platform-specific test
// files). A helper is invoked by making this test binary a sandbox.Run command
// with JOBS_SANDBOX_TEST_HELPER naming it; its return value is the exit code.
var testHelpers = map[string]func() int{}

func TestMain(m *testing.M) {
	sandbox.Init() // if this is a re-exec'd sandbox child, Init execs and never returns
	if h := testHelpers[os.Getenv("JOBS_SANDBOX_TEST_HELPER")]; h != nil {
		os.Exit(h())
	}
	os.Exit(m.Run())
}

func TestSandbox(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "sandbox suite")
}
