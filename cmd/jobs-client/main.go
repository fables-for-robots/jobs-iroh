// Command jobs-client is the jobs-iroh end-user CLI: hermetic local builds
// (build) and build-then-execute (run) against the embedded store under
// --data-dir/JOBS_DATA_DIR. Command wiring lives in clientcli.
package main

import (
	"fmt"
	"os"

	"github.com/jobs-build/jobs-iroh/clientcli"
	"github.com/jobs-build/jobs-iroh/sandbox"
)

func main() {
	// sandbox.Init() MUST run first: the local build commands re-exec
	// /proc/self/exe to enter the build sandbox; Init detects the re-exec'd
	// child and performs the in-namespace setup before the CLI parser sees
	// anything.
	sandbox.Init()
	if err := clientcli.App().Run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
