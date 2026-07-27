package recipe

import (
	"strings"
	"testing"

	"github.com/jobs-build/jobs-iroh/builddef"
	"github.com/jobs-build/jobs-iroh/importdef"
)

func cfg(t *testing.T, src Source) EvalConfig {
	t.Helper()
	p, err := importdef.CanonicalParams(map[string]any{"v": int64(1)})
	if err != nil {
		t.Fatal(err)
	}
	return EvalConfig{Platform: "linux/amd64", Params: p, Source: src,
		SeedFetchers: []string{"tarball+https", "hostmusl", "github"}}
}

func TestEvalPlugins_ReturnsNamedInputs(t *testing.T) {
	recipeSrc := []byte(`
def plugins():
    return {
        "go": bld(source = imp(fetcher = "github", params = {"repo": "go-plugin"})),
    }
`)
	res, err := EvalPlugins(cfg(t, MapSource{}), recipeSrc)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Plugins) != 1 || res.Plugins["go"].Kind != "build" {
		t.Fatalf("plugins=%+v", res.Plugins)
	}
}

func TestEvalPlugins_AbsentFunctionIsEmpty(t *testing.T) {
	res, err := EvalPlugins(cfg(t, MapSource{}), []byte("x = 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Plugins) != 0 || len(res.Deps) != 0 {
		t.Fatalf("expected empty, got %+v", res)
	}
}

func TestEvalPlugins_StructReturnWithDeps(t *testing.T) {
	recipeSrc := []byte(`
def plugins():
    return struct(
        plugins = {"go": imp(fetcher = "tarball+https", params = {"url": "https://e/p.tgz"})},
        deps    = {"src_b": imp(fetcher = "github", params = {"owner": "o", "repo": "r", "ref": "abc"})},
    )
`)
	res, err := EvalPlugins(cfg(t, MapSource{}), recipeSrc)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Plugins) != 1 || res.Plugins["go"].Kind != "import" {
		t.Fatalf("plugins: %+v", res.Plugins)
	}
	if len(res.Deps) != 1 || res.Deps["src_b"].Kind != "import" {
		t.Fatalf("deps: %+v", res.Deps)
	}
}

func TestEvalPlugins_StructDepsOnly(t *testing.T) {
	recipeSrc := []byte(`
def plugins():
    return struct(deps = {"d": imp(fetcher = "github", params = {"owner": "o", "repo": "r", "ref": "a"})})
`)
	res, err := EvalPlugins(cfg(t, MapSource{}), recipeSrc)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Plugins) != 0 || len(res.Deps) != 1 {
		t.Fatalf("got %d plugins / %d deps", len(res.Plugins), len(res.Deps))
	}
}

func TestEvalPlugins_BadDepNameFails(t *testing.T) {
	recipeSrc := []byte(`
def plugins():
    return struct(deps = {"a/b": imp(fetcher = "github", params = {"owner": "o", "repo": "r", "ref": "a"})})
`)
	_, err := EvalPlugins(cfg(t, MapSource{}), recipeSrc)
	if err == nil || !strings.Contains(err.Error(), "path separators") {
		t.Fatalf("want dep-name error, got %v", err)
	}
}

// A typo'd struct field must fail at the resolve stage, not silently drop the
// declaration (code-review finding: permissive struct decoding).
func TestEvalPlugins_UnknownStructFieldFails(t *testing.T) {
	recipeSrc := []byte(`
def plugins():
    return struct(dep = {"b": imp(fetcher = "github", params = {"owner": "o", "repo": "r", "ref": "a"})})
`)
	_, err := EvalPlugins(cfg(t, MapSource{}), recipeSrc)
	if err == nil || !strings.Contains(err.Error(), `"dep" is not recognized`) {
		t.Fatalf("want unknown-field error, got %v", err)
	}
}

func TestEvalPlugins_DepsAccessInPluginsFails(t *testing.T) {
	recipeSrc := []byte(`
def plugins():
    x = deps["nope"]
    return {}
`)
	_, err := EvalPlugins(cfg(t, MapSource{}), recipeSrc)
	if err == nil || !strings.Contains(err.Error(), "not available during plugins()") {
		t.Fatalf("want deps-unavailable error, got %v", err)
	}
}

// A recipe whose build() references deps must COMPILE at the plugins() stage
// (Starlark checks all names at ExecFile time — the struct precedent).
func TestEvalPlugins_BuildUsingDepsCompiles(t *testing.T) {
	recipeSrc := []byte(`
def plugins():
    return struct(deps = {"d": imp(fetcher = "github", params = {"owner": "o", "repo": "r", "ref": "a"})})

def build():
    data = deps["d"].read("go.sum")
    return struct(inputs = {}, env = {}, script = "", runtime_deps = [])
`)
	if _, err := EvalPlugins(cfg(t, MapSource{}), recipeSrc); err != nil {
		t.Fatal(err)
	}
}

func TestEvalPlugins_SourceReadAndExists(t *testing.T) {
	src := MapSource{"go.mod": []byte("module x\n")}
	recipeSrc := []byte(`
def plugins():
    if not source.exists("go.mod"):
        fail("go.mod missing")
    data = source.read("go.mod")
    if data != b"module x\n":
        fail("unexpected go.mod")
    return {}
`)
	if _, err := EvalPlugins(cfg(t, src), recipeSrc); err != nil {
		t.Fatal(err)
	}
}

func TestEvalPlugins_RecipeErrorIsReturned(t *testing.T) {
	_, err := EvalPlugins(cfg(t, MapSource{}), []byte("def plugins():\n    fail(\"boom\")\n"))
	if err == nil {
		t.Fatal("expected error from fail()")
	}
}

// fakePlugin records the kwargs it was called with and returns a fixed response.
type fakePlugin struct {
	gotCall map[string]any
	resp    any
}

func (f *fakePlugin) Call(kwargs map[string]any) (any, error) {
	f.gotCall = kwargs
	return f.resp, nil
}

func TestEvalBuild_StructWithPluginCall(t *testing.T) {
	// The plugin returns a list with one struct holding an Input (the go2nix
	// shape, build.md §3.2). build() folds that Input into `inputs`.
	mod := map[string]any{
		"path":    "example.com/m",
		"version": "v1.0.0",
		"input": map[string]any{
			"kind":       "import",
			"definition": importDefBytes(t, "tarball+https", "https://example.com/m.zip"),
		},
	}
	fp := &fakePlugin{resp: []any{mod}}

	recipeSrc := []byte(`
def build():
    toolchain = imp(fetcher = "tarball+https", params = {"url": "https://go.dev/go.tgz"})
    mods = plugins["go"](go_mod = source.read("go.mod"))
    inputs = {"toolchain": toolchain}
    for m in mods:
        inputs["mod_" + m["path"]] = m["input"]
    return struct(
        inputs = inputs,
        env = {"GOFLAGS": "-mod=mod"},
        script = "go build",
        runtime_deps = [],
    )
`)
	src := MapSource{"go.mod": []byte("module x\n")}
	res, err := EvalBuild(cfg(t, src), recipeSrc, map[string]PluginSpec{"go": {Caller: fp}})
	if err != nil {
		t.Fatal(err)
	}
	if string(fp.gotCall["go_mod"].([]byte)) != "module x\n" {
		t.Fatalf("plugin kwargs=%#v", fp.gotCall)
	}
	if len(res.Inputs) != 2 {
		t.Fatalf("inputs=%d want 2 (toolchain + module)", len(res.Inputs))
	}
	// Inputs are named; the toolchain keeps the name the recipe gave it.
	names := map[string]bool{}
	for _, in := range res.Inputs {
		names[in.Name] = true
	}
	if !names["toolchain"] || !names["mod_example.com/m"] {
		t.Fatalf("input names=%v", names)
	}
	// Every import — recipe-made (toolchain) and plugin-emitted (module) —
	// arrives pinned to the build's platform (import-platform-pinning design).
	for _, in := range res.Inputs {
		if in.In.Kind != builddef.KindImport {
			continue
		}
		def, err := importdef.Decode(in.In.Definition)
		if err != nil {
			t.Fatal(err)
		}
		if def.Platform != "linux/amd64" {
			t.Fatalf("input %q Platform=%q want linux/amd64", in.Name, def.Platform)
		}
	}
	if res.Env["GOFLAGS"] != "-mod=mod" || res.Script != "go build" {
		t.Fatalf("result=%+v", res)
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

// TestEvalBuild_PinsInputSmuggledThroughParams: params rehydrate unpinned by
// design (opaque user data), so an Input spec routed from params into inputs /
// runtime_deps would bypass imp()'s construction-point pin — the eval-boundary
// normalization must catch it before it can enter Pinned.Inputs.
func TestEvalBuild_PinsInputSmuggledThroughParams(t *testing.T) {
	spec := map[string]any{
		"kind":       "import",
		"definition": importDefBytes(t, "gomod", "https://proxy.golang.org/m"),
	}
	p, err := importdef.CanonicalParams(map[string]any{"in": spec})
	if err != nil {
		t.Fatal(err)
	}
	recipeSrc := []byte(`
def build():
    return struct(inputs = {"a": params["in"]}, env = {}, script = "", runtime_deps = [params["in"]])
`)
	res, err := EvalBuild(EvalConfig{Platform: "linux/amd64", Params: p, Source: MapSource{}}, recipeSrc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Inputs) != 1 || len(res.RuntimeDeps) != 1 {
		t.Fatalf("inputs=%d runtime_deps=%d, want 1/1", len(res.Inputs), len(res.RuntimeDeps))
	}
	for name, in := range map[string]builddef.Input{"input": res.Inputs[0].In, "runtime_dep": res.RuntimeDeps[0]} {
		def, derr := importdef.Decode(in.Definition)
		if derr != nil {
			t.Fatal(derr)
		}
		if def.Platform != "linux/amd64" {
			t.Fatalf("%s Platform=%q want linux/amd64", name, def.Platform)
		}
	}
	// Normalizing inputs and runtime_deps together keeps runtime_deps ⊆ inputs.
	if err := res.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestEvalBuild_TupleFormAccepted(t *testing.T) {
	recipeSrc := []byte(`
def build():
    a = imp(fetcher = "f", params = {"u": "x"})
    return ({"a": a}, {"K": "v"}, "echo hi", [a])
`)
	res, err := EvalBuild(cfg(t, MapSource{}), recipeSrc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Inputs) != 1 || res.Script != "echo hi" || len(res.RuntimeDeps) != 1 {
		t.Fatalf("tuple result=%+v", res)
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestBuildResult_ValidateRejectsRuntimeDepNotInInputs(t *testing.T) {
	recipeSrc := []byte(`
def build():
    a = imp(fetcher = "f", params = {"u": "a"})
    b = imp(fetcher = "f", params = {"u": "b"})
    return struct(inputs = {"a": a}, env = {}, script = "", runtime_deps = [b])
`)
	res, err := EvalBuild(cfg(t, MapSource{}), recipeSrc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := res.Validate(); err == nil {
		t.Fatal("expected Validate to reject runtime_dep not in inputs")
	}
}

func TestEvalBuild_MissingFunctionErrors(t *testing.T) {
	_, err := EvalBuild(cfg(t, MapSource{}), []byte("x = 1\n"), nil)
	if err == nil {
		t.Fatal("expected error when build() is absent")
	}
}
