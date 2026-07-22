package builddef

import (
	"encoding/hex"
	"sort"
)

// CanonicalPinnedInputs returns a stable-ordered, deduplicated copy of in,
// sorted by Name. Names are the recipe-chosen JOBS_DEPS keys and are unique
// within a build, so sorting by Name yields a deterministic order for canonical
// CBOR (a byte-identical re-pin). Returns nil for empty input.
func CanonicalPinnedInputs(in []PinnedInput) []PinnedInput {
	if len(in) == 0 {
		return nil
	}
	out := append([]PinnedInput(nil), in...)
	sort.SliceStable(out, func(a, b int) bool { return out[a].Name < out[b].Name })

	seen := make(map[string]bool, len(out))
	deduped := out[:0]
	for _, pi := range out {
		if seen[pi.Name] {
			continue
		}
		seen[pi.Name] = true
		deduped = append(deduped, pi)
	}
	return deduped
}

// SortKeys returns a stable-ordered, deduplicated copy of ks, sorted by their
// hex representation. Intended for Pinned.RuntimeDeps (32-byte keys). Returns
// nil for empty input.
func SortKeys(ks [][]byte) [][]byte {
	if len(ks) == 0 {
		return nil
	}

	type withKey struct {
		k   []byte
		hex string
	}
	keyed := make([]withKey, len(ks))
	for i, k := range ks {
		keyed[i] = withKey{k: k, hex: hex.EncodeToString(k)}
	}

	idx := make([]int, len(keyed))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		return keyed[idx[a]].hex < keyed[idx[b]].hex
	})

	seen := make(map[string]bool, len(ks))
	out := make([][]byte, 0, len(ks))
	for _, i := range idx {
		h := keyed[i].hex
		if seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, keyed[i].k)
	}
	return out
}
