// Package api is the frame protocol of the jobs-build/1.0 and jobs-admin/1.0
// ALPNs: 4-byte big-endian length + CBOR frame {t, b}. One request per
// stream; watch/log streams stay open with the server pushing frames until
// terminal or the client closes.
//
// The build ALPN accepts: submit, watch, logs, cancel.
// The admin ALPN additionally accepts: requests, fleet, stats, refs, delete,
// diagnose.
package api

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/fxamacker/cbor/v2"

	"github.com/jobs-build/jobs-iroh/wire"
)

// MaxFrame bounds one frame's payload (matches the amber sync protocol).
const MaxFrame = 16 << 20

// Frame types, client → server.
const (
	TSubmit   = "submit"
	TWatch    = "watch"
	TLogs     = "logs"
	TCancel   = "cancel"
	TRequests = "requests"
	TFleet    = "fleet"
	TStats    = "stats"
	TRefs     = "refs"
	TDelete   = "delete"
	TDiagnose = "diagnose"
)

// Frame types, server → client.
const (
	TSubmitted     = "submitted"
	TSnapshot      = "snapshot"
	TLogView       = "logview"
	TLogChunk      = "logchunk"
	TOK            = "ok"
	TError         = "error"
	TRequestsReply = "requests-reply"
	TFleetReply    = "fleet-reply"
	TStatsReply    = "stats-reply"
	TRefsReply     = "refs-reply"
	TDiagnoseReply = "diagnose-reply"
)

// frame is the single wire envelope.
type frame struct {
	T string          `cbor:"t"`
	B cbor.RawMessage `cbor:"b,omitempty"`
}

// WriteFrame encodes v (nil for bodyless frames) and writes one frame.
func WriteFrame(w io.Writer, t string, v any) error {
	var body cbor.RawMessage
	if v != nil {
		b, err := cbor.Marshal(v)
		if err != nil {
			return fmt.Errorf("encode %s body: %w", t, err)
		}
		body = b
	}
	payload, err := cbor.Marshal(frame{T: t, B: body})
	if err != nil {
		return fmt.Errorf("encode %s frame: %w", t, err)
	}
	if len(payload) > MaxFrame {
		return fmt.Errorf("%s frame exceeds %d bytes", t, MaxFrame)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(payload)
	return err
}

// ReadFrame reads one frame, returning its type and raw body.
func ReadFrame(r io.Reader) (string, cbor.RawMessage, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return "", nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxFrame {
		return "", nil, fmt.Errorf("frame of %d bytes exceeds %d", n, MaxFrame)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return "", nil, err
	}
	var f frame
	if err := cbor.Unmarshal(payload, &f); err != nil {
		return "", nil, fmt.Errorf("decode frame: %w", err)
	}
	if f.T == "" {
		return "", nil, errors.New("frame without type")
	}
	return f.T, f.B, nil
}

// DecodeBody decodes a frame body into v.
func DecodeBody(b cbor.RawMessage, v any) error {
	if len(b) == 0 {
		return errors.New("frame without body")
	}
	return cbor.Unmarshal(b, v)
}

// --- payloads ---

// SubmitRequest submits one build. The client constructs the canonical
// builddef.Definition itself (identical to the local path, so local and
// remote submits of the same source produce the same K) and pushes the
// def's referenced objects (tree-source: the source tree closure) over
// jobs-amber-admin BEFORE submitting, under a client scratch ref.
type SubmitRequest struct {
	Def        []byte        `cbor:"def"` // canonical builddef.Definition bytes
	Resources  *ResourceSpec `cbor:"resources,omitempty"`
	ScratchRef string        `cbor:"scratchRef,omitempty"` // client push ref to clean up after commit
	// Label is a display-only name for the target (the resolved context
	// dir); it never enters identity. Optional — absent renders as today.
	Label string `cbor:"label,omitempty"`
}

// ResourceSpec optionally raises the target build's requirement.
type ResourceSpec struct {
	CPU    string `cbor:"cpu,omitempty"`    // "200m"
	Memory string `cbor:"memory,omitempty"` // "1Gi"
}

// Submitted acknowledges a submit.
type Submitted struct {
	RequestID string `cbor:"requestId"`
	K         []byte `cbor:"k"`
}

// WatchRequest opens a snapshot stream for one request until terminal.
type WatchRequest struct {
	RequestID string `cbor:"requestId"`
}

// Snapshot is one coalesced view of a request's closure.
type Snapshot struct {
	RequestID string      `cbor:"requestId"`
	Phase     string      `cbor:"phase"` // running | done | failed | cancelled
	Counts    wire.Counts `cbor:"counts"`
	Nodes     []NodeSnap  `cbor:"nodes,omitempty"`
	Terminal  bool        `cbor:"terminal,omitempty"`
}

// NodeSnap is one node in a snapshot. ElapsedMs is server-computed
// (clock-skew safety, like jobs).
type NodeSnap struct {
	Node       string `cbor:"node"`
	// Label is the node's display name (recipe dep name, dir, fetcher);
	// optional and display-only.
	Label      string `cbor:"label,omitempty"`
	Phase      string `cbor:"phase"`
	Gen        uint64 `cbor:"gen"`
	Runner     string `cbor:"runner,omitempty"`
	ElapsedMs  int64  `cbor:"elapsedMs,omitempty"`
	ErrSummary string `cbor:"errSummary,omitempty"`
}

// LogsRequest fetches one node's captured output (+ live follow).
type LogsRequest struct {
	Node   string `cbor:"node"`
	Follow bool   `cbor:"follow,omitempty"`
}

// LogView is the stored head/gap/tail of the newest attempt.
type LogView struct {
	Node    string `cbor:"node"`
	Gen     uint64 `cbor:"gen"`
	Head    []byte `cbor:"head,omitempty"`
	GapSize int64  `cbor:"gapSize,omitempty"`
	Tail    []byte `cbor:"tail,omitempty"`
}

// CancelRequest cancels (or, on the admin ALPN with delete, removes) a request.
type CancelRequest struct {
	RequestID string `cbor:"requestId"`
}

// Error is the terminal failure frame.
type Error struct {
	Code string `cbor:"code"`
	Text string `cbor:"text"`
}

// Error codes.
const (
	CodeBadRequest  = "bad-request"
	CodeNotFound    = "not-found"
	CodeIncomplete  = "incomplete-upload" // submit: def/tree objects missing
	CodeInternal    = "internal"
	CodeUnavailable = "unavailable"
)

// RequestsReply lists requests (admin).
type RequestsReply struct {
	Requests []RequestInfo `cbor:"requests,omitempty"`
}

// RequestInfo is one request row.
type RequestInfo struct {
	RequestID string      `cbor:"requestId"`
	K         []byte      `cbor:"k"`
	Platform  string      `cbor:"platform"`
	Phase     string      `cbor:"phase"`
	Counts    wire.Counts `cbor:"counts"`
	CreatedNs int64       `cbor:"createdNs"`
}

// FleetReply is the runner fleet snapshot (admin).
type FleetReply struct {
	Runners []wire.RunnerInfo `cbor:"runners,omitempty"`
}

// StatsReply is the server stats snapshot (admin).
type StatsReply struct {
	StoreBytes   int64 `cbor:"storeBytes"`
	RefCount     int   `cbor:"refCount"`
	UptimeNs     int64 `cbor:"uptimeNs"`
	Requests     int   `cbor:"requests"`
	NodesTracked int   `cbor:"nodesTracked"`
}

// RefsRequest browses refs by prefix (admin).
type RefsRequest struct {
	Prefix string `cbor:"prefix,omitempty"`
	Limit  int    `cbor:"limit,omitempty"`
}

// RefsReply lists refs.
type RefsReply struct {
	Refs []RefInfo `cbor:"refs,omitempty"`
}

// RefInfo is one ref row.
type RefInfo struct {
	Name      string `cbor:"name"`
	Key       []byte `cbor:"key"`
	CreatedNs int64  `cbor:"createdNs"`
}

// DiagnoseRequest fetches the durable failure trail (admin). Exactly one of
// RequestID/Node must be set. By request it covers the request's failing
// nodes — from the live graph, or, after a server restart, by scanning the
// FAILURES stream for records tagged with the request ID.
type DiagnoseRequest struct {
	RequestID   string `cbor:"requestId,omitempty"`
	Node        string `cbor:"node,omitempty"`
	MaxAttempts int    `cbor:"maxAttempts,omitempty"` // per node, newest first; server default applies when 0
	MaxLogBytes int64  `cbor:"maxLogBytes,omitempty"` // per attempt head+tail cap; 0 = record as stored
}

// DiagnoseReply is the failure report.
type DiagnoseReply struct {
	RequestID string          `cbor:"requestId,omitempty"` // set for by-request queries
	Phase     string          `cbor:"phase,omitempty"`     // request phase, when known
	Counts    wire.Counts     `cbor:"counts,omitempty"`
	Nodes     []NodeDiagnosis `cbor:"nodes,omitempty"`
	Truncated bool            `cbor:"truncated,omitempty"` // node list hit the server cap
}

// NodeDiagnosis is one failing node's trail, attempts newest first.
type NodeDiagnosis struct {
	Node     string          `cbor:"node"`
	Label    string          `cbor:"label,omitempty"` // display name, when known

	Kind     string          `cbor:"kind"`
	Platform string          `cbor:"platform,omitempty"`
	Phase    string          `cbor:"phase,omitempty"` // current phase, when the node is live in memory
	Gen      uint64          `cbor:"gen,omitempty"`   // current gen, when live
	Attempts []AttemptReport `cbor:"attempts,omitempty"`
}

// AttemptReport is one FailureRecord rendered for the client, plus whether
// MaxLogBytes cut the stored log snapshot.
type AttemptReport struct {
	wire.FailureRecord
	LogTruncated bool `cbor:"logTruncated,omitempty"`
}
