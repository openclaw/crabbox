//go:build windows

package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

type replayableSSHInput struct {
	reader *os.File
	path   string
}

// Windows inbox OpenSSH can stall waiting for EOF from Go's anonymous stdin
// pipe. Finite SSH payloads use a private regular file so EOF is file-backed.
func newReplayableSSHInput(data []byte) (*replayableSSHInput, error) {
	security, err := commandStreamFileSecurity()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(os.TempDir(), "crabbox-ssh-"+strings.TrimPrefix(newLeaseID(), "cbx_")+".tmp")
	pathUTF16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	const fileFlags = windows.FILE_ATTRIBUTE_NORMAL | windows.FILE_FLAG_SEQUENTIAL_SCAN
	handle, err := windows.CreateFile(
		pathUTF16,
		windows.GENERIC_WRITE,
		0,
		security,
		windows.CREATE_NEW,
		fileFlags,
		0,
	)
	if err != nil {
		return nil, err
	}
	writer := os.NewFile(uintptr(handle), path)
	if writer == nil {
		return nil, errors.Join(
			errors.New("create SSH input spool file"),
			windows.CloseHandle(handle),
			removeReplayableSSHInputPath(path),
		)
	}
	cleanupWriter := func(cause error) (*replayableSSHInput, error) {
		return nil, errors.Join(cause, writer.Close(), removeReplayableSSHInputPath(path))
	}
	if n, err := writer.Write(data); err != nil {
		return cleanupWriter(fmt.Errorf("write SSH input spool: %w", err))
	} else if n != len(data) {
		return cleanupWriter(fmt.Errorf("write SSH input spool: %w", io.ErrShortWrite))
	}
	if err := writer.Sync(); err != nil {
		return cleanupWriter(fmt.Errorf("flush SSH input spool: %w", err))
	}
	if err := writer.Close(); err != nil {
		return nil, errors.Join(
			fmt.Errorf("close SSH input spool writer: %w", err),
			removeReplayableSSHInputPath(path),
		)
	}

	reader, err := os.Open(path)
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("reopen SSH input spool read-only: %w", err),
			removeReplayableSSHInputPath(path),
		)
	}
	input := &replayableSSHInput{reader: reader, path: path}
	user, err := currentWindowsUserSID()
	if err != nil {
		return nil, errors.Join(err, input.close())
	}
	if err := verifyPrivateWindowsHandle(windows.Handle(reader.Fd()), false, user); err != nil {
		return nil, errors.Join(fmt.Errorf("verify SSH input spool privacy: %w", err), input.close())
	}
	return input, nil
}

func (input *replayableSSHInput) reset() (io.Reader, error) {
	if input.reader == nil {
		return nil, errors.New("SSH input spool is closed")
	}
	if _, err := input.reader.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return input.reader, nil
}

func (input *replayableSSHInput) close() error {
	var errs []error
	if input.reader != nil {
		errs = append(errs, input.reader.Close())
		input.reader = nil
	}
	if input.path != "" {
		if err := removeReplayableSSHInputPath(input.path); err != nil {
			errs = append(errs, err)
		} else {
			input.path = ""
		}
	}
	return errors.Join(errs...)
}

func removeReplayableSSHInputPath(path string) error {
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove SSH input spool: %w", err)
	}
	return nil
}
