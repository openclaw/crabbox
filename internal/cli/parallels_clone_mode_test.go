package cli

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestParallelsCloneModeByHostAndTarget(t *testing.T) {
	for _, tc := range []struct {
		name, target, arm64, mode, snapshot, wantMode, wantErr string
		remote                                                 bool
		wantWarning                                            bool
	}{
		{name: "macOS Apple silicon default", target: targetMacOS, arm64: "1", wantMode: "full"},
		{name: "remote Apple silicon default", target: targetMacOS, arm64: "1", remote: true, wantMode: "full"},
		{name: "macOS Intel default", target: targetMacOS, arm64: "0", snapshot: "snap", wantMode: "linked"},
		{name: "Intel absent ARM feature", target: targetMacOS, snapshot: "snap", wantMode: "linked"},
		{name: "remote Intel default", target: targetMacOS, arm64: "0", remote: true, snapshot: "snap", wantMode: "linked"},
		{name: "Linux default", target: targetLinux, arm64: "1", snapshot: "snap", wantMode: "linked"},
		{name: "Windows default", target: targetWindows, arm64: "1", snapshot: "snap", wantMode: "linked"},
		{name: "explicit linked warns", target: targetMacOS, arm64: "1", mode: "linked", snapshot: "snap", wantMode: "linked", wantWarning: true},
		{name: "remote explicit linked warns", target: targetMacOS, arm64: "1", remote: true, mode: "linked", snapshot: "snap", wantMode: "linked", wantWarning: true},
		{name: "explicit linked still requires snapshot", target: targetMacOS, arm64: "1", mode: "linked", wantErr: "linked clones require --parallels-source-snapshot"},
		{name: "explicit full", target: targetMacOS, arm64: "1", mode: "full", wantMode: "full"},
		{name: "explicit unlink", target: targetMacOS, arm64: "1", mode: "unlink", wantMode: "unlink"},
		{name: "Intel explicit linked", target: targetMacOS, arm64: "0", mode: "linked", snapshot: "snap", wantMode: "linked"},
		{name: "default does not drop snapshot", target: targetMacOS, arm64: "1", snapshot: "snap", wantErr: "--parallels-source-snapshot= --parallels-source-snapshot-id="},
		{name: "host detection fails closed", target: targetMacOS, arm64: "error", wantErr: "Parallels host architecture"},
		{name: "malformed architecture fails closed", target: targetMacOS, arm64: "unknown", wantErr: "Parallels host architecture"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			cfg := BaseConfig()
			cfg.TargetOS = tc.target
			if tc.mode != "" {
				cfg.Parallels.CloneMode = tc.mode
			}
			if tc.remote {
				cfg.Parallels.Host = "mac.example"
				cfg.Parallels.HostUser = "build"
			}
			runner := &parallelsCloneModeRunner{t: t, arm64: tc.arm64, remote: tc.remote}
			client := NewParallelsClient(cfg, runner)
			var warnings []string
			client.WarnClone = func(message string) {
				if len(runner.cloneArgs) != 0 {
					t.Fatal("warning must precede clone submission")
				}
				warnings = append(warnings, message)
			}
			_, err := client.Clone(context.Background(), "source", tc.snapshot, "cbx_abcdef123456", "mode", false)
			if tc.wantWarning {
				if len(warnings) != 1 || !strings.Contains(warnings[0], "have been observed") || !strings.Contains(warnings[0], "--parallels-clone-mode full") || !strings.Contains(warnings[0], "--parallels-source-snapshot= --parallels-source-snapshot-id=") {
					t.Fatalf("missing linked-clone warning and recovery guidance: %v", warnings)
				}
			} else if len(warnings) != 0 {
				t.Fatalf("unexpected warning: %v", warnings)
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || len(runner.cloneArgs) != 0 {
					t.Fatalf("err=%v clone=%v; want %q before clone", err, runner.cloneArgs, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(runner.cloneArgs) == 0 || slices.Contains(runner.cloneArgs, "--linked") != (tc.wantMode == "linked") {
				t.Fatalf("clone args=%v, want mode=%s", runner.cloneArgs, tc.wantMode)
			}
			if tc.snapshot != "" && !slices.Contains(runner.cloneArgs, tc.snapshot) {
				t.Fatalf("clone lost snapshot selector: %v", runner.cloneArgs)
			}
		})
	}
}

func TestParallelsCloneDefaultModeIPTimeout(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	synctest.Test(t, func(t *testing.T) {
		cfg := BaseConfig()
		cfg.TargetOS = targetMacOS
		runner := &parallelsCloneModeRunner{t: t, arm64: "1"}
		client := NewParallelsClient(cfg, runner)
		if _, err := client.Clone(context.Background(), "source", "", "cbx_abcdef123456", "mode", false); err != nil {
			t.Fatal(err)
		}
		_, err := client.WaitForIP(context.Background(), "clone", time.Second, ParallelsIPWaitAcquisition)
		if err == nil || !strings.Contains(err.Error(), "clone_mode=full") || strings.Contains(err.Error(), "retry with parallels.cloneMode=full") {
			t.Fatalf("timeout must describe the actual full clone: %v", err)
		}
	})
}

type parallelsCloneModeRunner struct {
	t         *testing.T
	arm64     string
	remote    bool
	cloneArgs []string
}

func (r *parallelsCloneModeRunner) Run(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	args := req.Args
	name := req.Name
	if r.remote {
		if req.Name != directSSHExecutable() || !slices.Contains(args, "build@mac.example") {
			r.t.Fatalf("must inspect/clone on selected remote host: %s %v", name, args)
		}
		command := strings.TrimPrefix(args[len(args)-1], "PATH=/usr/local/bin:/opt/homebrew/bin:$PATH ")
		parts := strings.Fields(strings.ReplaceAll(command, "'", ""))
		name, args = parts[0], parts[1:]
	}
	if name == "/usr/sbin/sysctl" && slices.Equal(args, []string{"-n", "-i", "hw.optional.arm64"}) {
		if r.arm64 == "error" {
			return LocalCommandResult{}, errors.New("synthetic host failure")
		}
		return LocalCommandResult{Stdout: r.arm64 + "\n"}, nil
	}
	if name == "prlctl" {
		switch args[0] {
		case "clone":
			r.cloneArgs = slices.Clone(args)
			return LocalCommandResult{}, nil
		case "snapshot-list":
			return LocalCommandResult{Stdout: `{"snap":{"name":"ready","state":"poweroff"}}`}, nil
		case "list":
			return LocalCommandResult{Stdout: `[{"ID":"clone","Name":"crabbox-cbx-abcdef123456-mode","State":"running"}]`}, nil
		}
	}
	r.t.Fatalf("unexpected command: %s %v", name, args)
	return LocalCommandResult{}, errors.New("unexpected command")
}
