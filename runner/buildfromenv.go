package runner

import (
	"context"
	"os"
	"path/filepath"

	"github.com/fables-for-robots/amber-store-core/key"
	"github.com/fables-for-robots/jobs-iroh/amber"
	"github.com/fables-for-robots/jobs-iroh/recipe"
)

// buildFromEnv is the materialized eval environment of a build-from tree at F:
// the env/ source, its on-disk root (for plugin sandboxes), and the platform,
// params, and effective recipe read from the F-tree (build-from design §4, §7).
type buildFromEnv struct {
	Source           recipe.Source
	SrcRoot          string
	Platform         string
	Params           []byte
	Recipe           []byte
	SourceContentKey key.Key // the env/ subtree content key (subbuild's build root)
	cleanup          func()
}

// loadBuildFromEnv tars the whole build-from tree at f to disk and reads its
// pieces. The recipe is the top-level BUILD.jobs override if present, else
// env/BUILD.jobs. Returns a *Outcome on error (the cleanup is a no-op then).
func loadBuildFromEnv(ctx context.Context, st *amber.Store, f key.Key) (buildFromEnv, *Outcome) {
	if o := ensureBuildFromTree(ctx, st, f); o != nil {
		return buildFromEnv{}, o
	}
	root, err := os.MkdirTemp("", "jobs-buildfrom-")
	if err != nil {
		o := retryable("materializing", err)
		return buildFromEnv{}, &o
	}
	cleanup := func() { _ = os.RemoveAll(root) }

	rc, err := st.Tar(ctx, f, "")
	if err != nil {
		cleanup()
		o := retryable("materializing", err)
		return buildFromEnv{}, &o
	}
	if err := extractTar(rc, root); err != nil {
		rc.Close()
		cleanup()
		o := retryable("materializing", err)
		return buildFromEnv{}, &o
	}
	rc.Close()

	srcContentKey, err := resolveSubdirKey(ctx, st, f, "env")
	if err != nil {
		cleanup()
		o := hard("materializing", "build-from tree missing env: "+err.Error(), 0)
		return buildFromEnv{}, &o
	}

	srcRoot := filepath.Join(root, "env")
	src := diskSource{root: srcRoot}

	params, err := os.ReadFile(filepath.Join(root, "params"))
	if err != nil {
		cleanup()
		o := hard("materializing", "build-from tree missing params: "+err.Error(), 0)
		return buildFromEnv{}, &o
	}
	platform, err := os.ReadFile(filepath.Join(root, "platform"))
	if err != nil {
		cleanup()
		o := hard("materializing", "build-from tree missing platform: "+err.Error(), 0)
		return buildFromEnv{}, &o
	}

	var recipeSrc []byte
	if b, err := os.ReadFile(filepath.Join(root, "BUILD.jobs")); err == nil {
		recipeSrc = b // top-level override
	} else if b, err := src.Read("BUILD.jobs"); err == nil {
		recipeSrc = b // env/BUILD.jobs
	} else {
		cleanup()
		o := hard("materializing", "no effective BUILD.jobs in build-from tree", 0)
		return buildFromEnv{}, &o
	}

	return buildFromEnv{
		Source:           src,
		SrcRoot:          srcRoot,
		Platform:         string(platform),
		Params:           params,
		Recipe:           recipeSrc,
		SourceContentKey: srcContentKey,
		cleanup:          cleanup,
	}, nil
}

// ensureBuildFromTree verifies the self-referential build-from-tree:F ref is
// readable before a raw content-key read of F's tree (loadBuildFromEnv's
// st.Tar, or the build executor's SourceKey:f mount). Single-store world:
// there is no closure pull (jobs pulled the ref through RemoteSync here) — the
// ref being absent is NOT an error (F may be local-only, e.g. on the local
// paths); only a genuine ref-store failure becomes a retryable Outcome.
func ensureBuildFromTree(ctx context.Context, st *amber.Store, f key.Key) *Outcome {
	if _, _, err := st.GetKey(ctx, "build-from-tree:"+f.String()); err != nil {
		o := retryable("pulling build-from tree", err)
		return &o
	}
	return nil
}

// ensureBuildDef verifies the build:K bookkeeping ref is readable before
// pullAndDecodeDefinition's st.File(K). Single-store analogue of jobs'
// closure pull: the ref being absent is NOT an error (K's def objects may be
// present without it); a ref-store failure is retryable.
func ensureBuildDef(ctx context.Context, st *amber.Store, k key.Key) *Outcome {
	if _, _, err := st.GetKey(ctx, "build:"+k.String()); err != nil {
		o := retryable("pulling build def", err)
		return &o
	}
	return nil
}
