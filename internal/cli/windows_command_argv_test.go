package cli

import (
	"reflect"
	"strings"
	"testing"
)

func windowsArgvFixture() []string {
	return []string{`@{n="SizeGB";e={$_.Size / 1GB}}`, `a\"b`, `a\\"b`, `C:\with spaces\`, `C:\plain\`, "two words", "", `%VAR%`, `^`, `&`, `|`, `$HOME`, "Grüße 世界 🦀", "a'b", "tab\there", "line\nbreak"}
}

func TestQuoteWindowsCommandArg(t *testing.T) {
	for _, tc := range []struct{ arg, want string }{
		{"", `""`}, {"plain", `"plain"`}, {"two words", `"two words"`},
		{`a"b`, `"a\"b"`}, {`a\"b`, `"a\\\"b"`}, {`a\\"b`, `"a\\\\\"b"`},
		{`C:\with spaces\`, `"C:\with spaces\\"`}, {`%VAR%^&|$`, `"%VAR%^&|$"`},
		{"Grüße 世界 🦀", `"Grüße 世界 🦀"`},
	} {
		t.Run(tc.arg, func(t *testing.T) {
			if got := quoteWindowsCommandArg(tc.arg); got != tc.want {
				t.Fatalf("quote(%q)=%q, want %q", tc.arg, got, tc.want)
			}
		})
	}
}

// Models CommandLineToArgvW's argument rules (argv[0] is a fixed executable).
// Odd backslashes escape a quote; even backslashes leave it as a delimiter.
func parseWindowsTestCommandLine(line string) []string {
	var args []string
	for i := 0; i < len(line); {
		for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
			i++
		}
		if i == len(line) {
			break
		}
		var arg strings.Builder
		quoted := false
		for i < len(line) {
			if !quoted && (line[i] == ' ' || line[i] == '\t') {
				break
			}
			slashes := 0
			for i < len(line) && line[i] == '\\' {
				slashes++
				i++
			}
			if i < len(line) && line[i] == '"' {
				arg.WriteString(strings.Repeat(`\`, slashes/2))
				if slashes%2 == 1 {
					arg.WriteByte('"')
				} else {
					quoted = !quoted
				}
				i++
			} else {
				arg.WriteString(strings.Repeat(`\`, slashes))
				if i < len(line) {
					arg.WriteByte(line[i])
					i++
				}
			}
		}
		args = append(args, arg.String())
	}
	return args
}

func TestQuoteWindowsCommandArgsRoundTrip(t *testing.T) {
	want := append([]string{"argv.exe"}, windowsArgvFixture()...)
	if got := parseWindowsTestCommandLine(quoteWindowsCommandArgs(want)); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	// Exercise every short combination around the quote/backslash boundary.
	alphabet := []string{`\`, `"`, " ", "x"}
	var check func(string, int)
	check = func(s string, depth int) {
		got := parseWindowsTestCommandLine(`argv.exe ` + quoteWindowsCommandArg(s))
		if len(got) != 2 || got[1] != s {
			t.Fatalf("round trip %q = %#v", s, got)
		}
		if depth > 0 {
			for _, char := range alphabet {
				check(s+char, depth-1)
			}
		}
	}
	check("", 5)
}

func windowsWrapperArguments(t *testing.T, command string) []string {
	t.Helper()
	script := decodePowerShellCommand(t, command)
	const prefix = "$__crabboxStart.Arguments = '"
	start := strings.Index(script, prefix)
	if start < 0 {
		t.Fatalf("wrapper does not supply a Windows command line directly to ProcessStartInfo: %s", script)
	}
	rest := script[start+len(prefix):]
	var value strings.Builder
	for i := 0; i < len(rest); i++ {
		if rest[i] == '\'' {
			if i+1 < len(rest) && rest[i+1] == '\'' {
				value.WriteByte('\'')
				i++
				continue
			}
			return parseWindowsTestCommandLine(`argv.exe ` + value.String())[1:]
		}
		value.WriteByte(rest[i])
	}
	t.Fatal("unterminated PowerShell argument literal")
	return nil
}

func TestWindowsNativeRemoteCommandArgvRoundTrip(t *testing.T) {
	want := windowsArgvFixture()
	command := windowsRemoteCommandWithEnvFiles(`C:\work`, nil, nil, append([]string{"argv.exe"}, want...))
	if got := windowsWrapperArguments(t, command); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestWindowsRemoteRunScriptArgvRoundTrip(t *testing.T) {
	path := `.crabbox\scripts\with space.ps1`
	command := windowsRemoteRunScriptCommandWithEnvFiles(`C:\work`, nil, nil, &RunScriptSpec{RemotePath: path}, windowsArgvFixture())
	want := append([]string{"-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", path}, windowsArgvFixture()...)
	if got := windowsWrapperArguments(t, command); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestWindowsRemoteShellPreservesPowerShellSource(t *testing.T) {
	source := "Write-Output 'Grüße 世界'; Get-PSDrive | Select-Object @{n=\"SizeGB\";e={$_.Used / 1GB}}"
	decoded := decodePowerShellCommand(t, windowsRemoteShellCommandWithEnvFiles(`C:\work`, nil, nil, source))
	if !strings.Contains(decoded, "\n"+source+"\n") {
		t.Fatalf("shell source changed: %s", decoded)
	}
}
