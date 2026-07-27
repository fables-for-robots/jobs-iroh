package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jobs-build/jobs-iroh/importdef"
)

func TestModulesFromGoSum_SingleModuleNoDeps(t *testing.T) {
	goSum := []byte("github.com/gorilla/mux v1.8.1 h1:TuBL49tXwgrFYWhqrNgrUNEY92u81SPhu7sTdzQEiWY=\n" +
		"github.com/gorilla/mux v1.8.1/go.mod h1:AKf9I4AEqPTmMytcMc0KkNouC66V3BtZ4qD5fmWSiMQ=\n")
	got := modulesFromGoSum(goSum)
	want := []module{{Path: "github.com/gorilla/mux", Version: "v1.8.1"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestModulesFromGoSum_SortedAndDeduped(t *testing.T) {
	goSum := []byte("example.com/b v2.0.0 h1:b=\n" +
		"example.com/b v2.0.0/go.mod h1:b=\n" +
		"example.com/a v1.0.0 h1:a=\n" +
		"example.com/a v1.0.0/go.mod h1:a=\n" +
		"example.com/a v1.0.0 h1:a=\n" + // duplicate zip line
		"\n") // blank line ignored
	got := modulesFromGoSum(goSum)
	want := []module{
		{Path: "example.com/a", Version: "v1.0.0"},
		{Path: "example.com/b", Version: "v2.0.0"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestModuleInput_GomodImportSpec(t *testing.T) {
	spec, err := moduleInput(module{Path: "github.com/gorilla/mux", Version: "v1.8.1"})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Kind != "import" {
		t.Fatalf("kind = %q, want import", spec.Kind)
	}
	def, err := importdef.Decode(spec.Definition)
	if err != nil {
		t.Fatalf("decode definition: %v", err)
	}
	if def.Fetcher != "gomod" {
		t.Fatalf("fetcher = %q, want gomod", def.Fetcher)
	}
	pj, err := def.ParamsJSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"github.com/gorilla/mux", "v1.8.1", "module", "version"} {
		if !strings.Contains(string(pj), want) {
			t.Fatalf("params %s missing %q", pj, want)
		}
	}
}

// Same module → byte-identical canonical definition (so its identity K is stable).
func TestModuleInput_Deterministic(t *testing.T) {
	a, err := moduleInput(module{Path: "x", Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := moduleInput(module{Path: "x", Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if string(a.Definition) != string(b.Definition) {
		t.Fatal("definition not deterministic")
	}
}
