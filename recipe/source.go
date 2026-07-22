package recipe

import (
	"fmt"

	"go.starlark.net/starlark"
)

// Source is read-only access to the build's source tree at `dir` (build.md
// §3.1). Paths are slash-separated and relative to the build root.
type Source interface {
	Read(path string) ([]byte, error)
	Exists(path string) bool
}

// MapSource is an in-memory Source (tests and simple callers).
type MapSource map[string][]byte

func (m MapSource) Read(path string) ([]byte, error) {
	b, ok := m[path]
	if !ok {
		return nil, fmt.Errorf("source: no such file %q", path)
	}
	return b, nil
}

func (m MapSource) Exists(path string) bool {
	_, ok := m[path]
	return ok
}

// sourceValue is the predeclared `source` Starlark handle exposing read/exists.
type sourceValue struct{ src Source }

var (
	_ starlark.Value    = sourceValue{}
	_ starlark.HasAttrs = sourceValue{}
)

func (sourceValue) String() string        { return "source" }
func (sourceValue) Type() string          { return "source" }
func (sourceValue) Freeze()               {}
func (sourceValue) Truth() starlark.Bool  { return starlark.True }
func (sourceValue) Hash() (uint32, error) { return 0, fmt.Errorf("source is unhashable") }

func (s sourceValue) AttrNames() []string { return []string{"read", "exists"} }

func (s sourceValue) Attr(name string) (starlark.Value, error) {
	switch name {
	case "read":
		return starlark.NewBuiltin("source.read", func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			var path string
			if err := starlark.UnpackArgs("read", args, kwargs, "path", &path); err != nil {
				return nil, err
			}
			data, err := s.src.Read(path)
			if err != nil {
				return nil, err
			}
			return starlark.Bytes(data), nil
		}), nil
	case "exists":
		return starlark.NewBuiltin("source.exists", func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			var path string
			if err := starlark.UnpackArgs("exists", args, kwargs, "path", &path); err != nil {
				return nil, err
			}
			return starlark.Bool(s.src.Exists(path)), nil
		}), nil
	default:
		return nil, nil
	}
}
