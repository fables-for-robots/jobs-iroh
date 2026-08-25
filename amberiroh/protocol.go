// Package amberiroh moves amber store content between peers over iroh QUIC.
//
// It is the transport half of the store: amber-store-core holds the
// content-addressed objects, this package ships them. One bidirectional QUIC
// stream carries one operation as length-prefixed CBOR frames, with amberpack
// payloads chunked into TData frames.
//
// The have/want loop (Send, Receive) transfers objects below a root: the
// receiver announces which keys it is missing, the sender answers each round
// with an amberpack of exactly those objects. Server answers push, pull and
// ref-list operations against a store directory; serve mounts it on the
// jobs-runner-amber/1.0 and jobs-amber-admin/1.0 ALPNs.
//
// Vendored from github.com/jobs-build/amber-store-iroh, which jobs-iroh was
// the only external consumer of. This copy is authoritative; upstream retains
// its own copy for the amber and amber-serve CLIs, and the two can drift.
package amberiroh

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/fxamacker/cbor/v2"
)

const (
	// ALPN identifies this protocol during the QUIC handshake. Server and
	// client must agree byte-for-byte or connections are rejected.
	ALPN = "amber-store-iroh/1"

	// MaxFrame bounds one frame's CBOR payload, limiting the memory a
	// peer can force us to allocate.
	MaxFrame = 16 << 20

	// ChunkSize is how many amberpack bytes ride in one TData frame.
	ChunkSize = 1 << 20
)

// Frame types. Type selects which Msg fields are meaningful.
const (
	TPush    = 1  // client→server: push request (Name, Root, CAS, ExpectedOld)
	TPull    = 2  // client→server: pull request (Name)
	TRefList = 3  // client→server: list all refs
	TRef     = 4  // server→client: raw reference record (Record)
	TRefs    = 5  // server→client: ref listing (Refs)
	TWants   = 6  // receiver→sender: keys to transfer next (Keys; empty = done)
	TData    = 7  // sender→receiver: amberpack payload chunk (Data)
	TDataEnd = 8  // sender→receiver: end of one pack payload
	TOK      = 9  // server→client: push committed (Key)
	TErr     = 10 // either direction: terminal failure (Code, Text, Current)
	TAttach  = 11 // client→server: attach this stream to a transfer (Token)
	TAccept  = 12 // server→client: sharded transfer accepted (Token)
	TPin     = 13 // client→server: keep these refs forever (Names) — GC pin-assert
)

// Error codes carried in TErr frames.
const (
	CodeCASMismatch = "cas-mismatch"
	CodeUnknownRef  = "unknown-ref"
	CodeBadRequest  = "bad-request"
	CodeInternal    = "internal"
)

// ErrProtocol reports a frame that is valid CBOR but wrong for the moment
// it arrived in the exchange.
var ErrProtocol = errors.New("protocol: unexpected frame")

// RefInfo is one reference in a TRefs listing.
type RefInfo struct {
	Name      string `cbor:"0,keyasint"`
	Key       []byte `cbor:"1,keyasint"`
	CreatedAt int64  `cbor:"2,keyasint"` // ns since the Unix epoch
	User      string `cbor:"3,keyasint,omitempty"`
}

// Msg is the single frame payload type.
// DataEndpointRec describes one of the server's data endpoints to a
// sharding client: the endpoint's own identity (data endpoints carry their
// own keys so each can hold a relay home connection — relays key sessions
// by endpoint ID) and its dial candidates as netaddr.TransportAddr strings
// ("ip:host:port", "relay:url"). The client trusts the identity because the
// record arrives on the control connection, which authenticated the server;
// the shard handshake then proves possession of the advertised key. Old
// peers ignore the field and keep direct-dialing DataPorts.
type DataEndpointRec struct {
	ID    []byte   `cbor:"0,keyasint"`
	Addrs []string `cbor:"1,keyasint,omitempty"`
}

type Msg struct {
	Type        int       `cbor:"0,keyasint"`
	Name        string    `cbor:"1,keyasint,omitempty"`
	Root        []byte    `cbor:"2,keyasint,omitempty"`
	CAS         bool      `cbor:"3,keyasint,omitempty"` // push precondition present (--force omits it)
	ExpectedOld []byte    `cbor:"4,keyasint,omitempty"` // with CAS: expected current key; nil = ref must not exist
	Record      []byte    `cbor:"5,keyasint,omitempty"` // canonical reference encoding, signature carried opaquely
	Refs        []RefInfo `cbor:"6,keyasint,omitempty"`
	Keys        [][]byte  `cbor:"7,keyasint,omitempty"`
	Data        []byte    `cbor:"8,keyasint,omitempty"`
	Key         []byte    `cbor:"9,keyasint,omitempty"`
	Code        string    `cbor:"10,keyasint,omitempty"`
	Text        string    `cbor:"11,keyasint,omitempty"`
	Current     []byte    `cbor:"12,keyasint,omitempty"` // cas-mismatch: the server's current key (nil = absent)
	Token       []byte    `cbor:"13,keyasint,omitempty"` // transfer token for TAttach/TAccept (and TRef on sharded pulls)
	DataConns   int       `cbor:"14,keyasint,omitempty"` // push/pull request: extra data connections the client will attach
	DataPorts   []uint16  `cbor:"15,keyasint,omitempty"` // TAccept/TRef: server data-endpoint UDP ports for the extra connections
	// DataEndpoints describes the data endpoints as punchable peers on
	// TAccept/TRef. Its presence doubles as the capability signal that the
	// server gathers attaches for the longer punch-friendly window.
	DataEndpoints []DataEndpointRec `cbor:"16,keyasint,omitempty"`
	// Names are the ref names of a TPin assert. Additive: old peers
	// ignore the field, old servers answer TPin itself with TErr.
	Names []string `cbor:"17,keyasint,omitempty"`
}

var (
	encMode cbor.EncMode
	decMode cbor.DecMode
)

func init() {
	var err error
	encMode, err = cbor.CanonicalEncOptions().EncMode()
	if err != nil {
		panic(err)
	}
	decMode, err = cbor.DecOptions{}.DecMode()
	if err != nil {
		panic(err)
	}
}

// WriteMsg writes one frame: 4-byte big-endian payload length, then the
// CBOR-encoded Msg.
func WriteMsg(w io.Writer, m Msg) error {
	payload, err := encMode.Marshal(m)
	if err != nil {
		return fmt.Errorf("protocol: encode frame: %w", err)
	}
	if len(payload) > MaxFrame {
		return fmt.Errorf("protocol: frame of %d bytes exceeds limit %d", len(payload), MaxFrame)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(payload)
	return err
}

// ReadMsg reads one frame. A clean end-of-stream before the header
// surfaces as io.EOF; a stream cut mid-frame as io.ErrUnexpectedEOF.
func ReadMsg(r io.Reader) (Msg, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return Msg{}, io.ErrUnexpectedEOF
		}
		return Msg{}, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxFrame {
		return Msg{}, fmt.Errorf("protocol: frame of %d bytes exceeds limit %d", n, MaxFrame)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return Msg{}, fmt.Errorf("protocol: short frame: %w", err)
	}
	var m Msg
	if err := decMode.Unmarshal(payload, &m); err != nil {
		return Msg{}, fmt.Errorf("protocol: decode frame: %w", err)
	}
	return m, nil
}

// RemoteError is a TErr frame surfaced as a Go error on the peer.
type RemoteError struct {
	Code    string
	Text    string
	Current []byte // cas-mismatch: the server's current key, nil when the ref is absent
}

func (e *RemoteError) Error() string {
	return fmt.Sprintf("remote: %s: %s", e.Code, e.Text)
}

// RemoteFromMsg converts a TErr frame into a *RemoteError.
func RemoteFromMsg(m Msg) *RemoteError {
	return &RemoteError{Code: m.Code, Text: m.Text, Current: m.Current}
}
