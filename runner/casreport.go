package runner

import (
	"github.com/fables-for-robots/jobs-iroh/amber"
	"github.com/fables-for-robots/jobs-iroh/events"
)

// emitCAS reports the node attempt's CAS summary (issue #95): what the output
// ingest newly stored into the local store and what the WriteRefs batch's
// server push actually uploaded vs found already present (always nothing in
// local mode — PushTotals.Reported is false there). Observability only;
// nil-Job-safe.
func emitCAS(ev *events.Job, ing amber.IngestStats, pt PushTotals) {
	ev.CAS(int64(ing.BytesStored), int(ing.ObjectsStored), int(ing.ObjectsDeduped),
		pt.Reported, pt.BytesPushed, pt.ObjectsPushed, pt.ObjectsTotal)
}
