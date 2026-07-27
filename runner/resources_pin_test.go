//go:build linux

package runner_test

import (
	"context"
	"testing"

	"github.com/jobs-build/jobs-iroh/builddef"
	"github.com/jobs-build/jobs-iroh/runner"
)

// TestRunPin_CarriesResources: a recipe declaring resources pins them into
// build-pinned:F (multi-job-runner design; F-deterministic, not part of K/F).
func TestRunPin_CarriesResources(t *testing.T) {
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
        resources = struct(cpu = "2", memory = "4Gi"),
    )
`
	srcInput, _ := mkImportInputWithOutput(t, ctx, st, "src-fetcher",
		"https://example.com/pin-resources-src.tgz", "BUILD.jobs", recipeSrc)
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
	if pinned.Resources == nil || pinned.Resources.CPUMilli != 2000 || pinned.Resources.MemBytes != 4<<30 {
		t.Fatalf("pinned resources = %+v, want cpu=2000 mem=4Gi", pinned.Resources)
	}
}
