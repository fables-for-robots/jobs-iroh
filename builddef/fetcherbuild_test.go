package builddef

import (
	"testing"

	"github.com/jobs-build/jobs-iroh/importdef"
)

func TestFetcherBuildShape(t *testing.T) {
	d, err := FetcherBuild("https://example.com/src.tar.gz", "ab12", "linux/arm64")
	if err != nil {
		t.Fatal(err)
	}
	if d.Platform != "linux/arm64" {
		t.Fatalf("platform: %q", d.Platform)
	}
	if d.Source.Kind != KindImport {
		t.Fatalf("source kind: %q", d.Source.Kind)
	}
	src, err := importdef.Decode(d.Source.Definition)
	if err != nil {
		t.Fatal(err)
	}
	if src.Fetcher != "tarball+https" || src.Platform != "linux/arm64" {
		t.Fatalf("source import: %+v", src)
	}
	pj, err := src.ParamsJSON()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"sha256":"ab12","strip":1,"url":"https://example.com/src.tar.gz"}`
	if string(pj) != want {
		t.Fatalf("params: %s want %s", pj, want)
	}
	// No-params build must encode as canonical CBOR null (0xf6), matching the
	// codebase convention — an empty map would derive a different K.
	if len(d.Params) != 1 || d.Params[0] != 0xf6 {
		t.Fatalf("params must be canonical null: %x", d.Params)
	}
	if _, err := d.Key(); err != nil {
		t.Fatal(err)
	}
}
