package amberiroh

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/jobs-build/amber-store-core/fstree"
	"github.com/jobs-build/amber-store-core/key"
	"github.com/jobs-build/amber-store-core/packstore"
	"github.com/jobs-build/amber-store-core/reference"
	"github.com/jobs-build/amber-store-core/refstore"
)

// Server answers amber-store-iroh operations against a single store.
// Access is open by design: any peer that can connect may push and pull.
type Server struct {
	log     *slog.Logger
	objects *packstore.Store
	refs    *refstore.Store
	jobs    int // completeness-walk parallelism; 0 = GOMAXPROCS

	// attachWait bounds how long a sharded transfer waits for the
	// client's promised data connections before proceeding with
	// whatever attached. 10s covers a punching attach — relay connect
	// then TAttach — while gather's early exit keeps fast attaches free.
	attachWait time.Duration
	transfers  transfers
	// dataPorts are the UDP ports of the extra data endpoints, offered
	// to sharding clients so their connections land on separate server
	// sockets (one endpoint's socket loop caps out well below a fast
	// link).
	dataPorts []uint16
	// dataEndpoints, when set, snapshots the data endpoints' identities and
	// live dial candidates for TAccept/TRef — a closure because relay and
	// QAD candidates appear asynchronously after bind.
	dataEndpoints func() []DataEndpointRec

	mu       sync.Mutex
	refLocks map[string]*sync.Mutex
}

// maxDataConns caps the extra data connections one transfer may request.
const maxDataConns = 16

// transfers routes attaching data streams to their in-progress transfer
// by token.
type transfers struct {
	mu      sync.Mutex
	pending map[string]chan io.ReadWriteCloser
}

// create registers a new transfer and returns its token.
func (t *transfers) create() ([]byte, error) {
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pending == nil {
		t.pending = make(map[string]chan io.ReadWriteCloser)
	}
	t.pending[string(token)] = make(chan io.ReadWriteCloser, maxDataConns)
	return token, nil
}

// attach hands rw to the transfer identified by token; ownership moves
// to the transfer on true.
func (t *transfers) attach(token []byte, rw io.ReadWriteCloser) bool {
	t.mu.Lock()
	ch, ok := t.pending[string(token)]
	t.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- rw:
		return true
	default:
		return false
	}
}

// gather collects up to n attached streams, waiting at most wait for
// stragglers — a client that fails to open some connections must not
// stall the transfer.
func (t *transfers) gather(token []byte, n int, wait time.Duration) []io.ReadWriteCloser {
	t.mu.Lock()
	ch := t.pending[string(token)]
	t.mu.Unlock()
	if ch == nil {
		return nil
	}
	var out []io.ReadWriteCloser
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for len(out) < n {
		select {
		case rw := <-ch:
			out = append(out, rw)
		case <-deadline.C:
			return out
		}
	}
	return out
}

// drop unregisters the token; streams attached but never gathered are
// closed.
func (t *transfers) drop(token []byte) {
	t.mu.Lock()
	ch, ok := t.pending[string(token)]
	delete(t.pending, string(token))
	t.mu.Unlock()
	if !ok {
		return
	}
	for {
		select {
		case rw := <-ch:
			rw.Close()
		default:
			return
		}
	}
}

// New wires a Server over an open packstore and refstore. The caller keeps
// ownership of both and closes them after the server stops.
func New(log *slog.Logger, objects *packstore.Store, refs *refstore.Store) *Server {
	return &Server{log: log, objects: objects, refs: refs, attachWait: 10 * time.Second, refLocks: map[string]*sync.Mutex{}}
}

// SetDataPorts records the data-endpoint ports advertised to sharding
// clients. Call before Serve.
func (s *Server) SetDataPorts(ports []uint16) { s.dataPorts = ports }

// SetDataEndpoints installs the data-endpoint snapshot advertised to
// sharding clients; call before Serve, like SetDataPorts.
func (s *Server) SetDataEndpoints(f func() []DataEndpointRec) { s.dataEndpoints = f }

// lockRef serializes ref commits per name so compare-and-swap is
// race-free under concurrent pushes. Entries are never removed; the map
// is bounded by the number of distinct ref names ever pushed.
func (s *Server) lockRef(name string) (unlock func()) {
	s.mu.Lock()
	l := s.refLocks[name]
	if l == nil {
		l = &sync.Mutex{}
		s.refLocks[name] = l
	}
	s.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// HandleStream serves one operation on one stream and closes it. The
// stream is FIN-closed after the final response frame; the connection
// stays up for further streams. remote identifies the connection's peer
// for per-operation logging.
func (s *Server) HandleStream(remote string, rw io.ReadWriteCloser) {
	m, err := ReadMsg(rw)
	if err != nil {
		rw.Close()
		s.log.Error("read request", "error", err)
		return
	}
	// A data stream attaching to an in-progress transfer changes hands:
	// the transfer owns and closes it. Everything else is request/response
	// on this stream.
	if m.Type == TAttach {
		if s.transfers.attach(m.Token, rw) {
			return
		}
		_ = WriteMsg(rw, Msg{Type: TErr, Code: CodeBadRequest, Text: "unknown transfer token"})
		rw.Close()
		s.log.Warn("attach with unknown token", "remote", remote)
		return
	}
	defer rw.Close()
	switch m.Type {
	case TRefList:
		err = s.handleRefList(rw)
	case TPush:
		err = s.handlePush(remote, rw, m)
	case TPull:
		err = s.handlePull(rw, m)
	default:
		err = s.fail(rw, CodeBadRequest, fmt.Errorf("unknown operation %d", m.Type))
	}
	if err != nil {
		s.log.Error("operation failed", "op", m.Type, "name", m.Name, "error", err)
	}
}

// fail sends a TErr frame (best effort) and returns err for logging.
func (s *Server) fail(w io.Writer, code string, err error) error {
	_ = WriteMsg(w, Msg{Type: TErr, Code: code, Text: err.Error()})
	return err
}

// failLocal reports a transfer failure to the peer as a TErr frame, as the
// spec requires of every failure — except when the error came from the peer
// itself (*RemoteError), which must not be echoed back.
func (s *Server) failLocal(w io.Writer, err error) error {
	var re *RemoteError
	if errors.As(err, &re) {
		return err
	}
	return s.fail(w, CodeInternal, err)
}

func (s *Server) handleRefList(rw io.ReadWriter) error {
	records, err := s.refs.All()
	if err != nil {
		return s.fail(rw, CodeInternal, err)
	}
	infos := make([]RefInfo, 0, len(records))
	for _, r := range records {
		rec, err := reference.Decode(r.Data)
		if err != nil {
			return s.fail(rw, CodeInternal, fmt.Errorf("reference %q: %w", r.Name, err))
		}
		infos = append(infos, RefInfo{Name: r.Name, Key: rec.Key, CreatedAt: rec.CreatedAt, User: rec.User})
	}
	return WriteMsg(rw, Msg{Type: TRefs, Refs: infos})
}

func (s *Server) handlePush(remote string, rw io.ReadWriter, m Msg) error {
	start := time.Now()
	if err := reference.ValidateName(m.Name); err != nil {
		return s.fail(rw, CodeBadRequest, err)
	}
	root, err := key.Parse(m.Root)
	if err != nil {
		return s.fail(rw, CodeBadRequest, fmt.Errorf("root key: %w", err))
	}
	// Early precondition check: reject before any transfer happens. The
	// authoritative re-check happens under the per-name lock at commit.
	if m.CAS {
		if err := s.checkCAS(rw, m); err != nil {
			return err
		}
	}
	channels, release, err := s.shardChannels(rw, m.DataConns)
	if err != nil {
		return s.fail(rw, CodeInternal, err)
	}
	defer release()
	stats, err := Receive(channels, s.objects, root, s.jobs, nil)
	if err != nil {
		return s.failLocal(rw, err)
	}
	unlock := s.lockRef(m.Name)
	defer unlock()
	if m.CAS {
		if err := s.checkCAS(rw, m); err != nil {
			return err
		}
	}
	rec := reference.Reference{Name: m.Name, Key: root[:], CreatedAt: time.Now().UnixNano()}
	raw, err := rec.Encode()
	if err != nil {
		return s.fail(rw, CodeInternal, err)
	}
	if err := s.refs.Put(m.Name, raw); err != nil {
		return s.fail(rw, CodeInternal, err)
	}
	if err := WriteMsg(rw, Msg{Type: TOK, Key: root[:]}); err != nil {
		return err
	}
	s.logPush(remote, m.Name, root, stats, time.Since(start))
	return nil
}

// logPush reports one committed push: "offered" is the object count of
// the whole pushed tree, "transferred" what actually crossed the wire —
// the difference is what deduplication saved. Counting offered walks the
// tree's interior nodes, a bounded local-read cost per push.
func (s *Server) logPush(remote, name string, root key.Key, stats Stats, d time.Duration) {
	offered := -1
	if keys, err := fstree.ReachableKeys(root, s.objects.Get); err == nil {
		offered = len(keys)
	} else {
		s.log.Warn("push accounting walk failed", "ref", name, "error", err)
	}
	throughput := 0.0
	if d > 0 {
		throughput = float64(stats.Bytes) / 1e6 / d.Seconds()
	}
	s.log.Info("push",
		"ref", name,
		"client", remote,
		"offered", offered,
		"transferred", stats.Received,
		"bytes", stats.Bytes,
		"duration", d.Round(time.Millisecond),
		"throughput", fmt.Sprintf("%.1f MB/s", throughput),
	)
}

// shardChannels sets up the data channels for a sharded transfer: it
// offers the client a token, waits briefly for the promised attaches,
// and returns the control stream plus whatever arrived. With
// dataConns == 0 it is a no-op single-channel setup.
func (s *Server) shardChannels(rw io.ReadWriter, dataConns int) ([]io.ReadWriter, func(), error) {
	if dataConns <= 0 {
		return []io.ReadWriter{rw}, func() {}, nil
	}
	if dataConns > maxDataConns {
		dataConns = maxDataConns
	}
	token, err := s.transfers.create()
	if err != nil {
		return nil, nil, err
	}
	accept := Msg{Type: TAccept, Token: token, DataPorts: s.dataPorts}
	if s.dataEndpoints != nil {
		accept.DataEndpoints = s.dataEndpoints()
	}
	if err := WriteMsg(rw, accept); err != nil {
		s.transfers.drop(token)
		return nil, nil, err
	}
	extras := s.transfers.gather(token, dataConns, s.attachWait)
	channels := make([]io.ReadWriter, 0, 1+len(extras))
	channels = append(channels, rw)
	for _, e := range extras {
		channels = append(channels, e)
	}
	release := func() {
		s.transfers.drop(token)
		for _, e := range extras {
			e.Close()
		}
	}
	return channels, release, nil
}

// checkCAS verifies the push precondition: ExpectedOld must equal the
// ref's current key (nil meaning "ref must not exist"). On mismatch it
// sends the cas-mismatch frame carrying the current key and returns an
// error that ends the operation.
func (s *Server) checkCAS(rw io.ReadWriter, m Msg) error {
	var current []byte
	raw, err := s.refs.Get(m.Name)
	switch {
	case err == nil:
		rec, decErr := reference.Decode(raw)
		if decErr != nil {
			return s.fail(rw, CodeInternal, decErr)
		}
		current = rec.Key
	case errors.Is(err, refstore.ErrNotFound):
		// current stays nil: the ref does not exist.
	default:
		return s.fail(rw, CodeInternal, err)
	}
	if bytes.Equal(current, m.ExpectedOld) {
		return nil
	}
	_ = WriteMsg(rw, Msg{
		Type:    TErr,
		Code:    CodeCASMismatch,
		Text:    fmt.Sprintf("remote ref %q changed", m.Name),
		Current: current,
	})
	return fmt.Errorf("cas mismatch on %q", m.Name)
}

func (s *Server) handlePull(rw io.ReadWriter, m Msg) error {
	raw, err := s.refs.Get(m.Name)
	if errors.Is(err, refstore.ErrNotFound) {
		return s.fail(rw, CodeUnknownRef, fmt.Errorf("ref %q not found", m.Name))
	}
	if err != nil {
		return s.fail(rw, CodeInternal, err)
	}
	ref := Msg{Type: TRef, Record: raw}
	var token []byte
	if m.DataConns > 0 {
		token, err = s.transfers.create()
		if err != nil {
			return s.fail(rw, CodeInternal, err)
		}
		ref.Token = token
		ref.DataPorts = s.dataPorts
		if s.dataEndpoints != nil {
			ref.DataEndpoints = s.dataEndpoints()
		}
	}
	if err := WriteMsg(rw, ref); err != nil {
		if token != nil {
			s.transfers.drop(token)
		}
		return err
	}
	channels := []io.ReadWriter{rw}
	if token != nil {
		n := min(m.DataConns, maxDataConns)
		extras := s.transfers.gather(token, n, s.attachWait)
		defer func() {
			s.transfers.drop(token)
			for _, e := range extras {
				e.Close()
			}
		}()
		for _, e := range extras {
			channels = append(channels, e)
		}
	}
	// One Send loop per channel; the client deals its wants across them
	// and ends every loop with an empty TWants.
	errs := make([]error, len(channels))
	var wg sync.WaitGroup
	for i, ch := range channels {
		wg.Add(1)
		go func(i int, ch io.ReadWriter) {
			defer wg.Done()
			errs[i] = Send(ch, s.objects, nil)
		}(i, ch)
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return s.failLocal(rw, err)
	}
	return nil
}
