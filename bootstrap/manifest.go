// Package bootstrap owns the JOBS bootstrap floor: the embedded seed artifacts
// ({tarball+https, shell, hostmusl, github}) that the jobs binary self-seeds
// into amber on startup, and the fetcher manifest — since the
// recipe-declared-fetchers design (2026-07-10) an inventory of those seed
// leaves ONLY. Every other fetcher/plugin is declared at its point of use: a
// recipe's fetcher() value or a plugin artifact's bundled pins. See
// architecture/bootstrap.md and
// docs/superpowers/specs/2026-07-10-recipe-declared-fetchers-design.md.
package bootstrap

import (
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
	jobsiroh "github.com/jobs-build/jobs-iroh"
)

// Source is how a fetcher artifact is obtained. Only "embedded" remains:
// buildable fetchers moved out of the manifest into recipes
// (recipe-declared-fetchers design §8) — a "build" entry is a parse error so a
// stale manifest fails loudly.
type Source string

// SourceEmbedded means the artifact ships in the jobs binary (a seed leaf).
const SourceEmbedded Source = "embedded"

// Entry is one seed leaf in the manifest.
type Entry struct {
	Name     string `toml:"name"`
	Platform string `toml:"platform"` // the embedded artifact's platform
	Source   Source `toml:"source"`
}

// Manifest is the parsed seed inventory.
type Manifest struct {
	Fetchers []Entry `toml:"fetcher"`
}

// LoadBytes parses and validates a manifest from TOML bytes.
func LoadBytes(b []byte) (*Manifest, error) {
	var m Manifest
	if err := toml.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Load reads and parses a manifest file.
func Load(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return LoadBytes(b)
}

// LoadEmbedded parses the manifest embedded in the binary (the repo's
// fetchers.toml at build time).
func LoadEmbedded() (*Manifest, error) {
	return LoadBytes(jobsiroh.FetchersTOML)
}

// Validate reports whether the manifest is well-formed: every entry is an
// embedded seed leaf with a name and platform, and there are no duplicates. A
// "build" (or any non-embedded) source is an error: buildable fetchers are
// recipe-declared, not manifest entries.
func (m *Manifest) Validate() error {
	seen := map[string]bool{}
	for _, e := range m.Fetchers {
		if e.Name == "" {
			return fmt.Errorf("manifest has a fetcher entry with an empty name")
		}
		if e.Source != SourceEmbedded {
			return fmt.Errorf("fetcher %q: source %q is not supported — the manifest lists embedded seed leaves only; declare buildable fetchers in the recipe (fetcher(name, url=, sha256=)) or a plugin's bundled pins", e.Name, e.Source)
		}
		if e.Platform == "" {
			return fmt.Errorf("fetcher %q: embedded entry requires a platform", e.Name)
		}
		dk := e.Name + "\x00" + e.Platform
		if seen[dk] {
			return fmt.Errorf("duplicate fetcher entry %q for platform %q", e.Name, e.Platform)
		}
		seen[dk] = true
	}
	return nil
}
