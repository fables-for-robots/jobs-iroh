package recipe

import (
	"fmt"

	"github.com/fxamacker/cbor/v2"
	"github.com/jobs-build/jobs-iroh/builddef"
	"go.starlark.net/starlark"
)

// Fetcher is the Starlark value a recipe constructs with fetcher(): a named,
// recipe-declared fetcher carrying the canonical builddef.Definition that
// builds it. Passing it as imp(fetcher = ...) embeds Def into the import's
// definition (FetcherDef), making the fetcher an ordinary build dependency of
// the import (recipe-declared-fetchers design §3).
type Fetcher struct {
	Name string
	Def  cbor.RawMessage // canonical builddef.Definition
}

var _ starlark.Value = (*Fetcher)(nil)

func (f *Fetcher) String() string        { return "fetcher(" + f.Name + ")" }
func (f *Fetcher) Type() string          { return "Fetcher" }
func (f *Fetcher) Freeze()               {}
func (f *Fetcher) Truth() starlark.Bool  { return starlark.True }
func (f *Fetcher) Hash() (uint32, error) { return 0, fmt.Errorf("Fetcher is unhashable") }

// makeFetcher returns the fetcher(name, url=, sha256=, build=None) builtin.
// url+sha256 is sugar for the dominant case (a tarball+https archive of the
// fetcher's source with strip=1 — builddef.FetcherBuild); build= accepts an
// explicit bld() Input for exotic cases (source fetched by another declared
// fetcher, extra params, build_file). Exactly one form is required.
func makeFetcher(platform string) *starlark.Builtin {
	return starlark.NewBuiltin("fetcher", func(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var name, url, sha256 string
		build := starlark.Value(starlark.None)
		if err := starlark.UnpackArgs("fetcher", args, kwargs,
			"name", &name, "url?", &url, "sha256?", &sha256, "build?", &build); err != nil {
			return nil, err
		}
		if name == "" {
			return nil, fmt.Errorf("fetcher: name is required")
		}
		haveURL := url != "" || sha256 != ""
		haveBuild := build != starlark.None
		if haveURL == haveBuild {
			return nil, fmt.Errorf("fetcher %q: exactly one of url+sha256 or build= is required", name)
		}
		if haveBuild {
			in, ok := build.(*Input)
			if !ok || in.In.Kind != builddef.KindBuild {
				return nil, fmt.Errorf("fetcher %q: build= must be a bld(...) Input", name)
			}
			return &Fetcher{Name: name, Def: in.In.Definition}, nil
		}
		if url == "" || sha256 == "" {
			return nil, fmt.Errorf("fetcher %q: url and sha256 are both required", name)
		}
		def, err := builddef.FetcherBuild(url, sha256, platform)
		if err != nil {
			return nil, fmt.Errorf("fetcher %q: %w", name, err)
		}
		canon, err := def.Canonical()
		if err != nil {
			return nil, fmt.Errorf("fetcher %q: %w", name, err)
		}
		return &Fetcher{Name: name, Def: canon}, nil
	})
}
