//go:build windows

package hyperv

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

const (
	guestCredentialProbeUser  = "crabbox-probe-user"
	guestCredentialProbeValue = "Probe-Value-Zq7!x"
)

// TestGuestCredentialSurvivesCorePollutedModulePath covers a host-side failure
// that fires before any guest communication. A PowerShell 7 caller adds its Core
// module directories to PSModulePath, and an intermediate process passes them
// through unchanged, so a credential built with module cmdlets in Windows
// PowerShell finds the Core copy of Microsoft.PowerShell.Security first and
// cannot load it. The observed result was a null credential and a warmup that
// failed at the readiness probe.
func TestGuestCredentialSurvivesCorePollutedModulePath(t *testing.T) {
	pollution := corePollutedModulePathEntries(t)
	env := environmentWithPSModulePath(
		append(os.Environ(), "_CRABBOX_GP="+guestCredentialProbeValue),
		strings.Join(append(pollution, os.Getenv("PSModulePath")), ";"),
	)

	assertModulePathPollutionIsObservable(t, env, pollution)

	backend := &backend{rt: core.RuntimeForProviderOperation(io.Discard)}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()

	result, err := backend.powershellWithEnv(ctx, guestCredentialProbeScript(), env)
	if err != nil {
		t.Fatalf("credential construction failed under a Core-polluted PSModulePath: %v: %s", err, strings.TrimSpace(result.Stderr))
	}
	if strings.TrimSpace(result.Stderr) != "" {
		t.Fatalf("credential construction reported errors: %s", strings.TrimSpace(result.Stderr))
	}
	if !strings.Contains(result.Stdout, "credential=ok") {
		t.Fatalf("credential was not constructed: stdout=%q stderr=%q", result.Stdout, result.Stderr)
	}

	effective := reportedModulePath(t, result.Stdout)
	// The credential is built from .NET types, so it must not depend on module
	// resolution: prove it was built while the child still saw the Core entries.
	for _, entry := range pollution {
		if !modulePathContains(effective, entry) {
			t.Fatalf("probe did not run under the polluted module path entry %q: %q", entry, effective)
		}
	}
}

// guestCredentialProbeScript exercises the production credential construction
// and reports the outcome the readiness probe depends on.
func guestCredentialProbeScript() string {
	return powershellCredentialPrelude(guestCredentialProbeUser) +
		`if ($null -eq $cred) { throw 'credential was not constructed' }; ` +
		`if ($cred.UserName -ne '` + guestCredentialProbeUser + `') { throw 'unexpected credential user' }; ` +
		`if ($cred.GetNetworkCredential().Password -ne $env:_CRABBOX_GP) { throw 'credential password did not round-trip' }; ` +
		`if ($Error.Count -ne 0) { throw ('module resolution errors: ' + $Error[0]) }; ` +
		`Write-Output 'credential=ok'; ` +
		`Write-Output ('modulepath=' + $env:PSModulePath)`
}

// corePollutedModulePathEntries builds the pollution the caller's shell would
// contribute. The marker directory keeps the case adversarial on any Windows
// host; a real PowerShell 7 installation reproduces the reported failure
// exactly, so it is added whenever one is present.
func corePollutedModulePathEntries(t *testing.T) []string {
	t.Helper()
	entries := []string{filepath.Join(t.TempDir(), "PowerShell", "7", "Modules")}
	programFiles := os.Getenv("ProgramFiles")
	if programFiles == "" {
		programFiles = `C:\Program Files`
	}
	coreModules := filepath.Join(programFiles, "PowerShell", "7", "Modules")
	if info, err := os.Stat(coreModules); err == nil && info.IsDir() {
		entries = append(entries, coreModules)
	} else {
		t.Logf("PowerShell 7 modules not installed at %s; using the marker entry only", coreModules)
	}
	return entries
}

// assertModulePathPollutionIsObservable proves the fixture really pollutes a
// child, so a passing probe reflects a credential built under the pollution.
func assertModulePathPollutionIsObservable(t *testing.T, env, pollution []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", `Write-Output ('modulepath=' + $env:PSModulePath)`)
	command.Env = env
	output, err := command.Output()
	if err != nil {
		t.Fatalf("control spawn failed: %v", err)
	}
	effective := reportedModulePath(t, string(output))
	for _, entry := range pollution {
		if !modulePathContains(effective, entry) {
			t.Fatalf("fixture no longer pollutes an uncorrected child with %q: %q", entry, effective)
		}
	}
}

func environmentWithPSModulePath(env []string, value string) []string {
	result := make([]string, 0, len(env)+1)
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if strings.EqualFold(name, "PSModulePath") {
			continue
		}
		result = append(result, entry)
	}
	return append(result, "PSModulePath="+value)
}

func reportedModulePath(t *testing.T, stdout string) string {
	t.Helper()
	for _, line := range strings.Split(stdout, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "modulepath="); ok {
			return value
		}
	}
	t.Fatalf("no module path reported in %q", stdout)
	return ""
}

func modulePathContains(modulePath, entry string) bool {
	for _, candidate := range strings.Split(modulePath, ";") {
		if strings.EqualFold(strings.TrimRight(strings.TrimSpace(candidate), `\`), strings.TrimRight(entry, `\`)) {
			return true
		}
	}
	return false
}
