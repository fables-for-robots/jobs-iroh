package main

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// goClosure computes the transitive local-package closure of the entry
// package dirs (source-closure design §8): pure filesystem + go/parser, no
// toolchain, deterministic. The //-rooted output covers reached package dirs
// (recursive covers — embeds/cgo/asm/testdata ride along), each involved
// module's go.mod/go.sum, and go.work(.sum) at the context root when a
// go.work was passed. Imports from EVERY .go file count — build-tag-ignored
// and _test.go included (cross-compile and `go test` safety; gosha's
// IgnoredFiles rationale). Stdlib (first path element without a dot, incl.
// the cgo pseudo-import "C") and external modules (the go.sum fetcher's
// territory) are skipped.
//
// A reached package that sits at a MODULE ROOT cannot be covered as a dir
// (that would cover the whole module — and the context root cannot be a
// covered path at all): its regular files are enumerated non-recursively
// and its //go:embed patterns are glob-resolved so subdir embed targets
// stay covered.
func goClosure(srcRoot, dir string, gomod, gowork []byte, entries []string) ([]string, error) {
	consumerPath := modulePath(gomod)
	if consumerPath == "" {
		return nil, fmt.Errorf("go_closure: go_mod has no module directive")
	}

	// Local module map: module path → root-relative dir ("" = context root).
	mods := map[string]string{consumerPath: dir}
	rels := relDirectives(gomod)
	if len(gowork) > 0 {
		rels = append(rels, relDirectives(gowork)...)
	}
	for _, r := range rels {
		mdir := rootRel(dir, r)
		if mdir == ".." || strings.HasPrefix(mdir, "../") {
			return nil, fmt.Errorf("go_closure: replace/use target %q escapes the context root", r)
		}
		mb, err := os.ReadFile(filepath.Join(srcRoot, filepath.FromSlash(mdir), "go.mod"))
		if err != nil {
			return nil, fmt.Errorf("go_closure: sibling %q has no readable go.mod: %w", mdir, err)
		}
		mp := modulePath(mb)
		if mp == "" {
			return nil, fmt.Errorf("go_closure: sibling %q go.mod has no module directive", mdir)
		}
		mods[mp] = mdir
	}
	modRoots := map[string]bool{}
	for _, d := range mods {
		modRoots[d] = true
	}

	// Transitive walk from the entry package dirs.
	var frontier []string
	for _, e := range entries {
		ed := rootRel(dir, e)
		if ed == ".." || strings.HasPrefix(ed, "../") {
			return nil, fmt.Errorf("go_closure: entry %q escapes the context root", e)
		}
		frontier = append(frontier, ed)
	}
	visited := map[string]bool{}
	usedMods := map[string]bool{consumerPath: true}
	for len(frontier) > 0 {
		pkgDir := frontier[0]
		frontier = frontier[1:]
		if visited[pkgDir] {
			continue
		}
		visited[pkgDir] = true
		imports, n, err := dirImports(hostPath(srcRoot, pkgDir))
		if err != nil {
			return nil, fmt.Errorf("go_closure: %s: %w", displayPath(pkgDir), err)
		}
		if n == 0 {
			return nil, fmt.Errorf("go_closure: %q contains no .go files", displayPath(pkgDir))
		}
		for _, imp := range imports {
			mp, mdir, ok := resolveLocal(mods, imp)
			if !ok {
				continue // stdlib or external
			}
			pd := rootRel(mdir, strings.TrimPrefix(strings.TrimPrefix(imp, mp), "/"))
			usedMods[mp] = true
			if !visited[pd] {
				frontier = append(frontier, pd)
			}
		}
	}

	// Assemble output paths.
	out := map[string]bool{}
	for pd := range visited {
		if !modRoots[pd] {
			out[pd] = true
			continue
		}
		files, err := regularFiles(hostPath(srcRoot, pd))
		if err != nil {
			return nil, fmt.Errorf("go_closure: %s: %w", displayPath(pd), err)
		}
		for _, f := range files {
			out[path.Join(pd, f)] = true
		}
		embeds, err := embedTargets(hostPath(srcRoot, pd))
		if err != nil {
			return nil, fmt.Errorf("go_closure: %s: %w", displayPath(pd), err)
		}
		for _, e := range embeds {
			out[path.Join(pd, e)] = true
		}
	}
	for mp := range usedMods {
		mdir := mods[mp]
		out[path.Join(mdir, "go.mod")] = true
		if _, err := os.Stat(filepath.Join(hostPath(srcRoot, mdir), "go.sum")); err == nil {
			out[path.Join(mdir, "go.sum")] = true
		}
	}
	if len(gowork) > 0 {
		for _, f := range []string{"go.work", "go.work.sum"} {
			if _, err := os.Stat(filepath.Join(srcRoot, f)); err == nil {
				out[f] = true
			}
		}
	}

	// Collapse nested under covered ancestors, sort, //-root.
	paths := make([]string, 0, len(out))
	for p := range out {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var collapsed []string
	for _, p := range paths {
		skip := false
		for _, k := range collapsed {
			if p == k || strings.HasPrefix(p, k+"/") {
				skip = true
				break
			}
		}
		if !skip {
			collapsed = append(collapsed, p)
		}
	}
	final := make([]string, len(collapsed))
	for i, p := range collapsed {
		final[i] = "//" + p
	}
	return final, nil
}

// rootRel joins a base root-relative dir with a relative path and cleans to
// root-relative form; "" means the context root itself.
func rootRel(base, rel string) string {
	p := path.Clean(path.Join(base, rel))
	if p == "." {
		return ""
	}
	return p
}

// hostPath maps a root-relative path to the host filesystem.
func hostPath(srcRoot, rel string) string {
	if rel == "" {
		return srcRoot
	}
	return filepath.Join(srcRoot, filepath.FromSlash(rel))
}

// displayPath renders "" as "." for error messages.
func displayPath(rel string) string {
	if rel == "" {
		return "."
	}
	return rel
}

// modulePath extracts the module directive's path from go.mod bytes.
func modulePath(gomod []byte) string {
	for _, line := range strings.Split(string(gomod), "\n") {
		line = strings.TrimSpace(line)
		if i := strings.Index(line, "//"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if strings.HasPrefix(line, "module ") {
			return strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "module ")), `"`)
		}
	}
	return ""
}

// dirImports parses EVERY .go file in one directory (ImportsOnly) and returns
// the sorted union of import paths, plus the .go file count.
func dirImports(hostDir string) ([]string, int, error) {
	ents, err := os.ReadDir(hostDir)
	if err != nil {
		return nil, 0, err
	}
	fset := token.NewFileSet()
	seen := map[string]bool{}
	n := 0
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		n++
		f, err := parser.ParseFile(fset, filepath.Join(hostDir, e.Name()), nil, parser.ImportsOnly)
		if err != nil {
			return nil, 0, fmt.Errorf("%s: %w", e.Name(), err)
		}
		for _, im := range f.Imports {
			seen[strings.Trim(im.Path.Value, `"`)] = true
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, n, nil
}

// resolveLocal longest-prefix-matches imp against the local module map.
// Stdlib (first element without a dot, incl. "C") and unmatched (external)
// → ok=false.
func resolveLocal(mods map[string]string, imp string) (mp, dir string, ok bool) {
	first := imp
	if i := strings.IndexByte(imp, '/'); i >= 0 {
		first = imp[:i]
	}
	if !strings.Contains(first, ".") {
		return "", "", false
	}
	best := ""
	for m := range mods {
		if (imp == m || strings.HasPrefix(imp, m+"/")) && len(m) > len(best) {
			best = m
		}
	}
	if best == "" {
		return "", "", false
	}
	return best, mods[best], true
}

// regularFiles lists the non-directory entries of one directory, sorted.
// (.git cannot appear — ingest excludes it — but skip it defensively.)
func regularFiles(hostDir string) ([]string, error) {
	ents, err := os.ReadDir(hostDir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() || e.Name() == ".git" {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}

// embedTargets line-scans every .go file in one directory for //go:embed
// directives and glob-resolves the patterns against the directory (fs.Glob —
// embed patterns are /-separated and cannot escape the package dir). A
// pattern matching nothing is a hard error: `go build` would fail on it too,
// and silently under-covering is the one failure mode the closure must not
// have. The optional all: prefix is stripped.
func embedTargets(hostDir string) ([]string, error) {
	ents, err := os.ReadDir(hostDir)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(hostDir, e.Name()))
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "//go:embed ") {
				continue
			}
			for _, pat := range strings.Fields(strings.TrimPrefix(line, "//go:embed ")) {
				pat = strings.Trim(strings.TrimPrefix(pat, "all:"), `"`)
				matches, err := fs.Glob(os.DirFS(hostDir), pat)
				if err != nil {
					return nil, fmt.Errorf("embed pattern %q: %w", pat, err)
				}
				if len(matches) == 0 {
					return nil, fmt.Errorf("embed pattern %q matches nothing in %s", pat, e.Name())
				}
				for _, m := range matches {
					seen[m] = true
				}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Strings(out)
	return out, nil
}
