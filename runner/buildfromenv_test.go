package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/fables-for-robots/jobs-iroh/importdef"
)

func TestLoadBuildFromEnv_SourceContentKey(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "BUILD.jobs"), []byte("def build(): pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	envKey, err := st.IngestDir(ctx, srcDir)
	if err != nil {
		t.Fatal(err)
	}
	params, err := importdef.CanonicalParams(nil)
	if err != nil {
		t.Fatal(err)
	}
	f, err := st.BuildFromTree(ctx, envKey, "", params, "linux/amd64", nil)
	if err != nil {
		t.Fatal(err)
	}

	env, o := loadBuildFromEnv(ctx, st, f)
	if o != nil {
		t.Fatalf("loadBuildFromEnv: %+v", *o)
	}
	defer env.cleanup()

	if env.SourceContentKey != envKey {
		t.Fatalf("SourceContentKey=%s want env subtree key %s", env.SourceContentKey, envKey)
	}
}
