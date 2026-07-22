// Package importdef defines the JOBS import job definition and its canonical
// encoding. The amber key of a definition's canonical CBOR is the job identity K
// (architecture/import.md §2).
package importdef

import (
	"encoding/json"
	"reflect"
	"sort"

	"github.com/fxamacker/cbor/v2"
)

// Definition is the import job definition. Its canonical CBOR is content-hashed
// by amber to produce the job identity K. Params holds the fetcher parameters as
// canonical CBOR (opaque to JOBS; transcoded to JSON for the fetcher).
type Definition struct {
	Fetcher      string          `cbor:"fetcher"`
	Params       cbor.RawMessage `cbor:"params"`
	RequiredTags []string        `cbor:"requiredTags,omitempty"`
	// Platform pins fetcher resolution to fetcher:<name>:<platform> regardless of
	// which runner runs the import, and is part of identity K. Since the
	// import-platform-pinning design (2026-07-09) every NEWLY created definition
	// carries the consuming build's platform — a fetcher's output can never depend
	// on which runner ran the import. Empty is a LEGACY-READ state only (defs
	// frozen inside pre-change Pinned trees): such an import resolves the fetcher
	// for the running runner's platform and places anywhere. omitempty keeps those
	// frozen defs' identity K unchanged.
	Platform string `cbor:"platform,omitempty"`
	// FetcherDef, when present, is the canonical CBOR of the builddef.Definition
	// that builds this import's fetcher (kind is implicitly "build"). It is part
	// of identity K — the fetcher's code is an input to the fetch, so two imports
	// fetching with different fetcher code are different jobs
	// (docs/superpowers/specs/2026-07-10-recipe-declared-fetchers-design.md).
	// Empty means a NAMED fetcher resolved via fetcher:<name>:<platform> (seed
	// leaves; and legacy refs frozen in pre-change trees). omitempty keeps every
	// existing definition's K unchanged.
	FetcherDef cbor.RawMessage `cbor:"fetcherDef,omitempty"`
}

// canonEnc encodes deterministically: sorted map keys / struct fields, shortest
// integer forms — so logically-identical values produce identical bytes.
var canonEnc = func() cbor.EncMode {
	m, err := cbor.CanonicalEncOptions().EncMode()
	if err != nil {
		panic(err)
	}
	return m
}()

// stringMapDec decodes CBOR maps with string keys so params transcode to JSON.
var stringMapDec = func() cbor.DecMode {
	m, err := cbor.DecOptions{DefaultMapType: reflect.TypeOf(map[string]any(nil))}.DecMode()
	if err != nil {
		panic(err)
	}
	return m
}()

// CanonicalParams encodes an arbitrary params value as canonical CBOR.
func CanonicalParams(v any) (cbor.RawMessage, error) {
	b, err := canonEnc.Marshal(v)
	if err != nil {
		return nil, err
	}
	return cbor.RawMessage(b), nil
}

// Canonical returns the canonical CBOR of the definition. RequiredTags are
// sorted and de-duplicated first so equal tag sets yield equal bytes; an empty
// set is omitted entirely.
func (d Definition) Canonical() ([]byte, error) {
	out := Definition{
		Fetcher:      d.Fetcher,
		Params:       d.Params,
		RequiredTags: canonTags(d.RequiredTags),
		Platform:     d.Platform,
		FetcherDef:   d.FetcherDef,
	}
	return canonEnc.Marshal(out)
}

// Decode parses canonical CBOR back into a Definition.
func Decode(b []byte) (Definition, error) {
	var d Definition
	if err := cbor.Unmarshal(b, &d); err != nil {
		return Definition{}, err
	}
	return d, nil
}

// ParamsJSON renders the params as JSON for the JOBS_FETCH_PARAMS env var.
func (d Definition) ParamsJSON() ([]byte, error) {
	if len(d.Params) == 0 {
		return []byte("null"), nil
	}
	var v any
	if err := stringMapDec.Unmarshal(d.Params, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

func canonTags(tags []string) []string {
	if len(tags) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(tags))
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
