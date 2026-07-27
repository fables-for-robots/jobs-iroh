//go:build linux

package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jobs-build/jobs-iroh/builddef"
	"github.com/jobs-build/jobs-iroh/importdef"
)

func TestWriteTreeSourceRefs(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	// A source subtree tk.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tk, err := st.IngestDir(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}

	// A KindBuild input whose Source is TreeInput(tk) — i.e. a subbuild.
	treeSrc, err := builddef.TreeInput(tk)
	if err != nil {
		t.Fatal(err)
	}
	params, _ := importdef.CanonicalParams(nil)
	subDef, err := builddef.Definition{Source: treeSrc, Platform: Platform(), Params: params}.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	subInput := builddef.Input{Kind: builddef.KindBuild, Definition: subDef}
	// A non-tree input that must be ignored (note its Definition is never decoded).
	importInput := builddef.Input{Kind: builddef.KindImport, Definition: []byte("ignored")}

	// subInput twice -> dedup to one ref.
	if o := writeTreeSourceRefs(ctx, st, NewLocalRefWriter(st), []builddef.Input{subInput, importInput, subInput}); o != nil {
		t.Fatalf("writeTreeSourceRefs: %+v", *o)
	}
	got, ok, err := st.GetKey(ctx, "build-from-tree:"+tk.String())
	if err != nil || !ok || got != tk {
		t.Fatalf("build-from-tree:tk = %v ok=%v err=%v, want %v", got, ok, err, tk)
	}
}
