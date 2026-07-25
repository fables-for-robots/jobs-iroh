package main

import (
	"path"
	"sort"
	"strings"
)

// relDirectives extracts the RELATIVE filesystem targets of a go.mod's
// replace directives (and, for go.work content, its use directives): the
// monorepo sibling closure. Go's own rules make this complete — replace
// directives are honored ONLY in the main module (a sibling's replaces are
// ignored), and filesystem replace targets MUST start with ./ or ../ — so
// the consumer's manifest literally enumerates every local sibling, no
// transitive walk needed (sibling-sources design §12.1). Hand-parsed like
// gosum.go: single-line and block forms, deterministic output order.
func relDirectives(gomod []byte) []string {
	var out []string
	inReplace, inUse := false, false
	for _, line := range strings.Split(string(gomod), "\n") {
		line = strings.TrimSpace(line)
		if i := strings.Index(line, "//"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		switch {
		case line == "":
			continue
		case inReplace || inUse:
			if line == ")" {
				inReplace, inUse = false, false
				continue
			}
			if inReplace {
				out = appendRelTarget(out, line)
			} else if t := relPath(line); t != "" {
				out = append(out, t)
			}
			continue
		case line == "replace (":
			inReplace = true
		case line == "use (":
			inUse = true
		case strings.HasPrefix(line, "replace "):
			out = appendRelTarget(out, strings.TrimPrefix(line, "replace "))
		case strings.HasPrefix(line, "use "):
			if t := relPath(strings.TrimPrefix(line, "use ")); t != "" {
				out = append(out, t)
			}
		}
	}
	sort.Strings(out)
	return dedup(out)
}

// appendRelTarget parses one replace clause "old [version] => new [version]"
// and appends new when it is a relative filesystem path.
func appendRelTarget(out []string, clause string) []string {
	parts := strings.SplitN(clause, "=>", 2)
	if len(parts) != 2 {
		return out
	}
	rhs := strings.Fields(strings.TrimSpace(parts[1]))
	if len(rhs) == 0 {
		return out
	}
	if t := relPath(rhs[0]); t != "" {
		return append(out, t)
	}
	return out
}

// relPath returns the cleaned path when s is a relative filesystem target
// (./x or ../x — Go's requirement for local targets), else "".
func relPath(s string) string {
	s = strings.Trim(s, `"`)
	if strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") || s == "." || s == ".." {
		return s
	}
	return ""
}

func dedup(in []string) []string {
	out := in[:0]
	for i, s := range in {
		if i > 0 && in[i-1] == s {
			continue
		}
		out = append(out, s)
	}
	return out
}

// siblingSources resolves the relative directive targets against the
// consumer's dir within the context, emitting sorted //-rooted covered
// paths for the recipe to forward into build() sources=. Targets that
// resolve inside the consumer dir itself are dropped (already covered by
// the dir seed); targets escaping the context root are emitted anyway —
// the pin-stage walker owns escape validation and its error message names
// the path.
func siblingSources(rels []string, dir string) []string {
	var out []string
	for _, r := range rels {
		p := path.Clean(path.Join(dir, r))
		if p == "." {
			continue
		}
		if dir != "" && (p == dir || strings.HasPrefix(p, dir+"/")) {
			continue // inside the build dir — the dir seed covers it
		}
		out = append(out, "//"+p)
	}
	sort.Strings(out)
	return dedup(out)
}
