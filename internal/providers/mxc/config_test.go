package mxc

import (
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestBuildConfigDefaultsToBlockedProcessContainer(t *testing.T) {
	t.Setenv("SystemRoot", `C:\Windows`)
	t.Setenv("SystemDrive", `C:`)
	t.Setenv("WINDIR", `C:\Windows`)
	t.Setenv("ComSpec", `C:\Windows\System32\cmd.exe`)
	t.Setenv("ProgramFiles", `C:\Program Files`)
	t.Setenv("PATH", `C:\Windows\System32;C:\Tools`)
	t.Setenv("PATHEXT", `.COM;.EXE;.BAT;.CMD`)
	t.Setenv("OS", `Windows_NT`)
	cfg := core.BaseConfig()
	cfg.MXC.ReadOnlyPaths = []string{`C:\Windows`}
	config, err := buildConfig(cfg, RunRequest{
		Repo:    core.Repo{Root: `C:\src\example`},
		Command: []string{"powershell.exe", "-Command", `Write-Output "hello world"`},
		Env:     map[string]string{"CI": "1"},
		Options: core.LeaseOptions{TTL: 2 * time.Minute},
	})
	if err != nil {
		t.Fatal(err)
	}
	if config.Version != "0.6.0-alpha" || config.Containment != "processcontainer" {
		t.Fatalf("config=%+v", config)
	}
	if config.Network.DefaultPolicy != "block" || config.Network.EnforcementMode != "both" {
		t.Fatalf("network=%+v", config.Network)
	}
	if config.Fallback.AllowDACLMutation {
		t.Fatal("host DACL mutation fallback must be disabled by default")
	}
	if !config.UI.Disable {
		t.Fatal("Win32k/UI access must be disabled by default")
	}
	if config.Process.Timeout != 120000 || !containsString(config.Process.Env, "CI=1") || !containsString(config.Process.Env, `SystemRoot=C:\Windows`) {
		t.Fatalf("process=%+v", config.Process)
	}
	if !containsFold(config.Filesystem.ReadWritePaths, `C:\src\example`) || !containsFold(config.Filesystem.ReadOnlyPaths, `C:\Windows`) {
		t.Fatalf("filesystem=%+v", config.Filesystem)
	}
	if containsFold(config.Filesystem.ReadOnlyPaths, `C:\Tools`) {
		t.Fatalf("PATH directory must not be broadly allowlisted: %+v", config.Filesystem.ReadOnlyPaths)
	}
	if !strings.Contains(config.Process.CommandLine, `\"hello world\"`) {
		t.Fatalf("commandLine=%q", config.Process.CommandLine)
	}
}

func TestWindowsProcessEnvironmentProtectsRequiredValues(t *testing.T) {
	t.Setenv("SystemRoot", `C:\Windows`)
	env := windowsProcessEnvironment(map[string]string{"systemroot": `C:\attacker`, "TOKEN": "secret"})
	if !containsString(env, `SystemRoot=C:\Windows`) || containsString(env, `systemroot=C:\attacker`) {
		t.Fatalf("env=%v", env)
	}
	if !containsString(env, "TOKEN=secret") {
		t.Fatalf("forwarded environment missing: %v", env)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestBuildConfigAllowsExplicitDACLMutationFallback(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.MXC.AllowDACLMutation = true
	cfg.MXC.AllowWindowsUI = true
	config, err := buildConfig(cfg, RunRequest{Command: []string{"cmd.exe", "/c", "exit", "0"}})
	if err != nil {
		t.Fatal(err)
	}
	if !config.Fallback.AllowDACLMutation {
		t.Fatal("explicit host DACL mutation fallback was not forwarded")
	}
	if config.UI.Disable {
		t.Fatal("explicit Windows UI capability was not forwarded")
	}
}

func TestBuildConfigShellRequiresWindowsUI(t *testing.T) {
	_, err := buildConfig(core.BaseConfig(), RunRequest{Command: []string{"npm", "test"}, ShellMode: true})
	if err == nil || !strings.Contains(err.Error(), "--mxc-allow-windows-ui") {
		t.Fatalf("err=%v", err)
	}
}

func TestBuildConfigRejectsVolumeRoot(t *testing.T) {
	_, err := buildConfig(core.BaseConfig(), RunRequest{Repo: core.Repo{Root: `C:\`}, Command: []string{"cmd.exe", "/c", "exit", "0"}})
	if err == nil || !strings.Contains(err.Error(), "volume root") {
		t.Fatalf("err=%v", err)
	}
}

func TestBuildIsolatedConfigUsesPrivateTemporaryDirectory(t *testing.T) {
	cfg := core.BaseConfig()
	config, _, cleanup, err := buildIsolatedConfig(cfg, RunRequest{Command: []string{"cmd.exe", "/c", "exit", "0"}, Env: map[string]string{"Temp": `C:\attacker`, "tmp": `C:\other`}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	temp := ""
	for _, entry := range config.Process.Env {
		if strings.HasPrefix(entry, "TEMP=") {
			temp = strings.TrimPrefix(entry, "TEMP=")
		}
	}
	if temp == "" || !containsFold(config.Filesystem.ReadWritePaths, temp) {
		t.Fatalf("private temp missing: env=%v paths=%v", config.Process.Env, config.Filesystem.ReadWritePaths)
	}
	for _, entry := range config.Process.Env {
		if strings.Contains(entry, `C:\attacker`) || strings.Contains(entry, `C:\other`) {
			t.Fatalf("untrusted temp override survived: %v", config.Process.Env)
		}
	}
	if _, err := os.Stat(temp); err != nil {
		t.Fatalf("private temp not created: %v", err)
	}
}

func TestWriteConfigFileUsesPrivatePermissions(t *testing.T) {
	dir := t.TempDir()
	path, cleanup, err := writeConfigFile(dir, mxcConfig{Version: "0.6.0-alpha"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
	var decoded mxcConfig
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Version != "0.6.0-alpha" {
		t.Fatalf("decoded=%+v", decoded)
	}
}

func TestQuoteWindowsArg(t *testing.T) {
	for input, want := range map[string]string{
		"plain":       "plain",
		"hello world": `"hello world"`,
		`a"b`:         `"a\"b"`,
		`C:\path\`:    `C:\path\`,
	} {
		if got := quoteWindowsArg(input); got != want {
			t.Fatalf("quoteWindowsArg(%q)=%q want %q", input, got, want)
		}
	}
}

func TestWindowsCommandLineRejectsCommandShim(t *testing.T) {
	_, err := windowsCommandLineWithLookPath([]string{"npm", "test"}, false, func(string) (string, error) {
		return `C:\Program Files\nodejs\npm.cmd`, nil
	})
	if err == nil || !strings.Contains(err.Error(), "rerun with --shell") {
		t.Fatalf("err=%v", err)
	}
}

func TestWindowsCommandLineUsesResolvedExecutable(t *testing.T) {
	commandLine, err := windowsCommandLineWithLookPath([]string{"powershell.exe", "-NoProfile"}, false, func(string) (string, error) {
		return `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(commandLine, `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe `) {
		t.Fatalf("commandLine=%q", commandLine)
	}
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}
