package recipe

import (
	"strings"
	"testing"
)

func evalNameRecipe(t *testing.T, nameLine string) (BuildResult, error) {
	t.Helper()
	recipeSrc := []byte(`
def build():
    return struct(
        inputs = {},
        env = {},
        script = "true",
        runtime_deps = [],
` + nameLine + `
    )
`)
	return EvalBuild(cfg(t, MapSource{}), recipeSrc, nil)
}

func TestEvalBuild_NameDecoded(t *testing.T) {
	res, err := evalNameRecipe(t, `        name = "jobs-server",`)
	if err != nil {
		t.Fatal(err)
	}
	if res.Name != "jobs-server" {
		t.Fatalf("res.Name = %q, want jobs-server", res.Name)
	}
}

func TestEvalBuild_NameAbsent(t *testing.T) {
	res, err := evalNameRecipe(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Name != "" {
		t.Fatalf("expected empty Name, got %q", res.Name)
	}
}

func TestEvalBuild_NameMustBeString(t *testing.T) {
	_, err := evalNameRecipe(t, `        name = 42,`)
	if err == nil || !strings.Contains(err.Error(), "name") {
		t.Fatalf("expected a name-type error, got %v", err)
	}
}
