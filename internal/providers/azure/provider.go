package azure

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}
type flagValues struct {
	Backend     *string
	OSDisk      *string
	SnapshotSKU *string
	OSDiskSKU   *string
}

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
		Features:    core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup, core.FeatureDesktop, core.FeatureBrowser, core.FeatureCode, core.FeatureTailscale},
		Coordinator: core.CoordinatorSupported,
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
	if err := p.RouteConfig(cfg, fs, values); err != nil {
		return err
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
	return core.AzureVMSizeCandidatesForConfig(cfg)[0]
}

func (Provider) ServerTypeForClass(class string) string {
	return core.AzureVMSizeCandidatesForClass(class)[0]
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return NewAzureLeaseBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	doctor, ok := backend.(core.DoctorBackend)
	if !ok {
		return nil, core.Exit(2, "azure doctor backend unavailable")
	}
	return doctor, nil
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
