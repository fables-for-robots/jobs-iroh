package recipe

import (
	"fmt"
	"path"
	"strings"

	"go.starlark.net/starlark"
)

// NormalizeSourcePath resolves one recipe-declared covered path
// (sibling-sources design §5.2) to root-relative form:
//
//	"//lib/common" → "lib/common"        (root-relative, prefix stripped)
//	"../lib"       → clean(dir + "/../lib") (sugar, resolved against dir)
//	"proto"        → clean(dir + "/proto")  (dir-relative, like source.read)
//
// The result must land strictly inside the context root and must not resolve
// to the root itself (covering the whole tree defeats the cutoff and is
// always a mistake). Absolute paths are rejected. The // form is surface
// syntax only — every landed path is plain root-relative.
func NormalizeSourcePath(p, dir string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("source path must be non-empty")
	}
	var out string
	switch {
	case strings.HasPrefix(p, "//"):
		out = path.Clean(strings.TrimPrefix(p, "//"))
		if strings.HasPrefix(out, "../") || out == ".." {
			return "", fmt.Errorf("source path %q escapes the context root", p)
		}
	case strings.HasPrefix(p, "/"):
		return "", fmt.Errorf("source path %q must not be absolute (use // for root-relative)", p)
	default:
		out = path.Clean(path.Join(dir, p))
		if strings.HasPrefix(out, "../") || out == ".." {
			return "", fmt.Errorf("source path %q escapes the context root (resolved against dir %q)", p, dir)
		}
	}
	if out == "." || out == "" {
		return "", fmt.Errorf("source path %q resolves to the context root — cover specific paths instead", p)
	}
	return out, nil
}

// decodeSources reads the optional sources= list of build()'s return: a list
// of path strings (// root-relative, or dir-relative with ../ sugar).
// Normalization happens in EvalBuild (it holds the build dir).
func decodeSources(v starlark.Value) ([]string, error) {
	l, ok := v.(*starlark.List)
	if !ok {
		return nil, fmt.Errorf("must be a list of path strings, got %s", v.Type())
	}
	out := make([]string, 0, l.Len())
	for i := 0; i < l.Len(); i++ {
		s, ok := starlark.AsString(l.Index(i))
		if !ok {
			return nil, fmt.Errorf("element %d is not a string (got %s)", i, l.Index(i).Type())
		}
		out = append(out, s)
	}
	return out, nil
}

// decodeGenerated reads the optional generated= dict of build()'s return:
// {path: content}, content a string or bytes value.
func decodeGenerated(v starlark.Value) (map[string][]byte, error) {
	d, ok := v.(*starlark.Dict)
	if !ok {
		return nil, fmt.Errorf("must be a dict {path: content}, got %s", v.Type())
	}
	out := make(map[string][]byte, d.Len())
	for _, item := range d.Items() {
		p, ok := starlark.AsString(item[0])
		if !ok {
			return nil, fmt.Errorf("key %v is not a string", item[0])
		}
		switch c := item[1].(type) {
		case starlark.String:
			out[p] = []byte(c)
		case starlark.Bytes:
			out[p] = []byte(c)
		default:
			return nil, fmt.Errorf("generated[%q] must be a string or bytes, got %s", p, item[1].Type())
		}
	}
	return out, nil
}

// normalizeBuildSources resolves the raw declared paths of a BuildResult
// against the build dir, in place: Sources, Closure and AllowEscaping
// element-wise, Generated keys re-mapped. Called by EvalBuild (the only
// holder of cfg.Dir).
func normalizeBuildSources(r *BuildResult, dir string) error {
	for i, p := range r.Sources {
		np, err := NormalizeSourcePath(p, dir)
		if err != nil {
			return fmt.Errorf("build() sources[%d]: %w", i, err)
		}
		r.Sources[i] = np
	}
	for i, p := range r.Closure {
		np, err := NormalizeSourcePath(p, dir)
		if err != nil {
			return fmt.Errorf("build() closure[%d]: %w", i, err)
		}
		r.Closure[i] = np
	}
	for i, p := range r.AllowEscaping {
		np, err := NormalizeSourcePath(p, dir)
		if err != nil {
			return fmt.Errorf("build() sources_allow_escaping[%d]: %w", i, err)
		}
		r.AllowEscaping[i] = np
	}
	if len(r.Generated) > 0 {
		ng := make(map[string][]byte, len(r.Generated))
		for p, content := range r.Generated {
			np, err := NormalizeSourcePath(p, dir)
			if err != nil {
				return fmt.Errorf("build() generated[%q]: %w", p, err)
			}
			if _, dup := ng[np]; dup {
				return fmt.Errorf("build() generated: %q declared twice after normalization", np)
			}
			ng[np] = content
		}
		r.Generated = ng
	}
	return nil
}
