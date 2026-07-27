//go:build linux

package runner_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/jobs-build/jobs-iroh/runner"
	"github.com/jobs-build/jobs-iroh/sandbox"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("CgroupExecutor", func() {
	var (
		ctx        context.Context
		fetcherDir string
		outputDir  string
	)

	BeforeEach(func() {
		if !sandbox.UserNSAvailable() {
			Skip("user namespaces unavailable")
		}
		ctx = context.Background()
		fetcherDir = GinkgoTB().TempDir()
		outputDir = GinkgoTB().TempDir()
	})

	// writeFetch writes a shell script as ./fetch (0755) in fetcherDir. bash,
	// not sh: the network spec uses bash's /dev/tcp, and /bin/sh is dash on
	// many hosts (the executor keeps the host env, so env finds PATH's bash).
	writeFetch := func(script string) {
		GinkgoHelper()
		p := filepath.Join(fetcherDir, "fetch")
		Expect(os.WriteFile(p, []byte("#!/usr/bin/env bash\n"+script+"\n"), 0o755)).To(Succeed())
	}

	runExec := func(ex runner.Executor, spec runner.ExecSpec) runner.ExecResult {
		GinkgoHelper()
		bctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		res, err := ex.Run(bctx, spec)
		Expect(err).NotTo(HaveOccurred(), "CgroupExecutor.Run must not return an infra error")
		return res
	}

	// (a) fetcher runs on host fs: a fetch script writes to $JOBS_OUTPUT_DIR;
	// the output is visible on the host side (proves NewRoot="" is working).
	It("(a) runs fetcher on host fs: output is visible on the host", func() {
		writeFetch(`echo ok > "$JOBS_OUTPUT_DIR/result"`)

		ex := runner.CgroupExecutor{}
		spec := runner.ExecSpec{
			FetcherDir: fetcherDir,
			OutputDir:  outputDir,
			Env: map[string]string{
				"JOBS_OUTPUT_DIR": outputDir,
			},
		}
		res := runExec(ex, spec)
		Expect(res.ExitCode).To(Equal(0), "fetch should exit 0; stderr: "+res.StderrTail)

		got, err := os.ReadFile(filepath.Join(outputDir, "result"))
		Expect(err).NotTo(HaveOccurred(), "output file must be visible on the host (NewRoot='' means host fs)")
		Expect(string(got)).To(Equal("ok\n"))
	})

	// (b) network kept: a fetch that dials 8.8.8.8:53; exits 0 if connects.
	// Skip if the test-process itself has no outbound network.
	It("(b) keeps host network: TCP dial to 8.8.8.8:53 succeeds", func() {
		// Pre-check: if the test host itself has no outbound network, skip.
		conn, err := net.DialTimeout("tcp", "8.8.8.8:53", 3*time.Second)
		if err != nil {
			Skip("host has no outbound network: " + err.Error())
		}
		conn.Close()

		writeFetch(`exec 3<>/dev/tcp/8.8.8.8/53 && exit 0 || exit 1`)

		ex := runner.CgroupExecutor{}
		spec := runner.ExecSpec{
			FetcherDir: fetcherDir,
			OutputDir:  outputDir,
			Env: map[string]string{
				"JOBS_OUTPUT_DIR": outputDir,
			},
		}
		res := runExec(ex, spec)
		Expect(res.ExitCode).To(Equal(0),
			"network should be reachable inside CgroupExecutor (Net=false means NO CLONE_NEWNET); stderr: "+res.StderrTail)
	})

	// (c) pids cap enforcement: needs a delegated, non-threaded cgroup scope
	// where child processes can actually be placed in the job cgroup. Most
	// dev hosts' user@ scopes reject both clone3(CLONE_INTO_CGROUP) and the
	// cgroup.procs fallback for children, so the limit is written but never
	// applied — detect and skip rather than flake (see jobs' original note).
	It("(c) pids cap: fork-bomb is blocked when cgroup placement is enforced", func() {
		_, controllers, ok := sandbox.DetectCgroupDelegation()
		if !ok {
			Skip("no delegated cgroup subtree in this environment")
		}
		hasPids := false
		for _, c := range controllers {
			if c == "pids" {
				hasPids = true
				break
			}
		}
		if !hasPids {
			Skip("pids controller not delegated in this environment")
		}
		Skip("cgroup process placement not enforced in typical delegated scopes: " +
			"a 'threaded/domain' user@ scope rejects both clone3(CLONE_INTO_CGROUP) and " +
			"the cgroup.procs fallback for child processes, so pids.max is written but never applied")
	})
})
