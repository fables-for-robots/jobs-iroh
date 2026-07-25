package builddef

import (
	"fmt"
	"strings"

	"github.com/fxamacker/cbor/v2"
)

// PluginResolved is the build-plugin-resolved:F ref content: the resolved
// plugins by name (build-from design §7) plus the recipe-declared resolution
// deps (resolution-deps design §4) — inputs materialized for the pin stage.
// Self-contained — each value embeds a complete import/build definition
// (build.md §4). The build definition is no longer carried here: the pin stage
// re-reads everything from the F-tree. omitempty keeps a dep-less resolve
// byte-identical to the pre-deps encoding.
type PluginResolved struct {
	Plugins map[string]Input `cbor:"plugins"`
	Deps    map[string]Input `cbor:"deps,omitempty"`
}

// ValidateDepName enforces that a resolution-dep name is a safe single path
// segment (it becomes /jobs/deps/<name> in the plugin sandbox): non-empty,
// bounded (a name over NAME_MAX would make the materialize mkdir fail forever),
// no separator or control bytes, not a dot dir (resolution-deps design §3.1).
func ValidateDepName(name string) error {
	if name == "" {
		return fmt.Errorf("resolution dep name must be non-empty")
	}
	if len(name) > 128 {
		return fmt.Errorf("resolution dep name %q exceeds 128 bytes", name[:32]+"…")
	}
	if strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("resolution dep name %q must not contain path separators", name)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("resolution dep name %q contains control characters", name)
		}
	}
	if name == "." || name == ".." {
		return fmt.Errorf("resolution dep name %q is reserved", name)
	}
	return nil
}

func EncodePluginResolved(p PluginResolved) ([]byte, error) { return canonEnc.Marshal(p) }

func DecodePluginResolved(b []byte) (PluginResolved, error) {
	var p PluginResolved
	err := cbor.Unmarshal(b, &p)
	return p, err
}

// PinnedInput is one input of a pinned build: a complete input definition plus
// the recipe-chosen name under which the build script finds it in JOBS_DEPS
// (build.md §7). The mount path is no longer pin-time-derivable — it is the
// dependency's content key (BOK), known only after the dep is built — so the
// runner injects name→/jobs/store/<BOK> at build time instead.
type PinnedInput struct {
	Name       string          `cbor:"name"`
	Kind       string          `cbor:"kind"`
	Definition cbor.RawMessage `cbor:"definition"`
}

// Pinned is the build-pinned:K ref content — the resolved job description
// (build.md §7). RuntimeDeps holds the 32-byte K of each runtime dependency, a
// subset of Inputs. Caches are the build's persistent cache declarations
// (build-cache design §4); omitempty keeps a cache-less Pinned byte-identical
// to the pre-cache encoding.
//
// Sources/Dir/Generated carry the covered source closure of a widened-context
// build (sibling-sources design §3.3): Sources is the sorted list of
// root-relative covered paths, Dir the build dir within the context (sandbox
// CWD), Generated the pin-synthesized files overlaid onto the covered tree
// (root-relative path → content, ≤ GeneratedMaxBytes total). All omitempty so
// legacy pins stay byte-identical.
type Pinned struct {
	Inputs      []PinnedInput     `cbor:"inputs"`
	Env         map[string]string `cbor:"env"`
	Script      string            `cbor:"script"`
	RuntimeDeps [][]byte          `cbor:"runtimeDeps"`
	Caches      []PinnedCache     `cbor:"caches,omitempty"`
	Resources   *PinnedResources  `cbor:"resources,omitempty"`
	Sources     []string          `cbor:"sources,omitempty"`
	Dir         string            `cbor:"dir,omitempty"`
	Generated   map[string][]byte `cbor:"generated,omitempty"`
}

// GeneratedMaxBytes caps the total inline size of Pinned.Generated (the
// contract of the sibling-sources arc, design §7): pin hard-fails above it.
const GeneratedMaxBytes = 1 << 20

// PinnedResources is the recipe-declared CPU/RAM requirement carried in Pinned
// (multi-job-runner design). Deterministic from F (it rides build-pinned:F).
// Note: since the sibling-sources arc, the pinned bytes are part of KP — a
// resource change re-keys the buildrun memo (it always re-keyed F via the
// recipe text, so this is no behavioral change). A nil pointer (the common
// case) encodes to nothing, keeping resource-less pins byte-identical to the
// pre-resources encoding.
type PinnedResources struct {
	CPUMilli int64 `cbor:"cpu_milli,omitempty"`
	MemBytes int64 `cbor:"mem_bytes,omitempty"`
}

func EncodePinned(p Pinned) ([]byte, error) { return canonEnc.Marshal(p) }

func DecodePinned(b []byte) (Pinned, error) {
	var p Pinned
	err := cbor.Unmarshal(b, &p)
	return p, err
}
