package serve

// Endpoint export, ported from amber-store-iroh cmd/amber-serve/main.go: the
// relay-mode probe and the announce stack (direct interface addresses, mDNS
// on the local link, pkarr over the internet) that make the server's
// endpoint ID resolvable by amberclient's discovery dial.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"time"

	"github.com/jobs-build/amber-store-iroh/relaymode"
	"github.com/tmc/go-iroh/dns"
	"github.com/tmc/go-iroh/iroh"
	"github.com/tmc/go-iroh/iroh/mdns"
	irohkey "github.com/tmc/go-iroh/key"
	"github.com/tmc/go-iroh/netaddr"
	"github.com/tmc/go-iroh/relay"
)

const (
	// onlineTimeout bounds the wait for the relay connection: go-iroh's
	// Online has no internal timeout and would otherwise wedge startup
	// forever on an offline host, where the server is still perfectly
	// usable over direct addresses and mDNS.
	onlineTimeout = 15 * time.Second
	// mdnsAnnounceInterval paces the mDNS re-announce loop. go-iroh's mdns
	// has no query responder — a resolver is only ever satisfied by an
	// announcement that arrives while it listens — and amberclient's
	// passive lookup waits one second, so announcing at least once per
	// second lands inside every lookup window.
	mdnsAnnounceInterval = time.Second
)

// serverRelayMode picks the relay fallback: an explicit --relay URL wins;
// otherwise the built-in map is reordered to prefer the lowest-latency
// relay (the stock selection can land on a far-away region). Probing is
// bounded and best-effort — on failure the default map is used as-is.
func serverRelayMode(ctx context.Context, flag string, log *slog.Logger) (relay.Mode, error) {
	if flag != "" {
		return relaymode.FromFlag(flag)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	m, err := relay.DefaultMap().PreferNearest(probeCtx, relay.HTTPConnectProber(nil))
	if err != nil {
		log.Warn("relay latency probe failed; using default relay map", "error", err)
		return relay.ModeDefault(), nil
	}
	if urls := m.URLs(); len(urls) > 0 {
		log.Info("preferred relay", "url", urls[0])
	}
	return relay.ModeCustom(m), nil
}

// announcer owns everything announce started: closing it stops the pkarr
// publisher's background re-publish loop and the mDNS listener/re-announce
// goroutines (which must not outlive serve.Run — Run is a library API, so
// an error return is not a process exit).
type announcer struct {
	cancel context.CancelFunc
	pub    io.Closer
}

func (a *announcer) Close() error {
	a.cancel()
	return a.pub.Close()
}

// announce connects the endpoint to its relay (best-effort, bounded) and
// publishes its addresses: direct interface addresses (or the operator's
// advertiseAddrs verbatim) over mDNS on the local link and, together with
// the relay, via pkarr over the internet.
func announce(ctx context.Context, ep *iroh.Endpoint, sk irohkey.SecretKey, advertiseAddrs []string, log *slog.Logger) (io.Closer, error) {
	// Best-effort: an offline or firewalled host still serves direct and
	// mDNS-resolved LAN clients; the published record then simply carries
	// no relay fallback.
	onlineCtx, onlineCancel := context.WithTimeout(ctx, onlineTimeout)
	err := ep.Online(onlineCtx)
	onlineCancel()
	if err != nil {
		log.Warn("relay connect failed; announcing direct addresses only", "error", err)
	}

	// The default wildcard bind address is not a dialable candidate and is
	// dropped from published records, which would leave clients relay-only.
	// Advertise real reachable addresses so peers can dial direct: the
	// operator's --advertise-addr list verbatim, or else the machine's
	// interface addresses on the bound port.
	var direct []netip.AddrPort
	if len(advertiseAddrs) > 0 {
		direct, err = parseAdvertiseAddrs(advertiseAddrs, ep.LocalAddr().Port())
		if err != nil {
			return nil, err
		}
	} else {
		ifaces, err := localIfaceAddrs()
		if err != nil {
			return nil, fmt.Errorf("interface addresses: %w", err)
		}
		direct = advertisedAddrPorts(ifaces, ep.LocalAddr().Port())
	}
	advertised := ep.Addr()
	directTransport := make([]netaddr.TransportAddr, 0, len(direct))
	for _, ap := range direct {
		ep.AddExternalAddr(ap)
		advertised = advertised.WithIP(ap)
		directTransport = append(directTransport, netaddr.IPAddr{Addr: ap})
	}

	ctx, cancel := context.WithCancel(ctx)

	// Advertise the direct addresses on the local link too, so same-LAN
	// clients resolve them even when pkarr is unreachable. Start is the
	// listen loop itself, not a launcher — it blocks until ctx ends, so it
	// gets its own goroutine; Publish is safe before Start. Publishing
	// repeats every second because resolvers are only satisfied by an
	// announcement arriving during their lookup window (see
	// mdnsAnnounceInterval); one packet per second is nothing on a LAN.
	disc := mdns.New(ep.ID())
	mdnsData := dns.NewEndpointData(directTransport...)
	disc.Publish(mdnsData)
	go func() {
		if err := disc.Start(ctx); err != nil && ctx.Err() == nil {
			log.Warn("mdns listener stopped", "error", err)
		}
	}()
	go func() {
		t := time.NewTicker(mdnsAnnounceInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				disc.Publish(mdnsData)
			}
		}
	}()

	// Publish the relay and direct addresses so clients can resolve the
	// endpoint ID over the internet; re-published in the background every
	// 5 minutes.
	pub, err := iroh.N0PkarrPublisher(sk, &iroh.PkarrPublisherConfig{
		AddrFilter: publishableAddrs,
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("pkarr publisher: %w", err)
	}
	pub.Publish(dns.NewEndpointData(advertised.Addrs()...))

	log.Info("server advertised", "addr", advertised)
	return &announcer{cancel: cancel, pub: pub}, nil
}
