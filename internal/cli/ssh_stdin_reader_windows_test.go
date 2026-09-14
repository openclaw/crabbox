//go:build windows

package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsStdinReadyWriter struct {
	buffer bytes.Buffer
	ready  chan struct{}
	once   sync.Once
}

func (w *windowsStdinReadyWriter) Write(p []byte) (int, error) {
	n, err := w.buffer.Write(p)
	if bytes.Contains(w.buffer.Bytes(), []byte("COPY_READY")) {
		w.once.Do(func() { close(w.ready) })
	}
	return n, err
}

// Win32-OpenSSH inherits overlapped byte pipes and waits for a pending write
// before closing stdin. Ordinary exec.Cmd pipes do not reproduce that contract.
func runWindowsSSHStdinCopy(t *testing.T, script string, payload []byte) ([]byte, string, error) {
	t.Helper()
	name, err := windows.UTF16PtrFromString(fmt.Sprintf(`\\.\pipe\crabbox-stdin-test-%d-%d`, os.Getpid(), time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	attributes := windows.SecurityAttributes{InheritHandle: 1}
	attributes.Length = uint32(unsafe.Sizeof(attributes))
	writer, err := windows.CreateNamedPipe(name, windows.PIPE_ACCESS_OUTBOUND|windows.FILE_FLAG_OVERLAPPED,
		windows.PIPE_TYPE_BYTE|windows.PIPE_WAIT, 1, 4096, 4096, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	var writerMu sync.Mutex
	writerClosed := false
	closeWriter := func() error {
		writerMu.Lock()
		defer writerMu.Unlock()
		if writerClosed {
			return nil
		}
		writerClosed = true
		return windows.CloseHandle(writer)
	}
	defer closeWriter()
	reader, err := windows.CreateFile(name, windows.GENERIC_READ, 0, &attributes, windows.OPEN_EXISTING, windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		t.Fatal(err)
	}
	input := os.NewFile(uintptr(reader), "overlapped-ssh-stdin")
	defer input.Close()
	event, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(event)
	overlapped := windows.Overlapped{HEvent: event}

	command := windowsPowerShellScriptCommand(t, script)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	handle := pondMeshExecCommand(ctx, nil, command.Path, command.Args[1:]...).(*pondMeshExecHandle)
	var output bytes.Buffer
	diagnostic := &windowsStdinReadyWriter{ready: make(chan struct{})}
	handle.cmd.Stdin, handle.cmd.Stdout, handle.cmd.Stderr = input, &output, diagnostic
	if err := handle.Start(); err != nil {
		t.Fatal(err)
	}
	// The child holds its inherited handle; the test must not keep a spare reader.
	if err := input.Close(); err != nil {
		cancel()
		_ = handle.Wait()
		t.Fatal(err)
	}
	written := make(chan error, 1)
	go func() {
		select {
		case <-diagnostic.ready:
		case <-ctx.Done():
			written <- ctx.Err()
			return
		}
		var count uint32
		writeErr := windows.WriteFile(writer, payload, &count, &overlapped)
		if errors.Is(writeErr, windows.ERROR_IO_PENDING) {
			writeErr = windows.GetOverlappedResult(writer, &overlapped, &count, true)
		}
		if writeErr == nil && int(count) != len(payload) {
			writeErr = fmt.Errorf("pipe wrote %d of %d bytes", count, len(payload))
		}
		// Closing after completion matches the SSH adapter's pending-write drain.
		writeErr = errors.Join(writeErr, closeWriter())
		written <- writeErr
	}()
	runErr := handle.Wait()
	writerMu.Lock()
	if !writerClosed {
		_ = windows.CancelIoEx(writer, &overlapped)
	}
	writerMu.Unlock()
	writeErr := <-written
	if ctx.Err() != nil {
		t.Fatalf("PowerShell stdin reader did not settle: %v; stderr=%s", ctx.Err(), diagnostic.buffer.String())
	}
	if writeErr != nil {
		t.Fatalf("stdin delivery: %v; PowerShell=%v; stderr=%s", writeErr, runErr, diagnostic.buffer.String())
	}
	return output.Bytes(), diagnostic.buffer.String(), runErr
}

func TestWindowsSSHStdinReaderBehavior(t *testing.T) {
	for _, test := range []struct {
		name      string
		declared  int
		supplied  int
		trailer   bool
		truncated bool
	}{
		{name: "small_frame_preserves_following_byte", declared: 48, supplied: 48, trailer: true},
		{name: "exact_frame_preserves_following_byte", declared: 4994, supplied: 4994, trailer: true},
		{name: "larger_than_copy_buffer", declared: 65537, supplied: 65537},
		{name: "premature_eof", declared: 4994, supplied: 48, truncated: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := make([]byte, test.supplied)
			for i := range payload {
				payload[i] = byte(i % 251)
			}
			want := fmt.Sprintf("%x", sha256.Sum256(payload))
			tailCheck := ""
			if test.trailer {
				payload = append(payload, 42)
				tailCheck = `if ([Console]::OpenStandardInput().ReadByte() -ne 42) { throw "frame consumed following byte or closed stdin" }` + "\n"
			}
			script := `$ErrorActionPreference = "Stop"
$memory = New-Object IO.MemoryStream
[Console]::Error.WriteLine("COPY_READY")
` + windowsPowerShellCopyExactInput("$memory", int64(test.declared)) + tailCheck + `
$hash = [Security.Cryptography.SHA256]::Create()
try { Write-Output ([BitConverter]::ToString($hash.ComputeHash($memory.ToArray()))).Replace("-", "").ToLowerInvariant() } finally { $hash.Dispose(); $memory.Dispose() }
`
			out, diagnostic, err := runWindowsSSHStdinCopy(t, script, payload)
			if test.truncated {
				if err == nil || !strings.Contains(diagnostic, "SSH stdin ended before the framed payload") || len(bytes.TrimSpace(out)) != 0 {
					t.Fatalf("truncated frame: err=%v output=%q stderr=%s", err, out, diagnostic)
				}
				return
			}
			if err != nil || strings.TrimSpace(string(out)) != want {
				t.Fatalf("copy err=%v hash=%q want=%q stderr=%s", err, out, want, diagnostic)
			}
		})
	}
}

func TestWindowsWitnessRedirectedStdinUploadBehavior(t *testing.T) {
	workdir := t.TempDir()
	payload := []byte("Write-Output 'utf8 café'\r\n")
	input := filepath.Join(workdir, "input")
	if err := os.WriteFile(input, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(workdir, "upload.ps1")
	if err := os.WriteFile(child, []byte(decodePowerShellCommand(t, windowsRemoteUploadUTF8BOMFileCommand(workdir, "result.ps1"))), 0o600); err != nil {
		t.Fatal(err)
	}
	powerShell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Fatal(err)
	}
	// This is the witness's real redirected-child boundary, not SSH stdin.
	script := `$ErrorActionPreference = "Stop"
$child = ` + psQuote(child) + `
$quoted = '"' + $child + '"'
$process = Start-Process -FilePath ` + psQuote(powerShell) + ` -ArgumentList @("-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", $quoted) -RedirectStandardInput ` + psQuote(input) + ` -NoNewWindow -PassThru -Wait
exit $process.ExitCode
`
	if out, err := runWindowsPowerShellScript(t, script); err != nil {
		t.Fatalf("redirected upload: %v: %s", err, out)
	}
	got, err := os.ReadFile(filepath.Join(workdir, "result.ps1"))
	want := append([]byte{0xef, 0xbb, 0xbf}, payload...)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("redirected bytes err=%v got=%x want=%x", err, got, want)
	}
}
