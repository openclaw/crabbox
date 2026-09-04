package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type bootstrapFixture struct {
	TailscaleMode string   `json:"tailscaleMode"`
	Name          string   `json:"name"`
	Target        string   `json:"target"`
	Architecture  string   `json:"architecture"`
	WindowsMode   string   `json:"windowsMode"`
	Desktop       bool     `json:"desktop"`
	Browser       bool     `json:"browser"`
	Code          bool     `json:"code"`
	Tailscale     bool     `json:"tailscale"`
	User          string   `json:"user"`
	WorkRoot      string   `json:"workRoot"`
	PublicKey     string   `json:"publicKey"`
	Ports         []string `json:"ports"`
}

func TestSharedBootstrapFixtures(t *testing.T) {
	data, err := os.ReadFile("../../testdata/bootstrap/fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []bootstrapFixture
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			cfg := defaultConfig()
			cfg.Provider = "aws"
			cfg.TargetOS = fixture.Target
			cfg.Architecture = fixture.Architecture
			cfg.WindowsMode = windowsModeNormal
			if fixture.WindowsMode != "" {
				cfg.WindowsMode = fixture.WindowsMode
			}
			cfg.Desktop, cfg.Browser, cfg.Code = fixture.Desktop, fixture.Browser, fixture.Code
			cfg.SSHUser, cfg.WorkRoot = fixture.User, fixture.WorkRoot
			cfg.SSHPort, cfg.SSHFallbackPorts = fixture.Ports[0], fixture.Ports[1:]
			fragments := map[string]string{}
			switch fixture.Target {
			case targetWindows:
				fragments["header"] = sharedWindowsHeader(cfg.SSHUser, fixture.PublicKey, windowsBootstrapWorkRoot(cfg), fixture.Ports)
				fragments["core"] = sharedWindowsCore()
				if cfg.WindowsMode != windowsModeWSL2 {
					fragments["runtime"] = sharedWindowsRuntime()
					fragments["runtimeGate"] = sharedWindowsRuntimeGate()
				}
				if cfg.WindowsMode == windowsModeWSL2 {
					fragments["prelude"] = sharedWindowsNativePrelude()
					fragments["trufflehog"] = sharedWslTruffleHogInstall()
				} else if cfg.Desktop {
					fragments["prelude"] = sharedWindowsDesktopPrelude()
					fragments["desktop"] = sharedWindowsDesktop()
				} else {
					fragments["prelude"] = sharedWindowsNativePrelude()
					fragments["finalize"] = sharedWindowsFinalize()
				}
			case targetMacOS:
				fragments["macos"] = sharedMacOS(cfg.SSHUser, fixture.PublicKey, cfg.WorkRoot, fixture.Ports)
			default:
				fragments["ssh"] = indentCloudInitRuncmd(sharedLinuxSSHRestart())
				if cfg.Code {
					fragments["code"] = sharedCodeServerInstall()
				}
			}
			if fixture.Tailscale {
				cfg.Tailscale.Enabled = true
				cfg.Tailscale.AuthKey = "fixture-not-a-live-key"
				t.Setenv("CRABBOX_TAILSCALE_INSTALL_MODE", fixture.TailscaleMode)
				t.Setenv("CRABBOX_TAILSCALE_VERSION", defaultTailscaleVersion)
				t.Setenv("CRABBOX_TAILSCALE_SHA256_AMD64", defaultTailscaleAMD64SHA256)
				t.Setenv("CRABBOX_TAILSCALE_SHA256_ARM64", defaultTailscaleARM64SHA256)
				if fixture.TailscaleMode == "pinned" {
					fragments["tailscale"] = sharedTailscalePinnedInstall(defaultTailscaleVersion, defaultTailscaleAMD64SHA256, defaultTailscaleARM64SHA256)
				} else {
					fragments["tailscale"] = sharedTailscalePackageInstall()
				}
			}
			script := awsUserData(cfg, fixture.PublicKey)
			if cfg.TargetOS == targetWindows {
				script = windowsBootstrapPowerShell(cfg, fixture.PublicKey)
				if cfg.WindowsMode == windowsModeWSL2 {
					assertWindowsRuntimeAbsent(t, script)
				}
			}
			for name, fragment := range fragments {
				if !strings.Contains(script, fragment) {
					t.Errorf("missing shared %s fragment", name)
				}
			}

			if fixture.Browser && !strings.Contains(script, googleLinuxSigningKeyFingerprint) {
				t.Error("missing shared browser signing fingerprint")
			}
		})
	}
}
