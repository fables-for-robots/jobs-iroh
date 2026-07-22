package runner

import (
	"context"
	"os"
	"path/filepath"

	"github.com/fables-for-robots/jobs-iroh/amber"
	"github.com/fables-for-robots/jobs-iroh/builddef"
)

// pluginDepsDir is the fixed in-plugin-sandbox root under which resolution deps
// are mounted, one subdir per dep name (resolution-deps design §3.3). Declared
// here (not in the linux-only plugincaller.go) because the pin stage derives
// dep.path() values from it on every platform.
const pluginDepsDir = "/jobs/deps"

// materializeDeps resolves every resolution dep's output artifact and extracts
// it to disk (resolution-deps design §6.2): name → host dir. The returned
// cleanup removes the whole tree; when a non-nil Outcome is returned, cleanup
// has already run. A missing output is retryable — the scheduler's pin|F edges
// (local path: the ensure loop) guarantee it eventually exists.
func materializeDeps(ctx context.Context, st *amber.Store, deps map[string]builddef.Input) (map[string]string, func(), *Outcome) {
	if len(deps) == 0 {
		return nil, func() {}, nil
	}
	root, err := os.MkdirTemp("", "jobs-pindeps-")
	if err != nil {
		o := retryable("materializing", err)
		return nil, nil, &o
	}
	cleanup := func() { _ = os.RemoveAll(root) }
	fail := func(o Outcome) (map[string]string, func(), *Outcome) {
		cleanup()
		return nil, nil, &o
	}

	dirs := make(map[string]string, len(deps))
	for name, in := range deps {
		// Defensive re-check: the name becomes a path segment here. Recipe
		// eval validates at production, but a hand-crafted PluginResolved
		// blob must not escape the deps root.
		if err := builddef.ValidateDepName(name); err != nil {
			return fail(hard("materializing", err.Error(), 0))
		}
		// Kind-aware artifact resolution, shared with the build-input path:
		// import → import-output tree, build → the c/ subtree (retryable while
		// the output does not exist yet).
		out, o := resolveInputOutputKey(ctx, st, builddef.PinnedInput{Name: name, Kind: in.Kind, Definition: in.Definition})
		if o != nil {
			cleanup()
			return nil, nil, o
		}
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fail(retryable("materializing", err))
		}
		rc, err := st.Tar(ctx, out, "")
		if err != nil {
			return fail(retryable("materializing", err))
		}
		exErr := extractTar(rc, dir)
		rc.Close()
		if exErr != nil {
			return fail(retryable("materializing", exErr))
		}
		dirs[name] = dir
	}
	return dirs, cleanup, nil
}
