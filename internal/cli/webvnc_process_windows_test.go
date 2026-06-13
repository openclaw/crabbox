//go:build windows

package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWebVNCDaemonProcessStartIdentityFromCreationTime(t *testing.T) {
	identity, err := webVNCDaemonProcessStartIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if value, err := strconv.ParseUint(identity, 10, 64); err != nil || value == 0 {
		t.Fatalf("invalid Windows process creation identity %q: %v", identity, err)
	}
}

func TestWebVNCDaemonProcessCommandFromNativeQuery(t *testing.T) {
	command, alive := webVNCDaemonProcessCommand(os.Getpid())
	if !alive {
		t.Fatal("current Windows test process was not inspectable")
	}
	name := strings.ToLower(filepath.Base(os.Args[0]))
	if !strings.Contains(strings.ToLower(command), name) {
		t.Fatalf("process command=%q missing executable %q", command, name)
	}
}

func TestQueryWindowsProcessCommandLineWithMockedRuntime(t *testing.T) {
	const want = `C:\Program Files\Crabbox\crabbox.exe webvnc daemon start --id test-box nonce-123`
	got, alive := inspectWindowsProcessCommandLine(1, mockWindowsProcessCommandLineQuery(t, want))
	if !alive {
		t.Fatal("mocked live Windows process reported dead")
	}
	if got != want {
		t.Fatalf("command=%q want=%q", got, want)
	}
}

func TestInspectWindowsProcessCommandLineFailsClosed(t *testing.T) {
	query := func(windows.Handle, int32, unsafe.Pointer, uint32, *uint32) error {
		return windows.ERROR_ACCESS_DENIED
	}
	command, alive := inspectWindowsProcessCommandLine(1, query)
	if !alive || command != "" {
		t.Fatalf("command=%q alive=%v, want unverified live process", command, alive)
	}
}

func TestQueryWindowsProcessCommandLineRejectsOutOfBoundsRuntimePointer(t *testing.T) {
	query := func(_ windows.Handle, _ int32, info unsafe.Pointer, infoLen uint32, returned *uint32) error {
		header := (*windows.NTUnicodeString)(info)
		header.Length = 2
		header.MaximumLength = 2
		header.Buffer = (*uint16)(unsafe.Add(info, uintptr(infoLen)))
		*returned = infoLen
		return nil
	}
	if _, err := queryWindowsProcessCommandLine(1, query); err == nil || !strings.Contains(err.Error(), "outside query response") {
		t.Fatalf("out-of-bounds query error=%v", err)
	}
}

func mockWindowsProcessCommandLineQuery(t *testing.T, command string) windowsProcessCommandLineQuery {
	t.Helper()
	encoded, err := windows.UTF16FromString(command)
	if err != nil {
		t.Fatal(err)
	}
	return func(_ windows.Handle, class int32, info unsafe.Pointer, infoLen uint32, returned *uint32) error {
		if class != int32(windows.ProcessCommandLineInformation) {
			return windows.ERROR_INVALID_PARAMETER
		}
		headerSize := int(unsafe.Sizeof(windows.NTUnicodeString{}))
		required := headerSize + len(encoded)*2
		*returned = uint32(required)
		if int(infoLen) < required {
			return windows.ERROR_INSUFFICIENT_BUFFER
		}
		text := (*uint16)(unsafe.Add(info, uintptr(headerSize)))
		copy(unsafe.Slice(text, len(encoded)), encoded)
		*(*windows.NTUnicodeString)(info) = windows.NTUnicodeString{
			Length:        uint16((len(encoded) - 1) * 2),
			MaximumLength: uint16(len(encoded) * 2),
			Buffer:        text,
		}
		return nil
	}
}
