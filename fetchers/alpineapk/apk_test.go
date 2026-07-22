package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

type tarEntry struct {
	name, body, link string
	typ              byte
	mode             int64
}

// gzTar builds one gzip-compressed tar archive — i.e. a single apk segment.
func gzTar(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: e.mode, Linkname: e.link}
		if e.typ == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestExtractAPKExtractsDataSkipsControl builds a realistic two-segment apk
// (a control segment of root dotfiles + a data segment) and asserts the data
// files/dirs/symlinks land in outDir while the control members are skipped.
func TestExtractAPKExtractsDataSkipsControl(t *testing.T) {
	control := gzTar(t, []tarEntry{
		{name: ".PKGINFO", body: "pkgname = foo\n", typ: tar.TypeReg, mode: 0o644},
		{name: ".SIGN.RSA.foo.pub", body: "sig", typ: tar.TypeReg, mode: 0o644},
		{name: ".post-install", body: "#!/bin/sh\n", typ: tar.TypeReg, mode: 0o755},
	})
	data := gzTar(t, []tarEntry{
		{name: "usr/", typ: tar.TypeDir, mode: 0o755},
		{name: "usr/bin/", typ: tar.TypeDir, mode: 0o755},
		{name: "usr/bin/foo", body: "hi", typ: tar.TypeReg, mode: 0o755},
		{name: "usr/lib/", typ: tar.TypeDir, mode: 0o755},
		{name: "usr/lib/libfoo.so.1", body: "ELF", typ: tar.TypeReg, mode: 0o644},
		{name: "usr/lib/libfoo.so", link: "libfoo.so.1", typ: tar.TypeSymlink, mode: 0o777},
	})
	apk := append(append([]byte{}, control...), data...)

	out := t.TempDir()
	if err := extractAPK(bytes.NewReader(apk), out); err != nil {
		t.Fatalf("extractAPK: %v", err)
	}

	// data regular file extracted, content + exec bit preserved
	fooPath := filepath.Join(out, "usr/bin/foo")
	fi, err := os.Stat(fooPath)
	if err != nil {
		t.Fatalf("usr/bin/foo: %v", err)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("usr/bin/foo not executable: %v", fi.Mode())
	}
	if b, _ := os.ReadFile(fooPath); string(b) != "hi" {
		t.Errorf("usr/bin/foo content = %q, want %q", b, "hi")
	}

	// symlink recreated verbatim
	if ln, err := os.Readlink(filepath.Join(out, "usr/lib/libfoo.so")); err != nil || ln != "libfoo.so.1" {
		t.Errorf("usr/lib/libfoo.so = %q, %v; want libfoo.so.1", ln, err)
	}

	// control members from BOTH the control segment must be skipped
	for _, ctl := range []string{".PKGINFO", ".SIGN.RSA.foo.pub", ".post-install"} {
		if _, err := os.Stat(filepath.Join(out, ctl)); !os.IsNotExist(err) {
			t.Errorf("control member %q should be skipped (stat err=%v)", ctl, err)
		}
	}
}

// gzTarPadded builds an apk segment whose gzip member has extra trailing zero
// bytes after the tar EOF marker — mimicking how real Alpine apks pad the tar to
// the 10240-byte blocking factor. tar.Reader stops at the EOF marker and leaves
// that padding undrained, which must not break advancing to the next segment.
func gzTarPadded(t *testing.T, entries []tarEntry, pad int) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: e.mode, Linkname: e.link}
		if e.typ == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := gw.Write(make([]byte, pad)); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestExtractAPKHandlesSegmentPadding reproduces the real-apk layout: the first
// (control) segment carries trailing padding tar.Reader won't consume. extractAPK
// must still advance to and extract the following data segment.
func TestExtractAPKHandlesSegmentPadding(t *testing.T) {
	control := gzTarPadded(t, []tarEntry{
		{name: ".PKGINFO", body: "pkgname = foo\n", typ: tar.TypeReg, mode: 0o644},
	}, 8192)
	data := gzTarPadded(t, []tarEntry{
		{name: "usr/lib/libfoo.so.1", body: "ELF", typ: tar.TypeReg, mode: 0o644},
	}, 6144)
	apk := append(append([]byte{}, control...), data...)

	out := t.TempDir()
	if err := extractAPK(bytes.NewReader(apk), out); err != nil {
		t.Fatalf("extractAPK with padded segments: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(out, "usr/lib/libfoo.so.1")); err != nil || string(b) != "ELF" {
		t.Errorf("usr/lib/libfoo.so.1 = %q, %v; want ELF", b, err)
	}
}

// TestExtractAPKRejectsTraversal ensures a data entry escaping outDir is an error
// (and is NOT mistaken for a control dotfile just because it starts with "..").
func TestExtractAPKRejectsTraversal(t *testing.T) {
	bad := gzTar(t, []tarEntry{{name: "../evil", body: "x", typ: tar.TypeReg, mode: 0o644}})
	if err := extractAPK(bytes.NewReader(bad), t.TempDir()); err == nil {
		t.Fatal("expected error for path traversal, got nil")
	}
}

// TestExtractAPKRejectsEscapingSymlink: a symlink whose target escapes outDir
// (Zip-Slip via symlink) must be rejected, not created.
func TestExtractAPKRejectsEscapingSymlink(t *testing.T) {
	bad := gzTar(t, []tarEntry{
		{name: "usr/lib/evil", link: "../../../../../../etc/passwd", typ: tar.TypeSymlink, mode: 0o777},
	})
	if err := extractAPK(bytes.NewReader(bad), t.TempDir()); err == nil {
		t.Fatal("expected error for symlink escaping outDir, got nil")
	}
}

// TestExtractAPKAllowsSameDirSymlink: the legit Alpine pattern (libz.so.1 ->
// libz.so.1.3.2) must keep working after the hardening.
func TestExtractAPKAllowsSameDirSymlink(t *testing.T) {
	good := gzTar(t, []tarEntry{
		{name: "usr/lib/libz.so.1.3.2", body: "ELF", typ: tar.TypeReg, mode: 0o644},
		{name: "usr/lib/libz.so.1", link: "libz.so.1.3.2", typ: tar.TypeSymlink, mode: 0o777},
	})
	out := t.TempDir()
	if err := extractAPK(bytes.NewReader(good), out); err != nil {
		t.Fatalf("same-dir symlink should be allowed: %v", err)
	}
	if ln, err := os.Readlink(filepath.Join(out, "usr/lib/libz.so.1")); err != nil || ln != "libz.so.1.3.2" {
		t.Errorf("symlink = %q, %v; want libz.so.1.3.2", ln, err)
	}
}

// TestExtractAPKRejectsEscapingHardlink: a hardlink source pointing outside
// outDir must be rejected.
func TestExtractAPKRejectsEscapingHardlink(t *testing.T) {
	bad := gzTar(t, []tarEntry{
		{name: "usr/lib/evil", link: "../../../../../../etc/passwd", typ: tar.TypeLink, mode: 0o644},
	})
	if err := extractAPK(bytes.NewReader(bad), t.TempDir()); err == nil {
		t.Fatal("expected error for hardlink escaping outDir, got nil")
	}
}

// TestExtractAPKRewritesAbsoluteSymlink: an absolute symlink target in an apk
// means "relative to the install root" (gcc ships usr/lib/bfd-plugins/
// liblto_plugin.so -> //usr/libexec/gcc/…/liblto_plugin.so), so it must be
// reinterpreted under outDir and written as a relative link — not rejected.
func TestExtractAPKRewritesAbsoluteSymlink(t *testing.T) {
	control := gzTar(t, []tarEntry{
		{name: ".PKGINFO", body: "pkgname = gcc\n", typ: tar.TypeReg, mode: 0o644},
	})
	data := gzTar(t, []tarEntry{
		{name: "usr/", typ: tar.TypeDir, mode: 0o755},
		{name: "usr/libexec/gcc/aarch64-alpine-linux-musl/14.2.0/", typ: tar.TypeDir, mode: 0o755},
		{name: "usr/libexec/gcc/aarch64-alpine-linux-musl/14.2.0/liblto_plugin.so", body: "ELF", typ: tar.TypeReg, mode: 0o755},
		{name: "usr/lib/bfd-plugins/", typ: tar.TypeDir, mode: 0o755},
		{name: "usr/lib/bfd-plugins/liblto_plugin.so", link: "//usr/libexec/gcc/aarch64-alpine-linux-musl/14.2.0/liblto_plugin.so", typ: tar.TypeSymlink, mode: 0o777},
	})
	apk := append(append([]byte{}, control...), data...)

	out := t.TempDir()
	if err := extractAPK(bytes.NewReader(apk), out); err != nil {
		t.Fatalf("extractAPK: %v", err)
	}

	link := filepath.Join(out, "usr/lib/bfd-plugins/liblto_plugin.so")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if filepath.IsAbs(target) {
		t.Errorf("link target %q still absolute; must be rewritten relative so the tree relocates", target)
	}
	// The rewritten link must resolve to the extracted file inside outDir.
	resolved := filepath.Clean(filepath.Join(filepath.Dir(link), target))
	want := filepath.Join(out, "usr/libexec/gcc/aarch64-alpine-linux-musl/14.2.0/liblto_plugin.so")
	if resolved != want {
		t.Errorf("link resolves to %q, want %q", resolved, want)
	}
	if _, err := os.Stat(resolved); err != nil {
		t.Errorf("resolved target: %v", err)
	}

	// A genuinely escaping relative target must still be rejected.
	evil := gzTar(t, []tarEntry{
		{name: "lib/", typ: tar.TypeDir, mode: 0o755},
		{name: "lib/pwn", link: "../../../etc/passwd", typ: tar.TypeSymlink, mode: 0o777},
	})
	apk2 := append(append([]byte{}, control...), evil...)
	if err := extractAPK(bytes.NewReader(apk2), t.TempDir()); err == nil {
		t.Error("escaping relative symlink accepted; want error")
	}
}

// TestRunStreamsLargePayload proves the fetcher does not buffer the .apk in
// memory: fetching a ~32MiB incompressible package must allocate far less than
// the payload. TotalAlloc is monotonic, so the bound is GC-independent. Also
// asserts no temp-file residue pollutes the output tree.
func TestRunStreamsLargePayload(t *testing.T) {
	big := make([]byte, 32<<20)
	rnd := rand.New(rand.NewSource(1))
	rnd.Read(big)
	body := gzTar(t, []tarEntry{{name: "usr/lib/blob", body: string(big), typ: tar.TypeReg, mode: 0o644}})
	sum := sha256.Sum256(body)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()
	oldMirror := mirror
	mirror = srv.URL
	defer func() { mirror = oldMirror }()

	outDir := t.TempDir()
	params, _ := json.Marshal(map[string]string{
		"branch": "v3.22", "repo": "main", "arch": "x86_64",
		"name": "blob", "version": "1.0-r0", "sha256": hex.EncodeToString(sum[:]),
	})
	getenv := func(k string) string {
		switch k {
		case "JOBS_OUTPUT_DIR":
			return outDir
		case "JOBS_FETCH_PARAMS":
			return string(params)
		}
		return ""
	}

	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	code := run(getenv, os.Stderr)
	runtime.ReadMemStats(&m1)
	if code != exitOK {
		t.Fatalf("run = %d, want %d", code, exitOK)
	}
	if alloc := m1.TotalAlloc - m0.TotalAlloc; alloc > 16<<20 {
		t.Fatalf("run allocated %d MiB for a %d MiB payload — download is buffered in memory", alloc>>20, len(body)>>20)
	}

	got, err := os.ReadFile(filepath.Join(outDir, "usr", "lib", "blob"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, big) {
		t.Fatal("extracted content differs")
	}
	ents, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != "usr" {
		t.Fatalf("output dir polluted: %v", ents)
	}
}
