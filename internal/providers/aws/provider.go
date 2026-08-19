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

var (
	_ core.ProviderClassProfileProvider = Provider{}
	_ core.ProviderClassSpecProvider    = Provider{}
)

// AWS publishes C7 compute-optimized instances at 2 GiB/vCPU, M7/M8
// general-purpose instances at 4 GiB/vCPU, and R7/R8 memory-optimized instances
// in its EC2 instance-family tables:
// https://docs.aws.amazon.com/ec2/latest/instancetypes/co.html
// https://docs.aws.amazon.com/ec2/latest/instancetypes/gp.html
// https://docs.aws.amazon.com/ec2/latest/instancetypes/mo.html
var memoryGiBPerVCPU = map[string]int{
	"c7a":      2,
	"c7g":      2,
	"c7i":      2,
	"c8i":      2,
	"m7a":      4,
	"m7g":      4,
	"m7i":      4,
	"m8i":      4,
	"m8i-flex": 4,
	"r7a":      8,
	"r7g":      8,
	"r8i":      8,
}

var classProfiles = buildClassProfiles()

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
		Features:         core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup, core.FeatureDesktop, core.FeatureBrowser, core.FeatureCode},
		Coordinator:      core.CoordinatorSupported,
		ClassDisposition: core.ProviderClassDispositionMapped,
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
	candidates := awsCandidatesForConfig(cfg)
	if len(candidates) == 0 {
		return ""
	}
	return candidates[0]
}

func (Provider) ServerTypeForClass(class string) string {
	return awsInstanceTypeCandidatesForClass(class)[0]
}

func (Provider) ClassProfiles() []core.ProviderClassProfile {
	return classProfiles
}

func (Provider) ClassSpecs() []core.ClassSpec {
	return core.ProviderClassSpecsFromProfiles(classProfiles)
}

func buildClassProfiles() []core.ProviderClassProfile {
	profiles := make([]core.ProviderClassProfile, 0, 20)
	for _, class := range core.CanonicalProviderClasses() {
		profiles = append(profiles,
			awsClassProfile(class, core.TargetLinux, "", core.ProviderClassArchitectureAMD64, awsInstanceTypeCandidatesForClass(class)),
			awsClassProfile(class, core.TargetLinux, "", core.ProviderClassArchitectureARM64, awsARM64InstanceTypeCandidatesForClass(class)),
			awsClassProfile(class, core.TargetWindows, core.WindowsModeNormal, core.ProviderClassArchitectureAMD64, awsWindowsInstanceTypeCandidatesForClass(class)),
			awsClassProfile(class, core.TargetWindows, core.WindowsModeWSL2, core.ProviderClassArchitectureAMD64, awsWSL2InstanceTypeCandidatesForClass(class)),
			awsClassProfile(class, core.TargetMacOS, "", core.ProviderClassArchitectureMixed, awsMacOSInstanceTypeCandidates()),
		)
	}
	return profiles
}

func awsClassProfile(class, target, windowsMode string, architecture core.ProviderClassArchitecture, candidates []string) core.ProviderClassProfile {
	machines := make([]core.ProviderClassMachine, 0, len(candidates))
	for _, instanceType := range candidates {
		machineArchitecture := architecture
		if architecture == core.ProviderClassArchitectureMixed {
			machineArchitecture = core.ProviderClassArchitectureARM64
			if instanceType == "mac1.metal" {
				machineArchitecture = core.ProviderClassArchitectureAMD64
			}
		}
		machines = append(machines, awsClassMachine(instanceType, machineArchitecture))
	}
	return core.ProviderClassProfileFromMachines(class, target, windowsMode, architecture, machines)
}

func awsCandidatesForConfig(cfg core.Config) []string {
	if candidates, matched := core.ProviderClassCandidatesForProfiles(classProfiles, cfg); matched {
		return candidates
	}
	if core.IsCanonicalProviderClass(cfg.Class) {
		return nil
	}
	if cfg.TargetOS == core.TargetMacOS {
		standard := cfg
		standard.Class = "standard"
		candidates, _ := core.ProviderClassCandidatesForProfiles(classProfiles, standard)
		return candidates
	}
	if cfg.TargetOS == core.TargetWindows {
		if cfg.WindowsMode == core.WindowsModeWSL2 {
			return awsWSL2InstanceTypeCandidatesForClass(cfg.Class)
		}
		return awsWindowsInstanceTypeCandidatesForClass(cfg.Class)
	}
	if cfg.Architecture == core.ArchitectureARM64 {
		return awsARM64InstanceTypeCandidatesForClass(cfg.Class)
	}
	return awsInstanceTypeCandidatesForClass(cfg.Class)
}

func awsClassMachine(instanceType string, architecture core.ProviderClassArchitecture) core.ProviderClassMachine {
	vcpus, memoryGiB := instanceShape(instanceType)
	machine := core.ProviderClassMachine{Type: instanceType, Architecture: architecture}
	if vcpus > 0 {
		machine.VCPU = &vcpus
	}
	if memoryGiB > 0 {
		machine.Memory = &core.ProviderMemory{Value: float64(memoryGiB), Unit: core.ProviderMemoryUnitGiB}
	}
	return machine
}

func instanceShape(instanceType string) (int, int) {
	vcpus := core.AWSInstanceTypeVCPUs(instanceType)
	if vcpus == 0 {
		return 0, 0
	}
	switch instanceType {
	case "t3.small", "t4g.small":
		return vcpus, 2
	case "t3.large":
		return vcpus, 8
	}
	family, _, ok := strings.Cut(strings.ToLower(strings.TrimSpace(instanceType)), ".")
	if !ok {
		return vcpus, 0
	}
	return vcpus, vcpus * memoryGiBPerVCPU[family]
}

func awsInstanceTypeCandidatesForClass(class string) []string {
	switch class {
	case "standard":
		return []string{"c7a.8xlarge", "c7i.8xlarge", "m7a.8xlarge", "m7i.8xlarge", "c7a.4xlarge", "t3.small"}
	case "fast":
		return []string{"c7a.16xlarge", "c7i.16xlarge", "m7a.16xlarge", "m7i.16xlarge", "c7a.12xlarge", "c7a.8xlarge", "t3.small"}
	case "large":
		return []string{"c7a.24xlarge", "c7i.24xlarge", "m7a.24xlarge", "m7i.24xlarge", "r7a.24xlarge", "c7a.16xlarge", "c7a.12xlarge", "t3.small"}
	case "beast":
		return []string{"c7a.48xlarge", "c7i.48xlarge", "m7a.48xlarge", "m7i.48xlarge", "r7a.48xlarge", "c7a.32xlarge", "c7i.32xlarge", "m7a.32xlarge", "c7a.24xlarge", "c7a.16xlarge", "t3.small"}
	default:
		return []string{class}
	}
}

func awsARM64InstanceTypeCandidatesForClass(class string) []string {
	switch class {
	case "standard":
		return []string{"c7g.8xlarge", "m7g.8xlarge", "r7g.8xlarge", "c7g.4xlarge", "t4g.small"}
	case "fast":
		return []string{"c7g.16xlarge", "m7g.16xlarge", "r7g.16xlarge", "c7g.12xlarge", "c7g.8xlarge", "t4g.small"}
	case "large", "beast":
		return []string{"c7g.16xlarge", "m7g.16xlarge", "r7g.16xlarge", "c7g.12xlarge", "t4g.small"}
	default:
		return []string{class}
	}
}

func awsWindowsInstanceTypeCandidatesForClass(class string) []string {
	switch class {
	case "standard":
		return []string{"m7i.large", "m7a.large", "t3.large"}
	case "fast":
		return []string{"m7i.xlarge", "m7a.xlarge", "t3.xlarge", "t3.large"}
	case "large":
		return []string{"m7i.2xlarge", "m7a.2xlarge", "t3.2xlarge", "t3.large"}
	case "beast":
		return []string{"m7i.4xlarge", "m7a.4xlarge", "m7i.2xlarge", "t3.large"}
	default:
		return []string{class}
	}
}

func awsWSL2InstanceTypeCandidatesForClass(class string) []string {
	switch class {
	case "standard":
		return []string{"m8i.large", "m8i-flex.large", "c8i.large", "r8i.large"}
	case "fast":
		return []string{"m8i.xlarge", "m8i-flex.xlarge", "c8i.xlarge", "r8i.xlarge", "m8i.large"}
	case "large":
		return []string{"m8i.2xlarge", "m8i-flex.2xlarge", "c8i.2xlarge", "r8i.2xlarge", "m8i.large"}
	case "beast":
		return []string{"m8i.4xlarge", "m8i-flex.4xlarge", "c8i.4xlarge", "r8i.4xlarge", "m8i.2xlarge", "m8i.large"}
	default:
		return []string{class}
	}
}

func awsMacOSInstanceTypeCandidates() []string {
	return []string{"mac2.metal", "mac2-m2.metal", "mac2-m2pro.metal", "mac-m4.metal", "mac-m4pro.metal", "mac-m4max.metal", "mac2-m1ultra.metal", "mac-m3ultra.metal", "mac1.metal"}
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
