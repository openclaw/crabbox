package cli

import (
	"strings"
	"testing"
)

func TestParseEnvProfileRedactedLiveSecrets(t *testing.T) {
	got := parseEnvProfile([]byte(`
# comment
export OPENAI_API_KEY='sk-test'
PLAIN=value # trailing
HASH=abc#def
URL=https://example.test/callback#fragment
SPACED="hello world"
IGNORED=$(op read secret)
BAD-NAME=value
`))
	if got["OPENAI_API_KEY"] != "sk-test" {
		t.Fatalf("OPENAI_API_KEY=%q", got["OPENAI_API_KEY"])
	}
	if got["PLAIN"] != "value" {
		t.Fatalf("PLAIN=%q", got["PLAIN"])
	}
	if got["HASH"] != "abc#def" {
		t.Fatalf("HASH=%q", got["HASH"])
	}
	if got["URL"] != "https://example.test/callback#fragment" {
		t.Fatalf("URL=%q", got["URL"])
	}
	if got["SPACED"] != "hello world" {
		t.Fatalf("SPACED=%q", got["SPACED"])
	}
	if _, ok := got["IGNORED"]; ok {
		t.Fatal("command substitution must not be parsed")
	}
	if _, ok := got["BAD-NAME"]; ok {
		t.Fatal("invalid env name parsed")
	}
}

func TestFormatShellEnvFileQuotesValues(t *testing.T) {
	got := formatShellEnvFile(map[string]string{
		"API_TOKEN": "abc#def",
		"QUOTE":     "it's ok",
	})
	if !containsAll(got, "export API_TOKEN='abc#def'\n", "export QUOTE='it'\\''s ok'\n") {
		t.Fatalf("env file=%q", got)
	}
}

func TestFormatPlainEnvFileForWindowsProfileHandoff(t *testing.T) {
	got := formatPlainEnvFile(map[string]string{"API_TOKEN": "abc#def"})
	if got != "API_TOKEN=abc#def\n" {
		t.Fatalf("plain env file=%q", got)
	}
}

func TestWindowsRemoteUploadRunEnvProfileWritesUTF8BOMBytes(t *testing.T) {
	got := windowsRemoteUploadRunEnvProfileCommand(`C:\crabbox\repo`, `.crabbox\env\run.env`)
	decoded := decodePowerShellCommand(t, got)
	for _, want := range []string{
		`Set-Location -LiteralPath 'C:\crabbox\repo'`,
		`$stdin = [Console]::OpenStandardInput()`,
		`$stdin.CopyTo($memory)`,
		`[byte[]](0xEF, 0xBB, 0xBF)`,
		`$fullPath = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($path)`,
		`[System.IO.File]::WriteAllBytes($fullPath, $out)`,
	} {
		if !strings.Contains(decoded, want) {
			t.Fatalf("windows env upload command missing %q in %q", want, decoded)
		}
	}
	if strings.Contains(decoded, "ReadToEnd()") || strings.Contains(decoded, "WriteAllText") {
		t.Fatalf("windows env upload command should preserve bytes, got %q", decoded)
	}
}

func TestRemoteProbeRunEnvProfileRedactsSecretValues(t *testing.T) {
	got := remoteProbeRunEnvProfileCommand("/work/repo", ".crabbox/env/live.env", []string{"OPENAI_API_KEY", "CI"})
	for _, want := range []string{
		".crabbox/env/live.env",
		"OPENAI_API_KEY",
		"secret=true",
		"%s=set",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("remote probe missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "sk-") {
		t.Fatalf("remote probe should not contain secret values: %q", got)
	}
}

func TestFormatRunEnvHelperSourcesProfileAndExecsCommand(t *testing.T) {
	got := formatRunEnvHelper(".crabbox/env/live.env")
	for _, want := range []string{
		"profile='.crabbox/env/live.env'",
		". \"$profile\"",
		"exec \"$@\"",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("env helper missing %q in %q", want, got)
		}
	}
}

func TestSafeEnvHelperNameRejectsPaths(t *testing.T) {
	if _, err := safeEnvHelperName("live"); err != nil {
		t.Fatalf("safe helper rejected: %v", err)
	}
	if _, err := safeEnvHelperName("../live"); err == nil {
		t.Fatal("path helper name accepted")
	}
}

func containsAll(s string, values ...string) bool {
	for _, value := range values {
		if !strings.Contains(s, value) {
			return false
		}
	}
	return true
}

func TestAllowedEnvFromProfilesOnlyForAllowlist(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "ambient")
	t.Setenv("CI", "1")
	env := allowedEnvFromProfiles([]string{"OPENAI_API_KEY", "CI"}, map[string]string{
		"OPENAI_API_KEY": "profile",
		"UNLISTED":       "secret",
	})
	if env["OPENAI_API_KEY"] != "profile" {
		t.Fatalf("profile value should override ambient allowlisted value: %#v", env)
	}
	if env["CI"] != "1" {
		t.Fatalf("ambient allowlisted value missing: %#v", env)
	}
	if _, ok := env["UNLISTED"]; ok {
		t.Fatalf("unlisted profile secret forwarded: %#v", env)
	}
}

func TestExternalDesktopPasswordNeverEntersRunEnvironmentForAnyTarget(t *testing.T) {
	t.Setenv("SCREEN_SHARING_PASSWORD", "operator-secret")
	t.Setenv("SCREEN_SAFE_VALUE", "preserved")
	for _, targetOS := range []string{targetLinux, targetMacOS, targetWindows} {
		t.Run(targetOS, func(t *testing.T) {
			profile := map[string]string{
				"SCREEN_SHARING_PASSWORD": "profile-secret",
				"SCREEN_SAFE_VALUE":       "profile-safe",
			}
			selection := runEnvSelection{
				Profile:   allowedProfileEnv([]string{"SCREEN_*"}, profile),
				Inline:    allowedEnvWithoutProfileKeys([]string{"SCREEN_*"}, profile),
				Effective: allowedEnvFromProfiles([]string{"SCREEN_*"}, profile),
			}
			selection.Inline["SCREEN_SHARING_PASSWORD"] = "expanded-secret"
			selection.Effective["SCREEN_SHARING_PASSWORD"] = "expanded-secret"
			cfg := Config{Provider: "external", TargetOS: targetOS}
			cfg.External.Connection.Desktop.PasswordEnv = "SCREEN_SHARING_PASSWORD"
			stripExternalDesktopPasswordFromRunEnv(cfg, &selection)
			for name, values := range map[string]map[string]string{"profile": selection.Profile, "inline": selection.Inline, "effective": selection.Effective} {
				if _, found := values["SCREEN_SHARING_PASSWORD"]; found {
					t.Fatalf("%s environment retained desktop password: %#v", name, values)
				}
			}
			if selection.Effective["SCREEN_SAFE_VALUE"] != "profile-safe" {
				t.Fatalf("unrelated allowed environment lost: %#v", selection.Effective)
			}
		})
	}
}

func TestTrustedExternalDesktopPasswordStaysLocalAfterProviderSwitch(t *testing.T) {
	t.Setenv("OPERATOR_ARD_PASSWORD", "operator-secret")
	t.Setenv("SAFE_REMOTE_VALUE", "preserved")
	cfg := Config{
		Provider: "aws",
		TargetOS: targetLinux,
		EnvAllow: []string{"OPERATOR_ARD_PASSWORD", "SAFE_REMOTE_VALUE"},
	}
	cfg.External.Connection.Desktop.PasswordEnv = "OPERATOR_ARD_PASSWORD"
	cfg.credentialProvenance.externalDesktopEnv = credentialSourceTrustedFile

	values := allowedRemoteEnv(cfg)
	if _, found := values["OPERATOR_ARD_PASSWORD"]; found {
		t.Fatalf("provider switch forwarded trusted desktop password: %#v", values)
	}
	if values["SAFE_REMOTE_VALUE"] != "preserved" {
		t.Fatalf("provider switch removed unrelated value: %#v", values)
	}
}

func TestResolvedTargetPasswordStaysOutOfRunAndCacheEnvironment(t *testing.T) {
	t.Setenv("ROUTED_SCREEN_PASSWORD", "operator-secret")
	t.Setenv("SAFE_REMOTE_VALUE", "preserved")
	cfg := Config{EnvAllow: []string{"ROUTED_SCREEN_PASSWORD", "SAFE_REMOTE_VALUE"}}
	selection := runEnvSelection{
		Profile:   allowedEnv(cfg.EnvAllow),
		Inline:    allowedEnv(cfg.EnvAllow),
		Effective: allowedEnv(cfg.EnvAllow),
	}
	target := SSHTarget{ChildEnvDenylist: []string{"ROUTED_SCREEN_PASSWORD"}}
	stripTargetCredentialsFromRunEnv(&selection, target)
	for name, values := range map[string]map[string]string{"profile": selection.Profile, "inline": selection.Inline, "effective": selection.Effective} {
		if _, ok := values["ROUTED_SCREEN_PASSWORD"]; ok {
			t.Fatalf("%s retained resolved credential: %#v", name, values)
		}
	}
	values := allowedRemoteEnvForTarget(cfg, target)
	if _, ok := values["ROUTED_SCREEN_PASSWORD"]; ok || values["SAFE_REMOTE_VALUE"] != "preserved" {
		t.Fatalf("resolved cache environment=%#v", values)
	}
}
