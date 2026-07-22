package events

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func collectOutput(t *testing.T, evs []Event) (string, bool) {
	t.Helper()
	var buf bytes.Buffer
	truncated := false
	for _, ev := range evs {
		switch ev.Type {
		case TypeExecOutput:
			buf.Write(ev.Data["chunk"].([]byte))
		case TypeExecOutputTruncated:
			truncated = true
		}
	}
	return buf.String(), truncated
}

func TestOutputWriterChunksAndFlushes(t *testing.T) {
	sink := &captureSink{}
	j := NewJob(sink, "build|00", "r1", []string{"req-1"})

	ow := j.Output("stderr", "building")
	big := strings.Repeat("x", 100<<10) // > flush threshold, > chunk max
	if _, err := ow.Write([]byte(big)); err != nil {
		t.Fatal(err)
	}
	if _, err := ow.Write([]byte("tail")); err != nil {
		t.Fatal(err)
	}
	if err := ow.Close(); err != nil {
		t.Fatal(err)
	}

	got, truncated := collectOutput(t, sink.events())
	if truncated {
		t.Fatal("unexpected truncation")
	}
	if got != big+"tail" {
		t.Fatalf("output mismatch: got %d bytes, want %d", len(got), len(big)+4)
	}
	for _, ev := range sink.events() {
		if ev.Type != TypeExecOutput {
			continue
		}
		if len(ev.Data["chunk"].([]byte)) > 32<<10 {
			t.Fatalf("chunk exceeds 32KiB: %d", len(ev.Data["chunk"].([]byte)))
		}
		if ev.Node != "build|00" || ev.Runner != "r1" || ev.Data["stream"] != "stderr" {
			t.Fatalf("chunk metadata wrong: %+v", ev)
		}
	}
}

func TestOutputWriterCapTruncates(t *testing.T) {
	sink := &captureSink{}
	j := NewJob(sink, "build|00", "r1", nil)

	ow := j.Output("stdout", "building")
	ow.cap = 10 // tiny cap for the test
	if _, err := ow.Write([]byte("0123456789ABCDEF")); err != nil {
		t.Fatal(err)
	}
	if _, err := ow.Write([]byte("more after truncation")); err != nil {
		t.Fatal(err)
	}
	if err := ow.Close(); err != nil {
		t.Fatal(err)
	}

	got, truncated := collectOutput(t, sink.events())
	if !truncated {
		t.Fatal("expected a truncation event")
	}
	if got != "0123456789" {
		t.Fatalf("got %q, want first 10 bytes", got)
	}
}

func TestOutputWriterTimedFlush(t *testing.T) {
	sink := &captureSink{}
	j := NewJob(sink, "build|00", "r1", nil)
	ow := j.Output("stderr", "building")
	if _, err := ow.Write([]byte("burst then silence\n")); err != nil {
		t.Fatal(err)
	}
	// No Close, no further writes: the 100ms timer must flush it.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := collectOutput(t, sink.events())
		if got == "burst then silence\n" {
			ow.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed flush never delivered the buffered output")
}

func TestJobLifecycleEvents(t *testing.T) {
	sink := &captureSink{}
	j := NewJob(sink, "import|aa", "r1", []string{"req-9"})
	j.Started("import")
	j.Phase("fetching")
	j.Finished("completed", 0)
	evs := sink.events()
	if len(evs) != 3 || evs[0].Type != TypeExecStarted || evs[1].Type != TypeExecPhase || evs[2].Type != TypeExecFinished {
		t.Fatalf("lifecycle mismatch: %+v", evs)
	}
	reqs, ok := evs[0].Data["requests"].([]any)
	if !ok || len(reqs) != 1 || reqs[0] != "req-9" {
		t.Fatalf("requests not stamped: %#v", evs[0].Data["requests"])
	}
	if evs[2].Data["outcome"] != "completed" {
		t.Fatalf("outcome missing: %+v", evs[2])
	}
	for _, ev := range evs {
		if ev.TS == 0 {
			t.Fatalf("TS not stamped: %+v", ev)
		}
	}
}

func TestNilJobIsSafe(t *testing.T) {
	j := NewJob(nil, "n", "r", nil)
	if j != nil {
		t.Fatal("nil sink must yield a nil Job")
	}
	j.Started("import")
	j.Phase("x")
	j.Progress("pulling", 3, 4096)
	j.Finished("completed", 0)
	if ow := j.Output("stderr", "x"); ow != nil {
		t.Fatal("nil Job must yield nil OutputWriter")
	}
}

func TestJobProgressEvent(t *testing.T) {
	sink := &captureSink{}
	j := NewJob(sink, "build|aa", "r1", []string{"req-2"})
	j.Progress("pulling", 42, 1<<20)
	evs := sink.events()
	if len(evs) != 1 || evs[0].Type != TypeExecProgress {
		t.Fatalf("progress event mismatch: %+v", evs)
	}
	d := evs[0].Data
	if d["phase"] != "pulling" {
		t.Fatalf("phase missing: %+v", d)
	}
	// CBOR round-trips small ints as uint64/int64; compare numerically.
	if objects, ok := d["objects"].(uint64); !ok || objects != 42 {
		t.Fatalf("objects wrong: %#v", d["objects"])
	}
	if bytes, ok := d["bytes"].(uint64); !ok || bytes != 1<<20 {
		t.Fatalf("bytes wrong: %#v", d["bytes"])
	}
}

// TestNopSink: a NopSink-backed Job is non-nil and everything still no-ops
// without panicking.
func TestNopSink(t *testing.T) {
	j := NewJob(NopSink{}, "build|00", "r1", nil)
	if j == nil {
		t.Fatal("NopSink must yield a usable (non-nil) Job")
	}
	j.Started("build")
	ow := j.Output("stdout", "building")
	if ow == nil {
		t.Fatal("non-nil Job must yield a writer")
	}
	if _, err := ow.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := ow.Close(); err != nil {
		t.Fatal(err)
	}
}
