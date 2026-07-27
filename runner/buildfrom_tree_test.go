package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jobs-build/jobs-iroh/builddef"
)

func TestResolveSourceContentTree_Tree(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	treeKey, err := st.IngestDir(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	in, err := builddef.TreeInput(treeKey)
	if err != nil {
		t.Fatal(err)
	}

	got, o := resolveSourceContentTree(ctx, st, in)
	if o != nil {
		t.Fatalf("resolve: %+v", *o)
	}
	if got != treeKey {
		t.Fatalf("got %s want %s", got, treeKey)
	}
}

func TestResolveSourceContentTree_TreeMalformed(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	in := builddef.Input{Kind: builddef.KindTree, Definition: []byte{0xff, 0xff}}
	_, o := resolveSourceContentTree(ctx, st, in)
	if o == nil || o.Class != "hard" {
		t.Fatalf("malformed tree def must be a hard outcome, got %+v", o)
	}
}
