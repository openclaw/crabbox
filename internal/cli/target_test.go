package cli

import (
	"strings"
	"testing"
)

func TestValidateProviderTargetRejectsUnsupportedAWSTargets(t *testing.T) {
	t.Run("macOS needs dedicated host", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Provider = "aws"
		cfg.TargetOS = targetMacOS
		err := validateProviderTarget(cfg)
		if err == nil || !strings.Contains(err.Error(), "requires CRABBOX_AWS_MAC_HOST_ID") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("Hetzner Windows needs an existing static host", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Provider = "hetzner"
		cfg.TargetOS = targetWindows
		err := validateProviderTarget(cfg)
		if err == nil || !strings.Contains(err.Error(), "managed provisioning supports target=linux only") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("Hetzner macOS points at AWS Mac or static hosts", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Provider = "hetzner"
		cfg.TargetOS = targetMacOS
		err := validateProviderTarget(cfg)
		if err == nil || !strings.Contains(err.Error(), "EC2 Mac Dedicated Host") {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestValidateProviderTargetAllowsAWSNativeWindows(t *testing.T) {
	for _, mode := range []string{windowsModeNormal, windowsModeWSL2} {
		t.Run(mode, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Provider = "aws"
			cfg.TargetOS = targetWindows
			cfg.WindowsMode = mode
			if err := validateProviderTarget(cfg); err != nil {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestValidateProviderTargetAllowsAzureNativeWindowsOnly(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "azure"
	cfg.TargetOS = targetWindows
	cfg.WindowsMode = windowsModeNormal
	if err := validateProviderTarget(cfg); err != nil {
		t.Fatalf("native err=%v", err)
	}

	cfg.WindowsMode = windowsModeWSL2
	err := validateProviderTarget(cfg)
	if err == nil || !strings.Contains(err.Error(), "native Windows only") {
		t.Fatalf("wsl2 err=%v", err)
	}
}

func TestValidateRequestedCapabilitiesRejectsAzureWindowsDesktop(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"desktop":   func(cfg *Config) { cfg.Desktop = true },
		"browser":   func(cfg *Config) { cfg.Browser = true },
		"code":      func(cfg *Config) { cfg.Code = true },
		"tailscale": func(cfg *Config) { cfg.Tailscale.Enabled = true },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Provider = "azure"
			cfg.TargetOS = targetWindows
			cfg.WindowsMode = windowsModeNormal
			mutate(&cfg)
			err := validateRequestedCapabilities(cfg)
			if err == nil || !strings.Contains(err.Error(), "SSH, sync, and run") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestValidateProviderTargetAllowsStaticNonLinux(t *testing.T) {
	for _, target := range []string{targetMacOS, targetWindows} {
		cfg := baseConfig()
		cfg.Provider = staticProvider
		cfg.TargetOS = target
		if err := validateProviderTarget(cfg); err != nil {
			t.Fatalf("target=%s err=%v", target, err)
		}
	}
}
