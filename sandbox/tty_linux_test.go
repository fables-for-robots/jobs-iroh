//go:build linux

package sandbox_test

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/jobs-build/jobs-iroh/sandbox"
)

// With a Tty set, the child's stdin must be a terminal (test -t 0 succeeds).
func TestControllingTTY(t *testing.T) {
	if !sandbox.UserNSAvailable() {
		t.Skip("user namespaces unavailable")
	}
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer ptmx.Close()

	got := make(chan string, 1)
	go func() { b, _ := io.ReadAll(ptmx); got <- string(b) }()

	code, err := sandbox.Run(context.Background(), sandbox.Config{
		Command:    []string{"/bin/sh", "-c", "test -t 0 && echo ISTTY"},
		Namespaces: sandbox.Namespaces{User: true},
		Tty:        tty,
	})
	tty.Close() // allow the reader to see EOF once the child has exited
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != 0 {
		t.Fatalf("child exit %d (stdin was not a tty)", code)
	}
	select {
	case out := <-got:
		if !strings.Contains(out, "ISTTY") {
			t.Fatalf("expected ISTTY, got %q", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout reading from pty master")
	}
}
