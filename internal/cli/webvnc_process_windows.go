//go:build windows

package cli

import (
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	windowsProcessStillActive         = 259
	windowsMaxProcessCommandLineUTF16 = 32768
)

func processBootIdentity() (string, error) {
	return "", nil
}

func processBootIdentityRequired() bool {
	return false
}

func validPersistedProcessBootIdentity(string) bool {
	return true
}

type windowsProcessCommandLineQuery func(
	proc windows.Handle,
	procInfoClass int32,
	procInfo unsafe.Pointer,
	procInfoLen uint32,
	retLen *uint32,
) error

func webVNCDaemonProcessStartIdentity(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("pid must be positive")
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(handle)
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		return "", err
	}
	value := uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime)
	if value == 0 {
		return "", fmt.Errorf("process %d start identity unavailable", pid)
	}
	return strconv.FormatUint(value, 10), nil
}

func webVNCDaemonProcessCommand(pid int) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return "", false
		}
		return "", true
	}
	defer windows.CloseHandle(handle)
	var exitCode uint32
	if err := windows.GetExitCodeProcess(handle, &exitCode); err != nil {
		return "", true
	}
	if exitCode != windowsProcessStillActive {
		return "", false
	}
	return inspectWindowsProcessCommandLine(handle, windows.NtQueryInformationProcess)
}

func inspectWindowsProcessCommandLine(handle windows.Handle, query windowsProcessCommandLineQuery) (string, bool) {
	command, err := queryWindowsProcessCommandLine(handle, query)
	if err != nil {
		// A live process whose command line cannot be inspected must not be
		// mistaken for a stale PID and signaled or discarded without identity
		// verification.
		return "", true
	}
	command = strings.TrimSpace(command)
	return command, true
}

func queryWindowsProcessCommandLine(handle windows.Handle, query windowsProcessCommandLineQuery) (string, error) {
	headerSize := int(unsafe.Sizeof(windows.NTUnicodeString{}))
	buffer := make([]byte, headerSize+windowsMaxProcessCommandLineUTF16*2)
	var returned uint32
	if err := query(
		handle,
		int32(windows.ProcessCommandLineInformation),
		unsafe.Pointer(&buffer[0]),
		uint32(len(buffer)),
		&returned,
	); err != nil {
		return "", fmt.Errorf("query Windows process command line: %w", err)
	}
	command, err := decodeWindowsProcessCommandLine(buffer, returned)
	runtime.KeepAlive(buffer)
	return command, err
}

func decodeWindowsProcessCommandLine(buffer []byte, returned uint32) (string, error) {
	headerSize := int(unsafe.Sizeof(windows.NTUnicodeString{}))
	if len(buffer) < headerSize || returned < uint32(headerSize) || returned > uint32(len(buffer)) {
		return "", fmt.Errorf("invalid Windows process command line response length %d", returned)
	}
	header := (*windows.NTUnicodeString)(unsafe.Pointer(&buffer[0]))
	if header.Buffer == nil || header.Length == 0 || header.Length%2 != 0 || header.MaximumLength < header.Length || header.MaximumLength%2 != 0 {
		return "", fmt.Errorf("invalid Windows process command line metadata")
	}
	base := uintptr(unsafe.Pointer(&buffer[0]))
	returnedEnd := base + uintptr(returned)
	bufferEnd := base + uintptr(len(buffer))
	start := uintptr(unsafe.Pointer(header.Buffer))
	length := uintptr(header.Length)
	maximum := uintptr(header.MaximumLength)
	if returnedEnd < base || bufferEnd < base || start < base || start > returnedEnd || length > returnedEnd-start || maximum > bufferEnd-start {
		return "", fmt.Errorf("Windows process command line points outside query response")
	}
	command := windows.UTF16ToString(unsafe.Slice(header.Buffer, int(header.Length/2)))
	if strings.TrimSpace(command) == "" {
		return "", fmt.Errorf("Windows process command line is empty")
	}
	return command, nil
}
