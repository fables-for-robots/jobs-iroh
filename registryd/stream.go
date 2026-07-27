package registryd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/jobs-build/jobs-iroh/amber"
	"github.com/jobs-build/jobs-iroh/runner"
)

// errStreamClosed answers a read that arrives after the handler tore the
// stream down — see the locking note on layerStream.
var errStreamClosed = errors.New("layer stream: closed")

// layerStream is a seekable view of a layer tar that exists nowhere: Seek is
// arithmetic over the size recorded at assembly, and the tar is (re)generated
// from the content-addressed store at the first Read after a seek. It is what
// lets http.ServeContent do the HTTP work — HEAD, Range, Content-Length,
// Content-Range, 416 — over a blob the registry never stored.
//
// A CAS stream cannot rewind, so a backwards seek restarts the generator and a
// forward seek discards. Whole-blob GETs (every normal pull) seek only to 0
// and read straight through; a ranged resume pays one discard of the offset,
// which bounds a request at one regeneration of the layer — the same work a
// plain GET of it does. (Multi-range asks are refused upstream, in
// streamLayer, for exactly that reason.)
//
// Every method locks, and a Read after Close fails instead of reopening the
// generator: http.ServeContent hands the reader to goroutines whose lifetime
// it does not bound, and a straggler must not race the handler's Close, nor
// start a generator that now has nobody to close it.
type layerStream struct {
	ctx  context.Context
	open func(ctx context.Context) (io.ReadCloser, error)
	size int64

	mu        sync.Mutex
	pos       int64 // logical offset the next Read serves
	rc        io.ReadCloser
	streamPos int64 // logical offset rc is positioned at
	err       error // sticky generator failure
	closed    bool
}

func newLayerStream(ctx context.Context, st *amber.Store, lay layerRecord) (*layerStream, error) {
	spec, err := lay.spec()
	if err != nil {
		return nil, err
	}
	return &layerStream{ctx: ctx, open: runner.LayerTarOpener(st, spec), size: lay.Size}, nil
}

func (s *layerStream) Seek(offset int64, whence int) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = s.pos + offset
	case io.SeekEnd:
		abs = s.size + offset
	default:
		return 0, fmt.Errorf("layer stream: invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("layer stream: negative position %d", abs)
	}
	s.pos = abs
	return abs, nil
}

func (s *layerStream) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, errStreamClosed
	}
	if s.err != nil {
		return 0, s.err
	}
	if s.pos >= s.size {
		return 0, io.EOF
	}
	if s.rc == nil || s.streamPos > s.pos {
		if err := s.restart(); err != nil {
			return 0, err
		}
	}
	if skip := s.pos - s.streamPos; skip > 0 {
		n, err := io.CopyN(io.Discard, s.rc, skip)
		s.streamPos += n
		if err != nil {
			return 0, s.fail(err)
		}
	}
	if int64(len(p)) > s.size-s.pos {
		p = p[:s.size-s.pos]
	}
	n, err := s.rc.Read(p)
	s.pos += int64(n)
	s.streamPos += int64(n)
	if err == io.EOF && s.pos < s.size {
		// The generator ran dry before the size recorded at assembly: the
		// store lost content it once had. Report it as the truncation it is
		// rather than as a clean end of blob.
		err = io.ErrUnexpectedEOF
	}
	if err != nil && err != io.EOF {
		return n, s.fail(err)
	}
	return n, err
}

// failure reports a generator error, if the stream hit one.
func (s *layerStream) failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if errors.Is(s.err, errStreamClosed) {
		return nil
	}
	return s.err
}

// restart drops any open generator and starts a fresh one from byte 0.
// Called with s.mu held.
func (s *layerStream) restart() error {
	s.closeStream()
	rc, err := s.open(s.ctx)
	if err != nil {
		return s.fail(err)
	}
	s.rc, s.streamPos = rc, 0
	return nil
}

// fail makes a generator error sticky: a half-produced tar must never be
// resumed as if it were intact. Called with s.mu held.
func (s *layerStream) fail(err error) error {
	s.closeStream()
	if s.err == nil {
		s.err = err
	}
	return s.err
}

// closeStream releases the current generator. Called with s.mu held.
func (s *layerStream) closeStream() {
	if s.rc != nil {
		_ = s.rc.Close()
		s.rc = nil
	}
}

// Close stops the tar generator. Always call it — the generator is a goroutine
// writing into a pipe, and only closing the read side unblocks it.
func (s *layerStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.closeStream()
	return nil
}
