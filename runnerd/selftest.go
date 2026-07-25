package runnerd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/fables-for-robots/jobs-iroh/amber"
	"github.com/fables-for-robots/jobs-iroh/bootstrap"
	"github.com/fables-for-robots/jobs-iroh/cover"
	"github.com/fables-for-robots/jobs-iroh/importdef"
	"github.com/fables-for-robots/jobs-iroh/runner"
)

// selfTestTimeout bounds the whole boot self-test, first-boot shell ingest
// included — a wedged sandbox must fail the boot, not hang it.
const selfTestTimeout = 2 * time.Minute

// selfTestBuildJobs is the self-test recipe: no plugins, no inputs, a script
// that only writes $out — the smallest build that still execs the shell
// inside the real namespace build sandbox (mount plan, remount-ro,
// pivot_root, the works).
const selfTestBuildJobs = `def build():
    return struct(
        inputs = {},
        env = {},
        script = 'echo self-test > "$out/ok"\n',
        runtime_deps = [],
    )
`

// bootSelfTest proves this host can actually execute a sandboxed build BEFORE
// the runner announces capacity: a host whose sandbox is broken (mount
// restrictions, missing userns, exotic TMPDIR mounts) otherwise joins the
// fleet and hard-fails every job the scheduler hands it. It seeds the
// embedded shell under a self-test-only ref — shell:<platform> stays
// pull-from-server, a local publish would shadow the server's shell — and
// drives a one-script build through the real local pipeline (plugin-resolve →
// pin → build in the namespace sandbox).
//
// The build params carry a per-boot nonce so F is fresh every boot — without
// it the join (doneness = ref existence) would satisfy the build from a
// previous boot's refs without exec'ing anything. The F-keyed refs are
// deleted afterwards: a nonce'd F never joins anything again.
func bootSelfTest(ctx context.Context, st *amber.Store, platform, cacheDir string, log *slog.Logger) error {
	if runtime.GOOS != "linux" {
		log.Warn("boot self-test skipped: sandboxed builds are linux-only")
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, selfTestTimeout)
	defer cancel()
	start := time.Now()

	shellRef := "selftest-shell:" + platform
	if _, err := bootstrap.SeedShellAs(ctx, st, platform, shellRef); err != nil {
		if errors.Is(err, bootstrap.ErrNoSeed) {
			log.Warn("boot self-test skipped: no embedded shell seed for this platform", "platform", platform)
			return nil
		}
		return fmt.Errorf("boot self-test: seed shell: %w", err)
	}

	srcDir, err := os.MkdirTemp("", "jobs-selftest-")
	if err != nil {
		return fmt.Errorf("boot self-test: create source dir: %w", err)
	}
	defer os.RemoveAll(srcDir)
	if err := os.WriteFile(filepath.Join(srcDir, "BUILD.jobs"), []byte(selfTestBuildJobs), 0o644); err != nil {
		return fmt.Errorf("boot self-test: write recipe: %w", err)
	}
	params, err := importdef.CanonicalParams(map[string]any{
		"selftest-nonce": fmt.Sprintf("%d", time.Now().UnixNano()),
	})
	if err != nil {
		return fmt.Errorf("boot self-test: encode params: %w", err)
	}

	var steps bytes.Buffer
	f, err := runner.BuildFromSource(ctx, st, runner.DevelopConfig{
		SourceDir: srcDir,
		Platform:  platform,
		Params:    []byte(params),
		ShellRef:  shellRef,
		CacheDir:  cacheDir,
	}, runner.NewProgress(&steps))
	if err != nil {
		log.Error("boot self-test build failed", "steps", steps.String(), "error", err)
		return fmt.Errorf("boot self-test build failed — this host cannot execute sandboxed builds, refusing to serve jobs (fix the environment or start with --skip-self-test): %w", err)
	}
	if _, ok, err := st.GetKey(ctx, "build-output:"+f.String()); err != nil {
		return fmt.Errorf("boot self-test: verify output: %w", err)
	} else if !ok {
		return errors.New("boot self-test: build completed but its build-output ref is missing")
	}
	cleanup := []string{
		"build-plugin-resolved:" + f.String(),
		"build-pinned:" + f.String(),
		"build-output:" + f.String(),
		"build-output-deps:" + f.String(),
	}
	// The KP-keyed refs of the sibling-sources arc (build-pinned/-output
	// ride under KP now; the F names above are aliases).
	if kp, ok, err := st.GetKey(ctx, cover.PinCoverRef(f)); err == nil && ok {
		cleanup = append(cleanup,
			"build-pinned:"+kp.String(),
			"build-output:"+kp.String(),
			"build-output-deps:"+kp.String(),
			cover.PinCoverRef(f),
		)
	}
	for _, name := range cleanup {
		if err := st.DeleteRef(ctx, name); err != nil {
			log.Debug("boot self-test: ref cleanup failed", "ref", name, "error", err)
		}
	}
	log.Info("boot self-test build passed", "wallMs", time.Since(start).Milliseconds())
	return nil
}
