//go:build linux

package hostshell_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestFetch_VendorsRunnableBash(t *testing.T) {
	out := t.TempDir()
	cmd := exec.Command("./fetch")
	cmd.Env = append(os.Environ(), "JOBS_OUTPUT_DIR="+out)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("fetch failed: %v", err)
	}
	bash := filepath.Join(out, "bin", "bash")
	if fi, err := os.Stat(bash); err != nil || fi.Mode()&0o111 == 0 {
		t.Fatalf("bin/bash missing or not executable: %v", err)
	}
	run := exec.Command(bash, "-c", "echo hi")
	run.Env = append(os.Environ(), "LD_LIBRARY_PATH="+filepath.Join(out, "lib"))
	b, err := run.Output()
	if err != nil {
		t.Fatalf("vendored bash failed to run: %v", err)
	}
	if string(b) != "hi\n" {
		t.Fatalf("vendored bash output = %q", b)
	}
}
