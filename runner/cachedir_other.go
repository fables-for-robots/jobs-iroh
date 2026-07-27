//go:build !linux

package runner

import (
	"context"
	"time"

	"github.com/jobs-build/jobs-iroh/amber"
	"github.com/jobs-build/jobs-iroh/events"
)

// finalizeCaches is unreachable off Linux (the build executor returns
// ErrUnsupported before any cache exists); stub keeps the package compiling.
func finalizeCaches(ctx context.Context, st *amber.Store, caches []CacheMount, platform string, buildStart time.Time, ev *events.Job) ([]Ref, *Outcome) {
	return nil, nil
}
