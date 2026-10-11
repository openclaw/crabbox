package testutil

import (
	"context"
	"io"
)

// CancelOnWrite ends a polling command after it delivers its first output.
func CancelOnWrite(writer io.Writer, cancel context.CancelFunc) io.Writer {
	return cancelWriter{Writer: writer, cancel: cancel}
}

type cancelWriter struct {
	io.Writer
	cancel context.CancelFunc
}

func (w cancelWriter) Write(data []byte) (int, error) {
	n, err := w.Writer.Write(data)
	w.cancel()
	return n, err
}
