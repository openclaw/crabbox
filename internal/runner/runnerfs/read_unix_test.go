//go:build !windows

package runnerfs

import (
	"errors"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestFIFOIsRejectedWithoutWaitingForWriter(t *testing.T) {
	root, dir := testRoot(t)
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := root.Read("pipe", 64); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNotRegular) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("FIFO open blocked")
	}
}
