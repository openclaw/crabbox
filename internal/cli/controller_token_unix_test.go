//go:build !windows

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestControllerTokenRequiresCurrentUserOwnership(t *testing.T) {
	stat := unix.Stat_t{Uid: uint32(os.Geteuid() + 1)}
	if err := validateControllerTokenFileOwner(&stat); err == nil || !strings.Contains(err.Error(), "current user") {
		t.Fatalf("foreign-owned token error=%v", err)
	}
}

func TestReadControllerTokenRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token-fifo")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := readAdapterToken(path)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected FIFO token to be rejected")
		}
	case <-time.After(time.Second):
		t.Fatal("FIFO token validation blocked while opening the file")
	}
}
