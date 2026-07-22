package builddef

import (
	"bytes"
	"testing"
)

func TestCanonicalPinnedInputs_OrderAndDedup(t *testing.T) {
	a := importInput(t, "tarball+https", "https://example.com/a.tgz")
	b := importInput(t, "tarball+https", "https://example.com/b.tgz")

	pi := func(name string, in Input) PinnedInput {
		return PinnedInput{Name: name, Kind: in.Kind, Definition: in.Definition}
	}

	// order1: toolchain, lib, toolchain (duplicate name "toolchain")
	inputs1 := []PinnedInput{pi("toolchain", a), pi("lib", b), pi("toolchain", a)}
	// order2: lib, toolchain (different order, no duplicate)
	inputs2 := []PinnedInput{pi("lib", b), pi("toolchain", a)}

	ak, err := a.Key()
	if err != nil {
		t.Fatal(err)
	}
	bk, err := b.Key()
	if err != nil {
		t.Fatal(err)
	}

	p1 := Pinned{
		Inputs:      CanonicalPinnedInputs(inputs1),
		Env:         map[string]string{},
		Script:      "build.sh",
		RuntimeDeps: SortKeys([][]byte{ak[:], bk[:], ak[:]}),
	}
	p2 := Pinned{
		Inputs:      CanonicalPinnedInputs(inputs2),
		Env:         map[string]string{},
		Script:      "build.sh",
		RuntimeDeps: SortKeys([][]byte{bk[:], ak[:]}),
	}

	// Duplicate in inputs1 should collapse to one entry.
	if len(p1.Inputs) != 2 {
		t.Fatalf("expected 2 deduplicated inputs, got %d", len(p1.Inputs))
	}

	// Duplicate in RuntimeDeps should collapse.
	if len(p1.RuntimeDeps) != 2 {
		t.Fatalf("expected 2 deduplicated RuntimeDeps, got %d", len(p1.RuntimeDeps))
	}

	// EncodePinned of both canonicalized Pinned must be byte-equal.
	enc1, err := EncodePinned(p1)
	if err != nil {
		t.Fatal(err)
	}
	enc2, err := EncodePinned(p2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(enc1, enc2) {
		t.Fatalf("canonicalized Pinned encodings differ:\n enc1=%x\n enc2=%x", enc1, enc2)
	}
}

func TestSortKeys_OrderAndDedup(t *testing.T) {
	k1 := bytes.Repeat([]byte{1}, 32)
	k2 := bytes.Repeat([]byte{2}, 32)

	// Two orderings + a duplicate.
	s1 := SortKeys([][]byte{k2, k1, k1})
	s2 := SortKeys([][]byte{k1, k2})

	if len(s1) != 2 {
		t.Fatalf("expected 2 deduplicated keys, got %d", len(s1))
	}

	if len(s1) != len(s2) {
		t.Fatalf("lengths differ: %d vs %d", len(s1), len(s2))
	}
	for i := range s1 {
		if !bytes.Equal(s1[i], s2[i]) {
			t.Fatalf("key[%d] differs: %x vs %x", i, s1[i], s2[i])
		}
	}
}

func TestCanonicalPinnedInputs_Empty(t *testing.T) {
	if CanonicalPinnedInputs(nil) != nil {
		t.Fatal("nil input should return nil")
	}
	if CanonicalPinnedInputs([]PinnedInput{}) != nil {
		t.Fatal("empty slice input should return nil")
	}
}

func TestSortKeys_Empty(t *testing.T) {
	if SortKeys(nil) != nil {
		t.Fatal("nil input should return nil")
	}
	if SortKeys([][]byte{}) != nil {
		t.Fatal("empty slice input should return nil")
	}
}
