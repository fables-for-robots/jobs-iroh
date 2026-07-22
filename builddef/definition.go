// Package builddef defines the JOBS build job definition and the unifying Input
// type, with their canonical CBOR encoding. The amber file key of a canonical
// definition is the job identity K (architecture/build.md §2, §4). An Input
// embeds the COMPLETE inner definition (import or build), so a build's K
// content-addresses its entire transitive input set.
package builddef

import (
	"reflect"

	"github.com/fables-for-robots/amber-store-core/key"
	"github.com/fables-for-robots/jobs-iroh/amber"
	"github.com/fxamacker/cbor/v2"
)

// Input kinds (build.md §4).
const (
	KindImport = "import"
	KindBuild  = "build"
)

// canonEnc encodes deterministically (sorted map keys, shortest ints) so equal
// values produce equal bytes — the property that makes K a content address.
var canonEnc = func() cbor.EncMode {
	m, err := cbor.CanonicalEncOptions().EncMode()
	if err != nil {
		panic(err)
	}
	return m
}()

// stringMapDec decodes CBOR maps with string keys so params/specs surface to
// Starlark and JSON as map[string]any.
var stringMapDec = func() cbor.DecMode {
	m, err := cbor.DecOptions{DefaultMapType: reflect.TypeOf(map[string]any(nil))}.DecMode()
	if err != nil {
		panic(err)
	}
	return m
}()

// Input is a complete import or build definition, tagged by kind (build.md §4).
// Definition holds the canonical CBOR of the inner import-def or build-def.
type Input struct {
	Kind       string          `cbor:"kind"`
	Definition cbor.RawMessage `cbor:"definition"`
}

// Key derives the input's identity K = amber.FileKey(definition) (build.md §4).
func (in Input) Key() (key.Key, error) {
	return amber.FileKey(in.Definition)
}

// Definition is the build job definition (build.md §2; superseded by the
// build-from design §5). Its canonical CBOR is content-hashed to produce the
// platform-SPECIFIC job identity K.
type Definition struct {
	Source    Input           `cbor:"source"`
	Dir       string          `cbor:"dir,omitempty"`
	Platform  string          `cbor:"platform"`
	Params    cbor.RawMessage `cbor:"params"`
	BuildJobs []byte          `cbor:"buildJobs,omitempty"` // optional override recipe (design §6)
	BuildFile string          `cbor:"buildFile,omitempty"` // optional recipe path relative to Dir (default BUILD.jobs)
}

// Canonical returns the canonical CBOR of the definition. BuildJobs is an
// optional override recipe and BuildFile an optional alternative recipe path
// (relative to Dir); both are omitted when empty (omitempty) so a build that
// uses neither encodes identically to before these fields existed.
func (d Definition) Canonical() ([]byte, error) {
	out := Definition{
		Source:    d.Source,
		Dir:       d.Dir,
		Platform:  d.Platform,
		Params:    d.Params,
		BuildJobs: d.BuildJobs,
		BuildFile: d.BuildFile,
	}
	return canonEnc.Marshal(out)
}

// Key derives the build's identity K from its canonical CBOR (build.md §2).
func (d Definition) Key() (key.Key, error) {
	canon, err := d.Canonical()
	if err != nil {
		return key.Key{}, err
	}
	return amber.FileKey(canon)
}

// DecodeDefinition parses canonical CBOR back into a Definition.
func DecodeDefinition(b []byte) (Definition, error) {
	var d Definition
	if err := cbor.Unmarshal(b, &d); err != nil {
		return Definition{}, err
	}
	return d, nil
}

// ParamsValue decodes the params CBOR to a string-keyed Go value, for surfacing
// to the recipe as the `params` Starlark global (build.md §3.1).
func (d Definition) ParamsValue() (any, error) {
	if len(d.Params) == 0 {
		return nil, nil
	}
	var v any
	if err := stringMapDec.Unmarshal(d.Params, &v); err != nil {
		return nil, err
	}
	return v, nil
}
