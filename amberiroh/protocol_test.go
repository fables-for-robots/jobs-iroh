package amberiroh

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"reflect"
	"testing"
)

func TestMsgRoundTrip(t *testing.T) {
	msgs := []Msg{
		{Type: TPush, Name: "backups/home", Root: bytes.Repeat([]byte{7}, 32), CAS: true, ExpectedOld: bytes.Repeat([]byte{9}, 32)},
		{Type: TPush, Name: "n", Root: bytes.Repeat([]byte{7}, 32), CAS: true}, // ExpectedOld nil = must not exist
		{Type: TPull, Name: "backups/home"},
		{Type: TRefList},
		{Type: TRef, Record: []byte{0xa1, 0x00, 0x01}},
		{Type: TRefs, Refs: []RefInfo{{Name: "a", Key: bytes.Repeat([]byte{1}, 32), CreatedAt: 42, User: "u"}}},
		{Type: TWants, Keys: [][]byte{bytes.Repeat([]byte{2}, 32), bytes.Repeat([]byte{3}, 32)}},
		{Type: TWants}, // empty wants = done
		{Type: TData, Data: []byte("payload")},
		{Type: TDataEnd},
		{Type: TOK, Key: bytes.Repeat([]byte{4}, 32)},
		{Type: TErr, Code: CodeCASMismatch, Text: "remote ref changed", Current: bytes.Repeat([]byte{5}, 32)},
		{Type: TPush, Name: "n", Root: bytes.Repeat([]byte{7}, 32), DataConns: 3},
		{Type: TAccept, Token: []byte("tok-1234"), DataPorts: []uint16{4242, 4243}},
		{Type: TAttach, Token: []byte("tok-1234")},
		{Type: TRef, Record: []byte{0xa1}, Token: []byte("tok-9")},
	}
	var buf bytes.Buffer
	for _, m := range msgs {
		if err := WriteMsg(&buf, m); err != nil {
			t.Fatalf("write %+v: %v", m, err)
		}
	}
	for _, want := range msgs {
		got, err := ReadMsg(&buf)
		if err != nil {
			t.Fatalf("read (want %+v): %v", want, err)
		}
		checkMsgEqual(t, got, want)
	}
	if _, err := ReadMsg(&buf); !errors.Is(err, io.EOF) {
		t.Fatalf("read past end: want io.EOF, got %v", err)
	}
}

func checkMsgEqual(t *testing.T, got, want Msg) {
	t.Helper()
	if len(got.DataPorts) != len(want.DataPorts) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}
	for i := range want.DataPorts {
		if got.DataPorts[i] != want.DataPorts[i] {
			t.Fatalf("dataports mismatch: got %v want %v", got.DataPorts, want.DataPorts)
		}
	}
	if got.DataConns != want.DataConns || !bytes.Equal(got.Token, want.Token) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}
	if got.Type != want.Type || got.Name != want.Name || got.CAS != want.CAS ||
		!bytes.Equal(got.Root, want.Root) || !bytes.Equal(got.ExpectedOld, want.ExpectedOld) ||
		!bytes.Equal(got.Record, want.Record) || !bytes.Equal(got.Data, want.Data) ||
		!bytes.Equal(got.Key, want.Key) || got.Code != want.Code || got.Text != want.Text ||
		!bytes.Equal(got.Current, want.Current) || len(got.Refs) != len(want.Refs) || len(got.Keys) != len(want.Keys) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}
	for i := range want.Refs {
		g, w := got.Refs[i], want.Refs[i]
		if g.Name != w.Name || g.CreatedAt != w.CreatedAt || g.User != w.User || !bytes.Equal(g.Key, w.Key) {
			t.Fatalf("refs[%d] mismatch: got %+v want %+v", i, g, w)
		}
	}
	for i := range want.Keys {
		if !bytes.Equal(got.Keys[i], want.Keys[i]) {
			t.Fatalf("keys[%d] mismatch", i)
		}
	}
}

func TestReadMsgRejectsOversizeFrame(t *testing.T) {
	var buf bytes.Buffer
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], MaxFrame+1)
	buf.Write(hdr[:])
	if _, err := ReadMsg(&buf); err == nil {
		t.Fatal("want error for oversize frame, got nil")
	}
}

func TestReadMsgTruncatedFrame(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMsg(&buf, Msg{Type: TDataEnd}); err != nil {
		t.Fatal(err)
	}
	trunc := buf.Bytes()[:buf.Len()-1]
	if _, err := ReadMsg(bytes.NewReader(trunc)); err == nil {
		t.Fatal("want error for truncated frame, got nil")
	}
}

func TestRemoteError(t *testing.T) {
	m := Msg{Type: TErr, Code: CodeUnknownRef, Text: "ref \"x\" not found"}
	re := RemoteFromMsg(m)
	if re.Code != CodeUnknownRef || re.Text != m.Text {
		t.Fatalf("RemoteFromMsg: %+v", re)
	}
	if re.Error() == "" {
		t.Fatal("empty Error()")
	}
}

func TestMsgDataEndpointsRoundTrip(t *testing.T) {
	in := Msg{Type: TAccept, Token: []byte{1}, DataPorts: []uint16{4001, 4002},
		DataEndpoints: []DataEndpointRec{
			{ID: bytes.Repeat([]byte{7}, 32), Addrs: []string{"ip:192.168.1.5:4001", "relay:https://euc1-1.relay.example./"}},
			{ID: bytes.Repeat([]byte{8}, 32), Addrs: []string{"ip:192.168.1.5:4002"}},
		}}
	var buf bytes.Buffer
	if err := WriteMsg(&buf, in); err != nil {
		t.Fatal(err)
	}
	out, err := ReadMsg(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip mismatch:\n in %+v\nout %+v", in, out)
	}
}

// Old peers must interoperate across field 16: an old decoder ignores it on
// new frames, and a new decoder yields nil for its absence on old frames.
func TestMsgDataEndpointsCompat(t *testing.T) {
	type oldMsg struct {
		Type      int      `cbor:"0,keyasint"`
		Token     []byte   `cbor:"13,keyasint,omitempty"`
		DataPorts []uint16 `cbor:"15,keyasint,omitempty"`
	}
	in := Msg{Type: TAccept, Token: []byte{1}, DataPorts: []uint16{4001},
		DataEndpoints: []DataEndpointRec{{ID: bytes.Repeat([]byte{7}, 32), Addrs: []string{"ip:127.0.0.1:4001"}}}}
	payload, err := encMode.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var old oldMsg
	if err := decMode.Unmarshal(payload, &old); err != nil {
		t.Fatalf("old decoder rejects new frame: %v", err)
	}
	if old.Type != TAccept || len(old.DataPorts) != 1 || len(old.Token) != 1 {
		t.Fatalf("old decode mangled fields: %+v", old)
	}
	oldPayload, err := encMode.Marshal(oldMsg{Type: TAccept, Token: []byte{1}, DataPorts: []uint16{4001}})
	if err != nil {
		t.Fatal(err)
	}
	var m Msg
	if err := decMode.Unmarshal(oldPayload, &m); err != nil {
		t.Fatal(err)
	}
	if m.DataEndpoints != nil {
		t.Fatalf("absent field decoded non-nil: %+v", m.DataEndpoints)
	}
}
