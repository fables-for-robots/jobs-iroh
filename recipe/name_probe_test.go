package recipe

import "testing"

func TestBuildNameParsesFromRecipe(t *testing.T) {
	recipeSrc := []byte(`def build():
    return struct(inputs={}, env={"MSG": "x"}, script="true", runtime_deps=[], name="shiny demo build")
`)
	res, err := EvalBuild(EvalConfig{Platform: "linux/amd64", Source: MapSource{}}, recipeSrc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Name != "shiny demo build" {
		t.Fatalf("res.Name = %q, want the recipe name", res.Name)
	}
}
