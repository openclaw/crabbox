//go:build !windows

package cli

import (
	"bytes"
	"io"
)

type replayableSSHInput struct {
	reader io.ReadSeeker
	prefix []byte
}

func newReplayableSSHInput(data []byte) (*replayableSSHInput, error) {
	return &replayableSSHInput{reader: bytes.NewReader(data)}, nil
}

func newReplayableSSHInputStream(prefix []byte, reader io.ReadSeeker, _ int64) (*replayableSSHInput, error) {
	return &replayableSSHInput{reader: reader, prefix: bytes.Clone(prefix)}, nil
}

func (input *replayableSSHInput) reset() (io.Reader, error) {
	if _, err := input.reader.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	if input.prefix != nil {
		return io.MultiReader(bytes.NewReader(input.prefix), input.reader), nil
	}
	return input.reader, nil
}

func (*replayableSSHInput) close() error {
	return nil
}
