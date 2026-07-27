//go:build linux

package sandbox_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jobs-build/jobs-iroh/sandbox"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// A build with many content-addressed inputs (the GitLab monolith stages ~850
// store mounts; its env alone is ~100KiB) produces a child config far beyond
// the kernel's MAX_ARG_STRLEN (~128KiB) limit on a single argv/env string. The
// config must therefore not travel inline in the child env var — Run spills
// big payloads to a temp file the child reads back.
var _ = Describe("oversized sandbox config", func() {
	It("re-execs despite a config beyond MAX_ARG_STRLEN", func() {
		if !sandbox.UserNSAvailable() {
			Skip("user namespaces unavailable")
		}
		bashPath, err := exec.LookPath("bash")
		Expect(err).NotTo(HaveOccurred())

		newRoot := GinkgoTB().TempDir()
		resultDir := GinkgoTB().TempDir()

		// Enough filler env that the marshaled config JSON is ~240KiB — well
		// past the per-string cap that E2BIGs an inline env payload.
		env := []string{"PATH=/usr/bin:/bin", "HOME=/tmp"}
		for i := 0; i < 3000; i++ {
			env = append(env, fmt.Sprintf("JOBS_TEST_FILLER_%04d=%s", i, strings.Repeat("x", 60)))
		}

		mounts := []sandbox.Mount{
			{Source: "", Target: "/", FSType: "tmpfs"},
			{Source: "/nix", Target: "/nix", ReadOnly: true},
			{Source: "/dev", Target: "/dev"},
			{Source: resultDir, Target: "/out"},
		}
		if _, err := os.Stat("/usr"); err == nil {
			mounts = append(mounts, sandbox.Mount{Source: "/usr", Target: "/usr", ReadOnly: true})
		}

		cfg := sandbox.Config{
			// Builtins only — the jail has no coreutils.
			Command: []string{bashPath, "-c", `n=0; for v in "${!JOBS_TEST_FILLER_@}"; do n=$((n+1)); done; echo "fillers=$n" > /out/result`},
			Env:     env,
			NewRoot: newRoot,
			Mounts:  mounts,
			Namespaces: sandbox.Namespaces{
				User: true, Mount: true, PID: true, UTS: true, IPC: true,
			},
			Stdout: GinkgoWriter,
			Stderr: GinkgoWriter,
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		code, err := sandbox.Run(ctx, cfg)
		Expect(err).NotTo(HaveOccurred(), "Run must not fail at setup/infra level")
		Expect(code).To(Equal(0))

		b, err := os.ReadFile(filepath.Join(resultDir, "result"))
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(string(b))).To(Equal("fillers=3000"), "the full env must reach the child")
	})
})
