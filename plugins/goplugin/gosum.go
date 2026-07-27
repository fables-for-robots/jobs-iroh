package main

import (
	"sort"
	"strings"

	"github.com/jobs-build/jobs-iroh/importdef"
)

// module is one Go module to fetch: its path and version.
type module struct {
	Path    string
	Version string
}

// inputSpec is the {kind, definition} Input wire form (build.md §4, §6).
// Definition is the canonical importdef CBOR encoded as a CBOR byte string, so
// the recipe runtime's asInputSpec rehydrates it into an Input.
type inputSpec struct {
	Kind       string `cbor:"kind"`
	Definition []byte `cbor:"definition"`
}

// modOut is one element of the plugin response: a named module + its import spec.
type modOut struct {
	Path    string    `cbor:"path"`
	Version string    `cbor:"version"`
	Input   inputSpec `cbor:"input"`
}

// modulesFromGoSum returns the unique module ZIP entries in go.sum — the lines of
// the form "<module> <version> h1:<hash>" (excluding the "<version>/go.mod" lines)
// — sorted by (path, version). This is the standard "what to fetch" set.
func modulesFromGoSum(goSum []byte) []module {
	seen := map[string]bool{}
	var out []module
	for _, line := range strings.Split(string(goSum), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue // blank / malformed line
		}
		path, ver := fields[0], fields[1]
		if strings.HasSuffix(ver, "/go.mod") {
			continue // the go.mod hash line, not the module zip
		}
		key := path + "@" + ver
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, module{Path: path, Version: ver})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Version < out[j].Version
	})
	return out
}

// moduleInput builds the gomod import spec for a module: a canonical importdef
// for fetcher "gomod" with params {module, version}.
func moduleInput(m module) (inputSpec, error) {
	params, err := importdef.CanonicalParams(map[string]any{
		"module":  m.Path,
		"version": m.Version,
	})
	if err != nil {
		return inputSpec{}, err
	}
	def, err := importdef.Definition{Fetcher: "gomod", Params: params}.Canonical()
	if err != nil {
		return inputSpec{}, err
	}
	return inputSpec{Kind: "import", Definition: def}, nil
}
