package tailbuf_test

import (
	"strings"
	"testing"

	"github.com/jobs-build/jobs-iroh/tailbuf"
)

func TestTailBuf_LargeWrite(t *testing.T) {
	const max = 4096
	const total = 10 * 1024

	b := tailbuf.New(max)

	// Build a 10KB payload: sequential bytes so we can verify the tail.
	payload := make([]byte, total)
	for i := range payload {
		payload[i] = byte(i % 251) // prime modulus for variety
	}

	n, err := b.Write(payload)
	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if n != total {
		t.Fatalf("Write returned n=%d, want %d", n, total)
	}

	got := b.String()
	if len(got) != max {
		t.Fatalf("String() length = %d, want %d", len(got), max)
	}

	want := string(payload[total-max:])
	if got != want {
		t.Fatal("String() does not equal the last 4096 bytes of the input")
	}
}

func TestTailBuf_ShortWrite(t *testing.T) {
	b := tailbuf.New(4096)

	msg := "hello, world"
	n, err := b.Write([]byte(msg))
	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if n != len(msg) {
		t.Fatalf("Write returned n=%d, want %d", n, len(msg))
	}
	if got := b.String(); got != msg {
		t.Fatalf("String() = %q, want %q", got, msg)
	}
}

func TestTailBuf_MultipleWrites(t *testing.T) {
	const max = 100
	b := tailbuf.New(max)

	// Write two halves; total = 120 bytes → keeps last 100.
	first := strings.Repeat("A", 60)
	second := strings.Repeat("B", 60)
	b.Write([]byte(first))  //nolint:errcheck
	b.Write([]byte(second)) //nolint:errcheck

	got := b.String()
	if len(got) != max {
		t.Fatalf("String() length = %d, want %d", len(got), max)
	}
	// Last 100 bytes = last 40 As + 60 Bs.
	want := strings.Repeat("A", 20) + strings.Repeat("B", 60) + strings.Repeat("A", 20)
	// Actually: total=120, keep last 100: bytes[20:120] = A[20:60]+B[0:60] = 40A + 60B
	want = strings.Repeat("A", 40) + strings.Repeat("B", 60)
	if got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
