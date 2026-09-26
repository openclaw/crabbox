package cli

import (
	"context"
	"errors"
	"fmt"
	"hash"
	"io"
	"sync/atomic"
)

var errSourceCopyLimit = errors.New("source exceeds accepted byte limit")

// Optional per-operation logical I/O accounting, including filesystem-cache hits.
type sourceReadCounterKey struct{}
type sourceReadCounter struct {
	bytes  atomic.Int64
	hashes atomic.Int64
}

// A negative-one limit is unbounded. A bounded copy may read one extra byte to
// detect growth, but never writes beyond the limit. Context cannot interrupt an
// already-blocked filesystem read or write.
func copySourceBytes(ctx context.Context, dst io.Writer, src io.Reader, limit int64) (written int64, err error) {
	counter, _ := ctx.Value(sourceReadCounterKey{}).(*sourceReadCounter)
	if _, hashing := dst.(hash.Hash); hashing && counter != nil {
		counter.hashes.Add(1)
	}
	if limit < -1 {
		return 0, fmt.Errorf("invalid source copy limit")
	}
	bufferSize := 128 * 1024
	if limit >= 0 && limit < int64(bufferSize) {
		bufferSize = int(limit) + 1
	}
	buf := make([]byte, bufferSize)
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		readBuf := buf
		if limit >= 0 && limit-written < int64(len(buf)) {
			readBuf = buf[:int(limit-written)+1]
		}
		n, readErr := src.Read(readBuf)
		if counter != nil {
			counter.bytes.Add(int64(n))
		}
		if err := ctx.Err(); err != nil {
			return written, err
		}
		if n > 0 {
			if limit >= 0 && int64(n) > limit-written {
				return written, errSourceCopyLimit
			}
			nw, writeErr := dst.Write(readBuf[:n])
			if nw < 0 || nw > n {
				return written, fmt.Errorf("invalid source copy write count")
			}
			written += int64(nw)
			if writeErr != nil {
				return written, writeErr
			}
			if nw != n {
				return written, io.ErrShortWrite
			}
		}
		if err := ctx.Err(); err != nil {
			return written, err
		}
		if readErr == io.EOF {
			return written, nil
		}
		if readErr != nil {
			return written, readErr
		}
	}
}
