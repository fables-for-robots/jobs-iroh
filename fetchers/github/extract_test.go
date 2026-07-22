package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

// buildTar gzips a tar from entries. Each entry: name, typeflag, body/linkname.
type tentry struct {
	name     string
	typeflag byte
	body     string // for reg files
	link     string // for symlinks
}

func buildTar(t *testing.T, entries []tentry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typeflag, Mode: 0o644}
		switch e.typeflag {
		case tar.TypeDir:
			hdr.Mode = 0o755
		case tar.TypeReg:
			hdr.Size = int64(len(e.body))
		case tar.TypeSymlink:
			hdr.Linkname = e.link
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestStripTop(t *testing.T) {
	cases := []struct {
		in  string
		rel string
		ok  bool
	}{
		{"owner-repo-sha/", "", false},
		{"owner-repo-sha/README.md", "README.md", true},
		{"owner-repo-sha/src/main.go", "src/main.go", true},
		{"./owner-repo-sha/x", "x", true},
	}
	for _, c := range cases {
		rel, ok := stripTop(c.in)
		if rel != c.rel || ok != c.ok {
			t.Fatalf("stripTop(%q)=(%q,%v) want (%q,%v)", c.in, rel, ok, c.rel, c.ok)
		}
	}
}

func TestExtractTarball_StripsWrapper(t *testing.T) {
	tb := buildTar(t, []tentry{
		{name: "owner-repo-sha/", typeflag: tar.TypeDir},
		{name: "owner-repo-sha/README.md", typeflag: tar.TypeReg, body: "hello\n"},
		{name: "owner-repo-sha/src/", typeflag: tar.TypeDir},
		{name: "owner-repo-sha/src/main.go", typeflag: tar.TypeReg, body: "package main\n"},
	})
	dest := t.TempDir()
	if err := extractTarball(bytes.NewReader(tb), dest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "README.md"))
	if err != nil || string(got) != "hello\n" {
		t.Fatalf("README.md: %q err=%v", got, err)
	}
	got, err = os.ReadFile(filepath.Join(dest, "src", "main.go"))
	if err != nil || string(got) != "package main\n" {
		t.Fatalf("src/main.go: %q err=%v", got, err)
	}
}

func TestExtractTarball_RejectsTraversal(t *testing.T) {
	tb := buildTar(t, []tentry{
		{name: "owner-repo-sha/", typeflag: tar.TypeDir},
		{name: "owner-repo-sha/../escape.txt", typeflag: tar.TypeReg, body: "x"},
	})
	dest := t.TempDir()
	err := extractTarball(bytes.NewReader(tb), dest)
	if err == nil || isRetryable(err) {
		t.Fatalf("expected hard error, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(dest), "escape.txt")); statErr == nil {
		t.Fatal("traversal wrote outside dest")
	}
}

func TestExtractTarball_RejectsEscapingSymlink(t *testing.T) {
	tb := buildTar(t, []tentry{
		{name: "owner-repo-sha/", typeflag: tar.TypeDir},
		{name: "owner-repo-sha/link", typeflag: tar.TypeSymlink, link: "../../etc/passwd"},
	})
	dest := t.TempDir()
	if err := extractTarball(bytes.NewReader(tb), dest); err == nil || isRetryable(err) {
		t.Fatalf("expected hard error, got %v", err)
	}
}

func TestExtractTarball_AllowsInternalSymlink(t *testing.T) {
	tb := buildTar(t, []tentry{
		{name: "owner-repo-sha/", typeflag: tar.TypeDir},
		{name: "owner-repo-sha/real.txt", typeflag: tar.TypeReg, body: "hi\n"},
		{name: "owner-repo-sha/link", typeflag: tar.TypeSymlink, link: "real.txt"},
	})
	dest := t.TempDir()
	if err := extractTarball(bytes.NewReader(tb), dest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "link"))
	if err != nil || string(got) != "hi\n" {
		t.Fatalf("link: %q err=%v", got, err)
	}
}

func TestExtractTarball_EmptyIsError(t *testing.T) {
	tb := buildTar(t, nil)
	if err := extractTarball(bytes.NewReader(tb), t.TempDir()); err == nil || isRetryable(err) {
		t.Fatalf("expected hard error for empty tarball, got %v", err)
	}
}

func TestExtractTarball_NotGzipIsRetryable(t *testing.T) {
	if err := extractTarball(bytes.NewReader([]byte("not gzip")), t.TempDir()); err == nil || !isRetryable(err) {
		t.Fatalf("expected retryable error, got %v", err)
	}
}

func TestExtractTarball_WrapperOnlyIsError(t *testing.T) {
	// Tarball contains only the bare wrapper directory entry — no content entries.
	tb := buildTar(t, []tentry{
		{name: "owner-repo-sha/", typeflag: tar.TypeDir},
	})
	err := extractTarball(bytes.NewReader(tb), t.TempDir())
	if err == nil || isRetryable(err) {
		t.Fatalf("expected hard error for wrapper-only tarball, got %v", err)
	}
}
