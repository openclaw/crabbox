// Package prefixbuffer retains output prefixes without imposing caller error or
// cancellation policy.
package prefixbuffer

import "bytes"

// Buffer acknowledges all input while retaining a prefix. Its zero value
// discards all input. It must not be copied after use or used concurrently.
// Limits bound retained bytes, not allocation capacity or process memory.
type Buffer struct {
	buffer    bytes.Buffer
	limit     int
	unlimited bool
	exceeded  bool
}

// NewLimited retains at most maxBytes; nonpositive limits discard all input.
func NewLimited(maxBytes int) Buffer {
	if maxBytes < 0 {
		maxBytes = 0
	}
	return Buffer{limit: maxBytes}
}

// NewUnlimited retains all input without setting Exceeded.
func NewUnlimited() Buffer {
	return Buffer{unlimited: true}
}

// Write returns the full input length and nil, including when input is discarded.
func (b *Buffer) Write(p []byte) (int, error) {
	if b.unlimited {
		return b.buffer.Write(p)
	}
	original := len(p)
	remaining := b.limit - b.buffer.Len()
	if len(p) > remaining {
		b.exceeded = true
		p = p[:remaining]
	}
	_, _ = b.buffer.Write(p)
	return original, nil
}

// Bytes returns a borrowed view of the retained prefix. Treat it as read-only;
// it is valid only until the next Write.
func (b *Buffer) Bytes() []byte { return b.buffer.Bytes() }

// String returns the retained prefix without a truncation marker.
func (b *Buffer) String() string { return b.buffer.String() }

// Exceeded reports whether any nonempty input has been discarded.
func (b *Buffer) Exceeded() bool { return b.exceeded }
