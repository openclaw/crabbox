package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Stock BSD tools only: no flock, lockf, GNU stat/date, or /proc helpers.
func workspaceOwnerBSDPath(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX sh")
	}
	dir := t.TempDir()
	for _, name := range []string{"sh", "uname", "mkdir", "rmdir", "chmod", "sed", "mv", "rm", "ps", "tr", "cut", "awk", "sleep", "cat", "base64", "wc", "nohup"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	date, err := exec.LookPath("date")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "date"), []byte("#!/bin/sh\n[ \"$#\" -eq 1 ] && [ \"$1\" = +%s ] || exit 64\nexec "+shellQuote(date)+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestWorkspaceOwnerBSDProtocol(t *testing.T) {
	path := workspaceOwnerBSDPath(t)
	key, token := workspaceOwnerKey("bsd-protocol"), strings.Repeat("a", 64)
	for _, tc := range []struct {
		name, state, child, gate string
		action                   workspaceOwnerAction
		want                     string
		code                     int
	}{
		{name: "acquire", action: workspaceOwnerAcquire, want: "ACQUIRED"},
		{name: "busy", state: "live", action: workspaceOwnerAcquire, want: "BUSY"},
		{name: "renew", state: "live", action: workspaceOwnerRenew, want: "RENEWED"},
		{name: "inspect", state: "live", action: workspaceOwnerInspect, want: "OWNED"},
		{name: "release", state: "live", action: workspaceOwnerRelease, want: "RELEASED"},
		{name: "recovery", state: "expired", action: workspaceOwnerAcquire, want: "RECOVERED"},
		{name: "expired", state: "expired", action: workspaceOwnerRenew, want: "EXPIRED", code: 75},
		{name: "mismatch", state: "other", action: workspaceOwnerRenew, want: "MISMATCH", code: 75},
		{name: "malformed", state: "invalid", action: workspaceOwnerAcquire, want: "AMBIGUOUS", code: 74},
		{name: "malformed child", state: "expired", child: "bad", action: workspaceOwnerAcquire, want: "AMBIGUOUS", code: 74},
		{name: "live child blocks recovery", state: "expired", child: "live", action: workspaceOwnerAcquire, want: "CHILD"},
		{name: "live child blocks release", state: "live", child: "live", action: workspaceOwnerRelease, want: "CHILD", code: 75},
		{name: "occupied gate cannot be stolen", state: "expired", gate: "directory", action: workspaceOwnerAcquire, want: "BUSY"},
		{name: "invalid gate", gate: "file", action: workspaceOwnerAcquire, want: "AMBIGUOUS", code: 74},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			root := filepath.Join(home, ".crabbox", "workspace-owners")
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			if tc.state != "" {
				stateToken, expiry := token, "9999999999"
				if tc.state == "expired" {
					expiry = "1"
				}
				if tc.state == "other" {
					stateToken = strings.Repeat("b", 64)
				}
				state := "v1\n" + stateToken + "\n" + expiry + "\n"
				if tc.state == "invalid" {
					state = "invalid"
				}
				if err := os.WriteFile(filepath.Join(root, key+".owner"), []byte(state), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.child != "" {
				child := tc.child
				if child == "live" {
					pid := strconv.Itoa(os.Getpid())
					identity, err := exec.Command("ps", "-o", "lstart=", "-p", pid).Output()
					if err != nil {
						t.Fatal(err)
					}
					child = pid + "\n" + strings.Join(strings.Fields(string(identity)), " ") + "\n"
				}
				if err := os.WriteFile(filepath.Join(root, key+".child"), []byte(child), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			gate := filepath.Join(root, key+".gate.portable")
			if tc.gate == "directory" {
				if err := os.Mkdir(gate, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if tc.gate == "file" {
				if err := os.WriteFile(gate, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/bin/sh", "-c", remoteWorkspaceOwnerPOSIX(workspaceOwnerRemoteRequest{Action: tc.action, Key: key, Token: token, TTL: time.Minute}))
			cmd.Env = []string{"HOME=" + home, "PATH=" + path}
			out, err := cmd.CombinedOutput()
			if string(out) != tc.want || cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != tc.code {
				t.Fatalf("out=%q err=%v; want %s / %d", out, err, tc.want, tc.code)
			}
			if tc.gate == "" {
				if _, err := os.Stat(gate); !os.IsNotExist(err) {
					t.Fatalf("gate leaked: %v", err)
				}
			}
		})
	}
}

func TestWorkspaceOwnerBSDConcurrentAcquireAndWitness(t *testing.T) {
	path, home := workspaceOwnerBSDPath(t), t.TempDir()
	key, token := workspaceOwnerKey("bsd-concurrent"), strings.Repeat("a", 64)
	run := func(script string) (string, error) {
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
		cmd.Env = []string{"HOME=" + home, "PATH=" + path}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	req := workspaceOwnerRemoteRequest{Action: workspaceOwnerAcquire, Key: key, Token: token, TTL: time.Minute}
	results := make(chan string, 12)
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			out, err := run(remoteWorkspaceOwnerPOSIX(req))
			if err != nil {
				results <- err.Error()
			} else {
				results <- out
			}
		})
	}
	wg.Wait()
	close(results)
	winners := 0
	for out := range results {
		if out == "ACQUIRED" {
			winners++
		} else if out != "BUSY" {
			t.Errorf("contender: %q", out)
		}
	}
	if winners != 1 {
		t.Fatalf("acquisition winners=%d", winners)
	}
	for _, tc := range []struct {
		command string
		code    int
	}{{"printf witness-ok", 0}, {"exit 23", 23}, {"printf later-ok", 0}} {
		out, err := run(remoteWorkspaceOwnerPOSIXWitness(key, token, tc.command))
		code := 0
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else {
				t.Fatal(err)
			}
		}
		if code != tc.code {
			t.Fatalf("witness %q out=%q err=%v", tc.command, out, err)
		}
	}
	req.Action = workspaceOwnerRelease
	if out, err := run(remoteWorkspaceOwnerPOSIX(req)); err != nil || out != "RELEASED" {
		t.Fatalf("release out=%q err=%v", out, err)
	}
}

func TestWorkspaceOwnerBSDDetachedAndRenewingCommand(t *testing.T) {
	testWorkspaceOwnerDetachedAndRenewingCommand(t, workspaceOwnerBSDPath(t))
}

func TestWorkspaceOwnerPOSIXDetachedAndRenewingCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX shell")
	}
	testWorkspaceOwnerDetachedAndRenewingCommand(t, os.Getenv("PATH"))
}

func testWorkspaceOwnerDetachedAndRenewingCommand(t *testing.T, path string) {
	for _, tc := range []struct {
		name, command string
		code          int
	}{
		{name: "detached daemon", command: `nohup sleep 30 </dev/null >"$HOME/daemon.log" 2>&1 & echo $! >"$HOME/daemon.pid"`},
		{name: "renewed stream", command: `i=0; while [ "$i" -lt 5 ]; do echo tick; sleep 1; i=$((i+1)); done; exit 23`, code: 23},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			key, token := workspaceOwnerKey(tc.name), strings.Repeat("c", 64)
			run := func(script string) (string, error) {
				ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
				cmd.Env = []string{"HOME=" + home, "PATH=" + path}
				out, err := cmd.CombinedOutput()
				return string(out), err
			}
			req := workspaceOwnerRemoteRequest{Action: workspaceOwnerAcquire, Key: key, Token: token, TTL: 3 * time.Second}
			if out, err := run(remoteWorkspaceOwnerPOSIX(req)); err != nil || out != "ACQUIRED" {
				t.Fatalf("acquire=%q %v", out, err)
			}
			done := make(chan struct{})
			renewed := make(chan error, 1)
			go func() {
				ticker := time.NewTicker(500 * time.Millisecond)
				defer ticker.Stop()
				renew := req
				renew.Action = workspaceOwnerRenew
				for {
					select {
					case <-done:
						renewed <- nil
						return
					case <-ticker.C:
						out, err := run(remoteWorkspaceOwnerPOSIX(renew))
						if err != nil || out != "RENEWED" {
							renewed <- fmt.Errorf("renew=%q %v", out, err)
							return
						}
					}
				}
			}()
			out, err := run(remoteWorkspaceOwnerPOSIXWitness(key, token, tc.command))
			close(done)
			renewErr := <-renewed
			if renewErr != nil {
				t.Fatal(renewErr)
			}
			code := 0
			if err != nil {
				if ee, ok := err.(*exec.ExitError); ok {
					code = ee.ExitCode()
				} else {
					t.Fatal(err)
				}
			}
			if code != tc.code {
				t.Fatalf("command=%q %v want %d", out, err, tc.code)
			}
			if tc.code == 23 && strings.Count(out, "tick\n") != 5 {
				t.Fatalf("lost stream output: %q", out)
			}
			if tc.name == "detached daemon" {
				data, err := os.ReadFile(filepath.Join(home, "daemon.pid"))
				if err != nil {
					t.Fatal(err)
				}
				pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
				if err != nil {
					t.Fatal(err)
				}
				process, err := os.FindProcess(pid)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = process.Kill() })
				time.Sleep(time.Second)
				if out, err := run(remoteWorkspaceOwnerPOSIXWitness(key, token, "kill -0 "+strconv.Itoa(pid))); err != nil {
					t.Fatalf("daemon did not survive subsequent command: %q %v", out, err)
				}
			}
			req.Action = workspaceOwnerRelease
			if out, err := run(remoteWorkspaceOwnerPOSIX(req)); err != nil || out != "RELEASED" {
				t.Fatalf("release=%q %v", out, err)
			}
		})
	}
}
