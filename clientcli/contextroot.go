package clientcli

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

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
//   - otherwise: git -C <source> rev-parse --show-toplevel; a non-git source
//     (or missing git binary) falls back to root = source.
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
		out, gerr := exec.Command("git", "-C", absSource, "rev-parse", "--show-toplevel").Output()
		if gerr != nil {
			return absSource, dir, nil // not a git repo (or no git) — pre-arc behavior
		}
		root = strings.TrimSpace(string(out))
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
