//go:build linux

package runner

// Driver-half internal tests of develop_linux.go plus the devSetup harness the
// local-build tests share. jobs' prepareDevelop/printScript/developRun/
// RunDevelop tests belong to the develop-PTY milestone and are not ported yet.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/fables-for-robots/jobs-iroh/amber"
	"github.com/fables-for-robots/jobs-iroh/importdef"
	"github.com/fables-for-robots/jobs-iroh/sandbox"
)

// TestImportFetchLabel verifies the fetch progress line shows the fetcher name +
// its arguments (params sorted by key), not the opaque content key.
func TestImportFetchLabel(t *testing.T) {
	params, err := importdef.CanonicalParams(map[string]any{"owner": "o", "repo": "r", "ref": "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := importFetchLabel(importdef.Definition{Fetcher: "github", Params: params}), "fetch github owner=o ref=v1 repo=r"; got != want {
		t.Fatalf("importFetchLabel = %q, want %q", got, want)
	}
	empty, err := importdef.CanonicalParams(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := importFetchLabel(importdef.Definition{Fetcher: "depfetch", Params: empty}), "fetch depfetch"; got != want {
		t.Fatalf("empty-params label = %q, want %q", got, want)
	}
}

const devBuildJobs = `
def plugins():
    return {"mod": imp(fetcher = "modplugin", params = {})}

def build():
    _ = plugins["mod"](manifest = source.read("greeting.txt"))
    dep = imp(fetcher = "depfetch", params = {})
    return struct(
        inputs = {"dep": dep},
        env = {},
        script = 'cat "$SRC/greeting.txt" > "$out/result"',
        runtime_deps = [],
    )
`

const devPluginFetcher = "#!/bin/sh\n" +
	"set -e\n" +
	"cat > \"$JOBS_OUTPUT_DIR/plugin\" <<'EOF'\n" +
	"#!/jobs/shell/bin/bash\n" +
	"cat > /dev/null\n" +
	"printf '\\200'\n" +
	"EOF\n" +
	"chmod +x \"$JOBS_OUTPUT_DIR/plugin\"\n"

const devDepFetcher = "#!/bin/sh\nset -e\nprintf depdata > \"$JOBS_OUTPUT_DIR/data\"\n"

// devSetup brings up a store + the shell artifact + the two fetchers, and
// returns a context, the store, the platform, and the source dir.
func devSetup(t *testing.T) (context.Context, *amber.Store, string, string) {
	t.Helper()
	if !sandbox.UserNSAvailable() {
		t.Skip("user namespaces unavailable")
	}
	ctx := context.Background()
	st := openTestStore(t)
	platform := Platform()

	// shell artifact
	shellOut := t.TempDir()
	fetch, err := filepath.Abs("../fetchers/hostshell/fetch")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(fetch)
	cmd.Env = append(os.Environ(), "JOBS_OUTPUT_DIR="+shellOut)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hostshell fetcher: %v\n%s", err, out)
	}
	sk, err := st.IngestDir(ctx, shellOut)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutRef(ctx, "shell:"+platform, sk); err != nil {
		t.Fatal(err)
	}

	// fetchers
	devRegisterFetcher(t, ctx, st, "modplugin", platform, devPluginFetcher)
	devRegisterFetcher(t, ctx, st, "depfetch", platform, devDepFetcher)

	// source dir
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "greeting.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "BUILD.jobs"), []byte(devBuildJobs), 0o644); err != nil {
		t.Fatal(err)
	}
	return ctx, st, platform, srcDir
}

func devRegisterFetcher(t *testing.T, ctx context.Context, st *amber.Store, name, platform, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fetch"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	k, err := st.IngestDir(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutRef(ctx, "fetcher:"+name+":"+platform, k); err != nil {
		t.Fatal(err)
	}
}
