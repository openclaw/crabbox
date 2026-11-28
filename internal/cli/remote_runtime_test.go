package cli

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/crabbox/internal/runtimeartifact"
)

func TestSSHTransportPreservesCancellationCause(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX SSH process fixture")
	}
	for _, route := range []string{"posix", "metadata", "windows", "plain", "diagnostics"} {
		for _, tc := range []struct {
			name  string
			cause error
			code  int
		}{
			{name: "success"},
			{name: "remote failure", code: 23},
			{name: "cancel", cause: errors.New("native control canceled by caller")},
			{name: "deadline", cause: context.DeadlineExceeded},
		} {
			t.Run(route+"/"+tc.name, func(t *testing.T) {
				dir := t.TempDir()
				isolateRunTestUserDirs(t, dir)
				command := fmt.Sprintf("exit %d", tc.code)
				if tc.cause != nil {
					command = "exec sleep 30"
				}
				writeExecutable(t, filepath.Join(dir, "ssh"), "#!/bin/sh\nprintf ready\n"+command+"\n")
				t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				var output bytes.Buffer
				stdout := writerFunc(func(p []byte) (int, error) {
					if tc.cause != nil {
						cancel(tc.cause)
					}
					return output.Write(p)
				})
				r := remoteNativeRuntime{target: SSHTarget{Host: "example.invalid", Port: "22", TargetOS: targetLinux, NoControlMaster: true}}
				var err error
				switch route {
				case "metadata":
					err = r.runMetadata(ctx, []string{"identity"}, stdout)
				case "posix":
					err = r.runPOSIX(ctx, "true", stdout)
				case "windows":
					r.target.TargetOS = targetWindows
					err = r.runWindows(ctx, "exit 0", stdout)
				default:
					transport := sshTransportPreparation{command: "true"}
					_, err = transport.runOnce(ctx, r.target, "2", "1", stdout, io.Discard, route == "diagnostics")
					if !transport.dispatched || transport.retirementUnconfirmed != (tc.cause != nil) {
						t.Fatalf("dispatched=%t retirementUnconfirmed=%t cause=%v", transport.dispatched, transport.retirementUnconfirmed, tc.cause)
					}
				}
				if output.String() != "ready" {
					t.Fatalf("transport did not start: output=%q error=%v", output.String(), err)
				}
				if tc.cause != nil {
					var exit *exec.ExitError
					if !errors.Is(err, tc.cause) || !errors.As(err, &exit) {
						t.Fatalf("canceled transport lost cause or process error: %v", err)
					}
				} else if exitCode(err) != tc.code {
					t.Fatalf("exit=%d error=%v, want %d", exitCode(err), err, tc.code)
				}
			})
		}
	}
}

func nativeRuntimeTestTarget(t *testing.T, checksumTool, gzipTool bool) SSHTarget {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX installation fixture")
	}
	root := t.TempDir()
	tools, transport := filepath.Join(root, "tools"), filepath.Join(root, "transport")
	for _, dir := range []string{tools, transport} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	names := []string{"sh", "env", "cat", "mkdir", "head", "wc", "chmod", "mv", "rm"}
	if checksumTool {
		names = append(names, "shasum")
	}
	if gzipTool {
		names = append(names, "gzip")
	}
	for _, name := range names {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("%s unavailable", name)
		}
		if err := os.Symlink(path, filepath.Join(tools, name)); err != nil {
			t.Fatal(err)
		}
	}
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("ssh unavailable")
	}
	isolateRunTestUserDirs(t, root)
	installPOSIXRunSSH(t, transport, ssh, os.Getenv("HOME"))
	t.Setenv("CRABBOX_POSIX_REMOTE_PATH", tools)
	t.Setenv("CRABBOX_POSIX_BASH_AVAILABLE", "false")
	t.Setenv("PATH", transport+string(os.PathListSeparator)+os.Getenv("PATH"))
	return SSHTarget{User: "fixture", Host: "127.0.0.1", Port: startTCPReadinessFixture(t), TargetOS: targetLinux, NoControlMaster: true}
}

func TestNativeRuntimeUploadAndVerification(t *testing.T) {
	for _, tc := range []struct {
		name                                 string
		checksum, short, compressed, damaged bool
	}{
		{name: "checksum utility", checksum: true},
		{name: "authenticated readback"},
		{name: "short upload", short: true},
		{name: "gzip checksum", compressed: true, checksum: true},
		{name: "gzip readback", compressed: true},
		{name: "damaged gzip", compressed: true, damaged: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := nativeRuntimeTestTarget(t, tc.checksum, tc.compressed)
			nonce, err := randomHex(16)
			if err != nil {
				t.Fatal(err)
			}
			h := &remoteNativeRuntime{target: target, nonce: nonce, path: "/tmp/crabbox-runtime-" + nonce + "/crabbox"}
			t.Cleanup(func() {
				if err := h.close(t.Context()); err != nil {
					t.Error(err)
				}
			})
			payload := []byte{'f', 'i', 'x', 't', 'u', 'r', 'e', 0, 255, '\n'}
			transportBytes := payload
			if tc.compressed {
				var packed bytes.Buffer
				writer := gzip.NewWriter(&packed)
				if _, err := writer.Write(payload); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				transportBytes = packed.Bytes()
				if tc.damaged {
					transportBytes = transportBytes[:len(transportBytes)-3]
				}
			}
			size := int64(len(transportBytes))
			if tc.short {
				size++
			}
			upload := sshTransportPreparation{command: nativeRuntimeUploadCommand(nonce, size, int64(len(payload)), tc.compressed), direct: bytes.NewReader(transportBytes)}
			_, err = upload.runOnce(t.Context(), target, "2", "1", io.Discard, io.Discard, false)
			if tc.short || tc.damaged {
				if err == nil {
					t.Fatal("short upload accepted")
				}
				if _, err := os.Lstat(h.path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("incomplete executable published: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				got, err := os.ReadFile(h.path)
				if err != nil || !bytes.Equal(got, payload) {
					t.Fatalf("uploaded bytes %q: %v", got, err)
				}
				info, err := os.Stat(h.path)
				if err != nil || info.Mode().Perm() != 0500 {
					t.Fatalf("uploaded executable mode: %v %v", info, err)
				}
				sum := sha256.Sum256(payload)
				if err := verifyNativeRuntimeBytes(t.Context(), h, runtimeartifact.Identity{Size: int64(len(payload)), SHA256: hex.EncodeToString(sum[:])}); err != nil {
					t.Fatal(err)
				}
			}
			if err := h.close(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(filepath.Dir(h.path)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("runtime staging retained: %v", err)
			}
		})
	}
}

func TestNativeRuntimeIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, record string
		valid        bool
	}{
		{"matching", `{"protocol":"CBX-REMOTE-1","os":"linux","arch":"amd64","runnable":true}`, true},
		{"wrong architecture", `{"protocol":"CBX-REMOTE-1","os":"linux","arch":"arm64","runnable":true}`, false},
		{"unsupported", `{"protocol":"CBX-REMOTE-1","os":"darwin","arch":"amd64","runnable":false}`, false},
		{"old protocol", `{"protocol":"CBX-REMOTE-0","os":"linux","arch":"amd64","runnable":true}`, false},
		{"trailing output", `{"protocol":"CBX-REMOTE-1","os":"linux","arch":"amd64","runnable":true} extra`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateNativeRuntimeIdentity([]byte(tc.record), "amd64"); (err == nil) != tc.valid {
				t.Fatalf("identity error=%v want valid=%t", err, tc.valid)
			}
		})
	}
}

func TestNativeRuntimeUploadPreparation(t *testing.T) {
	payload := []byte{0, 13, 10, 128, 255, 'x'}
	for _, wsl := range []bool{false, true} {
		r := &remoteNativeRuntime{nonce: strings.Repeat("a", 32), target: SSHTarget{TargetOS: targetLinux}}
		if wsl {
			r.target.TargetOS, r.target.WindowsMode, r.shell = targetWindows, windowsModeWSL2, wslStagePowerShell
		}
		transport, input, err := r.prepareUpload(bytes.NewReader(payload), int64(len(payload)), 15, true, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := input.close(); err != nil {
				t.Error(err)
			}
		})
		want := bytes.Clone(payload)
		if wsl {
			want = append([]byte(nativeRuntimeUploadScript(r.nonce, int64(len(payload)), 15, true)), payload...)
			if len(transport.command) >= wslStageLauncherCommandLimit || !strings.HasPrefix(transport.command, "& ([ScriptBlock]::Create(") {
				t.Fatal("WSL upload bypassed the selected PowerShell route")
			}
		} else if transport.command != nativeRuntimeUploadCommand(r.nonce, int64(len(payload)), 15, true) {
			t.Fatal("Linux uploader changed its command")
		}
		for range 2 {
			reader, err := transport.reset()
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(reader)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("wsl=%t replay bytes differ: %v", wsl, err)
			}
		}
	}
}

func TestNativeRuntimeReplayDispatch(t *testing.T) {
	target := nativeRuntimeTestTarget(t, false, false)
	prefix, payload := []byte("owned installer\n"), []byte{0, 13, 10, 128, 255}
	input, err := newReplayableSSHInputStream(prefix, bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := input.close(); err != nil {
			t.Error(err)
		}
	}()
	transport := sshTransportPreparation{command: remotePOSIXControlCommand("exec cat"), replay: input}
	for range 2 {
		var output bytes.Buffer
		if _, err := transport.runOnce(t.Context(), target, "2", "1", &output, io.Discard, false); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(output.Bytes(), append(bytes.Clone(prefix), payload...)) {
			t.Fatalf("replayed SSH input differs: %x", output.Bytes())
		}
	}
}
