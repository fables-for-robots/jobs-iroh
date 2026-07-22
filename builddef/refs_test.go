package builddef

import (
	"bytes"
	"strings"
	"testing"

	"github.com/fxamacker/cbor/v2"
)

func TestPinned_RoundTripCanonical(t *testing.T) {
	a := importInput(t, "tarball+https", "https://example.com/a.tgz")
	ak, err := a.Key()
	if err != nil {
		t.Fatal(err)
	}
	p := Pinned{
		Inputs:      []PinnedInput{{Name: "toolchain", Kind: a.Kind, Definition: a.Definition}},
		Env:         map[string]string{"GOFLAGS": "-mod=mod"},
		Script:      "go build",
		RuntimeDeps: [][]byte{ak[:]},
	}
	enc, err := EncodePinned(p)
	if err != nil {
		t.Fatal(err)
	}
	enc2, err := EncodePinned(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(enc, enc2) {
		t.Fatal("pinned encoding not deterministic")
	}
	got, err := DecodePinned(enc)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Inputs) != 1 || got.Inputs[0].Name != "toolchain" || got.Script != "go build" || len(got.RuntimeDeps) != 1 {
		t.Fatalf("decoded pinned mismatch: %+v", got)
	}
}

func TestPinnedResourcesOmitemptyByteStable(t *testing.T) {
	base := Pinned{Env: map[string]string{}, Script: "x"}
	b1, err := EncodePinned(base)
	if err != nil {
		t.Fatal(err)
	}
	// A Pinned with a nil Resources must encode identically to the pre-field bytes.
	withNil := base
	withNil.Resources = nil
	b2, _ := EncodePinned(withNil)
	if string(b1) != string(b2) {
		t.Fatal("nil Resources must not change encoding")
	}
	// Round-trip a set requirement.
	withNil.Resources = &PinnedResources{CPUMilli: 2000, MemBytes: 4 << 30}
	b3, _ := EncodePinned(withNil)
	got, err := DecodePinned(b3)
	if err != nil {
		t.Fatal(err)
	}
	if got.Resources == nil || got.Resources.CPUMilli != 2000 || got.Resources.MemBytes != 4<<30 {
		t.Fatalf("resources round-trip: %+v", got.Resources)
	}
}

func TestPluginResolved_RoundTrip(t *testing.T) {
	plug := importInput(t, "github", "https://github.com/jobs-tools/go-plugin")
	pr := PluginResolved{Plugins: map[string]Input{"go": plug}}
	enc, err := EncodePluginResolved(pr)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodePluginResolved(enc)
	if err != nil {
		t.Fatal(err)
	}
	if got.Plugins["go"].Kind != KindImport {
		t.Fatalf("decoded plugins mismatch: %+v", got)
	}
}

func TestPluginResolved_DepsRoundTrip(t *testing.T) {
	in := PluginResolved{
		Plugins: map[string]Input{"go": importInput(t, "github", "https://github.com/jobs-tools/go-plugin")},
		Deps:    map[string]Input{"src": importInput(t, "github", "https://github.com/acme/svc-b")},
	}
	b, err := EncodePluginResolved(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := DecodePluginResolved(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Deps) != 1 || out.Deps["src"].Kind != KindImport {
		t.Fatalf("deps lost in round-trip: %+v", out)
	}
}

// A dep-less PluginResolved must encode byte-identical to the pre-deps schema.
func TestPluginResolved_DeplessEncodingUnchanged(t *testing.T) {
	type legacy struct {
		Plugins map[string]Input `cbor:"plugins"`
	}
	enc, err := cbor.CanonicalEncOptions().EncMode()
	if err != nil {
		t.Fatal(err)
	}
	plugins := map[string]Input{"go": importInput(t, "github", "https://github.com/jobs-tools/go-plugin")}
	oldB, err := enc.Marshal(legacy{Plugins: plugins})
	if err != nil {
		t.Fatal(err)
	}
	newB, err := EncodePluginResolved(PluginResolved{Plugins: plugins})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(oldB, newB) {
		t.Fatalf("dep-less encoding changed:\nold %x\nnew %x", oldB, newB)
	}
}

func TestValidateDepName(t *testing.T) {
	for _, ok := range []string{"src", "svc-b", "a.b", "x_1"} {
		if err := ValidateDepName(ok); err != nil {
			t.Errorf("ValidateDepName(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "a/b", ".", "..", "/x", `a\b`, "a\x00b", "a\nb", strings.Repeat("x", 129)} {
		if err := ValidateDepName(bad); err == nil {
			t.Errorf("ValidateDepName(%q) = nil, want error", bad)
		}
	}
}
