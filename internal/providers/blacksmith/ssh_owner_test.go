//go:build darwin || linux

package blacksmith

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestBlacksmithRunOwnsNativeSSHMaster(t *testing.T) {
	for _, mode := range []string{"success", "error", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			isolateBlacksmithOwnership(t)
			t.Setenv("CRABBOX_CONTROLLER_PROCESS_TREE_OWNED", "")
			bin := t.TempDir()
			t.Setenv("PATH", bin+":/usr/bin:/bin")
			t.Setenv("CRABBOX_TEST_NATIVE_SSH_MODE", mode)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			_ = listener.(*net.TCPListener).SetDeadline(time.Now().Add(10 * time.Second))
			t.Setenv("CRABBOX_TEST_NATIVE_SSH_ADDRESS", listener.Addr().String())
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ssh := "#!/bin/sh\nexec " + core.ShellQuote(executable) + " -test.run=^TestBlacksmithNativeSSHHelper$ -- \"$@\"\n"
			// These are the native CLI's options, including its shared control path.
			native := "#!/bin/sh\nssh -o ControlMaster=auto -o ControlPath=/tmp/native-blacksmith-unused.sock -o ControlPersist=600 fixture\n"
			if mode == "error" {
				native += "exit 7\n"
			}
			for name, body := range map[string]string{"ssh": ssh, "blacksmith": native} {
				if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			const id = "tbx_native_ssh_owner"
			prepareBlacksmithGuestKey(t, id)
			backend := newTestBlacksmithBackend(core.BaseConfig(), core.RuntimeForProviderOperation(io.Discard).Exec)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan blacksmithRunOutcome, 1)
			go func() {
				done <- backend.runTestbox(ctx, id, []string{"true"}, false, false, nil, nil, nil, nil)
			}()
			child, err := listener.Accept()
			if err != nil {
				t.Fatal(err)
			}
			defer child.Close()
			_ = child.SetDeadline(time.Now().Add(10 * time.Second))
			var identity struct{ PID, Group int }
			if err := json.NewDecoder(child).Decode(&identity); err != nil {
				t.Fatal(err)
			}
			// Reap the deliberately escaped fixture on a red run as well.
			live := true
			t.Cleanup(func() {
				if live {
					_ = syscall.Kill(identity.PID, syscall.SIGKILL)
				}
			})
			if mode == "cancel" {
				cancel()
			} else if _, err := child.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
			select {
			case outcome := <-done:
				if mode == "success" && outcome.code != 0 || mode == "error" && outcome.code != 7 || mode == "cancel" && outcome.code == 0 {
					t.Fatalf("%s exit=%d", mode, outcome.code)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("native SSH command did not return")
			}
			_ = child.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			var signal [1]byte
			if _, err := child.Read(signal[:]); !errors.Is(err, io.EOF) && !errors.Is(err, syscall.ECONNRESET) {
				t.Fatalf("native SSH master survived Run return: pid=%d group=%d err=%v", identity.PID, identity.Group, err)
			}
			live = false
		})
	}
}

// Model OpenSSH's first-option-wins parsing and ControlPersist double fork:
// the master has its own session and no inherited console pipes.
func TestBlacksmithNativeSSHHelper(t *testing.T) {
	address := os.Getenv("CRABBOX_TEST_NATIVE_SSH_ADDRESS")
	if address == "" {
		return
	}
	if os.Getenv("CRABBOX_TEST_NATIVE_SSH_CHILD") == "1" {
		conn, err := net.Dial("tcp", address)
		if err != nil {
			os.Exit(90)
		}
		_ = json.NewEncoder(conn).Encode(struct{ PID, Group int }{os.Getpid(), syscall.Getpgrp()})
		var signal [1]byte
		_, _ = conn.Read(signal[:])
		if os.Getenv("CRABBOX_TEST_NATIVE_SSH_PERSIST") == "1" {
			_, _ = conn.Read(signal[:])
		}
		_ = conn.Close()
		os.Exit(0)
	}
	options := map[string]string{}
	for i, arg := range os.Args {
		if arg == "-o" && i+1 < len(os.Args) {
			name, value, ok := strings.Cut(os.Args[i+1], "=")
			if ok && options[name] == "" {
				options[name] = value
			}
		}
	}
	persist := options["ControlMaster"] != "no" && options["ControlPersist"] != "no" && options["ControlPath"] != "none"
	cmd := exec.Command(os.Args[0], "-test.run=^TestBlacksmithNativeSSHHelper$")
	cmd.Env = append(os.Environ(), "CRABBOX_TEST_NATIVE_SSH_CHILD=1")
	if persist {
		cmd.Env = append(cmd.Env, "CRABBOX_TEST_NATIVE_SSH_PERSIST=1")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
	if err := cmd.Start(); err != nil {
		os.Exit(91)
	}
	if !persist || os.Getenv("CRABBOX_TEST_NATIVE_SSH_MODE") == "cancel" {
		if err := cmd.Wait(); err != nil {
			os.Exit(92)
		}
	}
	os.Exit(0)
}
