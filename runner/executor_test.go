package runner

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFetch(t *testing.T, dir, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "fetch"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestSubprocess_ExitCodes(t *testing.T) {
	cases := []struct {
		name, script string
		want         int
	}{
		{"success", "#!/bin/sh\necho ok\nexit 0\n", 0},
		{"hard", "#!/bin/sh\necho boom >&2\nexit 1\n", 1},
		{"tempfail", "#!/bin/sh\nexit 75\n", 75},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fdir := t.TempDir()
			writeFetch(t, fdir, c.script)
			odir := t.TempDir()
			res, err := Subprocess{}.Run(context.Background(), ExecSpec{
				FetcherDir: fdir,
				OutputDir:  odir,
				Env:        map[string]string{"JOBS_OUTPUT_DIR": odir},
			})
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if res.ExitCode != c.want {
				t.Fatalf("exit=%d want %d", res.ExitCode, c.want)
			}
		})
	}
}

func TestSubprocess_WritesOutputAndCapturesStderr(t *testing.T) {
	fdir := t.TempDir()
	writeFetch(t, fdir, "#!/bin/sh\necho diag >&2\necho hello > \"$JOBS_OUTPUT_DIR/greeting.txt\"\n")
	odir := t.TempDir()
	res, err := Subprocess{}.Run(context.Background(), ExecSpec{
		FetcherDir: fdir,
		OutputDir:  odir,
		Env:        map[string]string{"JOBS_OUTPUT_DIR": odir},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d", res.ExitCode)
	}
	if !strings.Contains(res.StderrTail, "diag") {
		t.Fatalf("stderr tail=%q", res.StderrTail)
	}
	got, err := os.ReadFile(filepath.Join(odir, "greeting.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != "hello" {
		t.Fatalf("output=%q", got)
	}
}

func TestSubprocess_Sinks(t *testing.T) {
	fdir := t.TempDir()
	writeFetch(t, fdir, "#!/bin/sh\necho out-line\necho err-line >&2\n")
	odir := t.TempDir()
	var stdout, stderr bytes.Buffer
	res, err := Subprocess{}.Run(context.Background(), ExecSpec{
		FetcherDir: fdir,
		OutputDir:  odir,
		Env:        map[string]string{"JOBS_OUTPUT_DIR": odir},
		StdoutSink: &stdout,
		StderrSink: &stderr,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d", res.ExitCode)
	}
	if stdout.String() != "out-line\n" {
		t.Fatalf("stdout sink got %q", stdout.String())
	}
	// The exec banner leads the stderr stream, then the process's own stderr.
	wantErr := "jobs: exec " + filepath.Join(fdir, "fetch") + "\nerr-line\n"
	if stderr.String() != wantErr {
		t.Fatalf("stderr sink got %q, want %q", stderr.String(), wantErr)
	}
	// The 4KB tail for the failure path must still work alongside.
	if !strings.Contains(res.StderrTail, "err-line") {
		t.Fatalf("StderrTail lost: %q", res.StderrTail)
	}
}

// TestSubprocess_ExecBanner: the banner names the resolved entrypoint, the
// fetch params, and the secrets file PATH (never contents), and lands in
// both the sink stream and the failure tail.
func TestSubprocess_ExecBanner(t *testing.T) {
	fdir := t.TempDir()
	writeFetch(t, fdir, "#!/bin/sh\nexit 0\n")
	var stderr bytes.Buffer
	res, err := Subprocess{}.Run(context.Background(), ExecSpec{
		FetcherDir:  fdir,
		Env:         map[string]string{"JOBS_FETCH_PARAMS": `{"name":"x"}`},
		SecretsFile: "/tmp/secrets.json",
		StderrSink:  &stderr,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d", res.ExitCode)
	}
	want := "jobs: exec " + filepath.Join(fdir, "fetch") +
		` params={"name":"x"} secrets=/tmp/secrets.json` + "\n"
	if stderr.String() != want {
		t.Fatalf("stderr sink got %q, want %q", stderr.String(), want)
	}
	if !strings.Contains(res.StderrTail, "jobs: exec ") {
		t.Fatalf("banner missing from tail: %q", res.StderrTail)
	}
}

// TestSubprocess_RetriesETXTBSY reproduces the multi-slot fork/exec race
// (golang/go#22315): a concurrently forked child inherits the write fd of a
// just-extracted fetch binary and holds it until its own execve, so exec'ing
// the fetcher fails ETXTBSY. The writer always vanishes within milliseconds,
// so Run must retry briefly instead of failing the import.
func TestSubprocess_RetriesETXTBSY(t *testing.T) {
	fdir := t.TempDir()
	writeFetch(t, fdir, "#!/bin/sh\nexit 0\n")
	f, err := os.OpenFile(filepath.Join(fdir, "fetch"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(150*time.Millisecond, func() { f.Close() })

	res, err := Subprocess{}.Run(context.Background(), ExecSpec{FetcherDir: fdir})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.StderrTail)
	}
}
