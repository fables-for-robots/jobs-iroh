package registryd

import (
	"slices"
	"testing"
	"time"
)

func TestPinAsserterCoalesces(t *testing.T) {
	p := newPinAsserter()
	now := time.Now()

	due := p.due([]string{"a", "b"}, now)
	slices.Sort(due)
	if !slices.Equal(due, []string{"a", "b"}) {
		t.Fatalf("first due = %v", due)
	}
	if got := p.due([]string{"a", "b"}, now.Add(time.Minute)); len(got) != 0 {
		t.Fatalf("within the hour must coalesce, got %v", got)
	}
	if got := p.due([]string{"a"}, now.Add(2*time.Hour)); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("after an hour must re-assert, got %v", got)
	}
}

func TestPinAsserterRetryAndDisable(t *testing.T) {
	p := newPinAsserter()
	now := time.Now()
	_ = p.due([]string{"a"}, now)

	// A transport failure re-arms the names for the next serve.
	p.retry([]string{"a"})
	if got := p.due([]string{"a"}, now.Add(time.Second)); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("retry must re-arm, got %v", got)
	}

	// An old server disables asserting for good.
	p.disable()
	if got := p.due([]string{"a", "b"}, now.Add(3*time.Hour)); len(got) != 0 {
		t.Fatalf("disabled asserter returned %v", got)
	}
}

func TestPinNames(t *testing.T) {
	rec := imageRecord{K: "aa", F: "bb", Platform: "linux/amd64"}
	want := []string{"build-from:aa", "build:aa", "build-output:bb",
		"build-output-deps:bb", "shell:linux/amd64"}
	got := pinNames(rec)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("pinNames = %v, want %v", got, want)
	}
}
