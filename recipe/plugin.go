package recipe

import (
	"fmt"

	"go.starlark.net/starlark"
)

// PluginCaller invokes a resolved plugin with the keyword args of a Starlark
// call and returns the plugin's response (already CBOR-decoded to Go values with
// string map keys; Input specs are {kind, definition} maps). The hermetic
// subprocess implementation is SubprocessPlugin (build.md §6); tests use fakes.
type PluginCaller interface {
	Call(kwargs map[string]any) (any, error)
}

// pluginsMapping is the predeclared `plugins` Starlark value for build(): a
// read-only mapping name -> callable. Calling plugins["go"](k=v, ...) serializes
// the kwargs, invokes the PluginCaller, and converts the response back to
// Starlark, rehydrating Inputs (build.md §6).
type pluginsMapping struct {
	specs map[string]PluginSpec
	// platform is the build's platform, stamped into every KindImport input a
	// plugin response carries (import-platform-pinning design; see rehydrator).
	platform string
	// seeds are the import-capable seed fetcher names — the only names an
	// emitted import may use without a bundled pin (recipe-declared-fetchers §7).
	seeds map[string]bool
}

var (
	_ starlark.Value    = pluginsMapping{}
	_ starlark.Mapping  = pluginsMapping{}
	_ starlark.HasAttrs = pluginsMapping{}
)

func (pluginsMapping) String() string        { return "plugins" }
func (pluginsMapping) Type() string          { return "plugins" }
func (pluginsMapping) Freeze()               {}
func (pluginsMapping) Truth() starlark.Bool  { return starlark.True }
func (pluginsMapping) Hash() (uint32, error) { return 0, fmt.Errorf("plugins is unhashable") }

// AttrNames/Attr expose nothing; they exist only so `plugins` is a valid value.
func (pluginsMapping) AttrNames() []string                 { return nil }
func (pluginsMapping) Attr(string) (starlark.Value, error) { return nil, nil }

// Get implements plugins[name] -> a callable bound to that plugin.
func (p pluginsMapping) Get(k starlark.Value) (starlark.Value, bool, error) {
	name, ok := starlark.AsString(k)
	if !ok {
		return nil, false, fmt.Errorf("plugins key must be a string")
	}
	spec, ok := p.specs[name]
	if !ok {
		return nil, false, nil
	}
	return p.callable(name, spec), true, nil
}

func (p pluginsMapping) callable(name string, spec PluginSpec) *starlark.Builtin {
	return starlark.NewBuiltin("plugin:"+name, func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		if len(args) > 0 {
			return nil, fmt.Errorf("plugin %q takes only keyword arguments", name)
		}
		call := make(map[string]any, len(kwargs))
		for _, kv := range kwargs {
			ks, _ := starlark.AsString(kv[0])
			gv, err := fromStarlark(kv[1])
			if err != nil {
				return nil, fmt.Errorf("plugin %q arg %s: %w", name, ks, err)
			}
			call[ks] = gv
		}
		resp, err := spec.Caller.Call(call)
		if err != nil {
			return nil, fmt.Errorf("plugin %q: %w", name, err)
		}
		// Enforcing rehydration: platform stamping + bundled-pin FetcherDef
		// injection for every emitted import (recipe-declared-fetchers §7).
		v, err := rehydrator{platform: p.platform, pins: spec.Pins, seeds: p.seeds, enforce: true}.convert(resp)
		if err != nil {
			return nil, fmt.Errorf("plugin %q: %w", name, err)
		}
		return v, nil
	})
}
