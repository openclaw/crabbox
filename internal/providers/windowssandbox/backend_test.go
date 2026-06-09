package windowssandbox

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderSpec(t *testing.T) {
	spec := Provider{}.Spec()
	if spec.Name != providerName {
		t.Fatalf("Name=%q", spec.Name)
	}
	if spec.Kind != core.ProviderKindDelegatedRun {
		t.Fatalf("Kind=%q, want delegated-run", spec.Kind)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetWindows || spec.Targets[0].WindowsMode != core.WindowsModeNormal {
		t.Fatalf("Targets=%#v, want native Windows only", spec.Targets)
	}
	if spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("Coordinator=%q, want never", spec.Coordinator)
	}
	if !spec.Features.Has(core.FeatureArchiveSync) {
		t.Fatalf("Features=%#v, want archive sync support", spec.Features)
	}
}

func TestApplyDefaultsSelectsNativeWindowsAndSecureWSBDefaults(t *testing.T) {
	cfg := Config{}
	applyDefaults(&cfg)
	if cfg.Provider != providerName {
		t.Fatalf("Provider=%q", cfg.Provider)
	}
	if cfg.TargetOS != core.TargetWindows || cfg.WindowsMode != core.WindowsModeNormal {
		t.Fatalf("target=%s mode=%s, want native Windows", cfg.TargetOS, cfg.WindowsMode)
	}
	if cfg.WindowsSandbox.Networking != "Enable" {
		t.Fatalf("Networking=%q", cfg.WindowsSandbox.Networking)
	}
	for name, got := range map[string]string{
		"vgpu":               cfg.WindowsSandbox.VGPU,
		"clipboard":          cfg.WindowsSandbox.Clipboard,
		"audioInput":         cfg.WindowsSandbox.AudioInput,
		"videoInput":         cfg.WindowsSandbox.VideoInput,
		"printerRedirection": cfg.WindowsSandbox.PrinterRedirection,
	} {
		if got != "Disable" {
			t.Fatalf("%s=%q, want Disable", name, got)
		}
	}
}

func TestWindowsSandboxConfigXML(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.WindowsSandbox.Workdir = `C:\work\repo`
	cfg.WindowsSandbox.Networking = "Disable"
	cfg.WindowsSandbox.VGPU = "Disable"
	cfg.WindowsSandbox.Clipboard = "Disable"
	cfg.WindowsSandbox.ProtectedClient = "Default"
	cfg.WindowsSandbox.AudioInput = "Disable"
	cfg.WindowsSandbox.VideoInput = "Disable"
	cfg.WindowsSandbox.PrinterRedirection = "Disable"
	cfg.WindowsSandbox.MemoryMB = 4096
	run := preparedRun{
		hostWorkspace: `C:\host\workspace`,
		hostControl:   `C:\host\control`,
	}
	data, err := windowsSandboxConfigXML(cfg, run)
	if err != nil {
		t.Fatal(err)
	}
	xml := string(data)
	for _, want := range []string{
		"<Configuration>",
		"<Networking>Disable</Networking>",
		"<vGPU>Disable</vGPU>",
		"<ClipboardRedirection>Disable</ClipboardRedirection>",
		"<MemoryInMB>4096</MemoryInMB>",
		"<HostFolder>C:\\host\\workspace</HostFolder>",
		"<SandboxFolder>C:\\work\\repo</SandboxFolder>",
		"<HostFolder>C:\\host\\control</HostFolder>",
		"<SandboxFolder>C:\\crabbox-control</SandboxFolder>",
		"<Command>powershell.exe -NoProfile -ExecutionPolicy Bypass -File C:\\crabbox-control\\run.ps1</Command>",
	} {
		if !strings.Contains(xml, want) {
			t.Fatalf("wsb xml missing %q:\n%s", want, xml)
		}
	}
}

func TestSandboxRunScriptQuotesCommandEnvAndKeepOnFailure(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.WindowsSandbox.Workdir = `C:\work\repo`
	script, err := sandboxRunScript(cfg, RunRequest{
		Command:       []string{"pwsh", "-NoProfile", "-Command", "Write-Output 'hi'"},
		KeepOnFailure: true,
		Env: map[string]string{
			"CI":     "true",
			"SECRET": "can't leak",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Set-Location -LiteralPath 'C:\\work\\repo'",
		"$env:CI = 'true'",
		"$env:SECRET = 'can''t leak'",
		"& 'pwsh' '-NoProfile' '-Command' 'Write-Output ''hi''' 1>> $stdout 2>> $stderr",
		"$keepOnFailure = $true",
		"Set-Content -LiteralPath $exitFile -Value $exitCode -Encoding ASCII",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q:\n%s", want, script)
		}
	}
}

func TestHostRunnerScriptStopsActualSandboxProcessOnTimeout(t *testing.T) {
	script := hostRunnerScript()
	for _, want := range []string{
		"function Stop-SandboxSession",
		"Get-Process -Name WindowsSandbox -ErrorAction SilentlyContinue | Stop-Process -Force",
		"if (Get-Process -Name WindowsSandbox -ErrorAction SilentlyContinue)",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("host runner missing %q:\n%s", want, script)
		}
	}
}

func TestRejectWindowsSandboxSyncOnly(t *testing.T) {
	err := rejectWindowsSandboxRunOptions(Provider{}.Spec(), RunRequest{SyncOnly: true})
	if err == nil || !strings.Contains(err.Error(), "--sync-only") {
		t.Fatalf("err=%v, want --sync-only rejection", err)
	}
}

func TestCopyManifestRejectsSymlinks(t *testing.T) {
	repo := t.TempDir()
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "target.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target.txt", filepath.Join(repo, "link.txt")); err != nil {
		t.Skipf("symlink creation unavailable on this host: %v", err)
	}
	err := copyManifest(context.Background(), repo, dst, SyncManifest{Files: []string{"link.txt"}})
	if err == nil || !strings.Contains(err.Error(), "does not support syncing symlink") {
		t.Fatalf("err=%v, want symlink rejection", err)
	}
}

func TestRunInvokesHostRunnerWithNoSync(t *testing.T) {
	oldOS := windowsSandboxHostOS
	windowsSandboxHostOS = "windows"
	defer func() { windowsSandboxHostOS = oldOS }()

	runner := &recordingRunner{}
	cfg := core.BaseConfig()
	cfg.WindowsSandbox.TempRoot = t.TempDir()
	cfg.TTL = 2 * time.Minute
	be := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner})
	result, err := be.(*backend).Run(context.Background(), RunRequest{
		NoSync:  true,
		Command: []string{"cmd.exe", "/c", "echo ok"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || !result.SyncDelegated {
		t.Fatalf("result=%#v", result)
	}
	if runner.name != "powershell.exe" {
		t.Fatalf("runner name=%q", runner.name)
	}
	joined := strings.Join(runner.args, "\n")
	for _, want := range []string{"-File", "host-runner.ps1", "-TimeoutSeconds\n120"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args missing %q: %#v", want, runner.args)
		}
	}
}

func TestRunKeepsWorkspaceOnFailureWithKeepOnFailure(t *testing.T) {
	oldOS := windowsSandboxHostOS
	windowsSandboxHostOS = "windows"
	defer func() { windowsSandboxHostOS = oldOS }()

	runner := &recordingRunner{result: LocalCommandResult{ExitCode: 7}, err: errors.New("exit status 7")}
	cfg := core.BaseConfig()
	cfg.WindowsSandbox.TempRoot = t.TempDir()
	be := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner})
	result, err := be.(*backend).Run(context.Background(), RunRequest{
		NoSync:        true,
		KeepOnFailure: true,
		Command:       []string{"cmd.exe", "/c", "exit 7"},
	})
	if err == nil {
		t.Fatal("expected run error")
	}
	if result.ExitCode != 7 {
		t.Fatalf("ExitCode=%d", result.ExitCode)
	}
	entries, readErr := os.ReadDir(cfg.WindowsSandbox.TempRoot)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 1 {
		t.Fatalf("temp entries=%d, want preserved workspace", len(entries))
	}
	if _, statErr := os.Stat(filepath.Join(cfg.WindowsSandbox.TempRoot, entries[0].Name(), "control", "run.ps1")); statErr != nil {
		t.Fatalf("preserved run script missing: %v", statErr)
	}
}

type recordingRunner struct {
	name   string
	args   []string
	result LocalCommandResult
	err    error
}

func (r *recordingRunner) Run(ctx context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	_ = ctx
	r.name = req.Name
	r.args = append([]string(nil), req.Args...)
	return r.result, r.err
}
