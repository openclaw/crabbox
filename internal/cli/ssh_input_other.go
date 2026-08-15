//go:build !windows

package cli

import (
	"bytes"
	"io"
)

type replayableSSHInput struct {
	reader *bytes.Reader
}

func newReplayableSSHInput(data []byte) (*replayableSSHInput, error) {
	return &replayableSSHInput{reader: bytes.NewReader(data)}, nil
}

func (input *replayableSSHInput) reset() (io.Reader, error) {
	if _, err := input.reader.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return input.reader, nil
}

func (*replayableSSHInput) close() error {
	return nil
}
