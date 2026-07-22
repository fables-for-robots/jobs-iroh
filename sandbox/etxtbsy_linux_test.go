//go:build linux

package sandbox_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/fables-for-robots/jobs-iroh/sandbox"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// A concurrently forked child of a multi-slot runner can inherit the write fd
// of a just-extracted binary and hold it until its own execve, so the sandbox
// child's execve of that binary fails ETXTBSY (golang/go#22315). The writer is
// always moribund — it vanishes as soon as the other child execs — so the
// sandbox child must retry briefly instead of dying with exit 127.
var _ = Describe("sandbox child exec ETXTBSY retry", func() {
	if !sandbox.UserNSAvailable() {
		Skip("user namespaces unavailable")
	}

	It("retries execve while the target is briefly open for write", func() {
		dir := GinkgoTB().TempDir()
		target := filepath.Join(dir, "prog")
		Expect(os.WriteFile(target, []byte("#!/bin/sh\nexit 0\n"), 0o755)).To(Succeed())

		// Keep a write fd open well past the sandbox re-exec + setup, so the
		// child's first execve attempts deterministically hit ETXTBSY.
		f, err := os.OpenFile(target, os.O_WRONLY, 0)
		Expect(err).NotTo(HaveOccurred())
		time.AfterFunc(400*time.Millisecond, func() { f.Close() })

		var stderr bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		code, err := sandbox.Run(ctx, sandbox.Config{
			Command:    []string{target},
			Env:        os.Environ(),
			Namespaces: sandbox.Namespaces{User: true, Mount: true, PID: true},
			Stderr:     &stderr,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(0), "stderr: "+stderr.String())
	})
})
