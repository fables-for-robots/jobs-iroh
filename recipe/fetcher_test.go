package recipe

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jobs-build/jobs-iroh/builddef"
	"github.com/jobs-build/jobs-iroh/importdef"
	"go.starlark.net/starlark"
)

// callBuiltinErr calls a builtin expecting an error.
func callBuiltinErr(t *testing.T, b *starlark.Builtin, kwargs []starlark.Tuple) error {
	t.Helper()
	_, err := starlark.Call(&starlark.Thread{Name: "t"}, b, nil, kwargs)
	if err == nil {
		t.Fatalf("call %s: expected error", b.Name())
	}
	return err
}

func kw(pairs ...string) []starlark.Tuple {
	out := make([]starlark.Tuple, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, starlark.Tuple{starlark.String(pairs[i]), starlark.String(pairs[i+1])})
	}
	return out
}

func TestFetcher_URLSugarEmbedsFetcherBuild(t *testing.T) {
	f := makeFetcher("linux/amd64")
	v := callBuiltin(t, f, kw("name", "gomod", "url", "https://example.com/fg.tar.gz", "sha256", "cafe"))
	fv, ok := v.(*Fetcher)
	if !ok {
		t.Fatalf("fetcher returned %T", v)
	}
	if fv.Name != "gomod" {
		t.Fatalf("name: %q", fv.Name)
	}
	want, err := builddef.FetcherBuild("https://example.com/fg.tar.gz", "cafe", "linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	wc, err := want.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fv.Def, wc) {
		t.Fatal("Def must equal the shared FetcherBuild synthesis (identity across layers)")
	}
}

func TestFetcher_ExplicitBuild(t *testing.T) {
	def, err := builddef.FetcherBuild("https://e/x.tar.gz", "aa", "linux/arm64")
	if err != nil {
		t.Fatal(err)
	}
	canon, err := def.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	bldIn := &Input{In: builddef.Input{Kind: builddef.KindBuild, Definition: canon}}
	f := makeFetcher("linux/arm64")
	v := callBuiltin(t, f, []starlark.Tuple{
		{starlark.String("name"), starlark.String("x")},
		{starlark.String("build"), bldIn},
	})
	fv := v.(*Fetcher)
	if !bytes.Equal(fv.Def, canon) {
		t.Fatal("explicit build= must be carried verbatim")
	}
}

func TestFetcher_Validation(t *testing.T) {
	f := makeFetcher("linux/amd64")
	imp := &Input{In: builddef.Input{Kind: builddef.KindImport, Definition: []byte{0xf6}}}
	cases := []struct {
		name   string
		kwargs []starlark.Tuple
		want   string
	}{
		{"neither form", kw("name", "x"), "exactly one of"},
		{"sha256 missing", kw("name", "x", "url", "u"), "both required"},
		{"both forms", append(kw("name", "x", "url", "u", "sha256", "s"),
			starlark.Tuple{starlark.String("build"), imp}), "exactly one of"},
		{"name required", kw("url", "u", "sha256", "s"), "missing argument for name"},
		{"build wrong kind", []starlark.Tuple{
			{starlark.String("name"), starlark.String("x")},
			{starlark.String("build"), imp}}, "must be a bld"},
	}
	for _, c := range cases {
		if err := callBuiltinErr(t, f, c.kwargs); !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: error %q does not contain %q", c.name, err, c.want)
		}
	}
}

func TestImp_FetcherValueEmbedsDef(t *testing.T) {
	f := callBuiltin(t, makeFetcher("linux/amd64"), kw("name", "gomod", "url", "https://e/fg.tar.gz", "sha256", "aa")).(*Fetcher)
	imp := makeImp("linux/amd64")
	v := callBuiltin(t, imp, []starlark.Tuple{
		{starlark.String("fetcher"), f},
		{starlark.String("params"), strDict(t, "module", "example.com/m")},
	})
	in := v.(*Input)
	def, err := importdef.Decode(in.In.Definition)
	if err != nil {
		t.Fatal(err)
	}
	if def.Fetcher != "gomod" || def.Platform != "linux/amd64" {
		t.Fatalf("def: %+v", def)
	}
	if !bytes.Equal(def.FetcherDef, f.Def) {
		t.Fatal("imp must embed the fetcher value's build def as FetcherDef")
	}
}

func TestImp_StringFetcherHasNoFetcherDef(t *testing.T) {
	imp := makeImp("linux/amd64")
	v := callBuiltin(t, imp, []starlark.Tuple{
		{starlark.String("fetcher"), starlark.String("tarball+https")},
		{starlark.String("params"), strDict(t, "url", "https://e/x.tgz")},
	})
	def, err := importdef.Decode(v.(*Input).In.Definition)
	if err != nil {
		t.Fatal(err)
	}
	if len(def.FetcherDef) != 0 {
		t.Fatal("string fetcher names must not carry a FetcherDef")
	}
}

func TestImp_FetcherWrongTypeRejected(t *testing.T) {
	imp := makeImp("linux/amd64")
	err := callBuiltinErr(t, imp, []starlark.Tuple{
		{starlark.String("fetcher"), starlark.MakeInt(3)},
		{starlark.String("params"), strDict(t, "url", "u")},
	})
	if !strings.Contains(err.Error(), "want a string") {
		t.Fatalf("error: %v", err)
	}
}
