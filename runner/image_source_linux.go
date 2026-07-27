//go:build linux

package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/jobs-build/jobs-iroh/amber"
)

// BuildImageFromSource builds cfg's target hermetically (exactly as `develop`
// and `run` prepare it) and writes a docker-load-able image tarball of the
// built artifact to opts.Output, reusing its JOBS.entrypoint. Linux-only: it
// drives the namespace build sandbox. Publishes build-output:F +
// build-output-deps:F so subsequent image builds of the same target skip the
// build.
func BuildImageFromSource(ctx context.Context, st *amber.Store, cfg DevelopConfig, platform string, opts ImageOptions) error {
	if platform == "" {
		platform = Platform()
	}
	p := NewProgress(os.Stderr)
	art, err := prepareSourceArtifact(ctx, st, cfg, p)
	if err != nil {
		return err
	}
	img, err := assembleImage(ctx, st, art.bokSelf, art.depBOKs, art.shellKey, art.ep, platform, opts.IncludeShell)
	if err != nil {
		return err
	}
	tag := opts.Tag
	if tag == "" {
		tag = strings.ToLower(filepath.Base(cfg.SourceDir)) + ":latest"
	}
	return writeImageTar(img, tag, opts.Output)
}
