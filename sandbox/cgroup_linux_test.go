//go:build linux

package sandbox_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fables-for-robots/jobs-iroh/sandbox"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Cgroup", func() {
	It("creates a leaf with pids.max and cleans up", func() {
		_, ctrls, ok := sandbox.DetectCgroupDelegation()
		if !ok {
			Skip("no delegated cgroup v2 subtree")
		}
		hasPids := false
		for _, c := range ctrls {
			if c == "pids" {
				hasPids = true
			}
		}
		if !hasPids {
			Skip("pids controller not delegated")
		}
		name := fmt.Sprintf("jobs-test-%d", os.Getpid())
		cg, err := sandbox.CreateCgroup(name, sandbox.CgroupLimits{PIDsMax: 5})
		Expect(err).NotTo(HaveOccurred())
		Expect(cg).NotTo(BeNil())
		DeferCleanup(func() { _ = cg.Close() })

		b, err := os.ReadFile(filepath.Join(cg.Dir(), "pids.max"))
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(string(b))).To(Equal("5"))

		Expect(cg.Close()).To(Succeed())
		_, statErr := os.Stat(cg.Dir())
		Expect(os.IsNotExist(statErr)).To(BeTrue())
	})
})
