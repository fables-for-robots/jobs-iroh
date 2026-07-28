# Punched Shard Endpoints Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Sharded store transfers hole-punch through NAT to the server's dedicated data endpoints, so a NAT'd server's transfers spread across all its sockets with zero router configuration.

**Architecture:** Each server data endpoint gets its own iroh identity plus relay home connection and QUIC address discovery, advertised in-band via a new `DataEndpoints` field on `TAccept`/`TRef` (also the capability signal for a 10s gather window). Client shard dials bind with the control dial's full stack (relay, net report, seeded candidates) and race the dedicated port together with the advertised candidates, authenticating the data endpoint's ID; QNT punches the winning relay path to direct mid-transfer. Extras are gated on the control path being direct.

**Tech Stack:** Go, go-iroh (draganm fork), fxamacker/cbor. Spec: `docs/design/2026-07-28-punched-shard-endpoints.md`.

## Global Constraints

- Build/test/vet through the Nix devShell: `nix develop -c go test ./…` etc.; `GOPRIVATE=github.com/jobs-build/*` comes from `.envrc`.
- No go-iroh fork changes — existing public API only (`iroh.Bind` options, `Endpoint.Addr()`, `Endpoint.Online`, `netaddr.ParseTransportAddr`, `key.EndpointIDFromSlice`).
- No ALPN bump: every old/new pairing must stay correct (at worst single-connection slow).
- Wire field numbers are frozen once shipped: the new Msg field is `16,keyasint,omitempty`, `DataEndpointRec` uses `0`/`1`.
- Commit after every task, message style matches repo history (imperative, no conventional-commit prefixes), with the standard trailers:
  `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>` and `Claude-Session: https://claude.ai/code/session_019UrT7JpW67wgpy5MzzvijD`.
- After touching any `_linux.go`/`_other.go` pair (none expected): darwin cross-vet. Run it once at the end regardless (Task 7).

---

### Task 1: Wire — `DataEndpointRec` + `Msg.DataEndpoints`

**Files:**
- Modify: `amberiroh/protocol.go` (Msg struct ~line 78-95; new type below RefInfo ~line 76)
- Test: `amberiroh/protocol_test.go` (package `amberiroh`, internal — `encMode`/`decMode` are accessible)

**Interfaces:**
- Produces: `type DataEndpointRec struct { ID []byte; Addrs []string }` and `Msg.DataEndpoints []DataEndpointRec` — used by Tasks 2, 3, 4, 6.

- [ ] **Step 1: Write the failing tests**

Append to `amberiroh/protocol_test.go` (add imports `bytes`, `reflect` if absent):

```go
func TestMsgDataEndpointsRoundTrip(t *testing.T) {
	in := Msg{Type: TAccept, Token: []byte{1}, DataPorts: []uint16{4001, 4002},
		DataEndpoints: []DataEndpointRec{
			{ID: bytes.Repeat([]byte{7}, 32), Addrs: []string{"ip:192.168.1.5:4001", "relay:https://euc1-1.relay.example./"}},
			{ID: bytes.Repeat([]byte{8}, 32), Addrs: []string{"ip:192.168.1.5:4002"}},
		}}
	var buf bytes.Buffer
	if err := WriteMsg(&buf, in); err != nil {
		t.Fatal(err)
	}
	out, err := ReadMsg(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip mismatch:\n in %+v\nout %+v", in, out)
	}
}

// Old peers must interoperate across field 16: an old decoder ignores it on
// new frames, and a new decoder yields nil for its absence on old frames.
func TestMsgDataEndpointsCompat(t *testing.T) {
	type oldMsg struct {
		Type      int      `cbor:"0,keyasint"`
		Token     []byte   `cbor:"13,keyasint,omitempty"`
		DataPorts []uint16 `cbor:"15,keyasint,omitempty"`
	}
	in := Msg{Type: TAccept, Token: []byte{1}, DataPorts: []uint16{4001},
		DataEndpoints: []DataEndpointRec{{ID: bytes.Repeat([]byte{7}, 32), Addrs: []string{"ip:127.0.0.1:4001"}}}}
	payload, err := encMode.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var old oldMsg
	if err := decMode.Unmarshal(payload, &old); err != nil {
		t.Fatalf("old decoder rejects new frame: %v", err)
	}
	if old.Type != TAccept || len(old.DataPorts) != 1 || len(old.Token) != 1 {
		t.Fatalf("old decode mangled fields: %+v", old)
	}
	oldPayload, err := encMode.Marshal(oldMsg{Type: TAccept, Token: []byte{1}, DataPorts: []uint16{4001}})
	if err != nil {
		t.Fatal(err)
	}
	var m Msg
	if err := decMode.Unmarshal(oldPayload, &m); err != nil {
		t.Fatal(err)
	}
	if m.DataEndpoints != nil {
		t.Fatalf("absent field decoded non-nil: %+v", m.DataEndpoints)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `nix develop -c go test ./amberiroh/ -run TestMsgDataEndpoints -v`
Expected: compile error — `undefined: DataEndpointRec`.

- [ ] **Step 3: Implement**

In `amberiroh/protocol.go`, below the `RefInfo` type:

```go
// DataEndpointRec describes one of the server's data endpoints to a
// sharding client: the endpoint's own identity (data endpoints carry their
// own keys so each can hold a relay home connection — relays key sessions
// by endpoint ID) and its dial candidates as netaddr.TransportAddr strings
// ("ip:host:port", "relay:url"). The client trusts the identity because the
// record arrives on the control connection, which authenticated the server;
// the shard handshake then proves possession of the advertised key. Old
// peers ignore the field and keep direct-dialing DataPorts.
type DataEndpointRec struct {
	ID    []byte   `cbor:"0,keyasint"`
	Addrs []string `cbor:"1,keyasint,omitempty"`
}
```

In `Msg`, after `DataPorts`:

```go
	// DataEndpoints describes the data endpoints as punchable peers on
	// TAccept/TRef. Its presence doubles as the capability signal that the
	// server gathers attaches for the longer punch-friendly window.
	DataEndpoints []DataEndpointRec `cbor:"16,keyasint,omitempty"`
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `nix develop -c go test ./amberiroh/ -v`
Expected: all PASS (including pre-existing tests).

- [ ] **Step 5: Commit**

```bash
git add amberiroh/protocol.go amberiroh/protocol_test.go
git commit -m "amberiroh: DataEndpoints wire field for punchable data endpoints"
```

---

### Task 2: Server — advertise records, widen the gather window

**Files:**
- Modify: `amberiroh/server.go` (struct ~line 22-41, `New` ~line 132, `SetDataPorts` ~line 135-137, `shardChannels` TAccept ~line 314, `handlePull` TRef ~line 376-381)
- Test: `amberiroh/server_test.go`

**Interfaces:**
- Consumes: `DataEndpointRec` (Task 1).
- Produces: `func (s *Server) SetDataEndpoints(f func() []DataEndpointRec)` — called by Task 6. TAccept/TRef frames now carry `DataEndpoints` whenever the closure is set.

- [ ] **Step 1: Write the failing tests**

Append to `amberiroh/server_test.go` (imports: `bytes`, `io`, `log/slog`, `net`, `reflect`, `time` as needed):

```go
func TestAttachWaitDefaultCoversPunching(t *testing.T) {
	s := New(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	if s.attachWait != 10*time.Second {
		t.Fatalf("attachWait %v, want 10s (punching attaches ride the relay first)", s.attachWait)
	}
}

func TestShardChannelsAdvertisesDataEndpoints(t *testing.T) {
	s := New(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	s.attachWait = 50 * time.Millisecond
	s.SetDataPorts([]uint16{4001})
	rec := DataEndpointRec{ID: bytes.Repeat([]byte{7}, 32), Addrs: []string{"ip:127.0.0.1:4001"}}
	s.SetDataEndpoints(func() []DataEndpointRec { return []DataEndpointRec{rec} })

	cli, srv := net.Pipe()
	got := make(chan Msg, 1)
	go func() {
		m, err := ReadMsg(cli)
		if err != nil {
			t.Error(err)
		}
		got <- m
		cli.Close()
	}()
	channels, release, err := s.shardChannels(srv, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if len(channels) != 1 {
		t.Fatalf("gathered %d channels, want control only", len(channels))
	}
	m := <-got
	if m.Type != TAccept {
		t.Fatalf("type %d, want TAccept", m.Type)
	}
	if !reflect.DeepEqual(m.DataEndpoints, []DataEndpointRec{rec}) {
		t.Fatalf("DataEndpoints %+v, want %+v", m.DataEndpoints, rec)
	}
	if !reflect.DeepEqual(m.DataPorts, []uint16{4001}) {
		t.Fatalf("DataPorts %+v", m.DataPorts)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `nix develop -c go test ./amberiroh/ -run 'TestAttachWait|TestShardChannels' -v`
Expected: compile error — `s.SetDataEndpoints undefined`; after stubbing, attachWait 5s ≠ 10s.

- [ ] **Step 3: Implement**

In `amberiroh/server.go`:

1. Struct, after `dataPorts`:

```go
	// dataEndpoints, when set, snapshots the data endpoints' identities and
	// live dial candidates for TAccept/TRef — a closure because relay and
	// QAD candidates appear asynchronously after bind.
	dataEndpoints func() []DataEndpointRec
```

2. `New` (~line 132): change `attachWait: 5 * time.Second` to `attachWait: 10 * time.Second`, and extend the comment on the `attachWait` field (~line 28-31) with: `10s covers a punching attach — relay connect then TAttach — while gather's early exit keeps fast attaches free.`

3. Below `SetDataPorts`:

```go
// SetDataEndpoints installs the data-endpoint snapshot advertised to
// sharding clients; call before Serve, like SetDataPorts.
func (s *Server) SetDataEndpoints(f func() []DataEndpointRec) { s.dataEndpoints = f }
```

4. `shardChannels` (~line 314), replace the TAccept write:

```go
	accept := Msg{Type: TAccept, Token: token, DataPorts: s.dataPorts}
	if s.dataEndpoints != nil {
		accept.DataEndpoints = s.dataEndpoints()
	}
	if err := WriteMsg(rw, accept); err != nil {
```

5. `handlePull` (~line 376-381), inside `if m.DataConns > 0 {`, after `ref.DataPorts = s.dataPorts`:

```go
		if s.dataEndpoints != nil {
			ref.DataEndpoints = s.dataEndpoints()
		}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `nix develop -c go test ./amberiroh/ -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add amberiroh/server.go amberiroh/server_test.go
git commit -m "amberiroh: advertise data endpoints on TAccept/TRef, 10s gather window"
```

---

### Task 3: Client — `shardTarget` candidate assembly (pure helper)

**Files:**
- Create: `amberclient/shardtarget.go`
- Test: `amberclient/shardtarget_internal_test.go` (package `amberclient`)

**Interfaces:**
- Consumes: `amberiroh.DataEndpointRec` (Task 1).
- Produces: `func shardTarget(i int, ctrlIP netip.Addr, haveCtrlIP bool, ports []uint16, eps []amberiroh.DataEndpointRec, bare bool) (irohkey.EndpointID, []netaddr.TransportAddr, bool, error)` — used by Task 4. `ok=false, err=nil` means "no records: run the legacy path".

- [ ] **Step 1: Write the failing tests**

Create `amberclient/shardtarget_internal_test.go`:

```go
package amberclient

import (
	"bytes"
	"net/netip"
	"testing"

	"github.com/jobs-build/jobs-iroh/amberiroh"
	"github.com/tmc/go-iroh/netaddr"
)

func recWith(idByte byte, addrs ...string) amberiroh.DataEndpointRec {
	return amberiroh.DataEndpointRec{ID: bytes.Repeat([]byte{idByte}, 32), Addrs: addrs}
}

func addrStrings(cands []netaddr.TransportAddr) []string {
	out := make([]string, len(cands))
	for i, c := range cands {
		out[i] = c.String()
	}
	return out
}

func TestShardTargetNoRecordsMeansLegacy(t *testing.T) {
	_, _, ok, err := shardTarget(0, netip.Addr{}, false, []uint16{4001}, nil, true)
	if ok || err != nil {
		t.Fatalf("ok=%v err=%v, want legacy fallthrough", ok, err)
	}
}

func TestShardTargetUnionsAndDedupes(t *testing.T) {
	ctrl := netip.MustParseAddr("192.0.2.1")
	eps := []amberiroh.DataEndpointRec{recWith(7, "ip:192.0.2.1:4001", "ip:10.0.0.5:4001", "relay:https://euc1-1.relay.example./")}
	id, cands, ok, err := shardTarget(0, ctrl, true, []uint16{4001}, eps, false)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if want, _ := irohkeyFromBytes(eps[0].ID); id != want {
		t.Fatalf("id %s, want record's", id)
	}
	got := addrStrings(cands)
	want := []string{"ip:192.0.2.1:4001", "ip:10.0.0.5:4001", "relay:https://euc1-1.relay.example./"}
	if len(got) != len(want) {
		t.Fatalf("candidates %v, want %v (dedup of the dedicated addr)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidates %v, want %v", got, want)
		}
	}
}

func TestShardTargetBareFiltersRelay(t *testing.T) {
	eps := []amberiroh.DataEndpointRec{recWith(7, "relay:https://euc1-1.relay.example./", "ip:10.0.0.5:4001")}
	_, cands, ok, err := shardTarget(0, netip.Addr{}, false, nil, eps, true)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	got := addrStrings(cands)
	if len(got) != 1 || got[0] != "ip:10.0.0.5:4001" {
		t.Fatalf("bare candidates %v, want the ip candidate only", got)
	}
}

func TestShardTargetCyclesRecords(t *testing.T) {
	eps := []amberiroh.DataEndpointRec{recWith(7, "ip:10.0.0.5:4001"), recWith(8, "ip:10.0.0.5:4002")}
	id, _, ok, err := shardTarget(3, netip.Addr{}, false, nil, eps, false)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if want, _ := irohkeyFromBytes(eps[1].ID); id != want {
		t.Fatalf("shard 3 of 2 records: id %s, want eps[1]'s", id)
	}
}

func TestShardTargetSkipsMalformedAddrs(t *testing.T) {
	eps := []amberiroh.DataEndpointRec{recWith(7, "not-a-transport-addr", "ip:10.0.0.5:4001")}
	_, cands, ok, err := shardTarget(0, netip.Addr{}, false, nil, eps, false)
	if err != nil || !ok || len(cands) != 1 {
		t.Fatalf("ok=%v err=%v cands=%v", ok, err, cands)
	}
}

func TestShardTargetAllFilteredIsError(t *testing.T) {
	eps := []amberiroh.DataEndpointRec{recWith(7, "relay:https://euc1-1.relay.example./")}
	_, _, _, err := shardTarget(0, netip.Addr{}, false, nil, eps, true)
	if err == nil {
		t.Fatal("no dialable candidates must error, not dial nothing")
	}
}

func TestShardTargetBadIDIsError(t *testing.T) {
	eps := []amberiroh.DataEndpointRec{{ID: []byte{1, 2, 3}, Addrs: []string{"ip:10.0.0.5:4001"}}}
	_, _, _, err := shardTarget(0, netip.Addr{}, false, nil, eps, false)
	if err == nil {
		t.Fatal("truncated ID must error")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `nix develop -c go test ./amberclient/ -run TestShardTarget -v`
Expected: compile error — `undefined: shardTarget` (and `irohkeyFromBytes`).

- [ ] **Step 3: Implement**

Create `amberclient/shardtarget.go`:

```go
package amberclient

import (
	"fmt"
	"net/netip"

	"github.com/jobs-build/jobs-iroh/amberiroh"
	irohkey "github.com/tmc/go-iroh/key"
	"github.com/tmc/go-iroh/netaddr"
)

// irohkeyFromBytes parses a wire-carried 32-byte endpoint ID.
func irohkeyFromBytes(b []byte) (irohkey.EndpointID, error) {
	return irohkey.EndpointIDFromSlice(b)
}

// shardTarget picks shard i's dial target from a TAccept/TRef. With
// DataEndpoints records the shard authenticates the record's own identity
// and races the dedicated port (on the address the control connection
// reached — spreading load across the server's sockets, as before) together
// with every advertised candidate; ok=false with a nil error means no
// records — the caller runs the legacy path. bare drops relay candidates:
// an endpoint bound without relays can never dial them. A malformed
// candidate is skipped (the rest still race); no dialable candidate at all
// is an error.
func shardTarget(i int, ctrlIP netip.Addr, haveCtrlIP bool, ports []uint16, eps []amberiroh.DataEndpointRec, bare bool) (irohkey.EndpointID, []netaddr.TransportAddr, bool, error) {
	if len(eps) == 0 {
		return irohkey.EndpointID{}, nil, false, nil
	}
	rec := eps[i%len(eps)]
	id, err := irohkeyFromBytes(rec.ID)
	if err != nil {
		return irohkey.EndpointID{}, nil, false, fmt.Errorf("data endpoint %d: bad id: %w", i%len(eps), err)
	}
	seen := make(map[string]bool, len(rec.Addrs)+1)
	var cands []netaddr.TransportAddr
	add := func(ta netaddr.TransportAddr) {
		if s := ta.String(); !seen[s] {
			seen[s] = true
			cands = append(cands, ta)
		}
	}
	if haveCtrlIP && len(ports) > 0 {
		add(netaddr.IPAddr{Addr: netip.AddrPortFrom(ctrlIP, ports[i%len(ports)])})
	}
	for _, s := range rec.Addrs {
		ta, err := netaddr.ParseTransportAddr(s)
		if err != nil {
			continue
		}
		if _, isRelay := ta.(netaddr.RelayAddr); isRelay && bare {
			continue
		}
		add(ta)
	}
	if len(cands) == 0 {
		return irohkey.EndpointID{}, nil, false, fmt.Errorf("data endpoint %d: no dialable candidates", i%len(eps))
	}
	return id, cands, true, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `nix develop -c go test ./amberclient/ -run TestShardTarget -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add amberclient/shardtarget.go amberclient/shardtarget_internal_test.go
git commit -m "amberclient: shardTarget assembles punch dial candidates per shard"
```

---

### Task 4: Client — punch-capable shard dials

**Files:**
- Modify: `amberclient/shard.go` (consts ~line 38-46, `extraConn` ~line 54-82, `attachExtras` ~line 107-121, `runSenders` ~line 236)
- Modify: `amberclient/client.go` (Client struct ~line 108-127, `Dial` ~line 192-196, `pullOnce` ~line 365)

**Interfaces:**
- Consumes: `shardTarget` (Task 3), `Msg.DataEndpoints` (Task 1), `seedLocalCandidates` (`amberclient/dial.go:135`), `raceConnect` (`amberclient/dial.go:181`).
- Produces: `extraConn(ctx, i, ports, eps)` and `attachExtras(ctx, token, ports, eps, n)` signatures; `Client.punchDials bool`. The existing e2e suite (`amberclient/sharded_test.go`) exercises the record-authenticated dial on loopback once Task 6 lands.

- [ ] **Step 1: Update the shard machinery**

In `amberclient/shard.go`:

1. Add to the const block (~line 38-46):

```go
	// punchAttachBudget replaces attachBudget when the server advertised
	// DataEndpoints (which is also the signal that it gathers for 10s): a
	// punching attach pays a relay connect before its TAttach, and the
	// budget must still land safely inside the server's window.
	punchAttachBudget = 8 * time.Second
```

2. Replace `extraConn` with:

```go
// extraConn opens one more connection for shard i. A server that advertised
// DataEndpoints records gets a record-authenticated dial: the dedicated
// port (on the address the control connection reached) races the record's
// own candidates. On a discovery-dialed client the shard endpoint binds
// with the control dial's full stack — relay mode, net report, seeded local
// candidates — which is what feeds QNT punch coordination, so a relay-won
// shard upgrades to direct mid-transfer exactly like the control connection
// did. A direct-addr client (Options.Addrs) stays bare, mirroring the
// control dial's own direct branch. Without records, the legacy path runs:
// dedicated ports under the server identity, then the control candidates.
func (c *Client) extraConn(ctx context.Context, i int, ports []uint16, eps []amberiroh.DataEndpointRec) (*iroh.Conn, *iroh.Endpoint, error) {
	ctrlIP, haveCtrlIP := c.remoteIP()
	id, cands, ok, terr := shardTarget(i, ctrlIP, haveCtrlIP, ports, eps, !c.punchDials)
	if terr != nil {
		return nil, nil, terr
	}
	if ok {
		ep, err := c.bindShardEndpoint(ctx)
		if err != nil {
			return nil, nil, err
		}
		conn, err := raceConnect(ctx, ep, id, c.alpn, cands)
		if err != nil {
			_ = ep.Shutdown(context.WithoutCancel(ctx))
			return nil, nil, err
		}
		return conn, ep, nil
	}

	var bindOpts []iroh.Option
	if c.bindAddr.IsValid() {
		// Same host as the pinned bind, port 0: every shard needs its own
		// socket.
		bindOpts = append(bindOpts, iroh.WithBindAddr(netip.AddrPortFrom(c.bindAddr.Addr(), 0)))
	}
	ep, err := iroh.Bind(ctx, bindOpts...)
	if err != nil {
		return nil, nil, err
	}
	if len(ports) > 0 && haveCtrlIP {
		dctx, cancel := context.WithTimeout(ctx, dedicatedDialTimeout)
		cand := []netaddr.TransportAddr{netaddr.IPAddr{Addr: netip.AddrPortFrom(ctrlIP, ports[i%len(ports)])}}
		conn, derr := raceConnect(dctx, ep, c.id, c.alpn, cand)
		cancel()
		if derr == nil {
			return conn, ep, nil
		}
	}
	conn, err := raceConnect(ctx, ep, c.id, c.alpn, c.cands)
	if err != nil {
		_ = ep.Shutdown(context.WithoutCancel(ctx))
		return nil, nil, err
	}
	return conn, ep, nil
}

// bindShardEndpoint binds one record-authenticated shard's endpoint.
func (c *Client) bindShardEndpoint(ctx context.Context) (*iroh.Endpoint, error) {
	var opts []iroh.Option
	if c.bindAddr.IsValid() {
		opts = append(opts, iroh.WithBindAddr(netip.AddrPortFrom(c.bindAddr.Addr(), 0)))
	}
	if c.punchDials {
		// See bindAndResolve's discovery branch for why each piece exists;
		// mDNS/pkarr are omitted — the candidates are already provided.
		opts = append(opts, iroh.WithRelayMode(relay.ModeDefault()), iroh.WithNetReport())
	}
	ep, err := iroh.Bind(ctx, opts...)
	if err != nil {
		return nil, err
	}
	if c.punchDials {
		seedLocalCandidates(ep)
	}
	return ep, nil
}
```

Note the delegation change: `remoteIP()` moves to the top of `extraConn`; delete the now-shadowed `if ip, ok := c.remoteIP(); ok {` wrapper from the legacy branch (the code above already reflects this). Add the `"github.com/tmc/go-iroh/relay"` import.

3. `attachExtras`: change the signature to
`func (c *Client) attachExtras(ctx context.Context, token []byte, ports []uint16, eps []amberiroh.DataEndpointRec, n int) ([]net.Conn, func())`,
and inside, replace the fixed budget and the `extraConn` call:

```go
	budget := attachBudget
	if len(eps) > 0 && c.punchDials {
		budget = punchAttachBudget
	}
```

(use `budget` in `context.WithTimeout(ctx, budget)`), and `c.extraConn(actx, i, ports, eps)`.

4. `runSenders` (~line 236): `c.attachExtras(ctx, first.Token, first.DataPorts, first.DataEndpoints, conns-1)`.

In `amberclient/client.go`:

5. Client struct, after `conns int`:

```go
	// punchDials records that the control dial went through discovery —
	// shard endpoints then bind the same relay/net-report stack so their
	// relay-won connections punch to direct. Direct-addr dials stay bare.
	punchDials bool
```

6. `Dial` (~line 194): add `punchDials: len(o.Addrs) == 0,` to the `&Client{…}` literal.

7. `pullOnce` (~line 365): `c.attachExtras(ctx, m.Token, m.DataPorts, m.DataEndpoints, conns-1)`.

- [ ] **Step 2: Build and run the package tests**

Run: `nix develop -c go build ./... && nix develop -c go test ./amberclient/ ./amberiroh/`
Expected: PASS (e2e sharded tests still run the legacy path until Task 6 makes the server advertise records).

- [ ] **Step 3: Commit**

```bash
git add amberclient/shard.go amberclient/client.go
git commit -m "amberclient: record-authenticated punch-capable shard dials"
```

---

### Task 5: Client — gate extras on a direct control path

**Files:**
- Modify: `amberclient/client.go` (Client struct, `transferConns` ~line 131-136, `Dial`)
- Test: `amberclient/gate_internal_test.go` (package `amberclient`)

**Interfaces:**
- Consumes: `Client.Path()` (`amberclient/path.go:55`).
- Produces: `transferConns` returns 1 while the control path is relayed — no API change; `pathFn func() (Path, bool)` test seam on Client.

- [ ] **Step 1: Write the failing test**

Create `amberclient/gate_internal_test.go`:

```go
package amberclient

import (
	"io"
	"log/slog"
	"testing"
)

func TestTransferConnsGatesOnRelayedPath(t *testing.T) {
	c := &Client{conns: 4, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	c.pathFn = func() (Path, bool) { return Path{Relayed: true, Addr: "relay:https://euc1-1.relay.example./"}, true }
	if got := c.transferConns(); got != 1 {
		t.Fatalf("relayed control path: conns %d, want 1 (extras move no extra bytes through the relay)", got)
	}
	if c.demoted.Load() {
		t.Fatal("the gate must skip, never demote — the path can upgrade")
	}
	c.pathFn = func() (Path, bool) { return Path{Relayed: false}, true }
	if got := c.transferConns(); got != 4 {
		t.Fatalf("direct control path: conns %d, want 4", got)
	}
	c.pathFn = func() (Path, bool) { return Path{}, false }
	if got := c.transferConns(); got != 4 {
		t.Fatalf("no path snapshot: conns %d, want 4 (don't gate blind)", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `nix develop -c go test ./amberclient/ -run TestTransferConnsGates -v`
Expected: compile error — `c.pathFn undefined`.

- [ ] **Step 3: Implement**

In `amberclient/client.go`:

1. Client struct, after `punchDials`:

```go
	// pathFn reports the control path for the shard gate; nil means
	// Client.Path. A test seam — production always uses the default.
	pathFn func() (Path, bool)
	// gateLogged keeps the relayed-path skip to one log line per
	// connection.
	gateLogged atomic.Bool
```

2. Replace `transferConns`:

```go
// transferConns is the connection count for the next transfer: the
// configured Conns until the connection demotes itself to 1 — and 1,
// without demoting, while the control path runs through a relay: extra
// relay connections move no additional bytes, and once hole punching lands
// (it commonly does moments after the dial) the next transfer shards again.
func (c *Client) transferConns() int {
	if c.demoted.Load() {
		return 1
	}
	if c.conns > 1 {
		pf := c.pathFn
		if pf == nil {
			pf = c.Path
		}
		if p, ok := pf(); ok && p.Relayed {
			if c.gateLogged.CompareAndSwap(false, true) {
				c.log.Info("amberclient: extras skipped while the control path is relayed; sharding resumes when it goes direct")
			}
			return 1
		}
	}
	return c.conns
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `nix develop -c go test ./amberclient/ -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add amberclient/client.go amberclient/gate_internal_test.go
git commit -m "amberclient: skip extras while the control path is relayed"
```

---

### Task 6: Server — first-class data endpoints in serve

**Files:**
- Modify: `serve/serve.go` (relay-mode hoist ~line 181-194, data endpoint block ~line 216-246; new helpers at the bottom of the file)
- Test: `serve/serve_test.go`

**Interfaces:**
- Consumes: `SetDataEndpoints` (Task 2), `serverRelayMode` (`serve/announce.go:45`), `onlineTimeout` (`serve/announce.go`), `hostaddr.LocalAddrPorts`, `Endpoint.Addr()`/`Online()`/`ID()`.
- Produces: running servers advertise `DataEndpoints` records; the loopback e2e suite (`amberclient/sharded_test.go`) now exercises the record-authenticated client path end to end.

- [ ] **Step 1: Write the failing test**

Append to `serve/serve_test.go` (confirm the file's package is `serve`; imports: `context`, `net/netip`, `github.com/tmc/go-iroh/iroh`, `github.com/tmc/go-iroh/netaddr`, `irohkey "github.com/tmc/go-iroh/key"`):

```go
func TestDataEndpointRecSnapshot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ep, err := iroh.Bind(ctx, iroh.WithBindAddr(netip.MustParseAddrPort("127.0.0.1:0")))
	if err != nil {
		t.Fatal(err)
	}
	defer ep.Shutdown(context.Background())

	seeded := []netip.AddrPort{netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), ep.LocalAddr().Port())}
	rec := dataEndpointRec(ep, seeded)

	id, err := irohkey.EndpointIDFromSlice(rec.ID)
	if err != nil {
		t.Fatalf("rec ID: %v", err)
	}
	if id != ep.ID() {
		t.Fatalf("rec ID %s, want endpoint's %s", id, ep.ID())
	}
	found := false
	for _, s := range rec.Addrs {
		ta, err := netaddr.ParseTransportAddr(s)
		if err != nil {
			t.Fatalf("advertised candidate %q does not parse: %v", s, err)
		}
		if ip, ok := ta.(netaddr.IPAddr); ok && ip.Addr == seeded[0] {
			found = true
		}
	}
	if !found {
		t.Fatalf("seeded addr %s missing from advertised candidates %v", seeded[0], rec.Addrs)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `nix develop -c go test ./serve/ -run TestDataEndpointRec -v`
Expected: compile error — `undefined: dataEndpointRec`.

- [ ] **Step 3: Implement**

In `serve/serve.go`:

1. Hoist the relay mode so the data-endpoint block can reuse it (~line 181): replace

```go
	if opts.Announce {
		relayMode, err := serverRelayMode(ctx, opts.RelayURL, log)
```

with

```go
	var relayMode relay.Mode
	if opts.Announce {
		relayMode, err = serverRelayMode(ctx, opts.RelayURL, log)
```

(keep the rest of the branch; add the `"github.com/tmc/go-iroh/relay"` import, plus `"github.com/jobs-build/jobs-iroh/hostaddr"` and `irohkey "github.com/tmc/go-iroh/key"` — drop any that end up unused).

2. Replace the data-endpoint bind loop (~line 216-235) with:

```go
	var dataRouters []*iroh.Router
	if n := min(opts.DataEndpoints, 15); n > 0 {
		// Bind every endpoint and publish ports + records BEFORE any router
		// accepts: SetDataPorts/SetDataEndpoints are unsynchronized writes
		// the handlers read (ambserver documents "call before Serve").
		//
		// Each data endpoint carries its OWN identity: a punchable endpoint
		// needs its own relay home connection and QAD-discovered mapping,
		// and relays key sessions by endpoint ID — sharing the server's key
		// would clash with the main endpoint's session. Shard dials
		// authenticate the identity advertised on the control connection
		// (amberiroh.DataEndpointRec). One consequence for OLD clients: their
		// dedicated-port dial expects the server's identity, fails the
		// handshake against these endpoints, and falls back to the control
		// candidates — they still shard, onto the main socket.
		var deps []*iroh.Endpoint
		var seeded [][]netip.AddrPort
		var ports []uint16
		for range n {
			var depOpts []iroh.Option
			if opts.BindAddr.IsValid() {
				depOpts = append(depOpts, iroh.WithBindAddr(netip.AddrPortFrom(opts.BindAddr.Addr(), 0)))
			}
			if opts.Announce {
				// Same rationale as the main endpoint's branch above: the
				// relay is the punch coordination channel and the QAD probe
				// target; without Announce there are no relays to probe.
				depOpts = append(depOpts, iroh.WithRelayMode(relayMode), iroh.WithNetReport())
			}
			dep, err := iroh.Bind(ctx, depOpts...)
			if err != nil {
				return fmt.Errorf("bind data endpoint: %w", err)
			}
			defer dep.Shutdown(context.WithoutCancel(ctx))
			if opts.Announce {
				go func(dep *iroh.Endpoint) {
					octx, cancel := context.WithTimeout(ctx, onlineTimeout)
					defer cancel()
					if err := dep.Online(octx); err != nil {
						log.Warn("data endpoint relay connect failed; its punch candidates stay direct-only", "error", err)
					}
				}(dep)
			}
			deps = append(deps, dep)
			seeded = append(seeded, seedDataEndpointAddrs(dep))
			ports = append(ports, dep.LocalAddr().Port())
		}
		amberSrv.SetDataPorts(ports)
		amberSrv.SetDataEndpoints(func() []amberiroh.DataEndpointRec {
			recs := make([]amberiroh.DataEndpointRec, len(deps))
			for i, dep := range deps {
				recs[i] = dataEndpointRec(dep, seeded[i])
			}
			return recs
		})
```

(the router wiring below the loop stays as-is).

3. Helpers at the bottom of the file:

```go
// dataEndpointRec snapshots one data endpoint's identity and live dial
// candidates for a TAccept/TRef frame — read per frame, so the relay and
// QAD candidates appear as soon as the endpoint learns them. The seeded
// interface addresses are unioned in explicitly: they are what a LAN client
// races and what a punching peer aims at first.
func dataEndpointRec(dep *iroh.Endpoint, seeded []netip.AddrPort) amberiroh.DataEndpointRec {
	id := dep.ID().Bytes()
	rec := amberiroh.DataEndpointRec{ID: id[:]}
	seen := map[string]bool{}
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			rec.Addrs = append(rec.Addrs, s)
		}
	}
	for _, ta := range dep.Addr().Addrs() {
		add(ta.String())
	}
	for _, ap := range seeded {
		add(netaddr.IPAddr{Addr: ap}.String())
	}
	return rec
}

// seedDataEndpointAddrs mirrors announce's direct-address seeding for one
// data endpoint: interface addresses on its own port become dial and QNT
// punch candidates. Best-effort — a failed walk leaves the endpoint bare,
// like the client's seedLocalCandidates.
func seedDataEndpointAddrs(dep *iroh.Endpoint) []netip.AddrPort {
	addrs, err := hostaddr.LocalAddrPorts(dep.LocalAddr().Port())
	if err != nil {
		return nil
	}
	for _, ap := range addrs {
		dep.AddExternalAddr(ap)
	}
	return addrs
}
```

(add the `"github.com/tmc/go-iroh/netaddr"` import; if `irohkey` ended up unused in serve.go itself, keep it only in the test).

- [ ] **Step 4: Run the serve and e2e suites**

Run: `nix develop -c go test ./serve/ ./amberclient/ ./amberiroh/`
Expected: all PASS — `TestShardedRoundTripDataEndpoints` now runs the record-authenticated dial (client `Addrs` set → bare shard endpoints, record IDs, dedicated + seeded candidates on loopback).

- [ ] **Step 5: Commit**

```bash
git add serve/serve.go serve/serve_test.go
git commit -m "serve: data endpoints carry their own punchable identities"
```

---

### Task 7: Full suite + cross-compile check

- [ ] **Step 1: Full test suite**

Run: `nix develop -c go test ./...`
Expected: all packages PASS. Investigate and fix any failure before proceeding (runnerd/clientcli/registryd compile against the changed amberclient — no source changes expected there, `attachExtras`/`extraConn` are unexported).

- [ ] **Step 2: Darwin cross-vet**

Run: `nix develop -c env CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go vet ./...`
Expected: clean.

- [ ] **Step 3: Commit (only if fixes were needed)**

```bash
git add -A && git commit -m "Fix cross-package fallout from punchable shard endpoints"
```

---

### Task 8: Docs — architecture, spec compat note, CLAUDE.md

**Files:**
- Modify: `docs/architecture/architecture.md` (locate the sharded-transfer / data-endpoint passages: `grep -n "data endpoint\|DataPorts\|shard" docs/architecture/architecture.md`)
- Modify: `docs/design/2026-07-28-punched-shard-endpoints.md` (§2.2)
- Modify: `CLAUDE.md` (package map rows for `amberiroh`, `amberclient`; `jobs-server` binary description)

- [ ] **Step 1: architecture.md**

Update the data-endpoint/sharding passages to state (adapting to the surrounding prose, keeping file:line-free style consistent with the document):

> Data endpoints carry their own iroh identities. Each binds with the server's relay mode and net report (when announcing), holds a relay home connection, and QAD-learns its public mapping; `TAccept`/`TRef` advertise per-endpoint `DataEndpoints` records — identity plus live dial candidates — alongside the `DataPorts` LAN fast path. A sharding client races the dedicated port with the advertised candidates under the record's identity; on discovery-dialed clients the shard endpoints bind the control dial's relay/net-report stack, so a relay-won shard hole-punches to direct mid-transfer. The record's presence signals the 10s attach gather window (up from 5s; gather still early-exits when every promised shard lands). Clients skip extras entirely — without demoting — while the control path itself is relayed. Old clients ignore the records: their dedicated-port dial fails the identity check and falls back to the control candidates, sharding onto the main socket.

- [ ] **Step 2: Spec compat note**

In `docs/design/2026-07-28-punched-shard-endpoints.md` §2.2, after the "Compatibility falls out…" paragraph, insert:

> One old-client wrinkle the per-endpoint keys introduce: an old client's
> dedicated-port dial authenticates the *server's* identity, which a new
> data endpoint no longer presents — the handshake fails and the client
> falls back to the control candidates. Old clients therefore still shard,
> but onto the main socket (no data-endpoint spread) even on a LAN.
> Correct-but-slower, consistent with the no-fence rule.

- [ ] **Step 3: CLAUDE.md touch-ups**

- `amberiroh` row: append `TAccept/TRef advertise per-endpoint DataEndpoints records (identity + candidates); their presence signals the 10s attach gather window.`
- `amberclient` row: append `Shard dials authenticate the advertised data-endpoint identity and, on discovery dials, bind the relay/net-report stack to hole-punch; extras are skipped (not demoted) while the control path is relayed.`
- `jobs-server` binary bullet: change the `--data-endpoints` clause to `--data-endpoints (default 3) binds extra UDP sockets with their own punchable identities for sharded store transfers`.

- [ ] **Step 4: Commit**

```bash
git add docs/architecture/architecture.md docs/design/2026-07-28-punched-shard-endpoints.md CLAUDE.md
git commit -m "architecture.md: punchable data endpoints for sharded transfers"
```

---

### Task 9: Release v0.19.0

Follow CLAUDE.md's release process exactly; the image push is public and requires user-granted sudo.

- [ ] **Step 1: CHANGELOG entry**

Prepend to `CHANGELOG.md`:

```markdown
## v0.19.0 — 2026-07-28

- **Sharded store transfers now hole-punch through NAT.** A NAT'd server's
  dedicated data endpoints were unreachable from outside — their
  kernel-assigned UDP ports have no NAT mapping, and the control
  connection's punched pinhole is four-tuple-scoped — so every shard attach
  timed out, the client demoted itself, and whole builds' inputs crossed on
  a single QUIC connection (~1.5 MB/s observed Hetzner ↔ NAT'd server).
  Data endpoints now carry their own iroh identities with a relay home
  connection and QUIC address discovery, advertised in-band on
  `TAccept`/`TRef`; shard dials race the dedicated port with the advertised
  candidates and, on discovery-dialed clients, bind the relay/net-report
  stack — a relay-won shard punches to direct mid-transfer exactly like the
  control connection does. The record's presence also signals the widened
  10s attach gather window (early-exit unchanged).
- Extras are now skipped — without the sticky demote — while the control
  path itself is relayed: extra relay connections move no additional bytes,
  and sharding resumes on the next transfer once hole punching lands.
- Compatibility: old runners/clients ignore the new field; their
  dedicated-port dial fails the new endpoints' identity check and falls
  back to the control candidates, so they still shard onto the main socket
  (correct, just without the socket spread). Old servers omit the field and
  new clients run the previous path unchanged. No ALPN bump.
```

- [ ] **Step 2: Version bump, commit, tag, push, GitHub release**

Set `version/version.go` `Version = "0.19.0"`, then:

```bash
git add version/version.go CHANGELOG.md
git commit -m "Release v0.19.0: sharded store transfers hole-punch through NAT"
git tag v0.19.0
git push origin main && git push origin v0.19.0
nix develop -c gh release create v0.19.0 --verify-tag --repo jobs-build/jobs-iroh \
  --title "v0.19.0 — sharded store transfers hole-punch through NAT" \
  --notes "<paste the CHANGELOG v0.19.0 section>"
```

(Release commit trailers as per Global Constraints.)

- [ ] **Step 3: Registry image (clean tree required)**

```bash
git status --porcelain   # must be empty
nix develop -c bash -c 'export GOPRIVATE="github.com/jobs-build/*"
  CGO_ENABLED=0 GOARCH=arm64 go build -o deploy/jobs-registry/jobs-registry-arm64 ./cmd/jobs-registry
  CGO_ENABLED=0 GOARCH=amd64 go build -o deploy/jobs-registry/jobs-registry-amd64 ./cmd/jobs-registry'
REV=$(git rev-parse HEAD)
sudo docker --config "$HOME/.docker" buildx build --builder jobs-multi \
  --platform linux/amd64,linux/arm64 --provenance=false --sbom=false \
  --label org.opencontainers.image.version="0.19.0" \
  --label org.opencontainers.image.revision="$REV" \
  --label org.opencontainers.image.source=https://github.com/jobs-build/jobs-iroh \
  --annotation "index:org.opencontainers.image.version=0.19.0" \
  --annotation "index:org.opencontainers.image.revision=$REV" \
  --annotation "index:org.opencontainers.image.source=https://github.com/jobs-build/jobs-iroh" \
  -t "dmilhdef/jobs-registry:v0.19.0" -t dmilhdef/jobs-registry:latest \
  --push deploy/jobs-registry
sudo docker --config "$HOME/.docker" buildx imagetools inspect "dmilhdef/jobs-registry:v0.19.0"
rm -f deploy/jobs-registry/jobs-registry-{amd64,arm64}
```

`imagetools inspect` must show exactly two platform entries and no `unknown/unknown` rows.

- [ ] **Step 4: Post-release verification note**

Tell the user: restart the server and the Hetzner runner on v0.19.0; the runner log should show `server connection is direct` and NO `sharded transfers disabled` line on the next big pull; server transfer-end logs report throughput.
