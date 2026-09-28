//go:build darwin || linux

package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWorkspaceOwnerInputRejectsTruncationBeforeExecution(t *testing.T) {
	for _, truncated := range []int{0, 1, 4, 256, -1} {
		t.Run(strconv.Itoa(truncated), func(t *testing.T) {
			home := t.TempDir()
			marker := filepath.Join(home, "owner-mutated")
			script := "touch " + shellQuote(marker) + "\n#" + strings.Repeat("x", 256)
			for len(script)%3 != 1 {
				script += "x"
			}
			input := base64.StdEncoding.EncodeToString([]byte(script))
			if truncated == -1 {
				input = ""
			} else {
				input = input[:len(input)-truncated]
			}
			command := remoteWorkspaceOwnerPOSIXInputLauncher(workspaceOwnerKey("fixture"), strings.Repeat("a", 64), len(script))
			cmd := exec.CommandContext(t.Context(), "sh", "-c", command)
			cmd.Env = append(os.Environ(), "HOME="+home)
			cmd.Stdin = strings.NewReader(input)
			output, err := cmd.CombinedOutput()
			if truncated == 0 {
				if err != nil {
					t.Fatalf("complete frame: %v: %s", err, output)
				}
				if _, err := os.Stat(marker); err != nil {
					t.Fatal(err)
				}
			} else {
				if exitCode(err) != 74 {
					t.Fatalf("truncated frame: err=%v output=%s", err, output)
				}
				if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("truncated frame executed: %v", err)
				}
			}
			entries, err := os.ReadDir(filepath.Join(home, ".crabbox", "workspace-owners"))
			if err != nil || len(entries) != 0 {
				t.Fatalf("staging remained: entries=%v err=%v", entries, err)
			}
		})
	}
}

func TestWorkspaceOwnerControlReusesConnectionAndJoinsClose(t *testing.T) {
	for _, cancelParent := range []bool{false, true} {
		t.Run(strconv.FormatBool(cancelParent), func(t *testing.T) {
			server := newForwardSSHServer(t, "fixture")
			close(server.release)
			server.enableReadinessSessions(t, t.TempDir(), true)
			target := SSHTarget{Host: "127.0.0.1", Port: strconv.Itoa(server.port()), User: "fixture", TargetOS: targetLinux,
				FallbackPorts: []string{}, DisableHostKeyChecking: true}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			control, closeControl, err := startWorkspaceOwnerControl(ctx, target)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = closeControl() })
			if control.ownerControlPath == "" || target.ownerControlPath != "" {
				t.Fatal("control connection must be private to owner target")
			}
			if cancelParent {
				cancel()
			}
			// Owner release and inspection still work after caller cancellation.
			for range 3 {
				if err := runSSHQuiet(context.WithoutCancel(ctx), control, "exit 0"); err != nil {
					t.Fatal(err)
				}
			}
			server.mu.Lock()
			handshakes := len(server.users)
			server.mu.Unlock()
			if handshakes != 1 {
				t.Fatalf("three control calls used %d authentications, want one", handshakes)
			}
			if err := closeControl(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(filepath.Dir(control.ownerControlPath)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("control directory survived close: %v", err)
			}
			if err := closeControl(); err != nil {
				t.Fatalf("repeated close: %v", err)
			}
		})
	}
}

func TestWorkspaceOwnerControlPreservesUnsupportedRoutes(t *testing.T) {
	for _, target := range []SSHTarget{
		{TargetOS: targetWindows}, {NoControlMaster: true}, {AuthSecret: true},
		{AuthoritativeKnownHosts: true}, {SSHConfigProxy: true}, {SSHConfigFile: "/provider/config"}, {ProxyCommand: "provider proxy"},
	} {
		got, closeControl, err := startWorkspaceOwnerControl(t.Context(), target)
		if err != nil || got.ownerControlPath != "" {
			t.Fatalf("unsupported route acquired control: %+v %v", got, err)
		}
		if err := closeControl(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWorkspaceOwnerControlSupportsOlderSSH(t *testing.T) {
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nfor arg do\n case \"$arg\" in ForkAfterAuthentication=*) exit 255;; esac\ndone\nexec " + shellQuote(ssh) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	server := newForwardSSHServer(t, "fixture")
	close(server.release)
	target := SSHTarget{Host: "127.0.0.1", Port: strconv.Itoa(server.port()), User: "fixture", TargetOS: targetLinux,
		FallbackPorts: []string{}, DisableHostKeyChecking: true}
	_, closeControl, err := startWorkspaceOwnerControl(t.Context(), target)
	if err != nil {
		t.Fatal(err)
	}
	if err := closeControl(); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceOwnerControlFallsBackAfterEarlyExit(t *testing.T) {
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip(err)
	}
	for _, code := range []string{"0", "127"} {
		t.Run(code, func(t *testing.T) {
			dir := t.TempDir()
			controlLog := filepath.Join(dir, "control-path")
			script := "#!/bin/sh\nmaster=; previous=\nfor arg do\n" +
				" if [ \"$previous\" = -S ]; then printf %s \"$arg\" > " + shellQuote(controlLog) + "; fi\n" +
				" [ \"$arg\" != -N ] || master=1\n previous=$arg\ndone\n" +
				"[ -z \"$master\" ] || exit " + code + "\nexec " + shellQuote(ssh) + " \"$@\"\n"
			if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			server := newForwardSSHServer(t, "fixture")
			close(server.release)
			server.enableReadinessSessions(t, t.TempDir(), true)
			target := SSHTarget{Host: "127.0.0.1", Port: strconv.Itoa(server.port()), User: "fixture", TargetOS: targetLinux,
				FallbackPorts: []string{}, DisableHostKeyChecking: true}
			got, closeControl, err := startWorkspaceOwnerControl(t.Context(), target)
			if err != nil || got.ownerControlPath != "" || !got.NoControlMaster {
				t.Fatalf("direct fallback: target=%+v err=%v", got, err)
			}
			if err := runSSHQuiet(t.Context(), got, "exit 0"); err != nil {
				t.Fatal(err)
			}
			if err := closeControl(); err != nil {
				t.Fatal(err)
			}
			path, err := os.ReadFile(controlLog)
			if err != nil || len(path) == 0 {
				t.Fatalf("control path: %q, %v", path, err)
			}
			if _, err := os.Lstat(filepath.Dir(string(path))); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed master left its directory: %v", err)
			}
		})
	}
}

func TestWorkspaceOwnerControlCanceledStartup(t *testing.T) {
	server := newForwardSSHServer(t, "fixture")
	// Withhold authentication until after startup is canceled.
	defer close(server.release)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	target := SSHTarget{Host: "127.0.0.1", Port: strconv.Itoa(server.port()), User: "fixture", TargetOS: targetLinux,
		FallbackPorts: []string{}, DisableHostKeyChecking: true}
	got, closeControl, err := startWorkspaceOwnerControl(ctx, target)
	if !errors.Is(err, context.DeadlineExceeded) || got.ownerControlPath != "" {
		t.Fatalf("canceled startup: target=%+v err=%v", got, err)
	}
	if err := closeControl(); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceOwnerCloseJoinsControlAfterReleaseFailure(t *testing.T) {
	closed := false
	owner, err := acquireWorkspaceOwnerWithTransport(t.Context(), SSHTarget{}, "fixture", io.Discard,
		workspaceOwnerTransportFunc(func(_ context.Context, req workspaceOwnerRemoteRequest) (string, error) {
			if req.Action == workspaceOwnerAcquire {
				return "ACQUIRED", nil
			}
			if closed {
				t.Error("control closed before owner release")
			}
			return "AMBIGUOUS", errors.New("fixture release failure")
		}), time.Second, time.Minute, time.Minute/2)
	if err != nil {
		t.Fatal(err)
	}
	owner.closeTransport = func() error { closed = true; return nil }
	if err := owner.Close(t.Context()); err == nil || !strings.Contains(err.Error(), "fixture release failure") || !closed {
		t.Fatalf("release failure must still close control: err=%v closed=%t", err, closed)
	}
}

func TestWorkspaceOwnerQuiesceClosesControlBeforeDestructiveRelease(t *testing.T) {
	closed := false
	owner, err := acquireWorkspaceOwnerWithTransport(t.Context(), SSHTarget{}, "fixture", io.Discard,
		workspaceOwnerTransportFunc(func(_ context.Context, req workspaceOwnerRemoteRequest) (string, error) {
			if req.Action == workspaceOwnerAcquire {
				return "ACQUIRED", nil
			}
			return "OWNED", nil
		}), time.Second, time.Minute, time.Minute/2)
	if err != nil {
		t.Fatal(err)
	}
	owner.closeTransport = func() error { closed = true; return nil }
	if err := owner.QuiesceForLeaseRelease(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !closed {
		t.Fatal("destructive release can discard the owner with its control connection still open")
	}
}
