package recipe

import (
	"bytes"
	"strings"
	"testing"

	"github.com/fables-for-robots/jobs-iroh/builddef"
	"github.com/fables-for-robots/jobs-iroh/importdef"
	"go.starlark.net/starlark"
)

func TestParseFetcherPins(t *testing.T) {
	pins, err := ParseFetcherPins([]byte(`
[[fetcher]]
name   = "npm"
url    = "https://example.com/fetcher-npm.tar.gz"
sha256 = "beef"
`))
	if err != nil {
		t.Fatal(err)
	}
	p, ok := pins.Entries["npm"]
	if !ok || p.URL != "https://example.com/fetcher-npm.tar.gz" || p.SHA256 != "beef" {
		t.Fatalf("pins: %+v", pins)
	}
	if _, err := ParseFetcherPins([]byte("[[fetcher]]\nname = \"x\"\n")); err == nil {
		t.Fatal("want error: url+sha256 required")
	}
	if _, err := ParseFetcherPins([]byte(`
[[fetcher]]
name   = "x"
url    = "u"
sha256 = "s"

[[fetcher]]
name   = "x"
url    = "u2"
sha256 = "s2"
`)); err == nil {
		t.Fatal("want error: duplicate pin")
	}
}

// emittedImport builds the {kind, definition} wire form a plugin response
// carries for an import naming `fetcher` (platform-less, no FetcherDef).
func emittedImport(t *testing.T, fetcher string) map[string]any {
	t.Helper()
	def, err := importdef.Definition{Fetcher: fetcher}.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"kind": builddef.KindImport, "definition": []byte(def)}
}

func TestRehydrationInjectsBundledPin(t *testing.T) {
	pins := &FetcherPins{Entries: map[string]FetcherPin{"gomod": {URL: "https://e/fg.tar.gz", SHA256: "aa"}}}
	r := rehydrator{platform: "linux/amd64", pins: pins, seeds: map[string]bool{"tarball+https": true}, enforce: true}
	v, err := r.convert(emittedImport(t, "gomod"))
	if err != nil {
		t.Fatal(err)
	}
	in, ok := v.(*Input)
	if !ok {
		t.Fatalf("rehydrated to %T", v)
	}
	def, err := importdef.Decode(in.In.Definition)
	if err != nil {
		t.Fatal(err)
	}
	if def.Platform != "linux/amd64" {
		t.Fatalf("platform not stamped: %+v", def)
	}
	want, err := builddef.FetcherBuild("https://e/fg.tar.gz", "aa", "linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	wc, err := want.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(def.FetcherDef, wc) {
		t.Fatal("bundled pin must be injected as FetcherDef")
	}
}

func TestRehydrationSeedPassthrough(t *testing.T) {
	r := rehydrator{platform: "linux/amd64", seeds: map[string]bool{"tarball+https": true}, enforce: true}
	v, err := r.convert(emittedImport(t, "tarball+https"))
	if err != nil {
		t.Fatal(err)
	}
	def, err := importdef.Decode(v.(*Input).In.Definition)
	if err != nil {
		t.Fatal(err)
	}
	if len(def.FetcherDef) != 0 {
		t.Fatal("seed names pass through without FetcherDef")
	}
	if def.Platform != "linux/amd64" {
		t.Fatal("platform must still be stamped")
	}
}

func TestRehydrationMissingPinFails(t *testing.T) {
	r := rehydrator{platform: "linux/amd64", seeds: map[string]bool{"tarball+https": true}, enforce: true}
	_, err := r.convert(emittedImport(t, "gomod"))
	if err == nil || !strings.Contains(err.Error(), `"gomod"`) {
		t.Fatalf("non-seed emitted fetcher with no bundle must error naming it, got %v", err)
	}
}

func TestNonEnforcingWrapperKeepsLegacyBehavior(t *testing.T) {
	// The params path (toStarlark with a bare platform) must not enforce pins.
	v, err := toStarlark(emittedImport(t, "gomod"), "linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	def, err := importdef.Decode(v.(*Input).In.Definition)
	if err != nil {
		t.Fatal(err)
	}
	if len(def.FetcherDef) != 0 || def.Platform != "linux/amd64" {
		t.Fatalf("wrapper changed semantics: %+v", def)
	}
}

func TestPluginResponseRehydratesThroughSpecs(t *testing.T) {
	// End-to-end through plugins["x"](...): the spec's pins drive injection.
	pins := &FetcherPins{Entries: map[string]FetcherPin{"gomod": {URL: "https://e/fg.tar.gz", SHA256: "aa"}}}
	def, err := importdef.Definition{Fetcher: "gomod"}.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	resp := []any{map[string]any{"kind": builddef.KindImport, "definition": []byte(def)}}
	pm := pluginsMapping{
		specs:    map[string]PluginSpec{"go": {Caller: staticCaller{resp: resp}, Pins: pins}},
		platform: "linux/amd64",
		seeds:    map[string]bool{"tarball+https": true},
	}
	cv, found, err := pm.Get(starlark.String("go"))
	if err != nil || !found {
		t.Fatalf("Get: %v %v", found, err)
	}
	out := callBuiltin(t, cv.(*starlark.Builtin), nil)
	lst := out.(*starlark.List)
	got, err := importdef.Decode(lst.Index(0).(*Input).In.Definition)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.FetcherDef) == 0 {
		t.Fatal("plugin path must inject FetcherDef")
	}
}

type staticCaller struct{ resp any }

func (s staticCaller) Call(map[string]any) (any, error) { return s.resp, nil }

func TestRehydrationRecursesIntoEmittedBuilds(t *testing.T) {
	// A plugin-emitted BUILD (pybackendplugin's sdist sub-builds) whose source
	// is an import naming a non-seed fetcher must get the same injection as a
	// top-level emitted import — not escape enforcement (review finding).
	srcImp, err := importdef.Definition{Fetcher: "pypi"}.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	params, err := importdef.CanonicalParams(nil)
	if err != nil {
		t.Fatal(err)
	}
	bd := builddef.Definition{
		Source:   builddef.Input{Kind: builddef.KindImport, Definition: srcImp},
		Platform: "linux/amd64",
		Params:   params,
	}
	bdCanon, err := bd.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	spec := map[string]any{"kind": builddef.KindBuild, "definition": []byte(bdCanon)}

	pins := &FetcherPins{Entries: map[string]FetcherPin{"pypi": {URL: "https://e/fp.tar.gz", SHA256: "cc"}}}
	r := rehydrator{platform: "linux/amd64", pins: pins, seeds: map[string]bool{"tarball+https": true}, enforce: true}
	v, err := r.convert(spec)
	if err != nil {
		t.Fatal(err)
	}
	got, err := builddef.DecodeDefinition(v.(*Input).In.Definition)
	if err != nil {
		t.Fatal(err)
	}
	srcDef, err := importdef.Decode(got.Source.Definition)
	if err != nil {
		t.Fatal(err)
	}
	if len(srcDef.FetcherDef) == 0 {
		t.Fatal("nested source import must get the bundled FetcherDef injected")
	}
	if srcDef.Platform != "linux/amd64" {
		t.Fatalf("nested source import platform: %q", srcDef.Platform)
	}

	// And with no matching pin it must FAIL the pin stage, not pass through.
	rNone := rehydrator{platform: "linux/amd64", seeds: map[string]bool{"tarball+https": true}, enforce: true}
	if _, err := rNone.convert(spec); err == nil {
		t.Fatal("emitted build with unpinned non-seed source must error")
	}
}
