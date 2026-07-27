package clientcli

import (
	"testing"

	"github.com/urfave/cli/v2"
)

// findCommand returns the App() command named name, or nil.
func findCommand(t *testing.T, name string) *cli.Command {
	t.Helper()
	for _, c := range App().Commands {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// flagNames flattens a command's flag names (incl. aliases) into a set.
func flagNames(cmd *cli.Command) map[string]bool {
	names := map[string]bool{}
	for _, f := range cmd.Flags {
		for _, n := range f.Names() {
			names[n] = true
		}
	}
	return names
}

// TestDevelopCommandRegistered is the port of jobs' registration lock-in: the
// develop command exists and carries the local-build flag surface (the
// signing-key/user/tags-file flags died with the signing plumbing).
func TestDevelopCommandRegistered(t *testing.T) {
	cmd := findCommand(t, "develop")
	if cmd == nil {
		t.Fatal("develop command not registered")
	}
	names := flagNames(cmd)
	for _, want := range []string{"data-dir", "source", "dir", "build-file", "platform", "shell-ref", "param"} {
		if !names[want] {
			t.Errorf("develop missing --%s flag", want)
		}
	}
}

// TestImageCommandRegistered locks in the image command surface: by-K or
// --source forms, -o/--output required, --tag, --no-shell, --platform.
func TestImageCommandRegistered(t *testing.T) {
	cmd := findCommand(t, "image")
	if cmd == nil {
		t.Fatal("image command not registered")
	}
	names := flagNames(cmd)
	for _, want := range []string{"data-dir", "output", "o", "tag", "no-shell", "source", "dir", "build-file", "platform", "shell-ref", "param"} {
		if !names[want] {
			t.Errorf("image missing --%s flag", want)
		}
	}
	// --source must NOT be required: the by-key form images an already-built
	// output without any source tree.
	for _, f := range cmd.Flags {
		if sf, ok := f.(*cli.StringFlag); ok && sf.Name == "source" && sf.Required {
			t.Error("image --source must be optional (by-key form)")
		}
	}
}

// TestSourceFlagOptional locks in the cwd default: no source-building command
// may require --source, because omitting it resolves the build from the
// current directory. A stray Required:true would turn that into a usage
// error.
func TestSourceFlagOptional(t *testing.T) {
	for _, name := range []string{"build", "run", "develop", "remote-build", "image"} {
		cmd := findCommand(t, name)
		if cmd == nil {
			t.Fatalf("%s command not registered", name)
		}
		found := false
		for _, f := range cmd.Flags {
			sf, ok := f.(*cli.StringFlag)
			if !ok || sf.Name != "source" {
				continue
			}
			found = true
			if sf.Required {
				t.Errorf("%s --source must be optional (it defaults to the cwd context)", name)
			}
		}
		if !found {
			t.Errorf("%s has no --source flag", name)
		}
	}
}
