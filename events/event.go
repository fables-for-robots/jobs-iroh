// Package events defines the jobs-iroh build-event schema (ported from jobs'
// build-events design): every fetch/build state change (server) and every
// execution step incl. process output (runners) becomes one Event, delivered
// at-least-once to a Sink. (Emitter, Boot, Seq) is the global dedup identity —
// Boot is random per process, so no counter needs to survive restarts.
//
// This is jobs' events package trimmed for jobs-iroh: the HTTP/Pebble Emitter
// is replaced by the minimal Sink seam (sink.go) — in jobs-iroh events ride
// core NATS subjects — while the Event schema, the type constants, and the
// runner-facing Job/OutputWriter scopes keep their exact semantics.
package events

import (
	"reflect"

	cbor "github.com/fxamacker/cbor/v2"
)

// Event is one build event. TS is unix nanoseconds (int64, never time.Time —
// CBOR time encoding would lose sub-second precision and consumer keys derive
// from TS directly). Events are NOT identity-bearing, so plain (not canonical)
// CBOR is fine.
type Event struct {
	Emitter string         `cbor:"emitter" json:"emitter"`
	Boot    string         `cbor:"boot" json:"boot"`
	Seq     uint64         `cbor:"seq" json:"seq"`
	TS      int64          `cbor:"ts" json:"ts"`
	Type    string         `cbor:"type" json:"type"`
	Request string         `cbor:"request,omitempty" json:"request,omitempty"`
	Node    string         `cbor:"node,omitempty" json:"node,omitempty"`
	Runner  string         `cbor:"runner,omitempty" json:"runner,omitempty"`
	Data    map[string]any `cbor:"data,omitempty" json:"data,omitempty"`
}

// Server-emitted event types (authoritative state changes).
const (
	TypeRequestReceived = "request.received"   // Request+Node(target); Data: submit summary
	TypeRequestNode     = "request.node"       // Request+Node: closure membership; Data: label, platform
	TypeRequestDone     = "request.done"       // Request+Node(target)
	TypeRequestFailed   = "request.failed"     // Request+Node(target); Data: status, err
	TypeNodeRegistered  = "node.registered"    // Node
	TypeNodeOffered     = "node.offered"       // Node+Runner; Data: claim
	TypeNodeStarted     = "node.started"       // Node+Runner (runner acked)
	TypeNodeDone        = "node.done"          // Node; Data: cached (bool)
	TypeNodeFailed      = "node.failed"        // Node+Runner; Data: class, exit, phase, stderr
	TypeNodeDeclined    = "node.declined"      // Node+Runner; Data: reason, fetcher, platform
	TypeNodeLeaseExp    = "node.lease-expired" // Node+Runner
	TypeNodeNamed       = "node.named"         // Node; Data: name (recipe-declared display name)
	TypeNodeQueued      = "node.queued"        // Node; Data: reason(capacity), cpu_milli, mem_bytes (requirement), runner, free_cpu_milli, free_mem_bytes (best-fit runner), running ([]string, its in-flight jobs). Debounced (~1/min per node).

	TypeRequestCancelled = "request.cancelled" // Request+Node(target)
	TypeNodeCancelled    = "node.cancelled"    // Node; Data: reason (target-cancelled|upstream-failed)
	TypeNodeRevived      = "node.revived"      // Node — a resubmit re-armed a cancelled/retired target; folds clear the node's cancelled verdict (issue #143)

	TypeNodeOfferDropped = "node.offer-dropped" // Node+Runner; Data: claim — offer send failed, placement rolled back (issue #132)
)

// Runner-emitted event types (execution detail).
const (
	TypeExecStarted         = "exec.started"          // Node+Runner; Data: kind, requests
	TypeExecPhase           = "exec.phase"            // Node+Runner; Data: phase
	TypeExecProgress        = "exec.progress"         // Node+Runner; Data: phase, objects, bytes (cumulative for this job; pull path), total (objects reachable; push path only)
	TypeExecOutput          = "exec.output"           // Node+Runner; Data: stream, phase, chunk([]byte), offset
	TypeExecOutputTruncated = "exec.output-truncated" // Node+Runner; Data: stream, cap, written
	TypeExecFinished        = "exec.finished"         // Node+Runner; Data: outcome, exit
	TypeExecCache           = "exec.cache"            // Node+Runner; Data: cache, stage(seeded|finalized), ms, bytes, files, cold|result
	TypeExecHeartbeat       = "exec.heartbeat"        // Node+Runner; Data: phase, cpu_ms?, mem_bytes?, mem_peak_bytes? (job-cgroup usage; keys absent when unreadable)
	TypeExecCAS             = "exec.cas"              // Node+Runner; Data: stored_bytes, stored_objects, stored_deduped (local ingest of the node's outputs), and — only when the batch pushed to the server — pushed_bytes, pushed_objects, push_total_objects
)

// TypeEmitterDropped is the self-reported buffer-overflow gap marker.
const TypeEmitterDropped = "emitter.dropped" // Data: count

var (
	encMode cbor.EncMode
	decMode cbor.DecMode
)

func init() {
	var err error
	encMode, err = cbor.EncOptions{}.EncMode()
	if err != nil {
		panic(err)
	}
	// DefaultMapType keeps Data's nested values JSON-marshalable
	// (map[string]any, never map[interface{}]interface{}).
	decMode, err = cbor.DecOptions{
		DefaultMapType: reflect.TypeOf(map[string]any(nil)),
	}.DecMode()
	if err != nil {
		panic(err)
	}
}

// EncodeBatch encodes one ingest batch.
func EncodeBatch(evs []Event) ([]byte, error) { return encMode.Marshal(evs) }

// DecodeBatch decodes one ingest batch.
func DecodeBatch(b []byte) ([]Event, error) {
	var evs []Event
	err := decMode.Unmarshal(b, &evs)
	return evs, err
}

// Encode/Decode are the single-event forms of the batch codec.
func Encode(ev Event) ([]byte, error) { return encMode.Marshal(ev) }

// Decode decodes one stored event.
func Decode(b []byte) (Event, error) {
	var ev Event
	err := decMode.Unmarshal(b, &ev)
	return ev, err
}
