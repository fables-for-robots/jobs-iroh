package recipe

import (
	"fmt"

	"github.com/fables-for-robots/jobs-iroh/builddef"
	"github.com/fables-for-robots/jobs-iroh/importdef"
	"github.com/fxamacker/cbor/v2"
	"go.starlark.net/starlark"
)

// Input is the Starlark value wrapping a builddef.Input. It exposes .kind. The
// mount path is no longer a recipe-time value (it is the dependency's content
// key, unknown until the dep is built); instead the recipe names each input in
// the build() `inputs` map and the build script resolves names to
// /jobs/store/<BOK> paths via the injected JOBS_DEPS env var (build.md §3.1).
type Input struct {
	In builddef.Input
}

var (
	_ starlark.Value    = (*Input)(nil)
	_ starlark.HasAttrs = (*Input)(nil)
)

func (i *Input) String() string        { return "Input(" + i.In.Kind + ")" }
func (i *Input) Type() string          { return "Input" }
func (i *Input) Freeze()               {}
func (i *Input) Truth() starlark.Bool  { return starlark.True }
func (i *Input) Hash() (uint32, error) { return 0, fmt.Errorf("Input is unhashable") }

func (i *Input) AttrNames() []string { return []string{"kind"} }

func (i *Input) Attr(name string) (starlark.Value, error) {
	switch name {
	case "kind":
		return starlark.String(i.In.Kind), nil
	default:
		return nil, nil // no such attribute
	}
}

// makeImp returns the `imp(fetcher, params, requiredTags=[])` builtin (build.md
// §3.1): construct an import Input from a complete import definition.
func makeImp(platform string) *starlark.Builtin {
	return starlark.NewBuiltin("imp", func(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		fetcherV := starlark.Value(starlark.None)
		var params starlark.Value
		var requiredTags *starlark.List
		// Every import is pinned to the build's platform (import-platform-pinning
		// design, 2026-07-09): the def's Platform is part of identity K and selects
		// fetcher:<name>:<platform> wherever the import runs — a fetcher's output
		// can never depend on which runner ran it. The transitional `platform=`
		// arg is tolerated only when it resolves to the build's platform (True, or
		// the literal build platform — what existing recipes pass); anything else
		// is an error. It will be removed once pinned example SHAs have churned.
		platformArg := starlark.Value(starlark.None)
		if err := starlark.UnpackArgs("imp", args, kwargs,
			"fetcher", &fetcherV, "params", &params, "requiredTags?", &requiredTags, "platform?", &platformArg); err != nil {
			return nil, err
		}
		// A string names a SEED fetcher (or a legacy registered ref); a
		// fetcher(...) value carries the build definition that produces the
		// fetcher, embedded below as FetcherDef (recipe-declared-fetchers §3).
		var fetcher string
		var fetcherDef cbor.RawMessage
		switch fv := fetcherV.(type) {
		case starlark.String:
			fetcher = string(fv)
		case *Fetcher:
			fetcher, fetcherDef = fv.Name, fv.Def
		default:
			return nil, fmt.Errorf("imp fetcher: want a string (seed name) or a fetcher(...) value, got %s", fetcherV.Type())
		}
		// Pure validation: the pinned platform is ALWAYS the build's (the makeImp
		// arg, used directly in the Definition literal below) — no branch selects
		// a different one, by design.
		switch pv := platformArg.(type) {
		case starlark.NoneType:
		case starlark.Bool:
			if !bool(pv) {
				return nil, fmt.Errorf("imp: platform=False (a platform-independent import) is no longer supported — every import is pinned to the build's platform")
			}
		case starlark.String:
			if string(pv) != platform {
				return nil, fmt.Errorf("imp: platform=%q does not match the build's platform %q — cross-platform imports are not supported", string(pv), platform)
			}
		default:
			return nil, fmt.Errorf("imp platform: want string or bool, got %s", platformArg.Type())
		}
		pv, err := fromStarlark(params)
		if err != nil {
			return nil, fmt.Errorf("imp params: %w", err)
		}
		cp, err := importdef.CanonicalParams(pv)
		if err != nil {
			return nil, fmt.Errorf("imp params encode: %w", err)
		}
		tags, err := stringList(requiredTags)
		if err != nil {
			return nil, fmt.Errorf("imp requiredTags: %w", err)
		}
		canon, err := importdef.Definition{Fetcher: fetcher, Params: cp, RequiredTags: tags, Platform: platform, FetcherDef: fetcherDef}.Canonical()
		if err != nil {
			return nil, err
		}
		return &Input{In: builddef.Input{Kind: builddef.KindImport, Definition: canon}}, nil
	})
}

// encodeParams canonicalizes an optional Starlark params value; absent/None
// encodes the same as nil (importdef.CanonicalParams(nil)).
func encodeParams(params starlark.Value) ([]byte, error) {
	if params != nil && params != starlark.None {
		pv, err := fromStarlark(params)
		if err != nil {
			return nil, fmt.Errorf("params: %w", err)
		}
		return importdef.CanonicalParams(pv)
	}
	return importdef.CanonicalParams(nil)
}

// newBuildInput assembles a build Input from an already-resolved source Input
// plus the recipe-supplied dir/platform/params and the recipe selector. Shared by
// bld() and subbuild() (the latter supplies a tree source). The recipe selector is
// either buildJobs (inline override content) or buildFile (an alternative recipe
// path relative to dir) — the two are mutually exclusive. Empty buildJobs/buildFile
// are omitted so they do not perturb identity (both are omitempty in the def).
func newBuildInput(source builddef.Input, dir, platform string, params starlark.Value, buildJobs, buildFile string) (*Input, error) {
	if buildJobs != "" && buildFile != "" {
		return nil, fmt.Errorf("buildJobs and build_file are mutually exclusive")
	}
	if err := validateBuildFile(buildFile); err != nil {
		return nil, err
	}
	cp, err := encodeParams(params)
	if err != nil {
		return nil, err
	}
	var override []byte
	if buildJobs != "" {
		override = []byte(buildJobs)
	}
	canon, err := builddef.Definition{
		Source: source, Dir: dir, Platform: platform, Params: cp, BuildJobs: override, BuildFile: buildFile,
	}.Canonical()
	if err != nil {
		return nil, err
	}
	return &Input{In: builddef.Input{Kind: builddef.KindBuild, Definition: canon}}, nil
}

// makeBld returns the `bld(source, dir="", platform=platform, params=None,
// buildJobs=None)` builtin (build-from design §5): construct a build Input.
func makeBld(platform string) *starlark.Builtin {
	return starlark.NewBuiltin("bld", func(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var source starlark.Value
		dir := ""
		plat := platform
		var params starlark.Value
		var buildJobs string
		var buildFile string
		if err := starlark.UnpackArgs("bld", args, kwargs,
			"source", &source, "dir?", &dir, "platform?", &plat, "params?", &params, "buildJobs?", &buildJobs, "build_file?", &buildFile); err != nil {
			return nil, err
		}
		src, ok := source.(*Input)
		if !ok {
			return nil, fmt.Errorf("bld: source must be an Input, got %s", source.Type())
		}
		return newBuildInput(src.In, dir, plat, params, buildJobs, buildFile)
	})
}

// stringList converts a Starlark list of strings to []string (nil for a nil list).
func stringList(l *starlark.List) ([]string, error) {
	if l == nil {
		return nil, nil
	}
	out := make([]string, 0, l.Len())
	for i := 0; i < l.Len(); i++ {
		s, ok := starlark.AsString(l.Index(i))
		if !ok {
			return nil, fmt.Errorf("element %d is not a string", i)
		}
		out = append(out, s)
	}
	return out, nil
}
