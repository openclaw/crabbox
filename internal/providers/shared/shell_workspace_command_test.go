package shared

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestShellWorkspaceCommandNative(t *testing.T) {
	if os.PathSeparator != '/' {
		t.Skip("POSIX workspace command")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh unavailable")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash unavailable")
	}
	for _, tc := range []struct {
		name    string
		command []string
		literal map[int]bool
		shell   bool
		want    string
		code    int
	}{
		{"argv", []string{"printf", "%s", "literal value"}, nil, false, "literal value", 0},
		{"explicit", []string{"printf '%s' \"$VALUE\"; exit 7"}, nil, true, "quotes ' and $()\nline", 7},
		{"inferred", []string{"printf '%s' inferred && printf '%s' source"}, nil, false, "inferredsource", 0},
		{"mixed", []string{"printf", "%s", ";", "&&", "printf", "%s", "done"}, map[int]bool{2: true}, false, ";done", 0},
		{"assignment", []string{"GREETING=hello world", "printenv", "GREETING"}, nil, false, "hello world\n", 0},
		{"empty shell", []string{""}, nil, true, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			workdir := filepath.Join(root, "work ' directory")
			intent, err := core.ParseCommandIntent(tc.command, tc.shell, tc.literal)
			if err != nil {
				t.Fatal(err)
			}
			script := ShellWorkspaceCommand(workdir, map[string]string{"VALUE": "quotes ' and $()\nline", "BAD-NAME": "must-not-enter-script", "": "must-not-enter-script"}, intent, "bash", "-lc")
			if strings.Contains(script, "must-not-enter-script") {
				t.Fatalf("invalid environment name retained: %s", script)
			}
			cmd := exec.Command(sh, "-s")
			cmd.Stdin = strings.NewReader(script)
			cmd.Env = []string{"HOME=" + root, "PATH=/usr/bin:/bin", "ENV=" + os.DevNull}
			out, runErr := cmd.CombinedOutput()
			code := 0
			if runErr != nil {
				var exitErr *exec.ExitError
				if !errors.As(runErr, &exitErr) {
					t.Fatal(runErr)
				}
				code = exitErr.ExitCode()
			}
			if string(out) != tc.want || code != tc.code {
				t.Fatalf("out=%q code=%d want=%q/%d script=%q", out, code, tc.want, tc.code, script)
			}
			if stat, err := os.Stat(workdir); err != nil || !stat.IsDir() {
				t.Fatalf("workspace not created: %v", err)
			}
		})
	}
	t.Run("setup failure blocks workload", func(t *testing.T) {
		root := t.TempDir()
		workdir := filepath.Join(root, "file")
		if err := os.WriteFile(workdir, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(root, "marker")
		intent, err := core.ParseCommandIntent([]string{"touch", marker}, false, nil)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(sh, "-s")
		cmd.Stdin = strings.NewReader(ShellWorkspaceCommand(workdir, nil, intent, "bash", "-lc"))
		cmd.Env = []string{"HOME=" + root, "PATH=/usr/bin:/bin", "ENV=" + os.DevNull}
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Fatalf("setup succeeded: %s", out)
		}
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("workload ran after failed setup: %v", err)
		}
	})
}
