package builddef

import (
	"fmt"

	"github.com/fables-for-robots/jobs-iroh/importdef"
)

// FetcherBuild synthesizes the standard build definition for a recipe-declared
// fetcher pinned to a source archive: a tarball+https import of {url, sha256}
// with strip:1 (drops the GitHub archive's leading "<repo>-<ref>/" dir so the
// source's BUILD.jobs lands at the build-from root), built with no params at
// `platform`. This is the fetcher(name, url=, sha256=) sugar's definition and
// the plugin-bundle instantiation — one synthesis, shared so keys agree across
// layers (recipe-declared-fetchers design §3, §7).
func FetcherBuild(url, sha256, platform string) (Definition, error) {
	params, err := importdef.CanonicalParams(map[string]any{
		"url":    url,
		"sha256": sha256,
		"strip":  1,
	})
	if err != nil {
		return Definition{}, fmt.Errorf("encode fetcher source params: %w", err)
	}
	imp := importdef.Definition{Fetcher: "tarball+https", Params: params, Platform: platform}
	impCanon, err := imp.Canonical()
	if err != nil {
		return Definition{}, fmt.Errorf("canonical fetcher source import: %w", err)
	}
	// No build params: canonical CBOR null (0xf6), matching the codebase
	// convention for a no-params build — an empty map (0xa0) would derive a
	// different K and break join/dedup with an equivalent submitted build.
	emptyParams, err := importdef.CanonicalParams(nil)
	if err != nil {
		return Definition{}, err
	}
	return Definition{
		Source:   Input{Kind: KindImport, Definition: impCanon},
		Platform: platform,
		Params:   emptyParams,
	}, nil
}
