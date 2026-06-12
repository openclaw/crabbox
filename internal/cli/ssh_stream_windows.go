//go:build windows

package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const commandStreamReadBuffer = 64 * 1024

var commandStreamRotateSize int64 = 64 * 1024 * 1024

// Windows inbox OpenSSH can hang after the remote command exits when Go
// connects its output to pipes. Regular files preserve the client's EOF
// behavior; briefly suspending the client lets drained spools rotate in place.
func runCommandWithPlatformStreams(cmd *exec.Cmd, stdout, stderr io.Writer) error {
	sharedOutput := stderr != nil && sameCommandStreamWriter(stdout, stderr)
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	stdoutSpool, err := newCommandStreamSpool()
	if err != nil {
		return err
	}
	defer stdoutSpool.close()
	spools := []*commandStreamSpool{stdoutSpool}
	copyCount := 1
	cmd.Stdout = stdoutSpool.output
	if sharedOutput {
		cmd.Stderr = stdoutSpool.output
	} else {
		stderrSpool, err := newCommandStreamSpool()
		if err != nil {
			return err
		}
		defer stderrSpool.close()
		spools = append(spools, stderrSpool)
		copyCount++
		cmd.Stderr = stderrSpool.output
	}

	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	copyResults := make(chan error, copyCount)
	go stdoutSpool.copyTo(stdout, done, copyResults)
	if !sharedOutput {
		go spools[1].copyTo(stderr, done, copyResults)
	}
	waitResult := make(chan error, 1)
	go func() {
		waitResult <- cmd.Wait()
	}()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	var suspended []windows.Handle
	for {
		select {
		case waitErr := <-waitResult:
			closeCommandThreadHandles(suspended)
			close(done)
			return errors.Join(append([]error{waitErr}, collectCommandStreamResults(copyResults, copyCount)...)...)
		case copyErr := <-copyResults:
			return failCommandStreams(cmd, waitResult, done, copyResults, suspended, copyErr, copyCount-1)
		case <-ticker.C:
			if len(suspended) == 0 {
				rotate, err := commandStreamRotationNeeded(spools...)
				if err != nil {
					return failCommandStreams(cmd, waitResult, done, copyResults, nil, err, copyCount)
				}
				if rotate {
					suspended, err = suspendCommandProcess(uint32(cmd.Process.Pid))
					if err != nil {
						return failCommandStreams(cmd, waitResult, done, copyResults, nil, err, copyCount)
					}
				}
			}
			if len(suspended) > 0 {
				drained, err := commandStreamsDrained(spools...)
				if err != nil {
					return failCommandStreams(cmd, waitResult, done, copyResults, suspended, err, copyCount)
				}
				if drained {
					var rotateErrs []error
					for _, spool := range spools {
						rotateErrs = append(rotateErrs, spool.rotate())
					}
					resumeErr := resumeCommandThreads(suspended)
					suspended = nil
					if err := errors.Join(errors.Join(rotateErrs...), resumeErr); err != nil {
						return failCommandStreams(cmd, waitResult, done, copyResults, nil, err, copyCount)
					}
				}
			}
		}
	}
}

func sameCommandStreamWriter(left, right io.Writer) bool {
	defer func() {
		_ = recover()
	}()
	return left == right
}

func collectCommandStreamResults(results <-chan error, count int) []error {
	errs := make([]error, 0, count)
	for range count {
		errs = append(errs, <-results)
	}
	return errs
}

type commandStreamSpool struct {
	output *os.File
	reader *os.File
	mu     sync.Mutex
	offset int64
}

func newCommandStreamSpool() (*commandStreamSpool, error) {
	security, err := commandStreamFileSecurity()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(os.TempDir(), "crabbox-ssh-"+strings.TrimPrefix(newLeaseID(), "cbx_")+".tmp")
	pathUTF16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	const shareMode = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE
	const fileFlags = windows.FILE_ATTRIBUTE_TEMPORARY | windows.FILE_FLAG_DELETE_ON_CLOSE
	outputHandle, err := windows.CreateFile(
		pathUTF16,
		windows.GENERIC_WRITE|windows.DELETE|windows.FILE_READ_ATTRIBUTES,
		shareMode,
		security,
		windows.CREATE_NEW,
		fileFlags,
		0,
	)
	if err != nil {
		return nil, err
	}
	readerHandle, err := windows.CreateFile(
		pathUTF16,
		windows.GENERIC_READ|windows.DELETE,
		shareMode,
		security,
		windows.OPEN_EXISTING,
		fileFlags,
		0,
	)
	if err != nil {
		_ = windows.CloseHandle(outputHandle)
		return nil, err
	}
	if err := markCommandStreamDeletePending(outputHandle, pathUTF16); err != nil {
		_ = windows.CloseHandle(readerHandle)
		_ = windows.CloseHandle(outputHandle)
		return nil, err
	}
	return &commandStreamSpool{
		output: os.NewFile(uintptr(outputHandle), path+"-output"),
		reader: os.NewFile(uintptr(readerHandle), path+"-reader"),
	}, nil
}

func (s *commandStreamSpool) close() {
	_ = s.reader.Close()
	_ = s.output.Close()
}

func (s *commandStreamSpool) copyTo(dst io.Writer, done <-chan struct{}, result chan<- error) {
	var buf [commandStreamReadBuffer]byte
	final := false
	for {
		s.mu.Lock()
		n, readErr := s.reader.Read(buf[:])
		s.mu.Unlock()
		if n > 0 {
			written, writeErr := dst.Write(buf[:n])
			if writeErr != nil {
				result <- writeErr
				return
			}
			if written != n {
				result <- io.ErrShortWrite
				return
			}
			s.mu.Lock()
			s.offset += int64(n)
			s.mu.Unlock()
			continue
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			result <- readErr
			return
		}
		if final {
			result <- nil
			return
		}
		select {
		case <-done:
			final = true
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func commandStreamRotationNeeded(spools ...*commandStreamSpool) (bool, error) {
	for _, spool := range spools {
		spool.mu.Lock()
		info, err := spool.output.Stat()
		spool.mu.Unlock()
		if err != nil {
			return false, err
		}
		if info.Size() >= commandStreamRotateSize {
			return true, nil
		}
	}
	return false, nil
}

func commandStreamsDrained(spools ...*commandStreamSpool) (bool, error) {
	for _, spool := range spools {
		spool.mu.Lock()
		info, err := spool.output.Stat()
		drained := err == nil && info.Size() == spool.offset
		spool.mu.Unlock()
		if err != nil {
			return false, err
		}
		if !drained {
			return false, nil
		}
	}
	return true, nil
}

func (s *commandStreamSpool) rotate() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	info, err := s.output.Stat()
	if err != nil {
		return err
	}
	if info.Size() != s.offset {
		return fmt.Errorf("rotate SSH output spool before drain: size=%d offset=%d", info.Size(), s.offset)
	}
	if err := s.output.Truncate(0); err != nil {
		return err
	}
	if _, err := s.output.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := s.reader.Seek(0, io.SeekStart); err != nil {
		return err
	}
	s.offset = 0
	return nil
}

func failCommandStreams(cmd *exec.Cmd, waitResult <-chan error, done chan<- struct{}, copyResults <-chan error, suspended []windows.Handle, cause error, remainingCopyResults int) error {
	closeCommandThreadHandles(suspended)
	_ = cmd.Process.Kill()
	waitErr := <-waitResult
	close(done)
	errs := []error{cause}
	if waitErr != nil && !isSSHCommandExitError(waitErr) {
		errs = append(errs, waitErr)
	}
	for range remainingCopyResults {
		errs = append(errs, <-copyResults)
	}
	return errors.Join(errs...)
}

var suspendThreadProc = windows.NewLazySystemDLL("kernel32.dll").NewProc("SuspendThread")

func suspendCommandProcess(processID uint32) ([]windows.Handle, error) {
	handles := make([]windows.Handle, 0, 4)
	seen := make(map[uint32]bool)
	for {
		added, err := suspendCommandThreads(processID, seen, &handles)
		if err != nil {
			_ = resumeCommandThreads(handles)
			return nil, err
		}
		if added == 0 {
			break
		}
	}
	if len(handles) == 0 {
		return nil, nil
	}
	return handles, nil
}

func suspendCommandThreads(processID uint32, seen map[uint32]bool, handles *[]windows.Handle) (int, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	if err := windows.Thread32First(snapshot, &entry); err != nil {
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			return 0, nil
		}
		return 0, err
	}
	added := 0
	for {
		if entry.OwnerProcessID == processID && !seen[entry.ThreadID] {
			thread, openErr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
			if openErr == nil {
				if err := suspendCommandThread(thread); err != nil {
					_ = windows.CloseHandle(thread)
					return added, err
				}
				seen[entry.ThreadID] = true
				*handles = append(*handles, thread)
				added++
			} else if !errors.Is(openErr, windows.ERROR_INVALID_PARAMETER) {
				return added, openErr
			}
		}
		if err := windows.Thread32Next(snapshot, &entry); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				break
			}
			return added, err
		}
	}
	return added, nil
}

func suspendCommandThread(thread windows.Handle) error {
	result, _, callErr := suspendThreadProc.Call(uintptr(thread))
	if result != uintptr(^uint32(0)) {
		return nil
	}
	if callErr != nil && !errors.Is(callErr, windows.ERROR_SUCCESS) {
		return callErr
	}
	return syscall.EINVAL
}

func resumeCommandThreads(handles []windows.Handle) error {
	var errs []error
	for _, thread := range handles {
		if _, err := windows.ResumeThread(thread); err != nil {
			errs = append(errs, err)
		}
		if err := windows.CloseHandle(thread); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func closeCommandThreadHandles(handles []windows.Handle) {
	for _, thread := range handles {
		_ = windows.CloseHandle(thread)
	}
}

func commandStreamFileSecurity() (*windows.SecurityAttributes, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		return nil, err
	}
	attributes := &windows.SecurityAttributes{SecurityDescriptor: sd}
	attributes.Length = uint32(unsafe.Sizeof(*attributes))
	return attributes, nil
}

func markCommandStreamDeletePending(handle windows.Handle, path *uint16) error {
	flags := uint32(windows.FILE_DISPOSITION_DELETE | windows.FILE_DISPOSITION_POSIX_SEMANTICS)
	if err := windows.SetFileInformationByHandle(
		handle,
		windows.FileDispositionInfoEx,
		(*byte)(unsafe.Pointer(&flags)),
		uint32(unsafe.Sizeof(flags)),
	); err == nil {
		return nil
	}
	return windows.DeleteFile(path)
}
