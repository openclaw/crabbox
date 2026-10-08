package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func TestWindowsScriptTransportDelivery(t *testing.T) {
	cases := []struct {
		name     string
		script   string
		wantFile bool
	}{
		{name: "empty", script: "", wantFile: false},
		{name: "at limit", script: strings.Repeat("a", windowsInlineScriptLimit), wantFile: false},
		{name: "over limit", script: strings.Repeat("a", windowsInlineScriptLimit+1), wantFile: true},
		{name: "UTF8 at byte limit", script: strings.Repeat("\u00e9", windowsInlineScriptLimit/2), wantFile: false},
		{name: "UTF8 over byte limit", script: strings.Repeat("\u00e9", windowsInlineScriptLimit/2) + "x", wantFile: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls, copies int
			var localPath string
			var output bytes.Buffer
			transport := windowsScriptTransport{
				run: func(ctx context.Context, target SSHTarget, command string, input io.Reader, stdout, stderr io.Writer) error {
					calls++
					if tc.script != "" && strings.Contains(command, tc.script) {
						t.Fatal("payload exposed on command line")
					}
					if tc.wantFile {
						if input != nil {
							t.Fatal("file transport sent a stdin payload")
						}
						if calls == 2 {
							if copies != 1 {
								t.Fatal("execution preceded copy")
							}
							_, _ = io.WriteString(stdout, "result")
						}
					} else {
						data, err := io.ReadAll(input)
						if err != nil || string(data) != tc.script {
							t.Fatalf("stdin=%q err=%v", data, err)
						}
						_, _ = io.WriteString(stdout, "result")
					}
					return nil
				},
				copy: func(ctx context.Context, target SSHTarget, local, remote string) error {
					copies++
					localPath = local
					if calls != 1 {
						t.Fatal("copy preceded private directory preparation")
					}
					data, err := os.ReadFile(local)
					want := append([]byte{0xEF, 0xBB, 0xBF}, []byte(tc.script)...)
					if err != nil || !bytes.Equal(data, want) {
						t.Fatalf("copied bytes differ: err=%v", err)
					}
					if !strings.HasPrefix(remote, `C:\ProgramData\crabbox\scripts\crabbox-windows-script-`) {
						t.Fatalf("remote path=%q", remote)
					}
					return nil
				},
			}
			if err := transport.execute(context.Background(), SSHTarget{}, tc.script, &output, nil); err != nil {
				t.Fatal(err)
			}
			wantCalls, wantCopies := 1, 0
			if tc.wantFile {
				wantCalls, wantCopies = 3, 1
				if _, err := os.Stat(localPath); !os.IsNotExist(err) {
					t.Fatalf("local script not removed: %v", err)
				}
			}
			if calls != wantCalls || copies != wantCopies || output.String() != "result" {
				t.Fatalf("calls=%d copies=%d output=%q", calls, copies, output.String())
			}
		})
	}
}

func TestWindowsScriptTransportFailureCleanup(t *testing.T) {
	for _, phase := range []string{"prepare", "copy", "execute", "cleanup"} {
		t.Run(phase, func(t *testing.T) {
			injected := errors.New("injected " + phase)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var events []string
			var localPath string
			transport := windowsScriptTransport{
				run: func(callCtx context.Context, target SSHTarget, command string, input io.Reader, stdout, stderr io.Writer) error {
					event := "prepare"
					if len(events) > 0 {
						event = "cleanup"
						if events[len(events)-1] == "copy" && phase != "copy" {
							event = "execute"
						}
					}
					events = append(events, event)
					if event == "cleanup" {
						if callCtx.Err() != nil {
							t.Fatal("cleanup inherited operation cancellation")
						}
						if _, bounded := callCtx.Deadline(); !bounded {
							t.Fatal("cleanup has no timeout")
						}
					}
					if event == phase {
						cancel()
						return injected
					}
					return nil
				},
				copy: func(ctx context.Context, target SSHTarget, local, remote string) error {
					localPath = local
					events = append(events, "copy")
					if phase == "copy" {
						cancel()
						return injected
					}
					return nil
				},
			}
			err := transport.execute(ctx, SSHTarget{}, strings.Repeat("x", windowsInlineScriptLimit+1), io.Discard, nil)
			if !errors.Is(err, injected) {
				t.Fatalf("error=%v, want %v", err, injected)
			}
			want := "prepare,copy,execute,cleanup"
			if phase == "prepare" {
				want = "prepare,cleanup"
			} else if phase == "copy" {
				want = "prepare,copy,cleanup"
			}
			if strings.Join(events, ",") != want {
				t.Fatalf("events=%v want=%s", events, want)
			}
			if localPath != "" {
				if _, err := os.Stat(localPath); !os.IsNotExist(err) {
					t.Fatalf("local script not removed: %v", err)
				}
			}
		})
	}
}

// The desktop launch payload embeds the launcher service source, so it must
// never be pushed through SSH stdin: Windows OpenSSH intermittently wedges its
// connection while forwarding a payload that large.
func TestWindowsDesktopLaunchPayloadUsesFileTransport(t *testing.T) {
	script := windowsDesktopLaunchPowerShell(
		`C:\crabbox\cbx_test\repo`,
		map[string]string{"CRABBOX_TEST": "1"},
		[]string{`C:\Program Files\Git\usr\bin\mintty.exe`, "-t", "Crabbox Desktop", "/usr/bin/bash", "-lc", "echo hi"},
	)
	if !windowsScriptNeedsFileTransport(script) {
		t.Fatalf("desktop launch payload (%d bytes) must use file transport", len(script))
	}
}
