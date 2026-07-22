package builddef

import (
	"testing"

	"github.com/fables-for-robots/jobs-iroh/amber"
)

func TestTreeInput_RoundTrip(t *testing.T) {
	k, err := amber.FileKey([]byte("hello tree"))
	if err != nil {
		t.Fatal(err)
	}
	in, err := TreeInput(k)
	if err != nil {
		t.Fatal(err)
	}
	if in.Kind != KindTree {
		t.Fatalf("kind=%q want %q", in.Kind, KindTree)
	}
	got, err := DecodeTreeKey(in.Definition)
	if err != nil {
		t.Fatal(err)
	}
	if got != k {
		t.Fatalf("DecodeTreeKey=%s want %s", got, k)
	}
}

func TestTreeInput_KeyStable(t *testing.T) {
	k, err := amber.FileKey([]byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := TreeInput(k)
	if err != nil {
		t.Fatal(err)
	}
	b, err := TreeInput(k)
	if err != nil {
		t.Fatal(err)
	}
	ak, err := a.Key()
	if err != nil {
		t.Fatal(err)
	}
	bk, err := b.Key()
	if err != nil {
		t.Fatal(err)
	}
	if ak != bk {
		t.Fatalf("tree Input K unstable: %s vs %s", ak, bk)
	}
}
