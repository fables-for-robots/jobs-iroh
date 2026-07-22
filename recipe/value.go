package recipe

import (
	"bytes"
	"fmt"

	"github.com/fables-for-robots/jobs-iroh/builddef"
	"github.com/fables-for-robots/jobs-iroh/importdef"
	"github.com/fxamacker/cbor/v2"
	"go.starlark.net/starlark"
)

// rehydrator carries the context for converting a Go value (typically a
// CBOR-decoded plugin response) into Starlark. A map with exactly
// {kind, definition} (kind import or build) is rehydrated into an *Input value
// (build.md §6). When platform is non-empty, every rehydrated KindImport
// definition is pinned to it (import-platform-pinning design). When enforce is
// set (the plugin-response path), a rehydrated import naming a non-seed
// fetcher without a FetcherDef gets one injected from the plugin's bundled
// pins — or fails when no pin matches (recipe-declared-fetchers design §7).
type rehydrator struct {
	platform string
	pins     *FetcherPins    // the running plugin's bundle; nil ⇒ none shipped
	seeds    map[string]bool // import-capable seed fetcher names
	enforce  bool            // true only while rehydrating a plugin response
}

// toStarlark is the non-enforcing conversion used outside plugin responses
// (the params global): platform stamping only, no pin injection.
func toStarlark(v any, importPlatform string) (starlark.Value, error) {
	return rehydrator{platform: importPlatform}.convert(v)
}

func (r rehydrator) convert(v any) (starlark.Value, error) {
	switch t := v.(type) {
	case nil:
		return starlark.None, nil
	case bool:
		return starlark.Bool(t), nil
	case string:
		return starlark.String(t), nil
	case []byte:
		return starlark.Bytes(t), nil
	case int:
		return starlark.MakeInt(t), nil
	case int64:
		return starlark.MakeInt64(t), nil
	case uint64:
		return starlark.MakeUint64(t), nil
	case float64:
		return starlark.Float(t), nil
	case []any:
		elems := make([]starlark.Value, 0, len(t))
		for _, e := range t {
			sv, err := r.convert(e)
			if err != nil {
				return nil, err
			}
			elems = append(elems, sv)
		}
		return starlark.NewList(elems), nil
	case map[string]any:
		if in, ok := asInputSpec(t); ok {
			rehydrated, err := r.rehydrateInput(in)
			if err != nil {
				return nil, err
			}
			return &Input{In: rehydrated}, nil
		}
		d := starlark.NewDict(len(t))
		for k, e := range t {
			sv, err := r.convert(e)
			if err != nil {
				return nil, err
			}
			if err := d.SetKey(starlark.String(k), sv); err != nil {
				return nil, err
			}
		}
		return d, nil
	default:
		return nil, fmt.Errorf("cannot convert %T to Starlark", v)
	}
}

// asInputSpec recognizes the {kind, definition} Input wire form (build.md §4,
// §6) and returns the corresponding builddef.Input.
func asInputSpec(m map[string]any) (builddef.Input, bool) {
	if len(m) != 2 {
		return builddef.Input{}, false
	}
	kind, ok := m["kind"].(string)
	if !ok || (kind != builddef.KindImport && kind != builddef.KindBuild) {
		return builddef.Input{}, false
	}
	def, ok := m["definition"].([]byte)
	if !ok {
		return builddef.Input{}, false
	}
	return builddef.Input{Kind: kind, Definition: cbor.RawMessage(def)}, true
}

// rehydrateInput re-canonicalizes a plugin-emitted input definition: a
// KindImport gets the build's platform stamped (import-platform-pinning) and —
// on the enforcing plugin-response path — the plugin's bundled FetcherDef
// injected for non-seed fetcher names (recipe-declared-fetchers design §7); a
// non-seed name with no matching pin fails the pin stage, nothing will ever
// provision it. A KindBuild recurses into its Source chain so an emitted
// sub-build's import (e.g. pybackendplugin's sdist builds fetching via pypi)
// gets the same treatment instead of escaping enforcement. Creation-time only —
// it runs while a plugin response is being rehydrated, never against frozen
// data (a stored plugin-resolved tree or Pinned is read back without passing
// through here).
func (r rehydrator) rehydrateInput(in builddef.Input) (builddef.Input, error) {
	if in.Kind == builddef.KindBuild {
		bdef, err := builddef.DecodeDefinition(in.Definition)
		if err != nil {
			return builddef.Input{}, fmt.Errorf("rehydrate plugin build: decode: %w", err)
		}
		if bdef.Source.Kind == builddef.KindTree {
			return in, nil
		}
		src, err := r.rehydrateInput(bdef.Source)
		if err != nil {
			return builddef.Input{}, err
		}
		if bytes.Equal(src.Definition, bdef.Source.Definition) {
			return in, nil
		}
		bdef.Source = src
		canon, err := bdef.Canonical()
		if err != nil {
			return builddef.Input{}, fmt.Errorf("rehydrate plugin build: encode: %w", err)
		}
		in.Definition = canon
		return in, nil
	}
	if in.Kind != builddef.KindImport {
		return in, nil
	}
	def, err := importdef.Decode(in.Definition)
	if err != nil {
		return builddef.Input{}, fmt.Errorf("rehydrate plugin import: decode: %w", err)
	}
	changed := false
	if r.platform != "" && def.Platform != r.platform {
		def.Platform = r.platform
		changed = true
	}
	if r.enforce && len(def.FetcherDef) == 0 && !r.seeds[def.Fetcher] {
		var pin FetcherPin
		ok := false
		if r.pins != nil {
			pin, ok = r.pins.Entries[def.Fetcher]
		}
		if !ok {
			return builddef.Input{}, fmt.Errorf("plugin emitted import with fetcher %q: not in the plugin's bundled %s and not a seed fetcher", def.Fetcher, PinsFileName)
		}
		fb, err := builddef.FetcherBuild(pin.URL, pin.SHA256, def.Platform)
		if err != nil {
			return builddef.Input{}, fmt.Errorf("rehydrate plugin import %q: %w", def.Fetcher, err)
		}
		canon, err := fb.Canonical()
		if err != nil {
			return builddef.Input{}, err
		}
		def.FetcherDef = canon
		changed = true
	}
	if !changed {
		return in, nil
	}
	canon, err := def.Canonical()
	if err != nil {
		return builddef.Input{}, fmt.Errorf("rehydrate plugin import: encode: %w", err)
	}
	in.Definition = canon
	return in, nil
}

// fromStarlark converts a Starlark value into a Go value with string map keys
// (suitable for canonical CBOR / JSON). An *Input becomes its {kind, definition}
// wire form so it can ride inside a plugin request.
func fromStarlark(v starlark.Value) (any, error) {
	switch t := v.(type) {
	case nil, starlark.NoneType:
		return nil, nil
	case starlark.Bool:
		return bool(t), nil
	case starlark.String:
		return string(t), nil
	case starlark.Bytes:
		return []byte(t), nil
	case starlark.Int:
		if i, ok := t.Int64(); ok {
			return i, nil
		}
		if u, ok := t.Uint64(); ok {
			return u, nil
		}
		return nil, fmt.Errorf("integer out of range")
	case starlark.Float:
		return float64(t), nil
	case *Input:
		return map[string]any{"kind": t.In.Kind, "definition": []byte(t.In.Definition)}, nil
	case *starlark.List:
		out := make([]any, 0, t.Len())
		for i := 0; i < t.Len(); i++ {
			e, err := fromStarlark(t.Index(i))
			if err != nil {
				return nil, err
			}
			out = append(out, e)
		}
		return out, nil
	case starlark.Tuple:
		out := make([]any, 0, t.Len())
		for i := 0; i < t.Len(); i++ {
			e, err := fromStarlark(t.Index(i))
			if err != nil {
				return nil, err
			}
			out = append(out, e)
		}
		return out, nil
	case *starlark.Dict:
		out := make(map[string]any, t.Len())
		for _, item := range t.Items() {
			ks, ok := starlark.AsString(item[0])
			if !ok {
				return nil, fmt.Errorf("dict key %v is not a string", item[0])
			}
			e, err := fromStarlark(item[1])
			if err != nil {
				return nil, err
			}
			out[ks] = e
		}
		return out, nil
	default:
		return nil, fmt.Errorf("cannot convert Starlark %s to Go", v.Type())
	}
}
