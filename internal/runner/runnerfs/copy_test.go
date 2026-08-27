package runnerfs

import (
	"context"
	"io"
	"testing"
)

type finalCopyRead struct{}

func (finalCopyRead) Read(data []byte) (int, error) { return copy(data, []byte("last")), io.EOF }

type cancelCopyWriter struct{ cancel context.CancelFunc }

func (writer cancelCopyWriter) Write(data []byte) (int, error) {
	writer.cancel()
	return len(data), nil
}

func TestCopyPreservesCancellationAtFinalWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := Copy(ctx, cancelCopyWriter{cancel}, finalCopyRead{}); err != context.Canceled {
		t.Fatalf("final cancellation lost: %v", err)
	}
}
