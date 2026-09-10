// Package tailbuffer retains raw output tails without imposing caller error,
// presentation or synchronization policy.
package tailbuffer

// Buffer acknowledges all input while retaining the last limited bytes. Its
// zero value discards all input. Do not copy it after use or use it concurrently.
// Limits bound retained bytes, not allocation capacity or process memory.
type Buffer struct {
	data     []byte
	limit    int
	exceeded bool
}

// NewLimited retains at most maxBytes; nonpositive limits discard all input.
func NewLimited(maxBytes int) Buffer {
	return Buffer{limit: max(0, maxBytes)}
}

// Write returns the full input length and nil, including when bytes are evicted.
func (b *Buffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.limit == 0 {
		b.exceeded = b.exceeded || n > 0
		return n, nil
	}
	if n >= b.limit {
		b.exceeded = b.exceeded || len(b.data) > 0 || n > b.limit
		b.data = append(b.data[:0], p[n-b.limit:]...)
		return n, nil
	}
	if overflow := n - (b.limit - len(b.data)); overflow > 0 {
		copy(b.data, b.data[overflow:])
		b.data = b.data[:len(b.data)-overflow]
		b.exceeded = true
	}
	b.data = append(b.data, p...)
	return n, nil
}

// Bytes returns a borrowed view of the retained raw tail. Treat it as read-only;
// it is valid only until the next Write.
func (b *Buffer) Bytes() []byte { return b.data }

// String returns the raw retained bytes without text normalization or a marker.
func (b *Buffer) String() string { return string(b.data) }

// Exceeded reports whether any input bytes have been discarded, including bytes
// evicted by a later Write. Exact fill alone does not exceed the limit.
func (b *Buffer) Exceeded() bool { return b.exceeded }
