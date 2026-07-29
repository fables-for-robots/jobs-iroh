//go:build linux

package runner

import (
	"context"
	"io"
	"testing"

	"github.com/jobs-build/jobs-iroh/amber"
	"github.com/jobs-build/jobs-iroh/builddef"
)

// TestEnsureInput_TreeIsNoOp verifies that a tree-kind Input in ensureInput
// returns nil without error. A tree source is already-present content; the
// build-from stage resolves it directly and ensureInput should treat it as a
// no-op. This is the regression test for the subbuild local-develop path.
func TestEnsureInput_TreeIsNoOp(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	d := &developDriver{
		ctx:        ctx,
		st:         st,
		rw:         NewLocalRefWriter(st),
		brc:        BuildRunCfg{},
		secrets:    nil,
		visited:    map[string]bool{},
		inProgress: map[string]bool{},
	}

	// Construct a tree Input: FileKey gives a valid amber content key without
	// storing anything; TreeInput wraps it into a KindTree Input.
	k, err := amber.FileKey([]byte("root"))
	if err != nil {
		t.Fatal(err)
	}
	in, err := builddef.TreeInput(k)
	if err != nil {
		t.Fatal(err)
	}

	p := NewProgress(io.Discard)
	if err := d.ensureInput(in, "", p); err != nil {
		t.Fatalf("ensureInput for tree input returned unexpected error: %v", err)
	}
}
