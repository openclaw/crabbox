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
