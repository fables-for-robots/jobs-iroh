package sandbox_test

import (
	"os"
	"testing"

	"github.com/fables-for-robots/jobs-iroh/sandbox"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestMain(m *testing.M) {
	sandbox.Init() // if this is a re-exec'd sandbox child, Init execs and never returns
	os.Exit(m.Run())
}

func TestSandbox(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "sandbox suite")
}
