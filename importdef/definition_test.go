package importdef

import (
	"bytes"
	"testing"

	"github.com/fxamacker/cbor/v2"
)

func mustParams(t *testing.T, v any) []byte {
	t.Helper()
	p, err := CanonicalParams(v)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCanonical_TagOrderAndDupsDoNotChangeBytes(t *testing.T) {
	p := mustParams(t, map[string]any{"url": "https://example.com/x.tgz"})
	a := Definition{Fetcher: "tarball+https", Params: p, RequiredTags: []string{"b", "a", "a"}}
	b := Definition{Fetcher: "tarball+https", Params: p, RequiredTags: []string{"a", "b"}}
	ab, err := a.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	bb, err := b.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ab, bb) {
		t.Fatalf("canonical bytes differ:\n a=%x\n b=%x", ab, bb)
	}
}

func TestDecode_RoundTripAndParamsJSON(t *testing.T) {
	p := mustParams(t, map[string]any{"url": "https://example.com/x.tgz", "n": 3})
	d := Definition{Fetcher: "tarball+https", Params: p, RequiredTags: []string{"a"}}
	enc, err := d.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(enc)
	if err != nil {
		t.Fatal(err)
	}
	if got.Fetcher != "tarball+https" || len(got.RequiredTags) != 1 || got.RequiredTags[0] != "a" {
		t.Fatalf("decoded mismatch: %+v", got)
	}
	js, err := got.ParamsJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(js, []byte(`"url":"https://example.com/x.tgz"`)) {
		t.Fatalf("params json missing url: %s", js)
	}
}

func TestCanonical_EmptyTagsOmitted(t *testing.T) {
	p := mustParams(t, map[string]any{"k": "v"})
	withEmpty := Definition{Fetcher: "f", Params: p, RequiredTags: []string{}}
	withNil := Definition{Fetcher: "f", Params: p}
	a, err := withEmpty.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	b, err := withNil.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("empty vs nil tags must encode identically:\n a=%x\n b=%x", a, b)
	}
}

func TestFetcherDefOmittedWhenEmpty(t *testing.T) {
	// Legacy-K stability: a def without FetcherDef must encode byte-identically
	// to the pre-change struct — no "fetcherDef" key may appear.
	d := Definition{Fetcher: "gomod", Platform: "linux/amd64"}
	b, err := d.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := cbor.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["fetcherDef"]; ok {
		t.Fatalf("empty FetcherDef must be omitted from canonical CBOR: %v", m)
	}
}

func TestFetcherDefRoundTripAndIdentity(t *testing.T) {
	fd, err := CanonicalParams(map[string]any{"marker": true}) // any canonical bytes will do
	if err != nil {
		t.Fatal(err)
	}
	with := Definition{Fetcher: "gomod", Platform: "linux/amd64", FetcherDef: cbor.RawMessage(fd)}
	without := Definition{Fetcher: "gomod", Platform: "linux/amd64"}
	bw, err := with.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	bo, err := without.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(bw, bo) {
		t.Fatal("FetcherDef must participate in identity")
	}
	back, err := Decode(bw)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back.FetcherDef, fd) {
		t.Fatal("FetcherDef must round-trip")
	}
}
