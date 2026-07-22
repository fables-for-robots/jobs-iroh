package recipe

import (
	"testing"

	"github.com/fables-for-robots/jobs-iroh/importdef"
	"go.starlark.net/starlark"
)

func TestRoundTrip_ScalarsAndContainers(t *testing.T) {
	in := map[string]any{
		"s":    "hi",
		"n":    int64(3),
		"b":    true,
		"list": []any{int64(1), "two"},
		"blob": []byte{0xde, 0xad},
	}
	sv, err := toStarlark(in, "")
	if err != nil {
		t.Fatal(err)
	}
	back, err := fromStarlark(sv)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := back.(map[string]any)
	if !ok {
		t.Fatalf("not a map: %#v", back)
	}
	if m["s"] != "hi" || m["n"] != int64(3) || m["b"] != true {
		t.Fatalf("scalars mismatch: %#v", m)
	}
	if blob, ok := m["blob"].([]byte); !ok || len(blob) != 2 || blob[0] != 0xde {
		t.Fatalf("blob mismatch: %#v", m["blob"])
	}
}

func TestRehydrate_InputMapBecomesInputValue(t *testing.T) {
	// A {kind, definition} map (the plugin Input wire form, build.md §6) must
	// rehydrate to an *Input Starlark value exposing .kind.
	spec := map[string]any{
		"kind":       "import",
		"definition": importDefBytes(t, "tarball+https", "https://example.com/x.tgz"),
	}
	sv, err := toStarlark(spec, "")
	if err != nil {
		t.Fatal(err)
	}
	iv, ok := sv.(*Input)
	if !ok {
		t.Fatalf("expected *Input, got %T", sv)
	}
	kindAttr, err := iv.Attr("kind")
	if err != nil {
		t.Fatal(err)
	}
	if s, ok := starlark.AsString(kindAttr); !ok || s != "import" {
		t.Fatalf("kind attr=%v", kindAttr)
	}
	if iv.In.Kind != "import" {
		t.Fatalf("kind=%q", iv.In.Kind)
	}
}

func TestRehydrate_PinsPluginEmittedImports(t *testing.T) {
	// A plugin response carries a platform-less import def (what all published
	// plugins emit); rehydration under a build platform must pin it
	// (import-platform-pinning design).
	spec := map[string]any{
		"kind":       "import",
		"definition": importDefBytes(t, "gomod", "https://proxy.golang.org/m"),
	}
	sv, err := toStarlark(spec, "linux/arm64")
	if err != nil {
		t.Fatal(err)
	}
	iv, ok := sv.(*Input)
	if !ok {
		t.Fatalf("expected *Input, got %T", sv)
	}
	def, err := importdef.Decode(iv.In.Definition)
	if err != nil {
		t.Fatal(err)
	}
	if def.Platform != "linux/arm64" {
		t.Fatalf("Platform=%q want linux/arm64", def.Platform)
	}
}

func TestRehydrate_NestedInputSpecInListIsPinned(t *testing.T) {
	// The go-plugin shape: a list of module structs each carrying an input spec.
	mod := map[string]any{
		"path":  "example.com/m",
		"input": map[string]any{"kind": "import", "definition": importDefBytes(t, "gomod", "https://proxy.golang.org/m")},
	}
	sv, err := toStarlark([]any{mod}, "linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	lst := sv.(*starlark.List)
	entry := lst.Index(0).(*starlark.Dict)
	inV, found, err := entry.Get(starlark.String("input"))
	if err != nil || !found {
		t.Fatalf("input entry: %v found=%v", err, found)
	}
	def, err := importdef.Decode(inV.(*Input).In.Definition)
	if err != nil {
		t.Fatal(err)
	}
	if def.Platform != "linux/amd64" {
		t.Fatalf("Platform=%q want linux/amd64", def.Platform)
	}
}
