package azure

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

var (
	_ core.ProviderClassProfileProvider = Provider{}
	_ core.ProviderClassSpecProvider    = Provider{}
)

type flagValues struct {
	Backend     *string
	OSDisk      *string
	SnapshotSKU *string
	OSDiskSKU   *string
}

var classProfiles = buildClassProfiles()

func (Provider) Name() string      { return "azure" }
func (Provider) Aliases() []string { return nil }
func (Provider) RoutingFlagNames() []string {
	return []string{"azure-backend"}
}
func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:   "azure",
		Family: "azure",
		Kind:   core.ProviderKindSSHLease,
		Targets: []core.TargetSpec{
			{OS: core.TargetLinux},
			{OS: core.TargetWindows, WindowsMode: "normal"},
			{OS: core.TargetWindows, WindowsMode: "wsl2"},
		},
		Features:         core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup, core.FeatureDesktop, core.FeatureBrowser, core.FeatureCode, core.FeatureTailscale},
		Coordinator:      core.CoordinatorSupported,
		ClassDisposition: core.ProviderClassDispositionMapped,
	}
}
func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return flagValues{
		Backend:     fs.String("azure-backend", defaults.AzureBackend, "Azure backend: vm or dynamic-sessions"),
		OSDisk:      fs.String("azure-os-disk", defaults.AzureOSDisk, "Azure OS disk mode: managed, ephemeral, ephemeral-preview, or auto"),
		SnapshotSKU: fs.String("azure-snapshot-sku", defaults.AzureSnapshotSKU, "Azure checkpoint snapshot storage SKU"),
		OSDiskSKU:   fs.String("azure-os-disk-sku", defaults.AzureOSDiskSKU, "Azure managed OS disk storage SKU"),
	}
}

func (Provider) RouteConfig(cfg *core.Config, fs *flag.FlagSet, values any) error {
	backend := cfg.AzureBackend
	if fs != nil && core.FlagWasSet(fs, "azure-backend") {
		flags, _ := values.(flagValues)
		if flags.Backend != nil {
			backend = *flags.Backend
		}
	}
	normalized, err := core.NormalizeAzureBackend(backend)
	if err != nil {
		return core.Exit(2, "%s", err)
	}
	cfg.AzureBackend = normalized
	if normalized == core.AzureBackendDynamicSessions {
		cfg.Provider = "azure-dynamic-sessions"
	} else {
		cfg.Provider = "azure"
	}
	return nil
}

func (p Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	backendExplicit := fs != nil && core.FlagWasSet(fs, "azure-backend")
	if !core.ProviderSelectionIsAuthoritativeRoute(*cfg) || backendExplicit {
		if err := p.RouteConfig(cfg, fs, values); err != nil {
			return err
		}
	}
	if cfg.Provider != p.Name() {
		return nil
	}
	flags, _ := values.(flagValues)
	if core.FlagWasSet(fs, "azure-os-disk") && flags.OSDisk != nil {
		mode, err := core.NormalizeAzureOSDiskMode(*flags.OSDisk)
		if err != nil {
			return err
		}
		cfg.AzureOSDisk = mode
		cfg.AzureOSDiskExplicit = true
	}
	if cfg.AzureOSDisk != "" {
		mode, err := core.NormalizeAzureOSDiskMode(cfg.AzureOSDisk)
		if err != nil {
			return err
		}
		cfg.AzureOSDisk = mode
	}
	if core.FlagWasSet(fs, "azure-snapshot-sku") && flags.SnapshotSKU != nil {
		cfg.AzureSnapshotSKU = *flags.SnapshotSKU
	}
	if cfg.AzureSnapshotSKU != "" {
		sku, err := core.NormalizeAzureSnapshotSKU(cfg.AzureSnapshotSKU)
		if err != nil {
			return err
		}
		cfg.AzureSnapshotSKU = sku
	}
	if core.FlagWasSet(fs, "azure-os-disk-sku") && flags.OSDiskSKU != nil {
		cfg.AzureOSDiskSKU = *flags.OSDiskSKU
	}
	if cfg.AzureOSDiskSKU != "" {
		sku, err := core.NormalizeAzureDiskSKU(cfg.AzureOSDiskSKU)
		if err != nil {
			return err
		}
		cfg.AzureOSDiskSKU = sku
	}
	return nil
}

func (Provider) PrepareLeaseClaimEndpoint(existing core.LeaseClaim, provider, slug string, server core.Server, allowProviderMetadata bool) (core.Server, error) {
	_ = allowProviderMetadata
	if provider != "azure" {
		return core.Server{}, core.Exit(2, "refusing to rewrite Azure lease=%s as provider=%s", existing.LeaseID, provider)
	}
	if slug != existing.Slug || server.Labels["lease"] != existing.LeaseID || server.Labels["slug"] != existing.Slug {
		return core.Server{}, core.Exit(2, "refusing to rewrite Azure lease=%s with mismatched label identity", existing.LeaseID)
	}
	if existing.CloudID != "" && server.CloudID != "" && existing.CloudID != server.CloudID {
		return core.Server{}, core.Exit(2, "refusing to rewrite Azure lease=%s with stale VM name", existing.LeaseID)
	}
	if existing.CloudImmutableID != "" && server.ImmutableID != "" && existing.CloudImmutableID != server.ImmutableID {
		return core.Server{}, core.Exit(2, "refusing to rewrite Azure lease=%s with stale immutable VM identity", existing.LeaseID)
	}
	if existing.CloudImmutableID == "" {
		server.ImmutableID = ""
	}
	labels := make(map[string]string, len(server.Labels))
	for key, value := range server.Labels {
		labels[key] = value
	}
	if existingValue := existing.Labels["provider_key"]; existingValue != "" {
		if cloudValue := labels["provider_key"]; cloudValue != "" && cloudValue != existingValue {
			return core.Server{}, core.Exit(2, "refusing to rewrite Azure lease=%s with mismatched provider_key", existing.LeaseID)
		}
		labels["provider_key"] = existingValue
	} else {
		delete(labels, "provider_key")
	}
	server.Labels = labels
	return server, nil
}

func (Provider) ServerTypeForConfig(cfg core.Config) string {
	if cfg.ServerTypeExplicit && strings.TrimSpace(cfg.ServerType) != "" {
		return strings.TrimSpace(cfg.ServerType)
	}
	profileConfig := cfg
	profileConfig.ServerType = ""
	candidates := core.AzureVMSizeCandidatesForProfiles(profileConfig, classProfiles)
	if len(candidates) == 0 {
		return ""
	}
	return candidates[0]
}

func (Provider) ServerTypeForClass(class string) string {
	return azureVMSizeCandidatesForClass(class)[0]
}

func (Provider) ClassProfiles() []core.ProviderClassProfile {
	return classProfiles
}

func (Provider) ClassSpecs() []core.ClassSpec {
	return core.ProviderClassSpecsFromProfiles(classProfiles)
}

func buildClassProfiles() []core.ProviderClassProfile {
	profiles := make([]core.ProviderClassProfile, 0, 30)
	for _, class := range core.CanonicalProviderClasses() {
		profiles = append(profiles,
			azureClassProfile(class, core.TargetLinux, "", core.ProviderClassArchitectureAMD64, azureVMSizeCandidatesForClass(class)),
			azureClassProfile(class, core.TargetLinux, "", core.ProviderClassArchitectureARM64, azureARM64VMSizeCandidatesForClass(class)),
			azureClassProfile(class, core.TargetWindows, core.WindowsModeNormal, core.ProviderClassArchitectureAMD64, azureWindowsVMSizeCandidatesForClass(class)),
			azureClassProfile(class, core.TargetWindows, core.WindowsModeNormal, core.ProviderClassArchitectureARM64, azureARM64VMSizeCandidatesForClass(class)),
			azureClassProfile(class, core.TargetWindows, core.WindowsModeWSL2, core.ProviderClassArchitectureAMD64, azureWindowsVMSizeCandidatesForClass(class)),
		)
	}
	return profiles
}

func azureClassProfile(class, target, windowsMode string, architecture core.ProviderClassArchitecture, candidates []string) core.ProviderClassProfile {
	machines := make([]core.ProviderClassMachine, 0, len(candidates))
	for _, vmSize := range candidates {
		machines = append(machines, azureClassMachine(vmSize, architecture))
	}
	return core.ProviderClassProfileFromMachines(class, target, windowsMode, architecture, machines)
}

func azureClassMachine(vmSize string, architecture core.ProviderClassArchitecture) core.ProviderClassMachine {
	vcpus, memoryGiB := vmShape(vmSize)
	machine := core.ProviderClassMachine{Type: vmSize, Architecture: architecture}
	if vcpus > 0 {
		machine.VCPU = &vcpus
	}
	if memoryGiB > 0 {
		machine.Memory = &core.ProviderMemory{Value: float64(memoryGiB), Unit: core.ProviderMemoryUnitGiB}
	}
	return machine
}

func vmShape(vmSize string) (int, int) {
	normalized := strings.ToLower(strings.TrimSpace(vmSize))
	const dPrefix = "standard_d"
	if strings.HasPrefix(normalized, dPrefix) && (len(normalized) == len(dPrefix) || normalized[len(dPrefix)] < '0' || normalized[len(dPrefix)] > '9') {
		return 0, 0
	}
	vcpus, ok := core.AzureVMSizeVCPUCount(vmSize)
	if !ok || vcpus <= 0 {
		return 0, 0
	}
	if !strings.HasPrefix(normalized, dPrefix) || (!strings.HasSuffix(normalized, "_v5") && !strings.HasSuffix(normalized, "_v6")) {
		return vcpus, 0
	}
	// Azure publishes both Dadsv6 and Ddsv6 at 4 GiB per vCPU:
	// https://learn.microsoft.com/azure/virtual-machines/sizes/general-purpose/dadsv6-series
	// https://learn.microsoft.com/azure/virtual-machines/sizes/general-purpose/ddsv6-series
	return vcpus, vcpus * 4
}

func azureVMSizeCandidatesForClass(class string) []string {
	switch class {
	case "tiny":
		return []string{"Standard_D2ads_v6", "Standard_D2ds_v6", "Standard_D2ads_v5", "Standard_D2ds_v5", "Standard_F2s_v2"}
	case "small":
		return []string{"Standard_D8ads_v6", "Standard_D8ds_v6", "Standard_F8s_v2", "Standard_D8ads_v5", "Standard_D8ds_v5", "Standard_D4ads_v6", "Standard_D4ds_v6", "Standard_F4s_v2"}
	case "standard":
		return []string{"Standard_D32ads_v6", "Standard_D32ds_v6", "Standard_F32s_v2", "Standard_D32ads_v5", "Standard_D32ds_v5", "Standard_D16ads_v6", "Standard_D16ds_v6", "Standard_F16s_v2"}
	case "fast":
		return []string{"Standard_D64ads_v6", "Standard_D64ds_v6", "Standard_F64s_v2", "Standard_D64ads_v5", "Standard_D64ds_v5", "Standard_D48ads_v6", "Standard_D48ds_v6", "Standard_F48s_v2", "Standard_D32ads_v6", "Standard_D32ds_v6", "Standard_F32s_v2"}
	case "large":
		return []string{"Standard_D96ads_v6", "Standard_D96ds_v6", "Standard_D96ads_v5", "Standard_D96ds_v5", "Standard_D64ads_v6", "Standard_D64ds_v6", "Standard_F64s_v2", "Standard_D48ads_v6", "Standard_D48ds_v6", "Standard_F48s_v2"}
	case "beast":
		return []string{"Standard_D192ds_v6", "Standard_D128ds_v6", "Standard_D96ads_v6", "Standard_D96ds_v6", "Standard_D96ads_v5", "Standard_D96ds_v5", "Standard_D64ads_v6", "Standard_D64ds_v6", "Standard_F64s_v2"}
	default:
		return []string{class}
	}
}

func azureARM64VMSizeCandidatesForClass(class string) []string {
	switch class {
	case "tiny":
		return []string{"Standard_D2pds_v6", "Standard_D2ps_v6"}
	case "small":
		return []string{"Standard_D8pds_v6", "Standard_D8ps_v6", "Standard_D4pds_v6", "Standard_D4ps_v6"}
	case "standard":
		return []string{"Standard_D32pds_v6", "Standard_D32ps_v6", "Standard_D16pds_v6", "Standard_D16ps_v6"}
	case "fast":
		return []string{"Standard_D64pds_v6", "Standard_D64ps_v6", "Standard_D48pds_v6", "Standard_D48ps_v6", "Standard_D32pds_v6", "Standard_D32ps_v6"}
	case "large":
		return []string{"Standard_D96pds_v6", "Standard_D96ps_v6", "Standard_D64pds_v6", "Standard_D64ps_v6", "Standard_D48pds_v6", "Standard_D48ps_v6"}
	case "beast":
		return []string{"Standard_D96pds_v6", "Standard_D96ps_v6", "Standard_D64pds_v6", "Standard_D64ps_v6"}
	default:
		return []string{class}
	}
}

func azureWindowsVMSizeCandidatesForClass(class string) []string {
	switch class {
	case "tiny":
		return []string{"Standard_D2ads_v6", "Standard_D2ds_v6", "Standard_D2ads_v5", "Standard_D2ds_v5", "Standard_D2as_v6"}
	case "small":
		return []string{"Standard_D8ads_v6", "Standard_D8ds_v6", "Standard_D8ads_v5", "Standard_D8ds_v5", "Standard_D8as_v6"}
	case "standard":
		return []string{"Standard_D2ads_v6", "Standard_D2ds_v6", "Standard_D2ads_v5", "Standard_D2ds_v5", "Standard_D2as_v6"}
	case "fast":
		return []string{"Standard_D4ads_v6", "Standard_D4ds_v6", "Standard_D4ads_v5", "Standard_D4ds_v5", "Standard_D4as_v6"}
	case "large":
		return []string{"Standard_D8ads_v6", "Standard_D8ds_v6", "Standard_D8ads_v5", "Standard_D8ds_v5", "Standard_D8as_v6"}
	case "beast":
		return []string{"Standard_D16ads_v6", "Standard_D16ds_v6", "Standard_D16ads_v5", "Standard_D16ds_v5", "Standard_D8ads_v6"}
	default:
		return []string{class}
	}
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return NewAzureLeaseBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	return shared.ConfigureDoctor("azure", func() (core.Backend, error) { return p.Configure(cfg, rt) })
}

func (Provider) NativeCheckpointCapability(req core.NativeCheckpointRequest) (core.NativeCheckpointCapability, bool) {
	if req.Server.CloudID == "" {
		return core.NativeCheckpointCapability{}, false
	}
	targetOS := firstNonBlank(req.Target.TargetOS, req.Config.TargetOS)
	if targetOS == core.TargetWindows && firstNonBlank(req.Target.WindowsMode, req.Config.WindowsMode) == core.WindowsModeNormal {
		if core.NormalizeCheckpointStrategy(req.Strategy) == core.CheckpointStrategyImage {
			return core.NativeCheckpointCapability{}, false
		}
		return core.NativeCheckpointCapability{Kind: core.CheckpointKindAzureOS, Direct: true}, true
	}
	if req.Config.Coordinator == "" || targetOS != core.TargetLinux {
		return core.NativeCheckpointCapability{}, false
	}
	if core.NormalizeCheckpointStrategy(req.Strategy) == core.CheckpointStrategyImage {
		return core.NativeCheckpointCapability{
			Kind:              core.CheckpointKindAzure,
			CreateUnsupported: "Azure managed images require a stopped/generalized source VM; use --strategy disk-snapshot for active Azure leases",
		}, true
	}
	return core.NativeCheckpointCapability{Kind: core.CheckpointKindAzureOS}, true
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func (Provider) ApplyNativeCheckpointForkConfig(req core.NativeCheckpointForkRequest) error {
	cfg := req.Config
	switch req.Record.Kind {
	case core.CheckpointKindAzure:
		cfg.AzureImage = firstNonBlank(req.Record.Resource, req.Record.ImageID)
	case core.CheckpointKindAzureOS:
		cfg.AzureSnapshot = firstNonBlank(req.Record.Resource, req.Record.ImageID)
	default:
		return core.Exit(2, "provider=azure does not support checkpoint kind=%s", req.Record.Kind)
	}
	if req.Record.Region != "" {
		cfg.AzureLocation = req.Record.Region
	}
	if resourceGroup := azureResourceGroup(firstNonBlank(req.Record.Resource, req.Record.ImageID)); resourceGroup != "" {
		cfg.AzureResourceGroup = resourceGroup
	}
	if subscription := azureSubscription(firstNonBlank(req.Record.Resource, req.Record.ImageID)); subscription != "" {
		cfg.AzureSubscription = subscription
	}
	if req.AzureOSDiskExplicit {
		mode, err := core.NormalizeAzureOSDiskMode(req.AzureOSDisk)
		if err != nil {
			return err
		}
		cfg.AzureOSDisk = mode
		cfg.AzureOSDiskExplicit = true
	}
	return nil
}

func (Provider) ApplyNativeCheckpointForkFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	flags, _ := values.(flagValues)
	if core.FlagWasSet(fs, "azure-os-disk-sku") && flags.OSDiskSKU != nil {
		sku, err := core.NormalizeAzureDiskSKU(*flags.OSDiskSKU)
		if err != nil {
			return err
		}
		cfg.AzureOSDiskSKU = sku
	}
	return nil
}

func azureResourceGroup(resourceID string) string {
	return azureResourceIDPart(resourceID, "resourceGroups")
}

func azureSubscription(resourceID string) string {
	return azureResourceIDPart(resourceID, "subscriptions")
}

func azureResourceIDPart(resourceID, name string) string {
	parts := strings.Split(strings.Trim(resourceID, "/"), "/")
	for index := 0; index+1 < len(parts); index += 1 {
		if strings.EqualFold(parts[index], name) {
			return parts[index+1]
		}
	}
	return ""
}
