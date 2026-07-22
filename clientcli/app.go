// Package clientcli wires the jobs-client command surface: hermetic LOCAL
// builds (`build`) and build-then-execute (`run`) over the embedded
// amber-store-core store under --data-dir. It is the jobs-iroh port of jobs'
// internal/jobscli local path with the store seam swapped (methods on
// *amber.Store) and the signing/grant plumbing deleted — local refs are plain
// unsigned refstore records. The remote commands (remote build, status,
// admin) arrive with the server milestone.
package clientcli

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/urfave/cli/v2"
)

// signalCtx derives the command context cancelled on Ctrl-C / SIGTERM so an
// in-flight build tears down its sandbox and the action's defers (store
// close, flock release) still run.
func signalCtx(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
}

// App builds the jobs-client CLI. A bare invocation prints help.
func App() *cli.App {
	return &cli.App{
		Name:  "jobs-client",
		Usage: "jobs-iroh end-user CLI: hermetic local builds over the embedded store",
		Commands: []*cli.Command{
			buildCmd(),
			runCmd(),
		},
	}
}
