package recipe

import (
	"fmt"

	"github.com/BurntSushi/toml"
)

// PinsFileName is the pin-bundle file a plugin artifact ships at its root: the
// fetchers its responses emit, pinned name → {url, sha256}
// (recipe-declared-fetchers design §7).
const PinsFileName = "fetchers.toml"

// FetcherPin pins one fetcher's source archive (tarball+https, strip=1 — the
// builddef.FetcherBuild shape).
type FetcherPin struct {
	URL    string
	SHA256 string
}

// FetcherPins is a plugin's bundled pin set, keyed by fetcher name.
type FetcherPins struct {
	Entries map[string]FetcherPin
}

type pinsFile struct {
	Fetcher []struct {
		Name   string `toml:"name"`
		URL    string `toml:"url"`
		SHA256 string `toml:"sha256"`
	} `toml:"fetcher"`
}

// ParseFetcherPins parses a bundled fetchers.toml. Every entry needs name, url
// and sha256; duplicates are an error.
func ParseFetcherPins(b []byte) (*FetcherPins, error) {
	var f pinsFile
	if err := toml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse fetcher pins: %w", err)
	}
	out := &FetcherPins{Entries: make(map[string]FetcherPin, len(f.Fetcher))}
	for _, e := range f.Fetcher {
		if e.Name == "" || e.URL == "" || e.SHA256 == "" {
			return nil, fmt.Errorf("fetcher pin %q: name, url and sha256 are all required", e.Name)
		}
		if _, dup := out.Entries[e.Name]; dup {
			return nil, fmt.Errorf("duplicate fetcher pin %q", e.Name)
		}
		out.Entries[e.Name] = FetcherPin{URL: e.URL, SHA256: e.SHA256}
	}
	return out, nil
}

// PluginSpec pairs a plugin's caller with its bundled fetcher pins (nil when
// the artifact ships none — a plugin that emits only seed fetchers).
type PluginSpec struct {
	Caller PluginCaller
	Pins   *FetcherPins
}
