package cli

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestManagedHeadlessWSLDisablesGUIBeforeLaunch(t *testing.T) {
	for _, test := range []struct {
		name, target, mode string
		desktop, browser   bool
		want               bool
	}{
		{"linux", targetLinux, "", false, false, false},
		{"native windows", targetWindows, windowsModeNormal, false, false, false},
		{"native desktop", targetWindows, windowsModeNormal, true, false, false},
		{"headless wsl2", targetWindows, windowsModeWSL2, false, false, true},
		{"wsl2 desktop", targetWindows, windowsModeWSL2, true, false, false},
		{"wsl2 browser", targetWindows, windowsModeWSL2, false, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.TargetOS, cfg.WindowsMode, cfg.Desktop, cfg.Browser = test.target, test.mode, test.desktop, test.browser
			script := cloudInit(cfg, "ssh-ed25519 test")
			if test.target == targetWindows {
				script = windowsBootstrapPowerShell(cfg, "ssh-ed25519 test")
			}
			if got := strings.Contains(script, "guiApplications=false"); got != test.want {
				t.Fatalf("headless WSL policy present=%t, want %t", got, test.want)
			}
			if test.want {
				write := strings.Index(script, "[IO.File]::WriteAllText($wslConfigPath")
				update := strings.Index(script, "wsl.exe --update")
				shutdown := strings.Index(script, "wsl.exe --shutdown")
				launch := strings.Index(script, "wsl.exe --set-default-version")
				if write < 0 || update <= write || shutdown <= update || launch <= shutdown ||
					!strings.Contains(script, "if ($wslConfigChanged)") {
					t.Fatal("WSLg policy must precede installation and restart changed configurations before distro launch")
				}
			}
		})
	}
}

func TestHeadlessWSLConfigPreservesOtherSettings(t *testing.T) {
	powerShell, err := exec.LookPath("pwsh")
	if err != nil {
		powerShell, err = exec.LookPath("powershell.exe")
	}
	if err != nil {
		t.Skip("PowerShell is unavailable")
	}
	cfg := baseConfig()
	cfg.TargetOS, cfg.WindowsMode = targetWindows, windowsModeWSL2
	bootstrap := windowsWSL2BootstrapPowerShell(cfg)
	start, end := strings.Index(bootstrap, "function ConvertTo-CrabboxHeadlessWSLConfig"), strings.Index(bootstrap, "$wslConfigPath =")
	if start < 0 || end <= start {
		t.Fatal("bootstrap does not configure the headless WSL runtime")
	}
	for _, test := range []struct{ name, input, want string }{
		{"missing file", "", "[wsl2]\nguiApplications=false"},
		{"already disabled", "[wsl2]\nguiApplications=false\n", "[wsl2]\nguiApplications=false\n"},
		{"preserve memory", "[wsl2]\nmemory=4GB\nguiApplications=true\n", "[wsl2]\nmemory=4GB\nguiApplications=false\n"},
		{"other section", "[experimental]\nsparseVhd=true", "[experimental]\nsparseVhd=true\n[wsl2]\nguiApplications=false"},
		{"insert before next section", "[wsl2]\nmemory=4GB\n[experimental]\nsparseVhd=true", "[wsl2]\nmemory=4GB\nguiApplications=false\n[experimental]\nsparseVhd=true"},
		{"case whitespace comments", "; keep\r\n [WSL2] ; keep\r\n GUIAPPLICATIONS = true\r\nprocessors=2", "; keep\n [WSL2] ; keep\nguiApplications=false\nprocessors=2"},
		{"same key in other section", "[other]\nguiApplications=true\n[wsl2]\nguiApplications=true", "[other]\nguiApplications=true\n[wsl2]\nguiApplications=false"},
		{"duplicate sections", "[wsl2]\nmemory=4GB\n[wsl2]\nguiApplications=true", "[wsl2]\nmemory=4GB\nguiApplications=false\n[wsl2]\nguiApplications=false"},
	} {
		t.Run(test.name, func(t *testing.T) {
			script := bootstrap[start:end] + "\n$first = ConvertTo-CrabboxHeadlessWSLConfig " + psQuote(test.input) + `
$second = ConvertTo-CrabboxHeadlessWSLConfig $first
@{first=$first;second=$second} | ConvertTo-Json -Compress`
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, powerShell, "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
			if err != nil {
				t.Fatalf("config conversion: %v: %s", err, out)
			}
			var got struct{ First, Second string }
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("invalid config result: %v: %s", err, out)
			}
			if got.First != test.want || got.Second != test.want {
				t.Fatalf("first=%q second=%q, want %q on both passes", got.First, got.Second, test.want)
			}
		})
	}
}
