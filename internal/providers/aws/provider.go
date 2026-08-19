package aws

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

var _ core.ProviderClassSpecProvider = Provider{}

// AWS publishes C7 compute-optimized instances at 2 GiB/vCPU, M7/M8
// general-purpose instances at 4 GiB/vCPU, and R7/R8 memory-optimized instances
// in its EC2 instance-family tables:
// https://docs.aws.amazon.com/ec2/latest/instancetypes/co.html
// https://docs.aws.amazon.com/ec2/latest/instancetypes/gp.html
// https://docs.aws.amazon.com/ec2/latest/instancetypes/mo.html
var memoryGiBPerVCPU = map[string]int{
	"c7a": 2,
	"c7g": 2,
	"c7i": 2,
	"m7a": 4,
	"m7g": 4,
	"m7i": 4,
	"m8i": 4,
	"r7a": 8,
	"r7g": 8,
	"r8i": 8,
}

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

func (Provider) ClassSpecs() []core.ClassSpec {
	classes := core.MachineClassOrder
	specs := make([]core.ClassSpec, 0, len(classes))
	for _, class := range classes {
		instanceType := core.AWSInstanceTypeCandidatesForClass(class)[0]
		vcpus, memoryGB := instanceShape(instanceType)
		specs = append(specs, core.ClassSpec{Class: class, Type: instanceType, VCPUs: vcpus, MemoryGB: memoryGB})
	}
	return specs
}

func instanceShape(instanceType string) (int, int) {
	vcpus := core.AWSInstanceTypeVCPUs(instanceType)
	if vcpus == 0 {
		return 0, 0
	}
	family, _, ok := strings.Cut(strings.ToLower(strings.TrimSpace(instanceType)), ".")
	if !ok {
		return vcpus, 0
	}
	return vcpus, vcpus * memoryGiBPerVCPU[family]
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
