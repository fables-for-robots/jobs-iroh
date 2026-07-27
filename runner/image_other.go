//go:build !linux

package runner

import (
	"context"
	"fmt"

	"github.com/jobs-build/jobs-iroh/amber"
)

// BuildImageFromSource is Linux-only because it drives the namespace build
// sandbox. (BuildImageByKey, which only reads amber trees, is cross-platform
// and lives in image.go.)
func BuildImageFromSource(ctx context.Context, st *amber.Store, cfg DevelopConfig, platform string, opts ImageOptions) error {
	return fmt.Errorf("jobs-client image --source is only supported on Linux (build sandbox unavailable)")
}
