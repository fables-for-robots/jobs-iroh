package events

import (
	"bytes"
	"testing"
)

func TestBatchRoundTrip(t *testing.T) {
	in := []Event{
		{Emitter: "server-0", Boot: "abcd", Seq: 1, TS: 1234567890, Type: TypeNodeDone,
			Node: "import|00ff", Data: map[string]any{"cached": true}},
		{Emitter: "r1", Boot: "ef01", Seq: 7, TS: 42, Type: TypeExecOutput,
			Node: "build|00ff", Runner: "r1",
			Data: map[string]any{"stream": "stderr", "chunk": []byte("hello\n"), "offset": int64(0)}},
	}
	b, err := EncodeBatch(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := DecodeBatch(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d events", len(out))
	}
	if out[0].Type != TypeNodeDone || out[0].Seq != 1 || out[0].Data["cached"] != true {
		t.Fatalf("event 0 mismatch: %+v", out[0])
	}
	chunk, ok := out[1].Data["chunk"].([]byte)
	if !ok || !bytes.Equal(chunk, []byte("hello\n")) {
		t.Fatalf("chunk did not round-trip as []byte: %#v", out[1].Data["chunk"])
	}
}

func TestSingleRoundTrip(t *testing.T) {
	in := Event{Emitter: "e", Boot: "b", Seq: 3, TS: 99, Type: TypeRequestNode,
		Request: "req-1", Node: "import|aa", Data: map[string]any{"label": "import github"}}
	b, err := Encode(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if out.Request != "req-1" || out.Data["label"] != "import github" {
		t.Fatalf("mismatch: %+v", out)
	}
}
