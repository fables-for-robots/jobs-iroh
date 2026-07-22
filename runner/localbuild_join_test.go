//go:build linux

package runner

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// joinBuildJobs is a minimal BUILD.jobs whose build() has no inputs and writes a
// valid JOBS.entrypoint to $out so prepareSourceArtifact can fully resolve the
// artifact (resolveByKeyArtifact requires JOBS.entrypoint).
const joinBuildJobs = `
def plugins():
    return {}

def build():
    return struct(
        inputs = {},
        env = {},
        script = '''
printf '{"command":"echo","args":["hi"],"env":{}}' > "$out/JOBS.entrypoint"
''',
        runtime_deps = [],
    )
`

// TestLocalBuild_SecondRunJoins verifies that the content-addressed join works:
// after a first prepareSourceArtifact call tags all four F-keyed refs
// (build-plugin-resolved:F, build-pinned:F, build-output:F, build-output-deps:F),
// a second call skips the build step entirely and reports the cached state.
func TestLocalBuild_SecondRunJoins(t *testing.T) {
	ctx, st, platform, _ := devSetup(t)

	// Look up the shell key registered by devSetup so localBuildFrom can use it.
	shellKey, ok, err := st.GetKey(ctx, "shell:"+platform)
	if err != nil {
		t.Fatalf("resolve shell ref: %v", err)
	}
	if !ok {
		t.Fatal("shell artifact not registered — devSetup should have done this")
	}

	// Write a minimal source that produces a JOBS.entrypoint (required by
	// prepareSourceArtifact's resolveByKeyArtifact call). The devSetup source
	// does not write JOBS.entrypoint, so we need a separate source dir.
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "BUILD.jobs"), []byte(joinBuildJobs), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := DevelopConfig{SourceDir: srcDir, Platform: platform, CacheDir: t.TempDir()}

	var first, second bytes.Buffer
	if _, err := prepareSourceArtifact(ctx, st, cfg, NewProgress(&first)); err != nil {
		t.Fatalf("first build: %v", err)
	}

	// F is deterministic — recompute it to assert each step was tagged.
	f, err := localBuildFrom(ctx, st, BuildRunCfg{Platform: platform, ShellKey: shellKey, CacheDir: cfg.CacheDir}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"build-plugin-resolved:", "build-pinned:", "build-output:", "build-output-deps:"} {
		if _, ok, gerr := st.GetKey(ctx, name+f.String()); gerr != nil || !ok {
			t.Fatalf("after first build, %s%s missing (ok=%v err=%v)", name, f, ok, gerr)
		}
	}

	if _, err := prepareSourceArtifact(ctx, st, cfg, NewProgress(&second)); err != nil {
		t.Fatalf("second build: %v", err)
	}
	if !strings.Contains(second.String(), "✓ build  (cached)") {
		t.Fatalf("second run did not join (expected a cached build step):\n%s", second.String())
	}
}
