package recipe

import (
	"fmt"
	"sort"
	"strings"

	"go.starlark.net/starlark"
)

// depsUnavailable is the plugins()-stage `deps` predeclared value. The name
// must resolve at compile time in BOTH stages (see basePredeclared's struct
// note), but resolution deps are DECLARED by plugins() — so any access there
// is a hard recipe error (resolution-deps design §3.2).
type depsUnavailable struct{}

var (
	_ starlark.Value   = depsUnavailable{}
	_ starlark.Mapping = depsUnavailable{}
)

func (depsUnavailable) String() string        { return "deps" }
func (depsUnavailable) Type() string          { return "deps" }
func (depsUnavailable) Freeze()               {}
func (depsUnavailable) Truth() starlark.Bool  { return starlark.True }
func (depsUnavailable) Hash() (uint32, error) { return 0, fmt.Errorf("deps is unhashable") }

func (depsUnavailable) Get(starlark.Value) (starlark.Value, bool, error) {
	return nil, false, fmt.Errorf("deps are not available during plugins() — they are declared here and materialized for build()")
}

// DepSource is one materialized resolution dep at pin time: read access to its
// tree plus the fixed path it is mounted at inside every plugin call sandbox
// (resolution-deps design §3.2).
type DepSource struct {
	Source      Source
	SandboxPath string // "/jobs/deps/<name>"
}

// depsMapping is the build()-stage `deps` predeclared value: name → handle.
type depsMapping struct{ deps map[string]DepSource }

var (
	_ starlark.Value   = depsMapping{}
	_ starlark.Mapping = depsMapping{}
)

func (depsMapping) String() string        { return "deps" }
func (depsMapping) Type() string          { return "deps" }
func (depsMapping) Freeze()               {}
func (depsMapping) Truth() starlark.Bool  { return starlark.True }
func (depsMapping) Hash() (uint32, error) { return 0, fmt.Errorf("deps is unhashable") }

func (d depsMapping) Get(k starlark.Value) (starlark.Value, bool, error) {
	name, ok := starlark.AsString(k)
	if !ok {
		return nil, false, fmt.Errorf("deps key must be a string")
	}
	ds, ok := d.deps[name]
	if !ok {
		names := make([]string, 0, len(d.deps))
		for n := range d.deps {
			names = append(names, n)
		}
		sort.Strings(names)
		return nil, false, fmt.Errorf("no resolution dep %q (declared: %s)", name, strings.Join(names, ", "))
	}
	return depHandle{ds: ds}, true, nil
}

// depHandle exposes read/exists/path on one resolution dep, mirroring the
// `source` value (read/exists) plus path() for pointing plugins at files
// inside their sandbox mount.
type depHandle struct{ ds DepSource }

var (
	_ starlark.Value    = depHandle{}
	_ starlark.HasAttrs = depHandle{}
)

func (depHandle) String() string        { return "dep" }
func (depHandle) Type() string          { return "dep" }
func (depHandle) Freeze()               {}
func (depHandle) Truth() starlark.Bool  { return starlark.True }
func (depHandle) Hash() (uint32, error) { return 0, fmt.Errorf("dep is unhashable") }

func (depHandle) AttrNames() []string { return []string{"exists", "path", "read"} }

func (h depHandle) Attr(name string) (starlark.Value, error) {
	switch name {
	case "read":
		return starlark.NewBuiltin("dep.read", func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			var path string
			if err := starlark.UnpackArgs("read", args, kwargs, "path", &path); err != nil {
				return nil, err
			}
			data, err := h.ds.Source.Read(path)
			if err != nil {
				return nil, err
			}
			return starlark.Bytes(data), nil
		}), nil
	case "exists":
		return starlark.NewBuiltin("dep.exists", func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			var path string
			if err := starlark.UnpackArgs("exists", args, kwargs, "path", &path); err != nil {
				return nil, err
			}
			return starlark.Bool(h.ds.Source.Exists(path)), nil
		}), nil
	case "path":
		return starlark.NewBuiltin("dep.path", func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			rel := ""
			if err := starlark.UnpackArgs("path", args, kwargs, "rel?", &rel); err != nil {
				return nil, err
			}
			if rel == "" {
				return starlark.String(h.ds.SandboxPath), nil
			}
			if strings.HasPrefix(rel, "/") {
				return nil, fmt.Errorf("dep.path: %q must be relative", rel)
			}
			for _, seg := range strings.Split(rel, "/") {
				if seg == ".." {
					return nil, fmt.Errorf("dep.path: %q must not traverse upward", rel)
				}
			}
			return starlark.String(h.ds.SandboxPath + "/" + rel), nil
		}), nil
	default:
		return nil, nil
	}
}
