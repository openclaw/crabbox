package cli

import (
	"flag"
	"os"
	"path"
	"strings"
)

const (
	targetLinux         = "linux"
	targetMacOS         = "macos"
	targetWindows       = "windows"
	targetWorkerRuntime = "worker-runtime"

	windowsModeNormal = "normal"
	windowsModeWSL2   = "wsl2"

	defaultPOSIXWorkRoot   = "/work/crabbox"
	defaultMacOSWorkRoot   = "/Users/ec2-user/crabbox"
	defaultWindowsWorkRoot = `C:\crabbox`
)

const (
	TargetLinux         = targetLinux
	TargetMacOS         = targetMacOS
	TargetWindows       = targetWindows
	TargetWorkerRuntime = targetWorkerRuntime
	WindowsModeNormal   = windowsModeNormal
	WindowsModeWSL2     = windowsModeWSL2
)

func normalizeTargetConfig(cfg *Config) {
	cfg.TargetOS = normalizeTargetOS(cfg.TargetOS)
	cfg.WindowsMode = normalizeWindowsMode(cfg.WindowsMode)
	if cfg.Provider == "aws" && cfg.TargetOS == targetMacOS {
		if cfg.SSHUser == baseConfig().SSHUser {
			cfg.SSHUser = "ec2-user"
		}
		// EC2 Mac uses Apple's socket-activated Remote Login service, whose
		// launchd socket remains on port 22 even when sshd_config lists others.
		cfg.SSHPort = "22"
		cfg.SSHFallbackPorts = nil
	}
	if cfg.Provider == "aws" && cfg.TargetOS == targetWindows && cfg.WindowsMode == windowsModeWSL2 && cfg.SSHUser == baseConfig().SSHUser {
		cfg.SSHUser = "Administrator"
	}
	if isStaticProvider(cfg.Provider) {
		if cfg.Static.User != "" && cfg.SSHUser == baseConfig().SSHUser {
			cfg.SSHUser = cfg.Static.User
		}
	}
	if shouldDeriveTargetWorkRoot(cfg) {
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
	if cfg.Provider == "sealos-devbox" && (IsSealosDevboxWorkRootExplicit(cfg) || (!IsWorkRootExplicit(cfg) && isDefaultWorkRoot(cfg.WorkRoot))) {
		cfg.WorkRoot = EffectiveSealosDevboxWorkRoot(*cfg)
	}
}

func shouldDeriveTargetWorkRoot(cfg *Config) bool {
	if strings.TrimSpace(cfg.WorkRoot) == "" {
		return true
	}
	if IsWorkRootExplicit(cfg) {
		return false
	}
	// The external provider owns its work root. This also covers persisted
	// routing state, whose root must survive target normalization verbatim.
	if (cfg.Provider == "external" || cfg.Provider == "exec-provider") && strings.TrimSpace(cfg.External.WorkRoot) != "" && cfg.WorkRoot == cfg.External.WorkRoot {
		return false
	}
	return isDefaultWorkRoot(cfg.WorkRoot)
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
	case targetLinux, targetMacOS, targetWindows, targetWorkerRuntime:
	default:
		return exit(2, "target must be linux, macos, windows, or worker-runtime")
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
	machineTarget := cfg.TargetOS != targetWorkerRuntime
	if machineTarget && (provider.Name() == "tart" || provider.Name() == "apple-vm" || provider.Name() == "lume" || provider.Name() == "aws-lambda-microvm") && cfg.architectureExplicit && effectiveArchitectureForConfig(cfg) != ArchitectureARM64 {
		return exit(2, "provider=%s supports architecture=arm64 only", provider.Name())
	}
	if machineTarget && effectiveArchitectureForConfig(cfg) == ArchitectureARM64 {
		if !providerSupportsARM64(provider.Name()) {
			return exit(2, "architecture=arm64 currently supports provider=azure, provider=aws, provider=tart, provider=apple-container, provider=apple-vm, provider=lume, provider=aws-lambda-microvm, or provider=external")
		}
		if cfg.TargetOS != targetLinux &&
			!(provider.Name() == "azure" && cfg.TargetOS == targetWindows) &&
			!((provider.Name() == "tart" || provider.Name() == "lume") && cfg.TargetOS == targetMacOS) &&
			!(provider.Name() == "external" && (cfg.TargetOS == targetMacOS || cfg.TargetOS == targetWindows)) {
			return exit(2, "architecture=arm64 currently supports target=linux, provider=azure target=windows, provider=tart/provider=lume target=macos, or provider=external target=macos/windows only")
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

func providerSupportsARM64(name string) bool {
	switch name {
	case "azure", "aws", "tart", "apple-container", "apple-vm", "lume", "aws-lambda-microvm", "external":
		return true
	default:
		return false
	}
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
	if provider == "lume" {
		return "provider=lume supports target=macos only"
	}
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
	prepareProviderDefaults(cfg)
	if hasClaim {
		restoreStaticClaimIdentity(cfg, claim)
		restoreStaticClaimTarget(cfg, fs, claim)
	}
	normalizeTargetConfig(cfg)
	return validateTargetConfig(*cfg)
}

// autoRouteExternalLease restores the provider configuration captured when an
// external lease was acquired. This keeps existing leases addressable after
// the user's current external configuration changes.
func autoRouteExternalLease(cfg *Config, fs *flag.FlagSet, id string) error {
	providerExplicit := flagWasSet(fs, "provider")
	if providerExplicit {
		cfg.providerExplicit = true
	}
	if flagWasSet(fs, "target") {
		cfg.targetFlagExplicit = true
	}
	if flagWasSet(fs, "windows-mode") {
		cfg.windowsModeFlagExplicit = true
	}
	return autoRouteExternalLeaseWithHints(
		cfg,
		id,
		flagWasSet(fs, "external-routing-file"),
		IsExternalDesktopTargetExplicit(cfg),
		IsExternalDesktopWindowsModeExplicit(cfg),
	)
}

func autoRouteExternalLeaseForConfig(cfg *Config, id string) error {
	return autoRouteExternalLeaseWithHints(cfg, id, false, IsExternalDesktopTargetExplicit(cfg), IsExternalDesktopWindowsModeExplicit(cfg))
}

func routeExternalLeaseClaim(cfg *Config, leaseID string) error {
	path, err := ExternalRoutingPath(leaseID)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return exit(2, "external routing state is missing for lease %s; refusing unverified cleanup", leaseID)
		}
		return err
	}
	cfg.External.RoutingFile = ""
	cfg.External.routingLoaded = false
	return autoRouteExternalLeaseForConfig(cfg, leaseID)
}

func autoRouteExternalLeaseWithHints(cfg *Config, id string, routingExplicit, targetExplicit, windowsModeExplicit bool) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil
	}
	provider, providerErr := ProviderFor(cfg.Provider)
	providerSelected := providerErr == nil && provider.Name() == "external"
	if cfg.providerExplicit {
		if providerErr != nil || provider.Name() != "external" {
			return nil
		}
	}
	if routingExplicit {
		if !cfg.providerExplicit {
			cfg.Provider = "external"
		}
		return restoreExternalLeaseTarget(cfg, targetExplicit, windowsModeExplicit)
	}
	if providerSelected && strings.TrimSpace(cfg.External.RoutingFile) != "" {
		if !cfg.External.routingLoaded {
			if err := loadExternalRoutingConfig(cfg, cfg.External.RoutingFile, false); err != nil {
				return err
			}
		}
		return restoreExternalLeaseTarget(cfg, targetExplicit, windowsModeExplicit)
	}
	claim, ok, err := uniqueExternalLeaseClaim(id, providerSelected)
	if err != nil || !ok {
		return err
	}
	path, err := ExternalRoutingPath(claim.LeaseID)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			if providerSelected {
				return restoreExternalLeaseTarget(cfg, targetExplicit, windowsModeExplicit)
			}
			return exit(2, "external routing state is missing for lease %s; select provider=external explicitly only if the current lifecycle still owns it", claim.LeaseID)
		}
		return err
	}
	if !cfg.providerExplicit {
		cfg.Provider = "external"
	}
	if err := loadExternalRoutingConfig(cfg, path, true); err != nil {
		return err
	}
	return restoreExternalLeaseTarget(cfg, targetExplicit, windowsModeExplicit)
}

func loadExternalRoutingConfig(cfg *Config, path string, claimBound bool) error {
	return loadExternalRoutingConfigWithDigest(cfg, path, "", claimBound)
}

func loadExternalRoutingConfigWithDigest(cfg *Config, path, expectedDigest string, claimBound bool) error {
	var (
		routing ExternalConfig
		err     error
	)
	if strings.TrimSpace(expectedDigest) == "" {
		routing, err = LoadExternalRouting(path)
	} else {
		routing, err = LoadExternalRoutingWithDigest(path, expectedDigest)
	}
	if err != nil {
		return err
	}
	if claimBound {
		cfg.credentialProvenance.externalRouting = credentialSourceRepository
		if routing.routingCredentialVersion >= externalRoutingCredentialVersion {
			cfg.credentialProvenance.externalRouting = credentialSourceTrustedFile
		}
	}
	explicitTargetOS := cfg.TargetOS
	explicitWindowsMode := cfg.WindowsMode
	explicitTargetSource := cfg.credentialProvenance.externalDesktopTarget
	explicitWindowsModeSource := cfg.credentialProvenance.externalDesktopMode
	targetExplicit := IsExternalDesktopTargetExplicit(cfg)
	windowsModeExplicit := IsExternalDesktopWindowsModeExplicit(cfg)
	windowsModeDerivedFromTarget := targetExplicit && normalizeTargetOS(explicitTargetOS) != targetWindows && !windowsModeExplicit
	PreserveExternalDesktopChildEnvironmentBoundary(cfg)
	cfg.External = routing
	cfg.TargetOS, cfg.WindowsMode = ExternalRoutingTarget(routing)
	MarkExternalRoutingCredentialSources(cfg)
	MarkExternalRoutingTargetRestored(cfg, !targetExplicit, !windowsModeExplicit && !windowsModeDerivedFromTarget)
	ApplyExternalDesktopEnvironmentOverrides(cfg)
	if targetExplicit {
		cfg.TargetOS = explicitTargetOS
		cfg.credentialProvenance.externalDesktopTarget = explicitTargetSource
	}
	if windowsModeExplicit {
		cfg.WindowsMode = explicitWindowsMode
		cfg.credentialProvenance.externalDesktopMode = explicitWindowsModeSource
	} else if windowsModeDerivedFromTarget {
		cfg.WindowsMode = windowsModeNormal
		cfg.credentialProvenance.externalDesktopMode = explicitTargetSource
	}
	if strings.TrimSpace(routing.WorkRoot) != "" {
		cfg.WorkRoot = routing.WorkRoot
	}
	return nil
}

func restoreExternalLeaseTarget(cfg *Config, targetExplicit, windowsModeExplicit bool) error {
	prepareProviderDefaults(cfg)
	if strings.TrimSpace(cfg.External.WorkRoot) != "" {
		cfg.WorkRoot = cfg.External.WorkRoot
	}
	if !targetExplicit {
		cfg.TargetOS, _ = ExternalRoutingTarget(cfg.External)
		cfg.inferredTargetProvider = ""
	}
	if !windowsModeExplicit {
		cfg.WindowsMode = windowsModeNormal
		if normalizeTargetOS(cfg.TargetOS) == targetWindows {
			_, cfg.WindowsMode = ExternalRoutingTarget(cfg.External)
		}
	}
	normalizeTargetConfig(cfg)
	return validateTargetConfig(*cfg)
}

func uniqueExternalLeaseClaim(identifier string, providerSelected bool) (leaseClaim, bool, error) {
	exact, exists, err := readLeaseClaimWithPresence(identifier)
	if err != nil {
		return leaseClaim{}, false, err
	}
	if exists {
		if exact.Provider != "external" {
			return leaseClaim{}, false, nil
		}
		return exact, true, nil
	}
	claims, err := listLeaseClaims()
	if err != nil {
		return leaseClaim{}, false, err
	}
	matches := make([]leaseClaim, 0, 1)
	externalMatches := make([]leaseClaim, 0, 1)
	for _, claim := range claims {
		if !leaseClaimMatchesIdentifier(claim, identifier) {
			continue
		}
		matches = append(matches, claim)
		if claim.Provider == "external" {
			externalMatches = append(externalMatches, claim)
		}
	}
	if len(externalMatches) == 0 {
		return leaseClaim{}, false, nil
	}
	candidates := matches
	if providerSelected {
		candidates = externalMatches
	}
	if len(candidates) > 1 {
		if providerSelected {
			// The configured external lifecycle scope remains the tiebreaker.
			return leaseClaim{}, false, nil
		}
		ids := make([]string, 0, len(candidates))
		for _, claim := range candidates {
			ids = append(ids, claim.LeaseID)
		}
		return leaseClaim{}, false, exit(2, "multiple lease claims match %q: %s; use a lease id or an explicit provider", identifier, strings.Join(ids, ", "))
	}
	return externalMatches[0], true, nil
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
		cfg.inferredTargetProvider = ""
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
	workRoot := strings.TrimSpace(server.Labels["work_root"])
	normalizeTargetConfig(cfg)
	if workRoot != "" {
		cfg.WorkRoot = workRoot
	}
	target.TargetOS = cfg.TargetOS
	target.WindowsMode = cfg.WindowsMode
	ApplyTargetChildEnvironmentBoundary(*cfg, target)
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
