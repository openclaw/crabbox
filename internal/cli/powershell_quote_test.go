package cli

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func powerShellQuoteCases() []string {
	values := []string{"", "plain", "Don’t panic", "line\r\n'@\n\"@\nend", "🦀 $HOME `n $(throw 'injected')"}
	// Exercise every delimiter, adjacent pairs, and mixed quote injection payloads.
	quotes := []rune{'\'', '‘', '’', '‚', '‛', '"', '“', '”', '„'}
	for _, left := range quotes {
		values = append(values, string(left), "a"+string(left)+";throw 'injected';"+string(left), "trailing"+string(left))
		for _, right := range quotes {
			values = append(values, string([]rune{left, right}), "a"+string(left)+"$(`n)"+string(right)+"z")
		}
	}
	return values
}

func forEachQuotePowerShell(t *testing.T, test func(*testing.T, string)) {
	t.Helper()
	found := false
	for _, name := range []string{"pwsh", "powershell.exe"} {
		if shell, err := exec.LookPath(name); err == nil {
			found = true
			t.Run(name, func(t *testing.T) { test(t, shell) })
		}
	}
	if !found {
		t.Skip("PowerShell is not installed")
	}
}

func quotePowerShellOutput(t *testing.T, shell, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "quote.ps1")
	// Windows PowerShell 5.1 needs a BOM to read Unicode source as UTF-8.
	if err := os.WriteFile(path, append([]byte{0xef, 0xbb, 0xbf}, script...), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(shell, "-NoLogo", "-NoProfile", "-NonInteractive", "-File", path).CombinedOutput()
	if err != nil {
		t.Fatalf("PowerShell rejected generated script: %v\n%s", err, out)
	}
	return strings.ReplaceAll(string(out), "\r\n", "\n")
}

const quotePowerShellPrint = "[Console]::Out.WriteLine([Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes([string]$value)))\n"

func TestPSQuoteRoundTripsThroughPowerShell(t *testing.T) {
	var script, want strings.Builder
	script.WriteString("$ErrorActionPreference = 'Stop'\n")
	for _, value := range powerShellQuoteCases() {
		script.WriteString("$value = " + psQuote(value) + "\n" + quotePowerShellPrint)
		want.WriteString(base64.StdEncoding.EncodeToString([]byte(value)) + "\n")
	}
	forEachQuotePowerShell(t, func(t *testing.T, shell string) {
		if got := quotePowerShellOutput(t, shell, script.String()); got != want.String() {
			t.Fatalf("literal round trip changed data or executed injected output:\ngot %q\nwant %q", got, want.String())
		}
	})
}

func TestPowerShellGeneratedCommandPreservesValues(t *testing.T) {
	workdir := filepath.Join(t.TempDir(), "work‘’‚‛“”„")
	if err := os.Mkdir(workdir, 0o700); err != nil {
		t.Fatal(err)
	}
	values := powerShellQuoteCases()
	var script, want strings.Builder
	script.WriteString("function Test-QuoteArguments { foreach ($value in $args) { " + quotePowerShellPrint + " } }\n")
	for i, value := range values {
		key := fmt.Sprintf("CRABBOX_QUOTE_%d", i)
		script.WriteString(formatPowerShellCommandEnv(map[string]string{key: value}))
		script.WriteString("$value = $env:" + key + "\n" + quotePowerShellPrint)
		want.WriteString(base64.StdEncoding.EncodeToString([]byte(value)) + "\n")
	}
	// Parse the real EncodedCommand payload, including the native argv branch.
	script.WriteString(decodePowerShellCommand(t, windowsRemoteCommandWithEnvFiles(workdir, nil, nil, append([]string{"Test-QuoteArguments"}, values...))))
	for _, value := range values {
		want.WriteString(base64.StdEncoding.EncodeToString([]byte(value)) + "\n")
	}
	forEachQuotePowerShell(t, func(t *testing.T, shell string) {
		if got := quotePowerShellOutput(t, shell, script.String()); got != want.String() {
			t.Fatalf("generated command changed arguments/environment or emitted injected output:\ngot %q\nwant %q", got, want.String())
		}
	})
}

func TestPowerShellBootstrapPreservesValues(t *testing.T) {
	var script, want strings.Builder
	for _, value := range powerShellQuoteCases() {
		header := sharedWindowsHeader(value, value, value, []string{value})
		script.WriteString(header[strings.Index(header, "$user ="):strings.Index(header, "$base =")])
		script.WriteString("foreach ($value in @($user, $publicKey, $workRoot, $sshPorts[0])) { " + quotePowerShellPrint + " }\n")
		for range 4 {
			want.WriteString(base64.StdEncoding.EncodeToString([]byte(value)) + "\n")
		}
	}
	forEachQuotePowerShell(t, func(t *testing.T, shell string) {
		if got := quotePowerShellOutput(t, shell, script.String()); got != want.String() {
			t.Fatalf("bootstrap changed literal values:\ngot %q\nwant %q", got, want.String())
		}
	})
}

func TestPowerShellWSLBootstrapPreservesHereStringTerminators(t *testing.T) {
	cfg := baseConfig()
	cfg.TargetOS, cfg.WindowsMode = targetWindows, windowsModeWSL2
	cfg.WorkRoot = "/work/‘’‚‛“”„$(`n)\n'@\nthrow 'injected'\n@'\n’@\nthrow 'injected'\n@‘\nend"
	source := windowsWSL2BootstrapPowerShell(cfg)
	_, expected, _, err := splitWSLSetupLiteral(source)
	if err != nil {
		t.Fatal(err)
	}
	script := "$source = " + psQuote(source) + `
$tokens = $null; $errors = $null
$ast = [System.Management.Automation.Language.Parser]::ParseInput($source, [ref]$tokens, [ref]$errors)
if ($errors.Count) { throw ($errors | Out-String) }
$assignment = $ast.Find({ param($node) $node -is [System.Management.Automation.Language.AssignmentStatementAst] -and $node.Left.Extent.Text -eq '$linuxSetup' }, $true)
$literal = $assignment.Right.Expression
if ($literal -isnot [System.Management.Automation.Language.StringConstantExpressionAst]) { throw 'WSL setup must be a constant string' }
$value = $literal.Value
` + quotePowerShellPrint
	forEachQuotePowerShell(t, func(t *testing.T, shell string) {
		got := quotePowerShellOutput(t, shell, script)
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(got))
		if err != nil {
			t.Fatalf("WSL setup emitted unexpected output: %q: %v", got, err)
		}
		if string(decoded) != expected {
			t.Fatal("Go literal decoder disagrees with PowerShell about the emitted WSL setup")
		}
		for _, command := range []string{"mkdir -p ", "test -w ", "wslpath -w "} {
			if !strings.Contains(string(decoded), command+shellQuote(cfg.WorkRoot)) {
				t.Fatalf("WSL setup lost work root for %s: %q", command, decoded)
			}
		}
	})
}
