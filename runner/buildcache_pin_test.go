//go:build linux

package runner_test

import (
	"context"
	"testing"

	"github.com/jobs-build/jobs-iroh/builddef"
	"github.com/jobs-build/jobs-iroh/runner"
)

// TestRunPin_CarriesCaches: a recipe declaring caches pins them into
// build-pinned:F, canonically sorted by path (build-cache design §4).
func TestRunPin_CarriesCaches(t *testing.T) {
	ctx := context.Background()
	st := buildEvalStore(t)
	platform := runner.Platform()

	recipeSrc := `
def build():
    return struct(
        inputs = {},
        env = {},
        script = "true",
        runtime_deps = [],
        caches = {"/zz-cache": "cache-b", "/aa-cache": "cache-a"},
    )
`
	srcInput, _ := mkImportInputWithOutput(t, ctx, st, "src-fetcher",
		"https://example.com/pin-caches-src.tgz", "BUILD.jobs", recipeSrc)
	_, buildK := makeBuildDef(t, ctx, st, srcInput, platform)

	brc := runner.BuildRunCfg{Platform: platform, CacheDir: t.TempDir()}
	rw := runner.NewLocalRefWriter(st)

	bf := runner.RunBuildFrom(ctx, st, rw, brc, buildK)
	if bf.Failed || bf.Decline || bf.Cancelled {
		t.Fatalf("RunBuildFrom: %+v", bf)
	}
	f := bf.OutputKey

	if out := runner.RunPluginResolve(ctx, st, rw, brc, f); out.Failed || out.Decline || out.Cancelled {
		t.Fatalf("RunPluginResolve: %+v", out)
	}
	if out := runner.RunPin(ctx, st, rw, brc, f); out.Failed || out.Decline || out.Cancelled {
		t.Fatalf("RunPin: %+v", out)
	}

	pinnedKey, ok, err := st.GetKey(ctx, "build-pinned:"+f.String())
	if err != nil || !ok {
		t.Fatalf("build-pinned ref: ok=%v err=%v", ok, err)
	}
	b, err := st.ReadFile(ctx, pinnedKey)
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := builddef.DecodePinned(b)
	if err != nil {
		t.Fatal(err)
	}
	want := []builddef.PinnedCache{{Path: "/aa-cache", ID: "cache-a"}, {Path: "/zz-cache", ID: "cache-b"}}
	if len(pinned.Caches) != 2 || pinned.Caches[0] != want[0] || pinned.Caches[1] != want[1] {
		t.Fatalf("pinned caches = %+v, want %+v (sorted by path)", pinned.Caches, want)
	}
}
