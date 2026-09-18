package runtimeartifact

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
)

// CompressedPayload owns a temporary gzip stream for an optional upload. Size is
// the transport length; Identity describes the original executable bytes. The
// caller must Close the payload to remove its temporary file.
type CompressedPayload struct {
	file     *os.File
	identity Identity
	size     int64
	closed   bool
	removed  bool
	closeErr error
}

func (p *CompressedPayload) Read(b []byte) (int, error) { return p.file.Read(b) }
func (p *CompressedPayload) Seek(offset int64, whence int) (int64, error) {
	return p.file.Seek(offset, whence)
}
func (p *CompressedPayload) Identity() Identity { return p.identity }
func (p *CompressedPayload) Size() int64        { return p.size }

// Close closes the stream and removes only the file this payload created.
// Repeated calls retry failed removal without closing the stream twice. An
// already-absent temporary file is successful cleanup. The original Artifact
// remains caller-owned.
func (p *CompressedPayload) Close() error {
	if !p.closed {
		p.closeErr = p.file.Close()
		p.closed = true
	}
	var removeErr error
	if !p.removed {
		removeErr = os.Remove(p.file.Name())
		if errors.Is(removeErr, os.ErrNotExist) {
			removeErr = nil
		}
		p.removed = removeErr == nil
	}
	return errors.Join(p.closeErr, removeErr)
}

// CompressGzip reads an already-verified artifact into a private temporary gzip
// file and returns it positioned at zero. It does not reopen the artifact path.
// Once preparation begins, the original stream is rewound for raw fallback,
// including after cancellation. An already-canceled context leaves it untouched.
// Callers must not concurrently read, seek, or close that Artifact.
//
// This is only a transport representation. The caller must first establish target
// gzip support and retain the original identity for installation verification.
func CompressGzip(ctx context.Context, artifact *Artifact) (result *CompressedPayload, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if artifact == nil || artifact.stream == nil {
		return nil, fmt.Errorf("a verified artifact is required")
	}
	if _, err := artifact.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	var payload *CompressedPayload
	defer func() {
		_, rewindErr := artifact.Seek(0, io.SeekStart)
		err = errors.Join(err, rewindErr)
		if err != nil && payload != nil {
			err = errors.Join(err, payload.Close())
			result = nil
		}
	}()
	file, err := os.CreateTemp("", "crabbox-runtime-*.gz")
	if err != nil {
		return nil, err
	}
	payload = &CompressedPayload{file: file, identity: artifact.Identity()}
	writer := gzip.NewWriter(file)
	written, copyErr := io.Copy(writer, &contextReader{ctx, artifact})
	if err := errors.Join(copyErr, writer.Close()); err != nil {
		return nil, err
	}
	if written != payload.identity.Size {
		return nil, fmt.Errorf("artifact size changed during compression")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	payload.size, err = file.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return payload, nil
}
