package cli

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type architectureCapabilityTestProvider struct{ denyAMD64 bool }

func (architectureCapabilityTestProvider) Name() string {
	return "architecture-capability-test"
}
func (architectureCapabilityTestProvider) Aliases() []string { return nil }
func (architectureCapabilityTestProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:             "architecture-capability-test",
		Family:           "architecture-capability-test",
		Kind:             ProviderKindSSHLease,
		Targets:          []TargetSpec{{OS: targetLinux}, {OS: targetMacOS}, {OS: targetWindows, WindowsMode: windowsModeNormal}, {OS: targetWindows, WindowsMode: windowsModeWSL2}},
		Coordinator:      CoordinatorNever,
		ClassDisposition: ProviderClassDispositionUnmapped,
	}
}
func (architectureCapabilityTestProvider) RegisterFlags(*flag.FlagSet, Config) any {
	return noProviderFlags{}
}
func (architectureCapabilityTestProvider) ApplyFlags(*Config, *flag.FlagSet, any) error {
	return nil
}
func (architectureCapabilityTestProvider) Configure(Config, Runtime) (Backend, error) {
	return nil, nil
}
func (p architectureCapabilityTestProvider) SupportsArchitecture(_ Config, architecture string) bool {
	return architecture == ArchitectureARM64 || (architecture == ArchitectureAMD64 && !p.denyAMD64)
}

func TestProviderArchitectureCapabilityOwnsTargetAdmission(t *testing.T) {
	p := architectureCapabilityTestProvider{denyAMD64: true}
	providerRegistry[p.Name()] = p
	t.Cleanup(func() { delete(providerRegistry, p.Name()) })
	for _, target := range []string{targetLinux, targetMacOS, targetWindows} {
		for _, arch := range []string{ArchitectureAMD64, ArchitectureARM64} {
			t.Run(target+"/"+arch, func(t *testing.T) {
				cfg := baseConfig()
				cfg.Provider, cfg.TargetOS, cfg.Architecture = p.Name(), target, arch
				cfg.architectureExplicit = true
				err := validateProviderTarget(cfg)
				if (err == nil) != (arch == ArchitectureARM64) {
					t.Fatalf("capability admission architecture=%s: %v", arch, err)
				}
			})
		}
	}
	cfg := baseConfig()
	cfg.Provider, cfg.TargetOS, cfg.Architecture = p.Name(), targetWorkerRuntime, ArchitectureARM64
	if err := validateProviderTarget(cfg); err == nil {
		t.Fatal("capability bypassed ProviderSpec.Targets")
	}
}

func TestProviderArchitectureCapabilityAdmitsStaticArchitecture(t *testing.T) {
	provider := architectureCapabilityTestProvider{}
	for _, architecture := range []string{ArchitectureAMD64, ArchitectureARM64} {
		t.Run(architecture, func(t *testing.T) {
			cfg := baseConfig()
			cfg.TargetOS = targetLinux
			if !providerSupportsArchitecture(provider, cfg, architecture) {
				t.Fatalf("architecture capability rejected %s", architecture)
			}
		})
	}
}

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
	if err == nil ||
		!strings.Contains(err.Error(), "nested virtualization") ||
		!strings.Contains(err.Error(), "m8i.4xlarge") ||
		!strings.Contains(err.Error(), "class=tiny|small|standard|fast|large|beast") {
		t.Fatalf("err=%v", err)
	}
}

func TestValidateProviderTargetRejectsArchitectureTypeMismatch(t *testing.T) {
	tests := []struct {
		name         string
		provider     string
		architecture string
		serverType   string
		want         string
	}{
		{name: "aws arm with x86 type", provider: "aws", architecture: ArchitectureARM64, serverType: "c7a.48xlarge", want: "requires an ARM64 AWS instance type"},
		{name: "aws amd64 with arm type", provider: "aws", architecture: ArchitectureAMD64, serverType: "c7g.16xlarge", want: "requires an amd64 AWS instance type"},
		{name: "azure arm with x86 size", provider: "azure", architecture: ArchitectureARM64, serverType: "Standard_D96ds_v6", want: "requires an ARM64 Azure VM size"},
		{name: "azure amd64 with arm size", provider: "azure", architecture: ArchitectureAMD64, serverType: "Standard_D96pds_v6", want: "requires an amd64 Azure VM size"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Provider = tc.provider
			cfg.TargetOS = targetLinux
			cfg.Architecture = tc.architecture
			cfg.architectureExplicit = true
			cfg.ServerType = tc.serverType
			cfg.ServerTypeExplicit = true

			err := validateProviderTarget(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
		})
	}
}

func TestValidateProviderTargetIgnoresArchitectureForWorkerRuntime(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "cloudflare-dynamic-workers"
	cfg.TargetOS = targetWorkerRuntime
	cfg.Architecture = ArchitectureARM64
	cfg.architectureExplicit = true

	if err := validateProviderTarget(cfg); err != nil {
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

func TestValidateProviderTargetAllowsAzureWindowsARM64(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "azure"
	cfg.TargetOS = targetWindows
	cfg.WindowsMode = windowsModeNormal
	cfg.Architecture = ArchitectureARM64
	cfg.architectureExplicit = true
	cfg.ServerType = "Standard_D32pds_v6"
	cfg.ServerTypeExplicit = true
	cfg.AzureImage = "Contoso:windows-arm64:server:latest"
	if err := validateProviderTarget(cfg); err != nil {
		t.Fatalf("err=%v", err)
	}
}

func TestValidateProviderTargetAllowsTartMacOSARM64(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "tart"
	cfg.TargetOS = targetMacOS
	cfg.Architecture = ArchitectureARM64
	cfg.architectureExplicit = true
	if err := validateProviderTarget(cfg); err != nil {
		t.Fatalf("err=%v", err)
	}
}

func TestValidateProviderTargetAllowsExternalARM64Targets(t *testing.T) {
	for _, target := range []struct {
		os   string
		mode string
	}{
		{os: targetLinux, mode: windowsModeNormal},
		{os: targetMacOS, mode: windowsModeNormal},
		{os: targetWindows, mode: windowsModeNormal},
		{os: targetWindows, mode: windowsModeWSL2},
	} {
		cfg := baseConfig()
		cfg.Provider = "external"
		cfg.TargetOS = target.os
		cfg.WindowsMode = target.mode
		cfg.Architecture = ArchitectureARM64
		cfg.architectureExplicit = true
		if err := validateProviderTarget(cfg); err != nil {
			t.Errorf("target=%s mode=%s err=%v", target.os, target.mode, err)
		}
	}
}

func TestValidateProviderTargetRejectsTartExplicitAMD64(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "tart"
	cfg.TargetOS = targetMacOS
	cfg.Architecture = ArchitectureAMD64
	cfg.architectureExplicit = true
	err := validateProviderTarget(cfg)
	if err == nil || !strings.Contains(err.Error(), "supports architecture=arm64 only") {
		t.Fatalf("err=%v", err)
	}
}

func TestValidateProviderTargetAllowsLumeMacOSARM64(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "lume"
	cfg.TargetOS = targetMacOS
	cfg.Architecture = ArchitectureARM64
	cfg.architectureExplicit = true
	if err := validateProviderTarget(cfg); err != nil {
		t.Fatalf("err=%v", err)
	}
}

func TestValidateProviderTargetDefaultsLumeToARM64(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "lume"
	cfg.TargetOS = targetMacOS
	if got := effectiveArchitectureForConfig(cfg); got != ArchitectureARM64 {
		t.Fatalf("effective architecture=%q want arm64", got)
	}
}

func TestValidateProviderTargetRejectsLumeExplicitAMD64(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "lume"
	cfg.TargetOS = targetMacOS
	cfg.Architecture = ArchitectureAMD64
	cfg.architectureExplicit = true
	err := validateProviderTarget(cfg)
	if err == nil || !strings.Contains(err.Error(), "supports architecture=arm64 only") {
		t.Fatalf("err=%v", err)
	}
}

func TestValidateProviderTargetDefaultsAppleVMToARM64(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "apple-vm"
	cfg.TargetOS = targetLinux
	if got := effectiveArchitectureForConfig(cfg); got != ArchitectureARM64 {
		t.Fatalf("effective architecture=%q want arm64", got)
	}
	if err := validateProviderTarget(cfg); err != nil {
		t.Fatalf("err=%v", err)
	}
}

func TestValidateProviderTargetAllowsAppleVMExplicitARM64(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "apple-vm"
	cfg.TargetOS = targetLinux
	cfg.Architecture = ArchitectureARM64
	cfg.architectureExplicit = true
	if err := validateProviderTarget(cfg); err != nil {
		t.Fatalf("err=%v", err)
	}
}

func TestValidateProviderTargetRejectsAppleVMExplicitAMD64(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "apple-vm"
	cfg.TargetOS = targetLinux
	cfg.Architecture = ArchitectureAMD64
	cfg.architectureExplicit = true
	err := validateProviderTarget(cfg)
	if err == nil || !strings.Contains(err.Error(), "supports architecture=arm64 only") {
		t.Fatalf("err=%v", err)
	}
}

func TestValidateProviderTargetDefaultsAppleContainerToARM64(t *testing.T) {
	for _, provider := range []string{"apple-container", "apple", "applecontainer"} {
		cfg := baseConfig()
		cfg.Provider = provider
		cfg.TargetOS = targetLinux
		if got := effectiveArchitectureForConfig(cfg); got != ArchitectureARM64 {
			t.Errorf("provider=%s effective architecture=%q want arm64", provider, got)
		}
	}
}

func TestProviderSupportsARM64IncludesAppleContainer(t *testing.T) {
	if !providerSupportsARM64("apple-container") {
		t.Fatal("apple-container should support ARM64")
	}
	if providerSupportsARM64("apple-machine") {
		t.Fatal("apple-machine should remain conservative until upstream AMD64 support is proven")
	}
}

func TestValidateProviderTargetDefaultsAWSLambdaMicroVMToARM64(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "aws-lambda-microvm"
	cfg.TargetOS = targetLinux
	if got := effectiveArchitectureForConfig(cfg); got != ArchitectureARM64 {
		t.Fatalf("effective architecture=%q want arm64", got)
	}
	if err := validateProviderTarget(cfg); err != nil {
		t.Fatalf("err=%v", err)
	}
}

func TestValidateProviderTargetAllowsAWSLambdaMicroVMExplicitARM64(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "aws-lambda-microvm"
	cfg.TargetOS = targetLinux
	cfg.Architecture = ArchitectureARM64
	cfg.architectureExplicit = true
	if err := validateProviderTarget(cfg); err != nil {
		t.Fatalf("err=%v", err)
	}
}

func TestValidateProviderTargetRejectsAWSLambdaMicroVMExplicitAMD64(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "aws-lambda-microvm"
	cfg.TargetOS = targetLinux
	cfg.Architecture = ArchitectureAMD64
	cfg.architectureExplicit = true
	err := validateProviderTarget(cfg)
	if err == nil || !strings.Contains(err.Error(), "supports architecture=arm64 only") {
		t.Fatalf("err=%v", err)
	}
}

func TestValidateProviderTargetRejectsAzureWindowsARM64WSL2(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "azure"
	cfg.TargetOS = targetWindows
	cfg.WindowsMode = windowsModeWSL2
	cfg.Architecture = ArchitectureARM64
	cfg.architectureExplicit = true
	cfg.ServerType = "Standard_D32pds_v6"
	cfg.ServerTypeExplicit = true
	cfg.AzureImage = "Contoso:windows-arm64:server:latest"
	err := validateProviderTarget(cfg)
	if err == nil || !strings.Contains(err.Error(), "supports windows.mode=normal only") {
		t.Fatalf("err=%v", err)
	}
}

func TestValidateProviderTargetRejectsAzureWindowsARM64WithoutExplicitImage(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "azure"
	cfg.TargetOS = targetWindows
	cfg.WindowsMode = windowsModeNormal
	cfg.Architecture = ArchitectureARM64
	cfg.architectureExplicit = true
	cfg.ServerType = "Standard_D32pds_v6"
	cfg.ServerTypeExplicit = true
	err := validateProviderTarget(cfg)
	if err == nil || !strings.Contains(err.Error(), "requires azure.image") {
		t.Fatalf("err=%v", err)
	}
}

func TestValidateProviderTargetRejectsAWSWindowsARM64(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "aws"
	cfg.TargetOS = targetWindows
	cfg.WindowsMode = windowsModeNormal
	cfg.Architecture = ArchitectureARM64
	cfg.architectureExplicit = true
	err := validateProviderTarget(cfg)
	if err == nil || !strings.Contains(err.Error(), "provider=azure target=windows") {
		t.Fatalf("err=%v", err)
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

func TestLeaseCreateFlagsRejectExeDevNonLinuxTarget(t *testing.T) {
	defaults := baseConfig()
	fs := newFlagSet("test", io.Discard)
	values := registerLeaseCreateFlags(fs, defaults)
	if err := parseFlags(fs, []string{"--provider", "exe-dev", "--target", "macos"}); err != nil {
		t.Fatal(err)
	}
	cfg := defaults
	err := applyLeaseCreateFlags(&cfg, fs, values)
	if err == nil || !strings.Contains(err.Error(), "target=linux only") {
		t.Fatalf("err=%v", err)
	}
}

func TestLeaseCreateFlagsApplyExplicitSSHPort(t *testing.T) {
	defaults := baseConfig()
	fs := newFlagSet("test", io.Discard)
	values := registerLeaseCreateFlags(fs, defaults)
	if err := parseFlags(fs, []string{"--provider", "parallels", "--target", "macos", "--ssh-port", "22"}); err != nil {
		t.Fatal(err)
	}
	cfg := defaults
	if err := applyLeaseCreateFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.SSHPort != "22" || !IsSSHPortExplicit(&cfg) {
		t.Fatalf("ssh port=%q explicit=%t", cfg.SSHPort, IsSSHPortExplicit(&cfg))
	}
}

func TestNormalizeTargetConfigForcesAWSMacOSLaunchdSSHPort(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "aws"
	cfg.TargetOS = targetMacOS
	cfg.SSHPort = "2222"
	cfg.SSHFallbackPorts = []string{"22", "2200"}

	normalizeTargetConfig(&cfg)

	if cfg.SSHPort != "22" {
		t.Fatalf("SSHPort=%q, want launchd Remote Login port 22", cfg.SSHPort)
	}
	if cfg.SSHFallbackPorts != nil {
		t.Fatalf("SSHFallbackPorts=%v, want nil", cfg.SSHFallbackPorts)
	}
}

func TestNormalizeTargetConfigWorkRoots(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*Config)
		wantUser  string
		wantRoot  string
	}{
		{
			name: "static macOS uses static user",
			configure: func(cfg *Config) {
				cfg.Provider = staticProvider
				cfg.TargetOS = targetMacOS
				cfg.Static.User = "alice"
			},
			wantUser: "alice",
			wantRoot: "/Users/alice/crabbox",
		},
		{
			name: "static macOS uses explicit SSH user",
			configure: func(cfg *Config) {
				cfg.Provider = staticProvider
				cfg.TargetOS = targetMacOS
				cfg.Static.User = "alice"
				cfg.SSHUser = "builder"
				MarkSSHUserExplicit(cfg)
			},
			wantUser: "builder",
			wantRoot: "/Users/builder/crabbox",
		},
		{
			name: "static macOS preserves static work root",
			configure: func(cfg *Config) {
				cfg.Provider = staticProvider
				cfg.TargetOS = targetMacOS
				cfg.Static.User = "alice"
				cfg.Static.WorkRoot = "/Volumes/build/crabbox"
			},
			wantUser: "alice",
			wantRoot: "/Volumes/build/crabbox",
		},
		{
			name: "static macOS preserves generic work root",
			configure: func(cfg *Config) {
				cfg.Provider = staticProvider
				cfg.TargetOS = targetMacOS
				cfg.Static.User = "alice"
				cfg.WorkRoot = "/srv/crabbox"
				MarkWorkRootExplicit(cfg)
			},
			wantUser: "alice",
			wantRoot: "/srv/crabbox",
		},
		{
			name: "AWS macOS keeps EC2 Mac default",
			configure: func(cfg *Config) {
				cfg.Provider = "aws"
				cfg.TargetOS = targetMacOS
			},
			wantUser: "ec2-user",
			wantRoot: defaultMacOSWorkRoot,
		},
		{
			name: "static Linux keeps POSIX default",
			configure: func(cfg *Config) {
				cfg.Provider = staticProvider
				cfg.TargetOS = targetLinux
				cfg.Static.User = "alice"
			},
			wantUser: "alice",
			wantRoot: defaultPOSIXWorkRoot,
		},
		{
			name: "static native Windows keeps Windows default",
			configure: func(cfg *Config) {
				cfg.Provider = staticProvider
				cfg.TargetOS = targetWindows
				cfg.WindowsMode = windowsModeNormal
				cfg.Static.User = "alice"
			},
			wantUser: "alice",
			wantRoot: defaultWindowsWorkRoot,
		},
		{
			name: "static WSL2 keeps POSIX default",
			configure: func(cfg *Config) {
				cfg.Provider = staticProvider
				cfg.TargetOS = targetWindows
				cfg.WindowsMode = windowsModeWSL2
				cfg.Static.User = "alice"
			},
			wantUser: "alice",
			wantRoot: defaultPOSIXWorkRoot,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseConfig()
			tt.configure(&cfg)

			normalizeTargetConfig(&cfg)

			if cfg.SSHUser != tt.wantUser {
				t.Fatalf("SSHUser=%q want %q", cfg.SSHUser, tt.wantUser)
			}
			if cfg.WorkRoot != tt.wantRoot {
				t.Fatalf("WorkRoot=%q want %q", cfg.WorkRoot, tt.wantRoot)
			}
		})
	}
}

func TestNormalizeTargetConfigUsesSealosWorkRoot(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "sealos-devbox"
	cfg.SealosDevbox.WorkRoot = "/home/devbox/project"

	normalizeTargetConfig(&cfg)

	if cfg.WorkRoot != "/home/devbox/project" {
		t.Fatalf("WorkRoot=%q want Sealos provider root", cfg.WorkRoot)
	}
}

func TestNormalizeTargetConfigPreservesExplicitWorkRootForSealos(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "sealos-devbox"
	cfg.WorkRoot = "/srv/crabbox"
	MarkWorkRootExplicit(&cfg)

	normalizeTargetConfig(&cfg)

	if cfg.WorkRoot != "/srv/crabbox" {
		t.Fatalf("WorkRoot=%q want explicit root", cfg.WorkRoot)
	}
}

func TestNormalizeTargetConfigPrefersExplicitSealosWorkRoot(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "sealos-devbox"
	cfg.WorkRoot = "/srv/crabbox"
	MarkWorkRootExplicit(&cfg)
	cfg.SealosDevbox.WorkRoot = "/home/devbox/override"
	MarkSealosDevboxWorkRootExplicit(&cfg)

	normalizeTargetConfig(&cfg)

	if cfg.WorkRoot != "/home/devbox/override" {
		t.Fatalf("WorkRoot=%q want explicit Sealos root", cfg.WorkRoot)
	}
}

func TestNormalizeTargetConfigPreservesExplicitPlatformDefaultWorkRoot(t *testing.T) {
	tests := []struct {
		name        string
		targetOS    string
		windowsMode string
		workRoot    string
	}{
		{name: "linux root on macOS", targetOS: targetMacOS, windowsMode: windowsModeNormal, workRoot: defaultPOSIXWorkRoot},
		{name: "macOS root on Linux", targetOS: targetLinux, windowsMode: windowsModeNormal, workRoot: defaultMacOSWorkRoot},
		{name: "macOS root on WSL2", targetOS: targetWindows, windowsMode: windowsModeWSL2, workRoot: defaultMacOSWorkRoot},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.TargetOS = tt.targetOS
			cfg.WindowsMode = tt.windowsMode
			cfg.WorkRoot = tt.workRoot
			MarkWorkRootExplicit(&cfg)

			normalizeTargetConfig(&cfg)

			if cfg.WorkRoot != tt.workRoot {
				t.Fatalf("WorkRoot=%q want explicit root %q", cfg.WorkRoot, tt.workRoot)
			}
		})
	}
}

func TestNormalizeTargetConfigPreservesExternalWorkRoot(t *testing.T) {
	for _, provider := range []string{"external", "exec-provider"} {
		t.Run(provider, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Provider = provider
			cfg.TargetOS = targetMacOS
			cfg.External.WorkRoot = defaultPOSIXWorkRoot
			cfg.WorkRoot = cfg.External.WorkRoot

			normalizeTargetConfig(&cfg)

			if cfg.WorkRoot != defaultPOSIXWorkRoot {
				t.Fatalf("WorkRoot=%q want external root %q", cfg.WorkRoot, defaultPOSIXWorkRoot)
			}
		})
	}
}

func TestLoadConfigPreservesExternalWorkRootFromEnvironment(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	if err := os.WriteFile(configPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("CRABBOX_PROVIDER", "external")
	t.Setenv("CRABBOX_TARGET", "macos")
	t.Setenv("CRABBOX_WINDOWS_MODE", "")
	t.Setenv("CRABBOX_WORK_ROOT", "")
	t.Setenv("CRABBOX_EXTERNAL_COMMAND", "provider-adapter")
	t.Setenv("CRABBOX_EXTERNAL_WORK_ROOT", defaultPOSIXWorkRoot)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TargetOS != targetMacOS || cfg.WorkRoot != defaultPOSIXWorkRoot {
		t.Fatalf("loaded config target=%q workRoot=%q", cfg.TargetOS, cfg.WorkRoot)
	}
}

func TestLeaseCreateFlagsDoNotApplyPortableOSImageToAzureWindows(t *testing.T) {
	defaults := baseConfig()
	fs := newFlagSet("test", io.Discard)
	values := registerLeaseCreateFlags(fs, defaults)
	if err := parseFlags(fs, []string{"--provider", "azure", "--target", "windows", "--os", "ubuntu:24.04"}); err != nil {
		t.Fatal(err)
	}
	cfg := defaults
	if err := applyLeaseCreateFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if got := azureImageForConfig(cfg); got != defaultAzureWindowsImage {
		t.Fatalf("azure image=%q want %q", got, defaultAzureWindowsImage)
	}
}
