package recipe

import (
	"strings"
	"testing"
)

func depsCfg(t *testing.T) EvalConfig {
	c := cfg(t, MapSource{})
	c.Deps = map[string]DepSource{
		"b": {Source: MapSource{"go.sum": []byte("sumdata")}, SandboxPath: "/jobs/deps/b"},
	}
	return c
}

const depsRecipe = `
def build():
    return struct(
        inputs = {},
        env = {
            "SUM":  str(deps["b"].read("go.sum")),
            "HAS":  str(deps["b"].exists("go.sum")),
            "MISS": str(deps["b"].exists("nope")),
            "ROOT": deps["b"].path(),
            "FILE": deps["b"].path("go.sum"),
        },
        script = "",
        runtime_deps = [],
    )
`

func TestEvalBuild_DepsHandle(t *testing.T) {
	res, err := EvalBuild(depsCfg(t), []byte(depsRecipe), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"SUM": "sumdata", "HAS": "True", "MISS": "False",
		"ROOT": "/jobs/deps/b", "FILE": "/jobs/deps/b/go.sum",
	}
	for k, v := range want {
		if res.Env[k] != v {
			t.Errorf("env[%s] = %q, want %q", k, res.Env[k], v)
		}
	}
}

func TestEvalBuild_UnknownDepErrors(t *testing.T) {
	src := []byte("def build():\n    deps[\"nope\"]\n    return struct(inputs={}, env={}, script=\"\", runtime_deps=[])\n")
	_, err := EvalBuild(depsCfg(t), src, nil)
	if err == nil || !strings.Contains(err.Error(), `no resolution dep "nope"`) {
		t.Fatalf("want unknown-dep error, got %v", err)
	}
}

func TestEvalBuild_DepPathRejectsTraversal(t *testing.T) {
	for _, rel := range []string{"../x", "/abs", "a/../../b"} {
		src := []byte("def build():\n    deps[\"b\"].path(\"" + rel + "\")\n    return struct(inputs={}, env={}, script=\"\", runtime_deps=[])\n")
		if _, err := EvalBuild(depsCfg(t), src, nil); err == nil {
			t.Errorf("path(%q): want error, got nil", rel)
		}
	}
}
