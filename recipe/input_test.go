package recipe

import (
	"strings"
	"testing"

	"github.com/jobs-build/jobs-iroh/builddef"
	"github.com/jobs-build/jobs-iroh/importdef"
	"go.starlark.net/starlark"
)

// importDefBytes is a shared test helper: canonical CBOR of an import def.
func importDefBytes(t *testing.T, fetcher, url string) []byte {
	t.Helper()
	p, err := importdef.CanonicalParams(map[string]any{"url": url})
	if err != nil {
		t.Fatal(err)
	}
	b, err := importdef.Definition{Fetcher: fetcher, Params: p}.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func callBuiltin(t *testing.T, b *starlark.Builtin, kwargs []starlark.Tuple, args ...starlark.Value) starlark.Value {
	t.Helper()
	v, err := starlark.Call(&starlark.Thread{Name: "t"}, b, args, kwargs)
	if err != nil {
		t.Fatalf("call %s: %v", b.Name(), err)
	}
	return v
}

func TestImp_BuildsImportInput(t *testing.T) {
	imp := makeImp("linux/amd64")
	v := callBuiltin(t, imp, []starlark.Tuple{
		{starlark.String("fetcher"), starlark.String("tarball+https")},
		{starlark.String("params"), strDict(t, "url", "https://example.com/x.tgz")},
	})
	in, ok := v.(*Input)
	if !ok {
		t.Fatalf("imp returned %T", v)
	}
	if in.In.Kind != builddef.KindImport {
		t.Fatalf("kind=%q", in.In.Kind)
	}
	// .path is no longer exposed — the recipe names inputs and the build script
	// resolves them via JOBS_DEPS (build.md §3.1).
	pathAttr, err := in.Attr("path")
	if err != nil {
		t.Fatal(err)
	}
	if pathAttr != nil {
		t.Fatalf("path attr should be absent, got %v", pathAttr)
	}
	// Identity matches a hand-built import Input for the same params, pinned to
	// the build's platform (import-platform-pinning design).
	p, err := importdef.CanonicalParams(map[string]any{"url": "https://example.com/x.tgz"})
	if err != nil {
		t.Fatal(err)
	}
	wantDef, err := importdef.Definition{Fetcher: "tarball+https", Params: p, Platform: "linux/amd64"}.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	want := builddef.Input{Kind: builddef.KindImport, Definition: wantDef}
	wk, _ := want.Key()
	gk, _ := in.In.Key()
	if wk != gk {
		t.Fatalf("imp K=%s want %s", gk, wk)
	}
}

// impDef evaluates imp(fetcher, params, [platform]) under buildPlatform and
// returns the decoded definition (or the eval error).
func impDef(t *testing.T, buildPlatform string, kwargs []starlark.Tuple) (importdef.Definition, error) {
	t.Helper()
	v, err := starlark.Call(newThread(), makeImp(buildPlatform), nil, kwargs)
	if err != nil {
		return importdef.Definition{}, err
	}
	def, derr := importdef.Decode(v.(*Input).In.Definition)
	if derr != nil {
		t.Fatalf("decode def: %v", derr)
	}
	return def, nil
}

func TestImp_PinsBuildPlatformByDefault(t *testing.T) {
	def, err := impDef(t, "linux/arm64", []starlark.Tuple{
		{starlark.String("fetcher"), starlark.String("github")},
		{starlark.String("params"), strDict(t, "repo", "x")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if def.Platform != "linux/arm64" {
		t.Fatalf("Platform=%q want linux/arm64", def.Platform)
	}
}

func TestImp_PlatformTrueEqualsDefault(t *testing.T) {
	def, err := impDef(t, "linux/arm64", []starlark.Tuple{
		{starlark.String("fetcher"), starlark.String("hostmusl")},
		{starlark.String("params"), starlark.NewDict(0)},
		{starlark.String("platform"), starlark.Bool(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if def.Platform != "linux/arm64" {
		t.Fatalf("Platform=%q want linux/arm64", def.Platform)
	}
}

func TestImp_PlatformMatchingStringAccepted(t *testing.T) {
	def, err := impDef(t, "linux/amd64", []starlark.Tuple{
		{starlark.String("fetcher"), starlark.String("hostmusl")},
		{starlark.String("params"), starlark.NewDict(0)},
		{starlark.String("platform"), starlark.String("linux/amd64")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if def.Platform != "linux/amd64" {
		t.Fatalf("Platform=%q want linux/amd64", def.Platform)
	}
}

func TestImp_PlatformMismatchRejected(t *testing.T) {
	_, err := impDef(t, "linux/arm64", []starlark.Tuple{
		{starlark.String("fetcher"), starlark.String("hostmusl")},
		{starlark.String("params"), starlark.NewDict(0)},
		{starlark.String("platform"), starlark.String("linux/amd64")},
	})
	if err == nil || !strings.Contains(err.Error(), "does not match the build's platform") {
		t.Fatalf("want mismatch error, got %v", err)
	}
}

func TestImp_PlatformFalseRejected(t *testing.T) {
	_, err := impDef(t, "linux/arm64", []starlark.Tuple{
		{starlark.String("fetcher"), starlark.String("github")},
		{starlark.String("params"), starlark.NewDict(0)},
		{starlark.String("platform"), starlark.Bool(false)},
	})
	if err == nil || !strings.Contains(err.Error(), "no longer supported") {
		t.Fatalf("want platform=False error, got %v", err)
	}
}

func TestImp_IdentityForksPerPlatform(t *testing.T) {
	kw := func() []starlark.Tuple {
		return []starlark.Tuple{
			{starlark.String("fetcher"), starlark.String("github")},
			{starlark.String("params"), strDict(t, "repo", "x")},
		}
	}
	d1, err := impDef(t, "linux/amd64", kw())
	if err != nil {
		t.Fatal(err)
	}
	d2, err := impDef(t, "linux/arm64", kw())
	if err != nil {
		t.Fatal(err)
	}
	c1, _ := d1.Canonical()
	c2, _ := d2.Canonical()
	if string(c1) == string(c2) {
		t.Fatal("same canonical def on two platforms — platform not in identity")
	}
}

func TestBld_DefaultsPlatformToOuter(t *testing.T) {
	imp := makeImp("linux/arm64")
	src := callBuiltin(t, imp, []starlark.Tuple{
		{starlark.String("fetcher"), starlark.String("github")},
		{starlark.String("params"), strDict(t, "repo", "x")},
	})
	bld := makeBld("linux/arm64")
	v := callBuiltin(t, bld, []starlark.Tuple{{starlark.String("source"), src}})
	in, ok := v.(*Input)
	if !ok {
		t.Fatalf("bld returned %T", v)
	}
	if in.In.Kind != builddef.KindBuild {
		t.Fatalf("kind=%q", in.In.Kind)
	}
	d, err := builddef.DecodeDefinition(in.In.Definition)
	if err != nil {
		t.Fatal(err)
	}
	if d.Platform != "linux/arm64" {
		t.Fatalf("platform=%q want linux/arm64 (outer default)", d.Platform)
	}
}

func TestBld_buildJobsOverride(t *testing.T) {
	imp := makeImp("linux/amd64")
	srcV, err := starlark.Call(newThread(), imp, nil, []starlark.Tuple{
		{starlark.String("fetcher"), starlark.String("github")},
		{starlark.String("params"), starlark.NewDict(0)},
	})
	if err != nil {
		t.Fatal(err)
	}
	bld := makeBld("linux/amd64")
	v, err := starlark.Call(newThread(), bld, nil, []starlark.Tuple{
		{starlark.String("source"), srcV},
		{starlark.String("buildJobs"), starlark.String("def build(): pass\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	in := v.(*Input).In
	def, err := builddef.DecodeDefinition(in.Definition)
	if err != nil {
		t.Fatal(err)
	}
	if string(def.BuildJobs) != "def build(): pass\n" {
		t.Fatalf("BuildJobs=%q", def.BuildJobs)
	}
}

func TestBld_buildFile(t *testing.T) {
	imp := makeImp("linux/amd64")
	srcV := callBuiltin(t, imp, []starlark.Tuple{
		{starlark.String("fetcher"), starlark.String("github")},
		{starlark.String("params"), starlark.NewDict(0)},
	})
	bld := makeBld("linux/amd64")
	v := callBuiltin(t, bld, []starlark.Tuple{
		{starlark.String("source"), srcV},
		{starlark.String("build_file"), starlark.String("app-a.jobs")},
	})
	def, err := builddef.DecodeDefinition(v.(*Input).In.Definition)
	if err != nil {
		t.Fatal(err)
	}
	if def.BuildFile != "app-a.jobs" {
		t.Fatalf("BuildFile=%q want app-a.jobs", def.BuildFile)
	}
}

// build_file and buildJobs both name "which recipe" — supplying both is an error.
func TestBld_buildFileAndBuildJobsExclusive(t *testing.T) {
	imp := makeImp("linux/amd64")
	srcV := callBuiltin(t, imp, []starlark.Tuple{
		{starlark.String("fetcher"), starlark.String("github")},
		{starlark.String("params"), starlark.NewDict(0)},
	})
	bld := makeBld("linux/amd64")
	_, err := starlark.Call(newThread(), bld, nil, []starlark.Tuple{
		{starlark.String("source"), srcV},
		{starlark.String("buildJobs"), starlark.String("def build(): pass\n")},
		{starlark.String("build_file"), starlark.String("app.jobs")},
	})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("bld with both buildJobs and build_file must error 'mutually exclusive', got %v", err)
	}
}

func TestBld_buildFileRejectsBadPath(t *testing.T) {
	imp := makeImp("linux/amd64")
	srcV := callBuiltin(t, imp, []starlark.Tuple{
		{starlark.String("fetcher"), starlark.String("github")},
		{starlark.String("params"), starlark.NewDict(0)},
	})
	bld := makeBld("linux/amd64")
	for _, bad := range []string{"/abs.jobs", "../x.jobs", "a/../b.jobs", "./x.jobs", "a//b.jobs"} {
		_, err := starlark.Call(newThread(), bld, nil, []starlark.Tuple{
			{starlark.String("source"), srcV},
			{starlark.String("build_file"), starlark.String(bad)},
		})
		if err == nil || !strings.Contains(err.Error(), bad) {
			t.Fatalf("bld(build_file=%q) must error mentioning the path, got %v", bad, err)
		}
	}
}

// strDict builds a one-entry Starlark dict {key: value}.
func strDict(t *testing.T, key, value string) *starlark.Dict {
	t.Helper()
	d := starlark.NewDict(1)
	if err := d.SetKey(starlark.String(key), starlark.String(value)); err != nil {
		t.Fatal(err)
	}
	return d
}
