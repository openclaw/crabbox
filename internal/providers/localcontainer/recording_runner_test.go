package localcontainer

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestRecordedCommandFailureOmitsEnvironmentAndInput(t *testing.T) {
	const child = "CRABBOX_TEST_RECORDED_COMMAND_FAILURE"
	const envValue = "synthetic-environment-value"
	const inputValue = "synthetic-input-value"
	if os.Getenv(child) == "1" {
		runner := &recordingRunner{}
		_, _ = runner.Run(t.Context(), core.LocalCommandRequest{
			Name:  "docker",
			Args:  []string{"inspect", "fixture-container"},
			Env:   []string{"TEST_DIAGNOSTIC_VALUE=" + envValue},
			Stdin: strings.NewReader(inputValue),
		})
		_ = runner.commandSummary()
		input, err := io.ReadAll(runner.calls[0].Stdin)
		if err != nil || string(input) != inputValue || runner.calls[0].Env[0] != "TEST_DIAGNOSTIC_VALUE="+envValue {
			t.Fatal("diagnostic summary altered the captured request")
		}
		recordedArgsForCommand(t, runner, "rm")
		return
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestRecordedCommandFailureOmitsEnvironmentAndInput$", "-test.count=1")
	home := t.TempDir()
	// A failing recorder must never receive the test runner's real credentials.
	cmd.Env = []string{
		child + "=1", "HOME=" + home, "USERPROFILE=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
		"XDG_STATE_HOME=" + filepath.Join(home, "state"),
		"APPDATA=" + filepath.Join(home, "appdata"),
		"LOCALAPPDATA=" + filepath.Join(home, "localappdata"),
	}
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("diagnostic subprocess did not finish: %v", ctx.Err())
	}
	if err == nil {
		t.Fatal("missing-command assertion unexpectedly succeeded")
	}
	for _, want := range []string{"rm command was not recorded", "docker", "inspect", "fixture-container"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("failure omitted command context %q: %s", want, out)
		}
	}
	for _, forbidden := range []string{envValue, inputValue} {
		if strings.Contains(string(out), forbidden) {
			t.Errorf("failure exposed synthetic request value %q: %s", forbidden, out)
		}
	}
}
