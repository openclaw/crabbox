package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Exercise the generated PowerShell and .NET file operations locally; no VM or
// prlctl is involved. Only the guest paths and PowerShell executable are mapped.
func TestParallelsWindowsUploadPowerShell(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("local PowerShell is not installed")
	}
	for _, fault := range []string{"write-result-loss", "execution-result-loss", "corrupt-upload", "script-failure", "failed-execution-result-loss"} {
		t.Run(fault, func(t *testing.T) {
			root := t.TempDir()
			runner := &windowsUploadPowerShellRunner{t: t, root: root, pwsh: pwsh, fault: fault}
			marker := filepath.Join(root, "ran")
			script := "[IO.File]::AppendAllText(" + psQuote(marker) + ", 'ran')\n" + strings.Repeat("# upload bytes π\n", 15)
			if strings.Contains(fault, "fail") {
				script += "exit 23\n"
			}
			// A stale receipt and a longer previous upload cannot prove success.
			for path, data := range map[string]string{"fixture.ps1.b64": strings.Repeat("Z", 4000), "fixture.ps1.result": "old-token:0"} {
				if err := os.WriteFile(filepath.Join(root, path), []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cfg := BaseConfig()
			cfg.TargetOS = targetWindows
			cfg.Parallels.StartupTimeout = time.Minute
			err := NewParallelsClient(cfg, runner).runWindowsPowerShellFile(context.Background(), "vm-fixture", "fixture", script)
			wantErr := strings.Contains(fault, "fail") || fault == "corrupt-upload"
			if (err != nil) != wantErr {
				t.Fatalf("err=%v", err)
			}
			data, readErr := os.ReadFile(marker)
			if fault == "corrupt-upload" {
				if !os.IsNotExist(readErr) || !strings.Contains(err.Error(), "SHA-256 mismatch") {
					t.Fatalf("corrupt payload ran: data=%q read=%v upload=%v", data, readErr, err)
				}
			} else if readErr != nil || string(data) != "ran" {
				t.Fatalf("payload must execute exactly once: data=%q err=%v", data, readErr)
			}
			if fault == "failed-execution-result-loss" && !strings.Contains(err.Error(), "exit code 23") {
				t.Fatalf("lost failure not recovered from receipt: %v", err)
			}
		})
	}
}

type windowsUploadPowerShellRunner struct {
	t                 *testing.T
	root, pwsh, fault string
	writes            int
}

func (r *windowsUploadPowerShellRunner) Run(ctx context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	command := decodePowerShellCommand(r.t, strings.Join(req.Args[2:], " "))
	final := strings.Contains(command, "& powershell.exe")
	if final && r.fault == "corrupt-upload" {
		path := filepath.Join(r.root, "fixture.ps1.b64")
		data, err := os.ReadFile(path)
		if err != nil {
			r.t.Fatal(err)
		}
		data[0] = 'A'
		if err := os.WriteFile(path, data, 0o600); err != nil {
			r.t.Fatal(err)
		}
	}
	command = strings.ReplaceAll(command, `C:\ProgramData\crabbox`, r.root)
	command = strings.ReplaceAll(command, `\`, `/`)
	command = strings.ReplaceAll(command, "& powershell.exe", "& "+psQuote(r.pwsh))
	result, err := (execCommandRunner{}).Run(ctx, LocalCommandRequest{
		Name: r.pwsh, Args: []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-Command", command},
	})
	if strings.Contains(command, "$file.Position") || strings.Contains(command, "Add-Content") {
		r.writes++
		if err == nil && r.writes == 2 && r.fault == "write-result-loss" {
			return LocalCommandResult{ExitCode: 255, Stderr: "PrlJob_GetResult: Invalid argument"}, errors.New("controlled result loss")
		}
	}
	if final && strings.Contains(r.fault, "execution-result-loss") {
		return LocalCommandResult{ExitCode: 255, Stderr: "PrlJob_GetResult: Invalid argument"}, errors.New("controlled result loss")
	}
	return result, err
}
