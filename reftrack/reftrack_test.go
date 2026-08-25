package reftrack

import (
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestTouchAndExpire(t *testing.T) {
	tr := New()
	base := time.Now()
	tr.Reconcile([]string{"b"}, base.Add(-2*time.Hour)) // b: seeded two hours ago
	tr.Reconcile([]string{"a", "b"}, base)              // a: fresh; b keeps its old clock

	exp := tr.Expired(time.Hour, base)
	if !slices.Contains(exp, "b") {
		t.Fatalf("b should expire, got %v", exp)
	}
	if slices.Contains(exp, "a") {
		t.Fatalf("a is fresh, got %v", exp)
	}

	// A touch resets the clock (Touch stamps time.Now() ≈ base).
	tr.Touch("b")
	if exp := tr.Expired(time.Hour, base); len(exp) != 0 {
		t.Fatalf("nothing should expire after the touch, got %v", exp)
	}
}

func TestReconcileSeedsAndDrops(t *testing.T) {
	tr := New()
	now := time.Now()
	tr.Reconcile([]string{"x"}, now)
	e, ok := tr.Get("x")
	if !ok || !e.FirstSeen.Equal(now) || !e.LastAccess.Equal(now) {
		t.Fatalf("seed: got %+v ok=%v", e, ok)
	}
	// A later reconcile must not reset the clock of a known name…
	tr.Reconcile([]string{"x"}, now.Add(time.Hour))
	if e, _ := tr.Get("x"); !e.LastAccess.Equal(now) {
		t.Fatalf("reconcile reset the clock: %+v", e)
	}
	// …and drops vanished names.
	tr.Reconcile(nil, now.Add(2*time.Hour))
	if _, ok := tr.Get("x"); ok {
		t.Fatal("vanished name kept")
	}
}

func TestPinnedAndProtectedNeverExpire(t *testing.T) {
	tr := New()
	base := time.Now()
	tr.Reconcile([]string{"shell:linux/amd64", "fetcher:github:linux/amd64",
		"seed-src:shell:deadbeef", "keep", "doomed"}, base)
	tr.Pin("keep")
	exp := tr.Expired(time.Hour, base.Add(48*time.Hour))
	if !slices.Equal(exp, []string{"doomed"}) {
		t.Fatalf("want [doomed], got %v", exp)
	}
	tr.Unpin("keep")
	exp = tr.Expired(time.Hour, base.Add(48*time.Hour))
	slices.Sort(exp)
	if !slices.Equal(exp, []string{"doomed", "keep"}) {
		t.Fatalf("after unpin want [doomed keep], got %v", exp)
	}
}

func TestFamilyRule(t *testing.T) {
	tr := New()
	base := time.Now()
	out, deps := "build-output:abc", "build-output-deps:abc"
	tr.Reconcile([]string{out, deps}, base)

	// Touching either touches both.
	tr.Touch(deps)
	eo, _ := tr.Get(out)
	ed, _ := tr.Get(deps)
	if !eo.LastAccess.Equal(ed.LastAccess) || eo.LastAccess.Equal(base) {
		t.Fatalf("family clocks diverge: out=%v deps=%v", eo.LastAccess, ed.LastAccess)
	}

	// Expiry orders output strictly before deps.
	exp := tr.Expired(time.Nanosecond, base.Add(time.Hour))
	io, id := slices.Index(exp, out), slices.Index(exp, deps)
	if io == -1 || id == -1 || io > id {
		t.Fatalf("want output before deps, got %v", exp)
	}
}

func TestPinTouches(t *testing.T) {
	tr := New()
	tr.Pin("p")
	if e, ok := tr.Get("p"); !ok || !e.Pinned || e.LastAccess.IsZero() {
		t.Fatalf("pin must create+touch: %+v ok=%v", e, ok)
	}
}

func TestLoadFlushRoundTrip(t *testing.T) {
	tr := New()
	tr.Touch("a")
	tr.Pin("p")
	path := filepath.Join(t.TempDir(), "refaccess.cbor")
	if err := tr.Flush(path); err != nil {
		t.Fatal(err)
	}
	tr2 := New()
	if err := tr2.Load(path); err != nil {
		t.Fatal(err)
	}
	a1, _ := tr.Get("a")
	a2, ok := tr2.Get("a")
	if !ok || !a1.LastAccess.Equal(a2.LastAccess) {
		t.Fatalf("round trip lost a: %+v ok=%v", a2, ok)
	}
	if p, ok := tr2.Get("p"); !ok || !p.Pinned {
		t.Fatalf("round trip lost pin: %+v ok=%v", p, ok)
	}
}

func TestLoadMissingAndCorrupt(t *testing.T) {
	tr := New()
	if err := tr.Load(filepath.Join(t.TempDir(), "nope.cbor")); err != nil {
		t.Fatalf("missing snapshot must be a clean start: %v", err)
	}
	bad := filepath.Join(t.TempDir(), "bad.cbor")
	if err := writeFileAtomic(bad, []byte("not cbor")); err != nil {
		t.Fatal(err)
	}
	tr2 := New()
	if err := tr2.Load(bad); err == nil {
		t.Fatal("corrupt snapshot should report an error")
	}
	if total, _ := tr2.Counts(); total != 0 {
		t.Fatal("corrupt snapshot must leave the tracker empty")
	}
}
