package cli

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A native Windows install script too large for stdin is uploaded and run by a
// short remote command that reads only the four registration values from stdin.
// Run that exact command in real Windows PowerShell: the values must reach the
// uploaded installer intact (including spaces and non-ASCII), its exit status
// must pass through, the command must remove the staged script after running
// it even when it fails, the token must never appear on the command line, and a
// truncated stdin must stop before the installer runs rather than register with
// a missing token. (The private staging directory itself is removed by the
// uploader's cleanup; see TestWindowsRunnerScriptStagingIsPrivateAndFresh.)
func TestWindowsUploadedRunnerInstallReceivesCredentialsFromStdin(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows PowerShell is required")
	}
	if _, err := exec.LookPath("powershell.exe"); err != nil {
		t.Skip("Windows PowerShell is required")
	}
	values := map[string]string{
		"repo":   "example-org/my-app",
		"name":   "runner name with spaces",
		"labels": "self-hosted,windows,ünïcode",
		"token":  "fixture-registration-value",
	}
	run := func(t *testing.T, input string) (data, out string, installerLeft bool, err error) {
		t.Helper()
		installer := filepath.Join(t.TempDir(), "install-runner.ps1")
		if err := os.WriteFile(installer, []byte(`$values = [ordered]@{ repo = $env:RUNNER_REPO; name = $env:RUNNER_NAME; labels = $env:RUNNER_LABELS; token = $env:RUNNER_TOKEN }
[IO.File]::WriteAllText($env:RESULT_PATH, ($values | ConvertTo-Json -Compress), [Text.UTF8Encoding]::new($false))
exit 3
`), 0o600); err != nil {
			t.Fatal(err)
		}
		remote := githubActionsRunnerInstallRemoteCommandForUploadedScript(installer)
		for _, value := range values {
			if strings.Contains(remote, value) {
				t.Fatalf("remote command carries %q: %s", value, remote)
			}
		}
		result := filepath.Join(t.TempDir(), "result.json")
		fields := strings.Fields(remote)
		cmd := exec.Command(fields[0], fields[1:]...)
		cmd.Env = windowsPowerShellTestEnv(t, "RESULT_PATH="+result)
		cmd.Stdin = strings.NewReader(input)
		output, runErr := cmd.CombinedOutput()
		_, statErr := os.Stat(installer)
		recorded, _ := os.ReadFile(result)
		return string(recorded), string(output), !os.IsNotExist(statErr), runErr
	}

	t.Run("credentials reach the uploaded installer", func(t *testing.T) {
		data, out, installerLeft, err := run(t, githubActionsRunnerCredentialInput(values["repo"], values["name"], values["labels"], values["token"]))
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
			t.Fatalf("installer exit status must pass through: err=%v\n%s", err, out)
		}
		if installerLeft {
			t.Fatal("the remote command did not remove the uploaded installer after running it")
		}
		var got map[string]string
		if err := json.Unmarshal([]byte(data), &got); err != nil {
			t.Fatalf("installer did not record its values (%q): %v\n%s", data, err, out)
		}
		for key, want := range values {
			if got[key] != want {
				t.Fatalf("%s=%q, want %q", key, got[key], want)
			}
		}
	})

	t.Run("truncated input stops before the installer runs", func(t *testing.T) {
		full := githubActionsRunnerCredentialInput(values["repo"], values["name"], values["labels"], values["token"])
		truncated := strings.Join(strings.SplitAfter(full, "\n")[:3], "")
		data, out, _, err := run(t, truncated)
		if err == nil || data != "" || !strings.Contains(out, "missing runner registration input") {
			t.Fatalf("a missing token must fail before the installer runs: err=%v result=%q\n%s", err, data, out)
		}
	})
}
