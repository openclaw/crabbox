package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	maxRunLogBytes              = 8 * 1024 * 1024
	coordinatorRunLogChunkBytes = 64 * 1024

	runLogFallbackPreviewBytes = 64 * 1024
)

type runLogBuffer struct {
	mu        sync.Mutex
	data      []byte
	digest    hash.Hash
	truncated bool
}

type runLogSnapshot struct {
	Log        string
	Truncated  bool
	FullSHA256 string
}

func (b *runLogBuffer) Write(p []byte) (int, error) {
	return b.write(p, true)
}

// Captured streams still belong to the raw digest, but not the retained log.
type capturedRunLogWriter struct{ buffer *runLogBuffer }

func (w capturedRunLogWriter) Write(p []byte) (int, error) {
	return w.buffer.write(p, false)
}

func (b *runLogBuffer) write(p []byte, retain bool) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.digest == nil {
		b.digest = sha256.New()
	}
	_, _ = b.digest.Write(p)
	if !retain {
		b.truncated = b.truncated || len(p) > 0
		return len(p), nil
	}
	n := len(p)
	// Keep enough preceding bytes to recognize a rune spanning the tail cut,
	// without copying an arbitrarily large Write into the bounded buffer.
	if len(p) > maxRunLogBytes+utf8.UTFMax-1 {
		p = p[len(p)-maxRunLogBytes-(utf8.UTFMax-1):]
		b.data = b.data[:0]
		b.truncated = true
	}
	b.data = append(b.data, p...)
	if overflow := len(b.data) - maxRunLogBytes; overflow > 0 {
		start := max(0, overflow-(utf8.UTFMax-1))
		for start < overflow {
			_, size := utf8.DecodeRune(b.data[start:])
			start += size
		}
		copy(b.data, b.data[start:])
		b.data = b.data[:len(b.data)-start]
		b.truncated = true
	}
	return n, nil
}

func (b *runLogBuffer) Snapshot() runLogSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Normalize only the snapshot: a later Write may complete its trailing rune.
	log, changed := retainedRunLogText(string(b.data), maxRunLogBytes)
	fullDigest := sha256Digest(nil)
	if b.digest != nil {
		fullDigest = "sha256:" + hex.EncodeToString(b.digest.Sum(nil))
	}
	return runLogSnapshot{Log: log, Truncated: b.truncated || changed, FullSHA256: fullDigest}
}

func (b *runLogBuffer) String() string {
	return b.Snapshot().Log
}

// retainedRunLogText replaces each malformed byte with U+FFFD, then keeps a
// whole-codepoint tail. A changed representation is not a byte-complete log.
func retainedRunLogText(log string, maxBytes int) (string, bool) {
	changed := !utf8.ValidString(log)
	if changed {
		log = strings.Map(func(r rune) rune { return r }, log)
	}
	if len(log) <= maxBytes {
		return log, changed
	}
	start := len(log) - maxBytes
	for start < len(log) && !utf8.RuneStart(log[start]) {
		start++
	}
	return log[start:], true
}

func splitRunLogChunks(log string) []string {
	log, _ = retainedRunLogText(log, maxRunLogBytes)
	if len(log) == 0 {
		return nil
	}
	chunks := make([]string, 0, (len(log)+coordinatorRunLogChunkBytes-1)/coordinatorRunLogChunkBytes)
	for len(log) > coordinatorRunLogChunkBytes {
		end := coordinatorRunLogChunkBytes
		for !utf8.RuneStart(log[end]) {
			end--
		}
		chunks = append(chunks, log[:end])
		log = log[end:]
	}
	return append(chunks, log)
}

func runLogFallbackPreview(log string) string {
	preview, _ := retainedRunLogText(log, runLogFallbackPreviewBytes)
	return preview
}
