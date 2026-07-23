//go:build linux

package sandbox_test

// Regression: a MS_REMOUNT|MS_BIND pass in a user namespace must repeat the
// flags the mount inherited from its (locked) source. Reproduced exactly as
// seen in the field: a bind SOURCE on a nosuid/nodev/noatime mount, remounted
// read-only inside a fresh userns — before the fix the remount-ro EPERM'd
// ("remount-ro …: operation not permitted") and childSetup exited 126.
//
// The arrangement needs two namespace layers, because flag locking only
// happens to mounts COPIED into a userns at creation, never to mounts the
// userns makes itself:
//
//	layer 1 (helper, uid 0 in its own userns): mounts a nosuid|nodev|noatime
//	  tmpfs — unlocked here, this namespace owns it — then calls sandbox.Run
//	  again;
//	layer 2 (the sandbox under test): inherits that tmpfs as a LOCKED mount
//	  and must remount-ro a bind whose source sits on it.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/fables-for-robots/jobs-iroh/sandbox"
)

const lockedRemountHelperName = "locked-remount"

// Helper exit codes: 0 = the locked-source remount-ro succeeded end to end;
// skip codes = this environment cannot stage the scenario; else failure.
const (
	helperSkipTmpfs = 90 // tmpfs mount inside the userns failed
	helperSkipSpawn = 91 // nested sandbox could not even be spawned
)

func init() {
	testHelpers[lockedRemountHelperName] = lockedRemountHelper
	testHelpers["exit-zero"] = func() int { return 0 }
}

// lockedRemountHelper runs as uid 0 inside the layer-1 userns.
func lockedRemountHelper() int {
	fail := func(step string, err error) int {
		fmt.Fprintf(os.Stderr, "helper: %s: %v\n", step, err)
		return 1
	}
	locked, err := os.MkdirTemp("", "jobs-locked-tmpfs-")
	if err != nil {
		return fail("mkdtemp", err)
	}
	if err := unix.Mount("tmpfs", locked, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOATIME, ""); err != nil {
		fmt.Fprintln(os.Stderr, "helper: mount restricted tmpfs:", err)
		return helperSkipTmpfs
	}
	src := filepath.Join(locked, "src")
	if err := os.Mkdir(src, 0o755); err != nil {
		return fail("mkdir src", err)
	}
	if err := os.WriteFile(filepath.Join(src, "f"), []byte("x"), 0o644); err != nil {
		return fail("write src file", err)
	}
	exe, err := os.Executable()
	if err != nil {
		return fail("resolve executable", err)
	}
	newRoot, err := os.MkdirTemp("", "jobs-locked-root-")
	if err != nil {
		return fail("mkdtemp root", err)
	}

	// Layer 2: ro-bind the locked source into a tmpfs root and exec this test
	// binary in exit-zero helper mode — exit 0 proves childSetup (the
	// remount-ro of /src) got all the way through to the exec. Host userlands
	// are bound ro so a dynamically linked test binary still loads.
	mounts := []sandbox.Mount{
		{Source: "", Target: "/", FSType: "tmpfs"},
		{Source: src, Target: "/src", ReadOnly: true},
		{Source: filepath.Dir(exe), Target: "/helperbin", ReadOnly: true},
	}
	for _, d := range []string{"/nix", "/usr", "/lib", "/lib64", "/bin"} {
		if _, err := os.Stat(d); err == nil {
			mounts = append(mounts, sandbox.Mount{Source: d, Target: d, ReadOnly: true})
		}
	}
	code, err := sandbox.Run(context.Background(), sandbox.Config{
		Command:    []string{"/helperbin/" + filepath.Base(exe)},
		Env:        []string{"JOBS_SANDBOX_TEST_HELPER=exit-zero"},
		NewRoot:    newRoot,
		Mounts:     mounts,
		Namespaces: sandbox.Namespaces{User: true, Mount: true},
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper: spawn nested sandbox:", err)
		return helperSkipSpawn
	}
	if code != 0 {
		fmt.Fprintln(os.Stderr, "helper: nested sandbox exited", code)
		return 1
	}
	return 0
}

// TestLockedSourceRemountRO drives the two-layer arrangement above.
func TestLockedSourceRemountRO(t *testing.T) {
	if !sandbox.UserNSAvailable() {
		t.Skip("user namespaces unavailable")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	code, err := sandbox.Run(context.Background(), sandbox.Config{
		Command:    []string{exe},
		Env:        append(os.Environ(), "JOBS_SANDBOX_TEST_HELPER="+lockedRemountHelperName),
		Namespaces: sandbox.Namespaces{User: true, Mount: true},
		Stdout:     &out,
		Stderr:     &out,
	})
	if err != nil {
		t.Fatalf("layer-1 sandbox: %v", err)
	}
	switch code {
	case 0:
	case helperSkipTmpfs, helperSkipSpawn:
		t.Skipf("environment cannot stage the locked-mount scenario (helper exit %d):\n%s", code, out.String())
	default:
		t.Fatalf("remount-ro of a bind from a locked nosuid/nodev/noatime mount failed (helper exit %d):\n%s", code, out.String())
	}
}
