package clientcli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// runImage drives the real command surface with args, returning the error and
// stderr. The output path is inside a temp dir so a bug that reaches the
// tarball writer cannot touch the working tree.
func runImage(t *testing.T, args ...string) (error, string) {
	t.Helper()
	app := App()
	var out, errOut bytes.Buffer
	app.Writer = &out
	app.ErrWriter = &errOut
	full := append([]string{"jobs-client", "image",
		"-o", filepath.Join(t.TempDir(), "img.tar"),
		"--data-dir", t.TempDir(),
	}, args...)
	return app.Run(full), errOut.String()
}

// The positional build key is the mode discriminator, so naming a key AND a
// source is a contradiction — it must fail loudly rather than silently
// ignoring one of them (the pre-cwd-default behavior ignored the key).
func TestImageSourceAndKeyMutuallyExclusive(t *testing.T) {
	err, _ := runImage(t, "--source", t.TempDir(), "0011223344556677")
	if err == nil {
		t.Fatal("want an error when both --source and a build key are given")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error %q must say the two are mutually exclusive", err)
	}
}

// With neither a key nor --source, image resolves the source from the cwd —
// so outside any repo, with no recipe next to it, it must report the missing
// recipe (the old "provide --source or a build key" usage error is gone).
func TestImageNoArgsResolvesFromCwd(t *testing.T) {
	t.Chdir(t.TempDir())
	err, _ := runImage(t)
	if err == nil {
		t.Fatal("want an error: the temp cwd holds no BUILD.jobs")
	}
	if !strings.Contains(err.Error(), "BUILD.jobs") {
		t.Errorf("error %q must name the recipe it looked for", err)
	}
}
