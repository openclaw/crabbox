package cli

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// PowerShell also treats U+2018, U+2019, U+201A and U+201B as single quotes.
var powerShellQuoteCases = []string{
	"",
	"plain",
	"it's",
	"Don’t panic",
	"‘left’ and ‚low‛",
	"a’;Write-Output injected;’",
	"'‘’‚‛",
	"trailing’",
}

func TestPSQuoteRoundTripsThroughPowerShell(t *testing.T) {
	shell := ""
	for _, name := range []string{"pwsh", "powershell.exe"} {
		if path, err := exec.LookPath(name); err == nil {
			shell = path
			break
		}
	}
	if shell == "" {
		t.Skip("PowerShell is not installed")
	}
	for _, value := range powerShellQuoteCases {
		script := "$value = " + psQuote(value) + "\n" +
			"[Console]::Out.Write([Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($value)))\n"
		path := filepath.Join(t.TempDir(), "quote.ps1")
		if err := os.WriteFile(path, append([]byte{0xef, 0xbb, 0xbf}, script...), 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(shell, "-NoLogo", "-NoProfile", "-NonInteractive", "-File", path).CombinedOutput()
		if err != nil {
			t.Fatalf("PowerShell rejected psQuote(%q): %v\n%s", value, err, out)
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(out)))
		if err != nil || string(decoded) != value {
			t.Fatalf("PowerShell read psQuote(%q) as %q (output %q, err %v)", value, decoded, out, err)
		}
	}
}
