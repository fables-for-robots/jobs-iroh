package tarextract

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestExtractPreservesRelativeSymlinkTarget is the regression test for the
// busybox-tar bug: a relative symlink target with a leading "../" (as in a Node
// distribution's bin/npm -> ../lib/node_modules/npm/bin/npm-cli.js) must be
// restored VERBATIM so the link resolves. busybox tar strips the "../" and
// breaks it; Go's archive/tar (this package) must not.
func TestExtractPreservesRelativeSymlinkTarget(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	write := func(h *tar.Header, body string) {
		h.Size = int64(len(body))
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if body != "" {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	// top/ , top/bin/ , top/bin/npm -> ../lib/node_modules/npm/bin/npm-cli.js ,
	// top/lib/node_modules/npm/bin/npm-cli.js
	write(&tar.Header{Name: "top/", Typeflag: tar.TypeDir, Mode: 0o755}, "")
	write(&tar.Header{Name: "top/bin/", Typeflag: tar.TypeDir, Mode: 0o755}, "")
	write(&tar.Header{Name: "top/bin/npm", Typeflag: tar.TypeSymlink, Linkname: "../lib/node_modules/npm/bin/npm-cli.js"}, "")
	write(&tar.Header{Name: "top/lib/node_modules/npm/bin/npm-cli.js", Typeflag: tar.TypeReg, Mode: 0o755}, "#!/usr/bin/env node\n")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	out := t.TempDir()
	if err := Extract(bytes.NewReader(buf.Bytes()), out, 1); err != nil { // strip=1 drops "top/"
		t.Fatalf("Extract: %v", err)
	}

	link := filepath.Join(out, "bin", "npm")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("readlink bin/npm: %v", err)
	}
	if target != "../lib/node_modules/npm/bin/npm-cli.js" {
		t.Fatalf("symlink target corrupted: got %q, want %q (busybox-tar-style ../ stripping)", target, "../lib/node_modules/npm/bin/npm-cli.js")
	}
	// And it must actually resolve to the real file.
	if _, err := os.Stat(link); err != nil {
		t.Fatalf("bin/npm does not resolve (dangling symlink): %v", err)
	}
}
