package azure

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}
type flagValues struct {
	OSDisk *string
}

func (Provider) Name() string      { return "azure" }
func (Provider) Aliases() []string { return nil }
func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name: "azure",
		Kind: core.ProviderKindSSHLease,
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
		OSDisk: fs.String("azure-os-disk", defaults.AzureOSDisk, "Azure OS disk mode: managed, ephemeral, or auto"),
	}
}
func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	flags, _ := values.(flagValues)
	if core.FlagWasSet(fs, "azure-os-disk") && flags.OSDisk != nil {
		mode, err := core.NormalizeAzureOSDiskMode(*flags.OSDisk)
		if err != nil {
			return err
		}
		cfg.AzureOSDisk = mode
		cfg.AzureOSDiskExplicit = true
		return nil
	}
	if cfg.AzureOSDisk != "" {
		mode, err := core.NormalizeAzureOSDiskMode(cfg.AzureOSDisk)
		if err != nil {
			return err
		}
		cfg.AzureOSDisk = mode
	}
	return nil
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
	if req.Config.Coordinator == "" || req.Server.CloudID == "" {
		return core.NativeCheckpointCapability{}, false
	}
	if firstNonBlank(req.Target.TargetOS, req.Config.TargetOS) != core.TargetLinux {
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
