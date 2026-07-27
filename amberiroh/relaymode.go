package amberiroh

import (
	"fmt"

	"github.com/tmc/go-iroh/netaddr"
	"github.com/tmc/go-iroh/relay"
)

// FromFlag returns the relay mode for a --relay flag value: the default
// relay map when the flag is empty, otherwise a custom map containing
// exactly the given relay URL.
func FromFlag(url string) (relay.Mode, error) {
	if url == "" {
		return relay.ModeDefault(), nil
	}
	u, err := netaddr.ParseRelayURL(url)
	if err != nil {
		return relay.Mode{}, fmt.Errorf("parse --relay %q: %w", url, err)
	}
	return relay.ModeCustom(relay.MapFromURLs(u)), nil
}
