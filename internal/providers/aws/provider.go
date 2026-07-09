package aws

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string      { return "aws" }
func (Provider) Aliases() []string { return nil }
func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:   "aws",
		Family: "aws",
		Kind:   core.ProviderKindSSHLease,
		Targets: []core.TargetSpec{
			{OS: core.TargetLinux},
			{OS: core.TargetWindows, WindowsMode: "normal"},
			{OS: core.TargetWindows, WindowsMode: "wsl2"},
			{OS: core.TargetMacOS},
		},
		Features:    core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup, core.FeatureDesktop, core.FeatureBrowser, core.FeatureCode},
		Coordinator: core.CoordinatorSupported,
	}
}
func (Provider) RegisterFlags(*flag.FlagSet, core.Config) any { return core.NoProviderFlags() }
func (Provider) ApplyFlags(*core.Config, *flag.FlagSet, any) error {
	return nil
}

func (Provider) PrepareLeaseClaimEndpoint(existing core.LeaseClaim, provider, slug string, server core.Server, allowProviderMetadata bool) (core.Server, error) {
	_ = allowProviderMetadata
	if provider != "aws" {
		return core.Server{}, core.Exit(2, "refusing to rewrite AWS lease=%s as provider=%s", existing.LeaseID, provider)
	}
	if slug != existing.Slug || server.Labels["lease"] != existing.LeaseID || server.Labels["slug"] != existing.Slug {
		return core.Server{}, core.Exit(2, "refusing to rewrite AWS lease=%s with mismatched label identity", existing.LeaseID)
	}
	if existing.CloudID != "" && server.CloudID != "" && existing.CloudID != server.CloudID {
		return core.Server{}, core.Exit(2, "refusing to rewrite AWS lease=%s with stale instance identity", existing.LeaseID)
	}
	labels := make(map[string]string, len(server.Labels)+2)
	for key, value := range server.Labels {
		labels[key] = value
	}
	for _, key := range []string{"aws_key_pair_id", "aws_account_id"} {
		existingValue := existing.Labels[key]
		if cloudValue := labels[key]; existingValue != "" && cloudValue != "" && cloudValue != existingValue {
			return core.Server{}, core.Exit(2, "refusing to rewrite AWS lease=%s with mismatched %s", existing.LeaseID, key)
		}
		if existingValue != "" {
			labels[key] = existingValue
		} else {
			delete(labels, key)
		}
	}
	server.Labels = labels
	return server, nil
}

func (Provider) ServerTypeForConfig(cfg core.Config) string {
	return core.AWSInstanceTypeCandidatesForConfig(cfg)[0]
}

func (Provider) ServerTypeForClass(class string) string {
	return core.AWSInstanceTypeCandidatesForClass(class)[0]
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return NewAWSLeaseBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	doctor, ok := backend.(core.DoctorBackend)
	if !ok {
		return nil, core.Exit(2, "aws doctor backend unavailable")
	}
	return doctor, nil
}

func (Provider) NativeCheckpointCapability(req core.NativeCheckpointRequest) (core.NativeCheckpointCapability, bool) {
	if req.Server.CloudID == "" {
		return core.NativeCheckpointCapability{}, false
	}
	targetOS := firstNonBlank(req.Target.TargetOS, req.Config.TargetOS)
	strategy := core.NormalizeCheckpointStrategy(req.Strategy)
	if isWindowsNativeTarget(req) {
		if req.StrategyExplicit && strategy != core.CheckpointStrategyImage {
			return core.NativeCheckpointCapability{}, false
		}
		return core.NativeCheckpointCapability{
			Kind:   core.CheckpointKindAWSAMI,
			Direct: req.Config.Coordinator == "",
		}, true
	}
	if targetOS != core.TargetLinux && targetOS != core.TargetMacOS {
		return core.NativeCheckpointCapability{}, false
	}
	if req.Config.Coordinator == "" {
		if targetOS != core.TargetMacOS && strategy != core.CheckpointStrategyImage {
			return core.NativeCheckpointCapability{}, false
		}
		return core.NativeCheckpointCapability{Kind: core.CheckpointKindAWSAMI, Direct: true}, true
	}
	if targetOS == core.TargetMacOS || strategy == core.CheckpointStrategyImage {
		return core.NativeCheckpointCapability{Kind: core.CheckpointKindAWSAMI}, true
	}
	return core.NativeCheckpointCapability{Kind: core.CheckpointKindAWSEBS}, true
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func isWindowsNativeTarget(req core.NativeCheckpointRequest) bool {
	return firstNonBlank(req.Target.TargetOS, req.Config.TargetOS) == core.TargetWindows &&
		firstNonBlank(req.Target.WindowsMode, req.Config.WindowsMode) == core.WindowsModeNormal
}

func (Provider) ApplyNativeCheckpointForkConfig(req core.NativeCheckpointForkRequest) error {
	cfg := req.Config
	switch req.Record.Kind {
	case core.CheckpointKindAWSAMI:
		cfg.AWSAMI = req.Record.ImageID
	case core.CheckpointKindAWSEBS:
		cfg.AWSSnapshot = req.Record.ImageID
	default:
		return core.Exit(2, "provider=aws does not support checkpoint kind=%s", req.Record.Kind)
	}
	if req.Record.Region != "" {
		cfg.AWSRegion = req.Record.Region
	}
	if cfg.TargetOS == core.TargetMacOS {
		if req.Record.Direct && req.Record.HostID != "" {
			cfg.HostID = req.Record.HostID
			cfg.AWSMacHostID = req.Record.HostID
		}
		if !req.MarketExplicit {
			cfg.Capacity.Market = "on-demand"
		}
		core.NormalizeTargetConfig(cfg)
	}
	return nil
}
