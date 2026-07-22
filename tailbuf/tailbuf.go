// Package tailbuf provides a bounded tail buffer that retains only the last
// max bytes written to it. It implements io.Writer and is used by runner and
// recipe to capture the tail of subprocess stderr without unbounded growth.
package tailbuf

import "bytes"

// Buffer retains only the last max bytes written. It is safe for sequential
// use as an io.Writer (not concurrent). Write always returns len(p), nil.
type Buffer struct {
	buf bytes.Buffer
	max int
}

// New returns a Buffer that retains at most max bytes.
func New(max int) *Buffer {
	return &Buffer{max: max}
}

// Write appends p to the buffer, then trims the buffer so it holds at most
// max bytes (retaining the tail). Always returns len(p), nil.
func (b *Buffer) Write(p []byte) (int, error) {
	b.buf.Write(p)
	if b.buf.Len() > b.max {
		keep := append([]byte(nil), b.buf.Bytes()[b.buf.Len()-b.max:]...)
		b.buf.Reset()
		b.buf.Write(keep)
	}
	return len(p), nil
}

// String returns the retained tail as a string.
func (b *Buffer) String() string { return b.buf.String() }
