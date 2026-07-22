// Package jobsiroh holds repo-root embeds. go:embed can only reference files
// in the embedding package's own directory, so the checked-in fetchers.toml is
// embedded here and consumed via bootstrap.LoadEmbedded.
package jobsiroh

import _ "embed"

// FetchersTOML is the checked-in fetcher manifest (fetchers.toml), embedded so
// every binary carries the fetcher pins it was built with and a standalone
// binary can provision fetchers from any working directory.
//
//go:embed fetchers.toml
var FetchersTOML []byte
