//go:build !windows

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestClaimMetadataRejectsFIFOWithoutWaiting(t *testing.T) {
	isolateTestUserDirs(t)
	const id = "cbx_metadata_fifo"
	path, err := leaseClaimPath(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	// Keep the FIFO open so the baseline blocks in read, not an unjoinable open.
	writer, err := os.OpenFile(path, os.O_RDWR|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	done := make(chan error, 1)
	go func() { _, err := readLeaseClaim(id); done <- err }()
	select {
	case err := <-done:
		var fileErr *leaseClaimFileError
		if !errors.As(err, &fileErr) || fileErr.code != "read_error" {
			t.Fatalf("non-regular runtime read err=%v", err)
		}
	case <-time.After(3 * time.Second):
		// Readers already in read see EOF; a late opener sees a regular file.
		if err := writeLeaseClaimAtomic(path, leaseClaim{LeaseID: id}); err != nil {
			t.Error(err)
		}
		_ = writer.Close()
		<-done
		t.Fatal("claim reader waited on a FIFO")
	}
}
