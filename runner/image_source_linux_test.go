//go:build linux

package runner_test

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/fables-for-robots/jobs-iroh/runner"
	"github.com/fables-for-robots/jobs-iroh/sandbox"
	tarballimg "github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/validate"
)

// runnableBuildJobs is jobs' shared run/image e2e recipe fixture: the build
// emits a shell-shebang app.sh plus the JOBS.entrypoint that runs it.
const runnableBuildJobs = `
def build():
    return struct(
        inputs = {},
        env = {},
        script = '''
cat > "$out/app.sh" <<'SH'
#!/jobs/shell/bin/bash
echo "RAN:$*"
SH
chmod +x "$out/app.sh"
cat > "$out/JOBS.entrypoint" <<'EP'
{"command":"app.sh","args":["hi"],"env":{}}
EP
''',
        runtime_deps = [],
    )
`

// TestBuildImageFromSource is the port of jobs' `jobs image --source` e2e
// (internal/jobscli/image_e2e_test.go, ginkgo → plain testing): build a local
// source hermetically, write a docker-loadable tarball, and assert the
// manifest naming, config, and flattened layout — validated with
// go-containerregistry's tarball + validate packages, no docker daemon.
func TestBuildImageFromSource(t *testing.T) {
	if !sandbox.UserNSAvailable() {
		t.Skip("user namespaces unavailable")
	}
	ctx := context.Background()
	st := buildEvalStore(t)
	platform := runner.Platform()

	shellKey := buildShellArtifact(t, ctx, st)
	if err := st.PutRef(ctx, "shell:"+platform, shellKey); err != nil {
		t.Fatal(err)
	}

	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "BUILD.jobs"), []byte(runnableBuildJobs), 0o644); err != nil {
		t.Fatal(err)
	}

	buildOnce := func() []byte {
		var buf bytes.Buffer
		err := runner.BuildImageFromSource(ctx, st, runner.DevelopConfig{
			SourceDir: srcDir,
			Platform:  platform,
			CacheDir:  t.TempDir(),
		}, platform, runner.ImageOptions{Tag: "myapp:latest", IncludeShell: true, Output: &buf})
		if err != nil {
			t.Fatalf("BuildImageFromSource: %v", err)
		}
		return buf.Bytes()
	}

	tarBytes := buildOnce()
	tarPath := filepath.Join(t.TempDir(), "img.tar")
	if err := os.WriteFile(tarPath, tarBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	// The tarball is a valid docker-load image with the run entrypoint baked in.
	img, err := tarballimg.ImageFromPath(tarPath, nil)
	if err != nil {
		t.Fatalf("re-open image tar: %v", err)
	}
	if err := validate.Image(img); err != nil {
		t.Fatalf("validate.Image: %v", err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	// JOBS.entrypoint command "app.sh" resolves against the root; args ["hi"].
	wantEP := []string{"/app.sh", "hi"}
	if len(cf.Config.Entrypoint) != 2 || cf.Config.Entrypoint[0] != wantEP[0] || cf.Config.Entrypoint[1] != wantEP[1] {
		t.Errorf("Entrypoint = %v, want %v", cf.Config.Entrypoint, wantEP)
	}
	if cf.Config.WorkingDir != "/" {
		t.Errorf("WorkingDir = %q, want /", cf.Config.WorkingDir)
	}
	if cf.OS != "linux" {
		t.Errorf("OS = %q, want linux", cf.OS)
	}

	// manifest RepoTags name the image for docker load.
	mf, err := tarballimg.LoadManifest(func() (io.ReadCloser, error) { return os.Open(tarPath) })
	if err != nil {
		t.Fatalf("load tar manifest: %v", err)
	}
	if len(mf) != 1 || len(mf[0].RepoTags) != 1 || mf[0].RepoTags[0] != "myapp:latest" {
		t.Errorf("RepoTags = %+v, want [myapp:latest]", mf)
	}

	// Flattened layout: app.sh at the root; shell included → /bin/sh symlink +
	// /jobs/shell compat symlink.
	paths := map[string]*tar.Header{}
	layers, err := img.Layers()
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range layers {
		rc, err := l.Uncompressed()
		if err != nil {
			t.Fatal(err)
		}
		tr := tar.NewReader(rc)
		for {
			h, err := tr.Next()
			if err != nil {
				break
			}
			name := h.Name
			if len(name) > 2 && name[:2] == "./" {
				name = name[2:]
			}
			paths[name] = h
		}
		rc.Close()
	}
	if _, ok := paths["app.sh"]; !ok {
		t.Error("app.sh missing at image root")
	}
	if h, ok := paths["bin/sh"]; !ok || h.Typeflag != tar.TypeSymlink {
		t.Error("/bin/sh symlink missing (shell included)")
	}
	if _, ok := paths["jobs/shell"]; !ok {
		t.Error("/jobs/shell compat symlink missing")
	}

	// Reproducibility at the byte level: the same (now cached) build imaged
	// again yields an identical tarball.
	if again := buildOnce(); !bytes.Equal(tarBytes, again) {
		t.Errorf("source-mode image tarball not byte-identical across runs (%d vs %d bytes)", len(tarBytes), len(again))
	}
}
