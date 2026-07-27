package clientcli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// defaultBuildFile is the recipe name the upward search probes for when
// --build-file is unset — the same default runner.BuildFromEnv falls back to.
const defaultBuildFile = "BUILD.jobs"

// repoRoot walks up from dir looking for a `.git` entry and returns the
// directory holding it, or "" if there is none. The entry may be a DIRECTORY
// (ordinary clone) or a FILE (git worktree, submodule) — both mark a repo
// root, so the check is existence, not type.
//
// This is deliberately a filesystem walk rather than `git rev-parse
// --show-toplevel`: it is the single source of truth for both the context
// root (resolveContextRoot) and the upward-search ceiling (defaultSource),
// and two independent detections could disagree — worktrees, submodules,
// GIT_DIR, GIT_CEILING_DIRECTORIES and symlinked paths (git reports the
// physical path, filepath.Abs does not resolve symlinks) all produce
// divergence. It also drops the dependency on a `git` binary being on PATH.
func repoRoot(dir string) string {
	for {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "" // filesystem root, no repo
		}
		dir = parent
	}
}

// defaultSource fills in an omitted --source from the current directory: the
// nearest ancestor of cwd (cwd itself first) that holds the effective recipe,
// searching no higher than the context root. It runs BEFORE resolveContextRoot
// and only decides which directory that function is handed.
//
//   - dir != "": the user already named the build root, so no search happens —
//     cwd is the source and the usual rel+dir composition applies.
//   - ceiling: sourceRoot if given, else cwd when noRepoRoot, else the repo
//     root above cwd, else cwd (a non-repo tree searches cwd only).
//   - recipe: buildFile if given (a path relative to the build root, so a
//     value with slashes joins correctly), else BUILD.jobs.
//
// Nothing found is an error naming what was searched — a silent fallback to
// cwd would ingest the wrong tree and fail later with a confusing recipe
// error.
func defaultSource(cwd, dir, sourceRoot, buildFile string, noRepoRoot bool) (string, error) {
	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		return "", err
	}
	if dir != "" {
		return absCwd, nil
	}

	ceiling := absCwd
	switch {
	case sourceRoot != "":
		if ceiling, err = filepath.Abs(sourceRoot); err != nil {
			return "", err
		}
	case noRepoRoot:
		// ceiling stays absCwd — the pre-arc behavior ingests cwd itself.
	default:
		if r := repoRoot(absCwd); r != "" {
			ceiling = r
		}
	}

	// An explicit --source-root that does not contain the cwd has nothing to
	// search: say so here rather than walking to / and reporting a missing
	// recipe, which would hide the real mistake.
	if rel, rerr := filepath.Rel(ceiling, absCwd); rerr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("current directory %s is not under the context root %s", absCwd, ceiling)
	}

	recipe := buildFile
	if recipe == "" {
		recipe = defaultBuildFile
	}
	for cand := absCwd; ; cand = filepath.Dir(cand) {
		if _, err := os.Stat(filepath.Join(cand, recipe)); err == nil {
			return cand, nil
		}
		if cand == ceiling || cand == filepath.Dir(cand) {
			break
		}
	}
	if ceiling == absCwd {
		return "", fmt.Errorf("no %s in %s (pass --source <dir>)", recipe, absCwd)
	}
	return "", fmt.Errorf("no %s in %s or any parent up to %s (pass --source <dir>)", recipe, absCwd, ceiling)
}

// resolveContextRoot applies the repo-root context default (sibling-sources
// design §11.1): unless disabled, a --source dir inside a git repository is
// re-anchored so the INGEST ROOT is the repository toplevel and dir becomes
// the source path relative to it (composed with any explicit --dir beneath
// it). This is what makes sibling references (../lib, //lib/common)
// resolvable — the whole repo is the context, pin narrows to the covered
// closure. The rule is IDENTICAL for local and remote paths — divergence
// would silently kill the local↔remote F join. Identity never depends on
// git itself: the detection only chooses which tree gets ingested.
//
//   - sourceRoot != "": explicit root; source must live under it.
//   - noRepoRoot: the pre-arc behavior — root = source, dir unchanged.
//   - otherwise: the repo root above source; a source in no repository falls
//     back to root = source.
func resolveContextRoot(source, dir, sourceRoot string, noRepoRoot bool) (root, outDir string, err error) {
	absSource, err := filepath.Abs(source)
	if err != nil {
		return "", "", err
	}
	switch {
	case sourceRoot != "":
		root, err = filepath.Abs(sourceRoot)
		if err != nil {
			return "", "", err
		}
	case noRepoRoot:
		return absSource, dir, nil
	default:
		root = repoRoot(absSource)
		if root == "" {
			return absSource, dir, nil // not in a repo — pre-arc behavior
		}
	}
	rel, err := filepath.Rel(root, absSource)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("--source %s is not under the context root %s", source, root)
	}
	outDir = filepath.ToSlash(filepath.Join(rel, dir))
	if outDir == "." {
		outDir = ""
	}
	return root, outDir, nil
}

// resolveSource is the whole client-side context resolution in one call:
// infer an omitted --source from the cwd (defaultSource), then re-anchor it
// to the context root (resolveContextRoot). Every source-building command
// goes through exactly this, in exactly this order — the local↔remote F join
// depends on the two paths agreeing.
func resolveSource(source, dir, sourceRoot, buildFile string, noRepoRoot bool) (root, outDir string, err error) {
	if source == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", "", err
		}
		if source, err = defaultSource(cwd, dir, sourceRoot, buildFile, noRepoRoot); err != nil {
			return "", "", err
		}
	}
	return resolveContextRoot(source, dir, sourceRoot, noRepoRoot)
}

// contextLine is the one-line resolution report every source-building command
// prints to stderr before ingesting. The ingest root can be an entire
// repository, so what got resolved is always shown — inferred or not.
func contextLine(root, dir, buildFile string) string {
	if dir == "" {
		dir = "."
	}
	if buildFile == "" {
		buildFile = defaultBuildFile
	}
	return fmt.Sprintf("context: %s  (dir %s, recipe %s)", root, dir, buildFile)
}
