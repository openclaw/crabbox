//go:build !windows

package runnerfs

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestArchiveRejectsFIFOBeforeOpening(t *testing.T) {
	name := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(name, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, archive, err := CreateArchive(t.Context(), name, CreateOptions{}, DefaultArchiveLimits())
		if archive != nil {
			_ = archive.Close()
			_ = os.Remove(archive.Name())
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "special file") {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("archive blocked opening FIFO")
	}
}
