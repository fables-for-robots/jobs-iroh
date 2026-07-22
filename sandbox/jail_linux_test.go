//go:build linux

package sandbox_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/fables-for-robots/jobs-iroh/sandbox"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The jail specs prove the rootless namespace + pivot_root isolation end to end.
//
// NewRoot is assembled as a *writable tmpfs* into which we bind a minimal host
// userland read-only: /nix (the bash closure on this NixOS host) and /usr (a
// read-only tree the probe tries — and fails — to write, proving EROFS). /dev is
// bound read-write so bash's /dev/tcp and /dev/null work, and a host-backed dir
// is bound read-write at /out so the parent can read the probe's result after
// Run returns. A wholesale read-only bind of host "/" is deliberately avoided:
// its locked submounts (/run, /proc, ...) reject the recursive RO remount in a
// user namespace, and writable mount targets can't be created under it.
//
// The probe is the host bash (resolved on the parent; a /nix/store path that
// stays valid inside the ro-bound /nix). It uses only bash builtins — no
// external binaries — so it needs nothing beyond the bash closure.
var _ = Describe("rootless jail", func() {
	if !sandbox.UserNSAvailable() {
		Skip("user namespaces unavailable")
	}

	var (
		bashPath  string
		newRoot   string
		resultDir string
	)

	BeforeEach(func() {
		var err error
		bashPath, err = exec.LookPath("bash")
		Expect(err).NotTo(HaveOccurred(), "host bash must be resolvable")

		newRoot = GinkgoTB().TempDir()
		resultDir = GinkgoTB().TempDir()
	})

	// baseMounts builds NewRoot: tmpfs root + RO host userland + writable /dev and /out.
	baseMounts := func() []sandbox.Mount {
		m := []sandbox.Mount{
			{Source: "", Target: "/", FSType: "tmpfs"}, // writable new root
			{Source: "/nix", Target: "/nix", ReadOnly: true},
			{Source: "/dev", Target: "/dev"}, // rw: /dev/tcp, /dev/null
			{Source: resultDir, Target: "/out"}, // rw results, host-readable
		}
		if _, err := os.Stat("/usr"); err == nil {
			m = append(m, sandbox.Mount{Source: "/usr", Target: "/usr", ReadOnly: true})
		}
		return m
	}

	// runProbe runs the script inside the jail and returns trimmed /out/result.
	runProbe := func(net bool, script string) string {
		GinkgoHelper()
		cfg := sandbox.Config{
			Command: []string{bashPath, "-c", script},
			Env:     []string{"PATH=/usr/bin:/bin", "HOME=/tmp"},
			NewRoot: newRoot,
			Mounts:  baseMounts(),
			Namespaces: sandbox.Namespaces{
				User: true, Mount: true, PID: true, UTS: true, IPC: true, Net: net,
			},
			Stdout: GinkgoWriter,
			Stderr: GinkgoWriter,
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		code, err := sandbox.Run(ctx, cfg)
		Expect(err).NotTo(HaveOccurred(), "Run must not fail at setup/infra level")
		Expect(code).To(Equal(0), "probe script should exit 0")

		var data []byte
		Eventually(func() error {
			b, rerr := os.ReadFile(filepath.Join(resultDir, "result"))
			if rerr != nil {
				return rerr
			}
			data = b
			return nil
		}, 5*time.Second, 50*time.Millisecond).Should(Succeed())
		return strings.TrimSpace(string(data))
	}

	It("blocks the network with net=none (CLONE_NEWNET)", func() {
		out := runProbe(true, `
( exec 3<>/dev/tcp/8.8.8.8/53 ) 2>/dev/null && echo NET_OPEN > /out/result || echo NET_BLOCKED > /out/result
`)
		Expect(out).To(Equal("NET_BLOCKED"))
	})

	It("enforces the read-only mount (EROFS on write)", func() {
		out := runProbe(false, `
( : > /usr/.jobs_probe ) 2>/dev/null && echo RW > /out/result || echo RO > /out/result
`)
		Expect(out).To(Equal("RO"))
	})

	It("pivots into the new root and execs as PID 1 in a fresh procfs", func() {
		// Reading /proc/self/stat (the fresh procfs after pivot) and seeing PID 1
		// proves pivot_root + fresh /proc + the re-exec-into-pidns all worked, and
		// /out being writable proves the writable bind survived the pivot.
		out := runProbe(false, `
stat=$(</proc/self/stat); pid=${stat%% *}
if [ -w /out ]; then printf 'PIVOT_OK pid=%s\n' "$pid" > /out/result
else echo PIVOT_BAD > /out/result; fi
`)
		Expect(out).To(HavePrefix("PIVOT_OK"))
		Expect(out).To(ContainSubstring("pid=1"))
	})
})
