package cli

import (
	"flag"
	"path"
	"strings"
)

const (
	targetLinux   = "linux"
	targetMacOS   = "macos"
	targetWindows = "windows"

	windowsModeNormal = "normal"
	windowsModeWSL2   = "wsl2"

	defaultPOSIXWorkRoot   = "/work/crabbox"
	defaultMacOSWorkRoot   = "/Users/ec2-user/crabbox"
	defaultWindowsWorkRoot = `C:\crabbox`
)

const (
	TargetLinux       = targetLinux
	TargetMacOS       = targetMacOS
	TargetWindows     = targetWindows
	WindowsModeNormal = windowsModeNormal
	WindowsModeWSL2   = windowsModeWSL2
)

func normalizeTargetConfig(cfg *Config) {
	cfg.TargetOS = normalizeTargetOS(cfg.TargetOS)
	cfg.WindowsMode = normalizeWindowsMode(cfg.WindowsMode)
	if cfg.Provider == "aws" && cfg.TargetOS == targetMacOS && cfg.SSHUser == baseConfig().SSHUser {
		cfg.SSHUser = "ec2-user"
	}
	if cfg.Provider == "aws" && cfg.TargetOS == targetWindows && cfg.WindowsMode == windowsModeWSL2 && cfg.SSHUser == baseConfig().SSHUser {
		cfg.SSHUser = "Administrator"
	}
	if isStaticProvider(cfg.Provider) {
		if cfg.Static.User != "" && cfg.SSHUser == baseConfig().SSHUser {
			cfg.SSHUser = cfg.Static.User
		}
	}
	if isDefaultWorkRoot(cfg.WorkRoot) {
		cfg.WorkRoot = defaultWorkRootForTarget(cfg.TargetOS, cfg.WindowsMode)
	}
	if isStaticProvider(cfg.Provider) {
		if cfg.Static.Port != "" && cfg.SSHPort == baseConfig().SSHPort {
			cfg.SSHPort = cfg.Static.Port
		}
		if cfg.Static.WorkRoot != "" {
			cfg.WorkRoot = cfg.Static.WorkRoot
		}
	}
	if (cfg.Provider == "namespace-devbox" || cfg.Provider == "namespace") && isDefaultWorkRoot(cfg.WorkRoot) && cfg.Namespace.WorkRoot != "" {
		cfg.WorkRoot = cfg.Namespace.WorkRoot
	}
}

func isDefaultWorkRoot(value string) bool {
	switch value {
	case "", defaultPOSIXWorkRoot, defaultMacOSWorkRoot, defaultWindowsWorkRoot:
		return true
	default:
		return false
	}
}

func IsDefaultWorkRoot(value string) bool {
	return isDefaultWorkRoot(value)
}

func defaultWorkRootForTarget(targetOS, windowsMode string) string {
	if targetOS == targetMacOS {
		return defaultMacOSWorkRoot
	}
	if targetOS == targetWindows && windowsMode == windowsModeNormal {
		return defaultWindowsWorkRoot
	}
	return defaultPOSIXWorkRoot
}

func normalizeTargetOS(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "linux", "ubuntu":
		return targetLinux
	case "mac", "macos", "darwin", "osx":
		return targetMacOS
	case "win", "windows":
		return targetWindows
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func normalizeWindowsMode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "normal", "native", "powershell", "ps":
		return windowsModeNormal
	case "wsl", "wsl2":
		return windowsModeWSL2
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func validateTargetConfig(cfg Config) error {
	switch cfg.TargetOS {
	case targetLinux, targetMacOS, targetWindows:
	default:
		return exit(2, "target must be linux, macos, or windows")
	}
	if cfg.TargetOS != targetWindows && cfg.WindowsMode != windowsModeNormal {
		return exit(2, "windows.mode is only valid with target=windows")
	}
	if cfg.TargetOS == targetWindows {
		switch cfg.WindowsMode {
		case windowsModeNormal, windowsModeWSL2:
		default:
			return exit(2, "windows.mode must be normal or wsl2")
		}
	}
	return nil
}

func validateProviderTarget(cfg Config) error {
	provider, err := validateProviderTargetSupport(cfg)
	if err != nil {
		return err
	}
	if !providerSpecSupportsTarget(provider.Spec(), cfg.TargetOS, cfg.WindowsMode) {
		return exit(2, "%s", unsupportedManagedTargetMessageForConfig(provider.Name(), cfg))
	}
	if (provider.Name() == "tart" || provider.Name() == "apple-vz") && cfg.architectureExplicit && effectiveArchitectureForConfig(cfg) != ArchitectureARM64 {
		return exit(2, "provider=%s supports architecture=arm64 only", provider.Name())
	}
	if effectiveArchitectureForConfig(cfg) == ArchitectureARM64 {
		if provider.Name() != "azure" && provider.Name() != "aws" && provider.Name() != "tart" && provider.Name() != "apple-vz" {
			return exit(2, "architecture=arm64 currently supports provider=azure, provider=aws, provider=tart, or provider=apple-vz")
		}
		if cfg.TargetOS != targetLinux &&
			!(provider.Name() == "azure" && cfg.TargetOS == targetWindows) &&
			!(provider.Name() == "tart" && cfg.TargetOS == targetMacOS) {
			return exit(2, "architecture=arm64 currently supports target=linux, provider=azure target=windows, or provider=tart target=macos only")
		}
		if provider.Name() == "azure" && cfg.TargetOS == targetWindows && cfg.WindowsMode == windowsModeWSL2 {
			return exit(2, "provider=azure target=windows architecture=arm64 supports windows.mode=normal only; windows.mode=wsl2 requires nested virtualization, which Azure Cobalt ARM64 VM sizes do not support")
		}
		if provider.Name() == "azure" && cfg.TargetOS == targetWindows && !azureWindowsARM64HasExplicitImage(cfg) {
			return exit(2, "provider=azure target=windows architecture=arm64 requires azure.image or CRABBOX_AZURE_IMAGE with an ARM64 Windows image; the built-in Windows default is x64")
		}
	}
	if (cfg.TargetOS == targetLinux || (provider.Name() == "azure" && cfg.TargetOS == targetWindows)) && strings.TrimSpace(cfg.ServerType) != "" {
		switch provider.Name() {
		case "aws":
			if err := validateArchitectureServerType("AWS instance type", cfg, awsInstanceTypeIsARM64(cfg.ServerType)); err != nil {
				return err
			}
		case "azure":
			if err := validateArchitectureServerType("Azure VM size", cfg, azureVMSizeIsARM64(cfg.ServerType)); err != nil {
				return err
			}
		}
	}
	if provider.Name() == "aws" &&
		cfg.TargetOS == targetWindows &&
		cfg.WindowsMode == windowsModeWSL2 &&
		cfg.ServerTypeExplicit &&
		!awsInstanceTypeSupportsNestedVirtualization(cfg.ServerType) {
		return exit(2, "provider=aws target=windows windows.mode=wsl2 requires an instance type with AWS nested virtualization; %s is not supported. Use --type m8i.4xlarge or omit --type and choose class=standard|fast|large|beast", cfg.ServerType)
	}
	if cfg.Provider == "aws" && cfg.TargetOS == targetMacOS {
		if cfg.HostID == "" && cfg.AWSMacHostID == "" && cfg.Coordinator == "" {
			return exit(2, "provider=aws target=macos requires CRABBOX_HOST_ID, hostId, CRABBOX_AWS_MAC_HOST_ID, or aws.macHostId for an allocated host")
		}
		if cfg.Capacity.Market != "on-demand" {
			return exit(2, "provider=aws target=macos requires --market on-demand; EC2 Mac instances are not Spot")
		}
		return nil
	}
	return nil
}

func validateProviderTargetSupport(cfg Config) (Provider, error) {
	provider, err := ProviderFor(cfg.Provider)
	if err != nil {
		return nil, err
	}
	if !providerSpecSupportsTarget(provider.Spec(), cfg.TargetOS, cfg.WindowsMode) {
		return nil, exit(2, "%s", unsupportedManagedTargetMessageForConfig(provider.Name(), cfg))
	}
	return provider, nil
}

func validateArchitectureServerType(kind string, cfg Config, serverTypeARM64 bool) error {
	architecture := effectiveArchitectureForConfig(cfg)
	if architecture == ArchitectureARM64 && !serverTypeARM64 {
		return exit(2, "architecture=arm64 requires an ARM64 %s; %s is not ARM64", kind, cfg.ServerType)
	}
	if cfg.architectureExplicit && cfg.Architecture == ArchitectureAMD64 && serverTypeARM64 {
		return exit(2, "architecture=amd64 requires an amd64 %s; %s is ARM64", kind, cfg.ServerType)
	}
	return nil
}

func providerSpecSupportsTarget(spec ProviderSpec, targetOS, windowsMode string) bool {
	for _, target := range spec.Targets {
		if target.OS != targetOS {
			continue
		}
		if targetOS == targetWindows && target.WindowsMode != "" && target.WindowsMode != windowsMode {
			continue
		}
		return true
	}
	return false
}

func unsupportedManagedTargetMessageForConfig(provider string, cfg Config) string {
	target := cfg.TargetOS
	if provider == "azure" {
		if target == targetMacOS {
			return "provider=azure managed provisioning supports target=linux and Windows only; use provider=aws with an EC2 Mac Dedicated Host or provider=ssh for existing macOS hosts"
		}
		return "provider=azure managed provisioning supports target=linux and Windows only"
	}
	switch target {
	case targetWindows:
		return sprintf("provider=%s managed provisioning supports target=linux only; use provider=aws for managed Windows or provider=ssh for existing Windows hosts", provider)
	case targetMacOS:
		return sprintf("provider=%s managed provisioning supports target=linux only; use provider=aws with an EC2 Mac Dedicated Host or provider=ssh for existing macOS hosts", provider)
	default:
		return sprintf("provider=%s managed provisioning supports target=linux only", provider)
	}
}

func newTargetCoordinatorClient(cfg Config) (*CoordinatorClient, bool, error) {
	if isStaticProvider(cfg.Provider) && !shouldRegisterCoordinatorLease(cfg) {
		return nil, false, nil
	}
	return newCoordinatorClient(cfg)
}

func isStaticProvider(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "ssh", "static", "static-ssh":
		return true
	default:
		return false
	}
}

// staticLeaseIDPrefix is the prefix `staticLease` stamps on lease IDs it
// synthesises from a static SSH host.
const staticLeaseIDPrefix = "static_"

// autoRouteStaticLease infers `--provider ssh` from a `static_<slug>` lease ID
// and restores the original static host from the local claim when the caller
// did not already pass --static-host.
func autoRouteStaticLease(cfg *Config, fs *flag.FlagSet, id string) error {
	id = strings.TrimSpace(id)
	suffix, hasStaticPrefix := strings.CutPrefix(id, staticLeaseIDPrefix)
	if flagWasSet(fs, "provider") && !isStaticProvider(cfg.Provider) {
		return nil
	}
	claim, hasClaim, err := staticLeaseClaim(id)
	if err != nil {
		return err
	}
	if (!hasStaticPrefix || suffix == "") && !hasClaim {
		return nil
	}
	if !flagWasSet(fs, "provider") {
		cfg.Provider = staticProvider
	}
	if !isStaticProvider(cfg.Provider) {
		return nil
	}
	if hasClaim {
		restoreStaticClaimIdentity(cfg, claim)
		restoreStaticClaimTarget(cfg, fs, claim)
	}
	normalizeTargetConfig(cfg)
	return validateTargetConfig(*cfg)
}

func staticLeaseClaim(id string) (leaseClaim, bool, error) {
	claim, ok, err := resolveLeaseClaim(id)
	if err != nil || !ok || !isStaticProvider(claim.Provider) {
		return leaseClaim{}, false, err
	}
	return claim, true, nil
}

func restoreStaticClaimIdentity(cfg *Config, claim leaseClaim) {
	if strings.TrimSpace(claim.LeaseID) != "" {
		cfg.Static.ID = strings.TrimSpace(claim.LeaseID)
	}
	if strings.TrimSpace(claim.Slug) != "" {
		cfg.Static.Name = strings.TrimSpace(claim.Slug)
	}
}

func restoreStaticClaimTarget(cfg *Config, fs *flag.FlagSet, claim leaseClaim) {
	if !flagWasSet(fs, "static-host") && strings.TrimSpace(claim.StaticHost) != "" {
		cfg.Static.Host = strings.TrimSpace(claim.StaticHost)
	}
	if !flagWasSet(fs, "static-user") && strings.TrimSpace(claim.StaticUser) != "" {
		cfg.Static.User = strings.TrimSpace(claim.StaticUser)
	}
	if !flagWasSet(fs, "static-port") && strings.TrimSpace(claim.StaticPort) != "" {
		cfg.Static.Port = strings.TrimSpace(claim.StaticPort)
	}
	if !flagWasSet(fs, "static-work-root") && strings.TrimSpace(claim.StaticWorkRoot) != "" {
		cfg.Static.WorkRoot = strings.TrimSpace(claim.StaticWorkRoot)
	}
	if !flagWasSet(fs, "target") && strings.TrimSpace(claim.TargetOS) != "" {
		cfg.TargetOS = strings.TrimSpace(claim.TargetOS)
	}
	if !flagWasSet(fs, "windows-mode") && strings.TrimSpace(claim.WindowsMode) != "" {
		cfg.WindowsMode = strings.TrimSpace(claim.WindowsMode)
	}
}

func isWindowsNativeTarget(target SSHTarget) bool {
	return target.TargetOS == targetWindows && target.WindowsMode == windowsModeNormal
}

func isWindowsWSL2Target(target SSHTarget) bool {
	return target.TargetOS == targetWindows && target.WindowsMode == windowsModeWSL2
}

func applyResolvedLeaseConfig(cfg *Config, server Server, target *SSHTarget) {
	if cfg == nil || target == nil {
		return
	}
	configuredSSHUser := cfg.SSHUser
	if targetOS := firstNonBlank(server.Labels["target"], target.TargetOS); targetOS != "" {
		cfg.TargetOS = targetOS
	}
	if windowsMode := firstNonBlank(server.Labels["windows_mode"], target.WindowsMode); windowsMode != "" {
		cfg.WindowsMode = windowsMode
	} else if cfg.TargetOS != targetWindows {
		cfg.WindowsMode = ""
	}
	if workRoot := strings.TrimSpace(server.Labels["work_root"]); workRoot != "" {
		cfg.WorkRoot = workRoot
	}
	normalizeTargetConfig(cfg)
	target.TargetOS = cfg.TargetOS
	target.WindowsMode = cfg.WindowsMode
	if target.User == "" || target.User == configuredSSHUser {
		target.User = cfg.SSHUser
	}
}

func applyStoredLeaseClaimConfig(cfg *Config, claim leaseClaim) {
	if cfg == nil {
		return
	}
	labels := cloneStringMap(claim.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	if labels["work_root"] == "" {
		labels["work_root"] = strings.TrimSpace(claim.StaticWorkRoot)
	}
	target := SSHTarget{
		TargetOS:    claim.TargetOS,
		WindowsMode: claim.WindowsMode,
	}
	applyResolvedLeaseConfig(cfg, Server{Labels: labels}, &target)
}

func remoteJoin(cfg Config, parts ...string) string {
	values := make([]string, 0, len(parts)+1)
	if cfg.WorkRoot != "" {
		values = append(values, cfg.WorkRoot)
	}
	values = append(values, parts...)
	if cfg.TargetOS == targetWindows && cfg.WindowsMode == windowsModeNormal {
		return windowsPathJoin(values...)
	}
	return path.Join(values...)
}

func RemoteJoin(cfg Config, parts ...string) string {
	return remoteJoin(cfg, parts...)
}

func windowsPathJoin(parts ...string) string {
	out := ""
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		part = strings.ReplaceAll(part, "/", `\`)
		if out == "" {
			out = strings.TrimRight(part, `\`)
			continue
		}
		out = strings.TrimRight(out, `\`) + `\` + strings.Trim(part, `\`)
	}
	return out
}
