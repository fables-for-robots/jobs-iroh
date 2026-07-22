package runner

import (
	"context"
	"testing"
)

func TestLocalRefWriter_WriteRefAndReadBack(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	k, err := st.IngestFile(ctx, []byte("hello"))
	if err != nil {
		t.Fatalf("IngestFile: %v", err)
	}
	w := NewLocalRefWriter(st)
	if err := w.WriteRef(ctx, "import-output:test", k); err != nil {
		t.Fatalf("WriteRef: %v", err)
	}
	got, ok, err := st.GetKey(ctx, "import-output:test")
	if err != nil || !ok || got != k {
		t.Fatalf("read back: got=%v ok=%v err=%v, want %v true nil", got, ok, err, k)
	}
}

// TestLocalRefWriter_WriteRefs_BatchAndNoTotals: a batch writes every entry
// (readable back), and local mode never reports push totals.
func TestLocalRefWriter_WriteRefs_BatchAndNoTotals(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	k, err := st.IngestFile(ctx, []byte("first"))
	if err != nil {
		t.Fatalf("IngestFile: %v", err)
	}
	w := NewLocalRefWriter(st)
	tot, err := w.WriteRefs(ctx, []Ref{
		{Name: "build-from:test", Key: k},
		{Name: "build-from-tree:" + k.String(), Key: k},
	})
	if err != nil {
		t.Fatalf("WriteRefs: %v", err)
	}
	if tot.Reported {
		t.Fatalf("local mode must not report push totals: %+v", tot)
	}
	for _, name := range []string{"build-from:test", "build-from-tree:" + k.String()} {
		got, ok, err := st.GetKey(ctx, name)
		if err != nil || !ok || got != k {
			t.Fatalf("%s: got=%v ok=%v err=%v", name, got, ok, err)
		}
	}
}

// TestLocalRefWriter_WriteRefs_SequentialStopOnFirstError locks the ordered-
// batch invariant RunBuild depends on ([cacheRefs…, build-output-deps:F,
// build-output:F]): entries are written IN ORDER and the first failure stops
// the batch, so a later entry never lands without its predecessors.
func TestLocalRefWriter_WriteRefs_SequentialStopOnFirstError(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	k, err := st.IngestFile(ctx, []byte("payload"))
	if err != nil {
		t.Fatalf("IngestFile: %v", err)
	}
	w := NewLocalRefWriter(st)
	// "bad@name" violates reference.ValidateName ('@' is reserved), so the
	// middle entry fails.
	_, err = w.WriteRefs(ctx, []Ref{
		{Name: "before:ok", Key: k},
		{Name: "bad@name", Key: k},
		{Name: "after:must-not-land", Key: k},
	})
	if err == nil {
		t.Fatal("want an error from the invalid middle entry")
	}
	if _, ok, _ := st.GetKey(ctx, "before:ok"); !ok {
		t.Fatal("entry before the failure must have landed")
	}
	if _, ok, _ := st.GetKey(ctx, "after:must-not-land"); ok {
		t.Fatal("entry after the failure must NOT have landed (stop-on-first-error)")
	}
}
