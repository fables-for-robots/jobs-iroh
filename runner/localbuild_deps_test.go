//go:build linux

package runner

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// resolutionDepBuildJobs declares a subbuild as a RESOLUTION dep: the aux
// subtree is built before pin, and build() reads a file out of its output
// (resolution-deps design §6.3 — the local path's ensure loop must build it).
const resolutionDepBuildJobs = `
def plugins():
    return struct(deps = {"aux": subbuild(dir = "aux")})

def build():
    marker = str(deps["aux"].read("c-marker.txt"))
    return struct(
        inputs = {},
        env = {"FROM_DEP": marker},
        script = '''
printf '%s' "$FROM_DEP" > "$out/result"
printf '{"command":"echo","args":["hi"],"env":{}}' > "$out/JOBS.entrypoint"
''',
        runtime_deps = [],
    )
`

const resolutionDepAuxBuildJobs = `
def build():
    return struct(
        inputs = {},
        env = {},
        script = '''
printf 'dep-built' > "$out/c-marker.txt"
''',
        runtime_deps = [],
    )
`

// TestLocalBuild_ResolutionDep drives the LOCAL pipeline (jobs run path) for a
// recipe whose plugins() declares a subbuild resolution dep: ensurePinDeps must
// build the dep before RunPin materializes it for the deps handle.
func TestLocalBuild_ResolutionDep(t *testing.T) {
	ctx, st, platform, _ := devSetup(t)

	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "BUILD.jobs"), []byte(resolutionDepBuildJobs), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(srcDir, "aux"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "aux", "BUILD.jobs"), []byte(resolutionDepAuxBuildJobs), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := DevelopConfig{SourceDir: srcDir, Platform: platform, CacheDir: t.TempDir()}
	var progress bytes.Buffer
	art, err := prepareSourceArtifact(ctx, st, cfg, NewProgress(&progress))
	if err != nil {
		t.Fatalf("build with resolution dep failed: %v\nprogress:\n%s", err, progress.String())
	}

	// The output's result file must carry the dep-provided marker, proving the
	// pin stage read the materialized dep output.
	res, err := readTreeFile(ctx, st, art.bokSelf, "result")
	if err != nil {
		t.Fatalf("read result from artifact: %v", err)
	}
	if !strings.Contains(string(res), "dep-built") {
		t.Fatalf("result = %q, want dep-built", string(res))
	}
}
