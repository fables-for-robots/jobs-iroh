//go:build !linux

package runner

import (
	"context"

	"github.com/jobs-build/jobs-iroh/amber"
)

// NamespaceBuildExecutor is unavailable off Linux (no user namespaces /
// pivot_root hermetic sandbox).
type NamespaceBuildExecutor struct{}

// RunBuild always returns ErrUnsupported off Linux.
func (NamespaceBuildExecutor) RunBuild(ctx context.Context, st *amber.Store, spec BuildSpec) (BuildResult, error) {
	return BuildResult{ExitCode: -1}, ErrUnsupported
}

var _ BuildExecutor = NamespaceBuildExecutor{}
