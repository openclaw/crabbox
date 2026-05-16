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
		if err == nil || !strings.Contains(err.Error(), "requires CRABBOX_HOST_ID") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("macOS accepts provider neutral host id", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Provider = "aws"
		cfg.TargetOS = targetMacOS
		cfg.HostID = "h-000000000001"
		cfg.Capacity.Market = "on-demand"
		if err := validateProviderTarget(cfg); err != nil {
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

func TestValidateProviderTargetRejectsAWSWSL2ExactTypeWithoutNestedVirtualization(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "aws"
	cfg.TargetOS = targetWindows
	cfg.WindowsMode = windowsModeWSL2
	cfg.ServerType = "m7i.xlarge"
	cfg.ServerTypeExplicit = true

	err := validateProviderTarget(cfg)
	if err == nil || !strings.Contains(err.Error(), "nested virtualization") || !strings.Contains(err.Error(), "m8i.4xlarge") {
		t.Fatalf("err=%v", err)
	}
}

func TestValidateProviderTargetAllowsAzureWindowsModes(t *testing.T) {
	for _, mode := range []string{windowsModeNormal, windowsModeWSL2} {
		t.Run(mode, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Provider = "azure"
			cfg.TargetOS = targetWindows
			cfg.WindowsMode = mode
			if err := validateProviderTarget(cfg); err != nil {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestValidateRequestedCapabilitiesAllowsAzureWindowsDesktop(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "azure"
	cfg.TargetOS = targetWindows
	cfg.WindowsMode = windowsModeNormal
	cfg.Desktop = true
	if err := validateRequestedCapabilities(cfg); err != nil {
		t.Fatalf("desktop err=%v", err)
	}
}

func TestValidateRequestedCapabilitiesRejectsWindowsWSL2Desktop(t *testing.T) {
	for _, provider := range []string{"aws", "azure"} {
		t.Run(provider, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Provider = provider
			cfg.TargetOS = targetWindows
			cfg.WindowsMode = windowsModeWSL2
			cfg.Desktop = true
			err := validateRequestedCapabilities(cfg)
			if err == nil || !strings.Contains(err.Error(), "does not support desktop/VNC") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestValidateRequestedCapabilitiesRejectsAzureWindowsUnsupportedCapabilities(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
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
			if err == nil || !strings.Contains(err.Error(), "browser/code/tailscale") {
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
