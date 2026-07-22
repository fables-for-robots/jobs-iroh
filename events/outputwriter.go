package events

import (
	"sync"
	"time"
)

const (
	outputChunkMax   = 32 << 10 // max bytes per exec.output event
	outputFlushBytes = 64 << 10 // buffered bytes that force a flush
	outputFlushDelay = 100 * time.Millisecond
)

// DefaultOutputCap bounds one stream's total emitted bytes per job; past it
// the writer emits one exec.output-truncated and goes silent (writes still
// succeed — the underlying process must never see an error).
const DefaultOutputCap = int64(16 << 20)

// OutputWriter is an io.WriteCloser that coalesces writes into bounded
// exec.output chunk events: it flushes when 64 KiB accumulate or 100 ms
// after the first unflushed byte (so bursts before silence still surface
// promptly for live tailing). Safe for use by one writer goroutine plus the
// internal flush timer.
type OutputWriter struct {
	j      *Job
	stream string
	phase  string
	cap    int64

	mu        sync.Mutex
	buf       []byte
	offset    int64 // total bytes emitted so far
	written   int64 // total bytes accepted (incl. buffered + discarded)
	truncated bool
	closed    bool
	timer     *time.Timer
}

func newOutputWriter(j *Job, stream, phase string) *OutputWriter {
	return &OutputWriter{j: j, stream: stream, phase: phase, cap: DefaultOutputCap}
}

// Write buffers p. Always returns len(p), nil — output capture must never
// fail the captured process.
func (o *OutputWriter) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.written += int64(len(p))
	if o.truncated || o.closed {
		return len(p), nil
	}
	room := o.cap - o.offset - int64(len(o.buf))
	if int64(len(p)) >= room {
		o.buf = append(o.buf, p[:room]...)
		o.flushLocked()
		o.truncated = true
		o.stopTimerLocked()
		o.j.emit(TypeExecOutputTruncated, map[string]any{
			"stream": o.stream, "cap": o.cap, "written": o.written,
		})
		return len(p), nil
	}
	o.buf = append(o.buf, p...)
	if len(o.buf) >= outputFlushBytes {
		o.flushLocked()
		o.stopTimerLocked()
	} else if o.timer == nil {
		o.timer = time.AfterFunc(outputFlushDelay, o.timedFlush)
	}
	return len(p), nil
}

func (o *OutputWriter) timedFlush() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.timer = nil
	if !o.closed && !o.truncated {
		o.flushLocked()
	}
}

func (o *OutputWriter) stopTimerLocked() {
	if o.timer != nil {
		o.timer.Stop()
		o.timer = nil
	}
}

// flushLocked emits the buffer as ≤32KiB chunks. Call with o.mu held.
func (o *OutputWriter) flushLocked() {
	for len(o.buf) > 0 {
		n := len(o.buf)
		if n > outputChunkMax {
			n = outputChunkMax
		}
		chunk := make([]byte, n)
		copy(chunk, o.buf[:n])
		o.buf = o.buf[n:]
		o.j.emit(TypeExecOutput, map[string]any{
			"stream": o.stream, "phase": o.phase, "chunk": chunk, "offset": o.offset,
		})
		o.offset += int64(n)
	}
	o.buf = nil
}

// Close flushes the remainder and stops the timer. Always returns nil.
func (o *OutputWriter) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil
	}
	o.closed = true
	o.stopTimerLocked()
	if !o.truncated {
		o.flushLocked()
	}
	return nil
}
