package hetzner

import (
	"flag"
	"strconv"
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

// Hetzner publishes these dedicated-vCPU shapes in its General Purpose plan:
// https://www.hetzner.com/cloud/general-purpose/
var serverShapes = map[string]struct {
	vcpus    int
	memoryGB int
}{
	"ccx13": {vcpus: 2, memoryGB: 8},
	"ccx23": {vcpus: 4, memoryGB: 16},
	"ccx33": {vcpus: 8, memoryGB: 32},
	"ccx43": {vcpus: 16, memoryGB: 64},
	"ccx53": {vcpus: 32, memoryGB: 128},
	"ccx63": {vcpus: 48, memoryGB: 192},
}

var classProfiles = buildClassProfiles()

func (Provider) Name() string      { return "hetzner" }
func (Provider) Aliases() []string { return nil }
func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:             "hetzner",
		Family:           "hetzner",
		Kind:             core.ProviderKindSSHLease,
		Targets:          []core.TargetSpec{{OS: core.TargetLinux}},
		Features:         core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup, core.FeatureDesktop, core.FeatureBrowser, core.FeatureCode, core.FeatureTailscale, core.FeatureCheckpoint, core.FeatureFork, core.FeatureSnapshot},
		Coordinator:      core.CoordinatorSupported,
		ClassDisposition: core.ProviderClassDispositionMapped,
	}
}

func (Provider) NativeCheckpointCapability(req core.NativeCheckpointRequest) (core.NativeCheckpointCapability, bool) {
	if strings.TrimSpace(req.Config.Coordinator) != "" || firstNonBlank(req.Target.TargetOS, req.Config.TargetOS) != core.TargetLinux {
		return core.NativeCheckpointCapability{}, false
	}
	serverID, err := strconv.ParseInt(strings.TrimSpace(req.Server.CloudID), 10, 64)
	if err != nil || serverID <= 0 {
		return core.NativeCheckpointCapability{}, false
	}
	capability := core.NativeCheckpointCapability{Kind: core.CheckpointKindHetzner, Direct: true, RetireSource: true}
	if core.NormalizeCheckpointStrategy(req.Strategy) == core.CheckpointStrategyImage {
		capability.CreateUnsupported = "Hetzner native checkpoints use project snapshots; --strategy image is unsupported, use --strategy disk-snapshot"
	}
	return capability, true
}

func (Provider) ApplyNativeCheckpointForkConfig(req core.NativeCheckpointForkRequest) error {
	if req.Record.Kind != core.CheckpointKindHetzner || !req.Record.Direct {
		return core.Exit(2, "provider=hetzner does not support checkpoint kind=%s", req.Record.Kind)
	}
	if _, err := parsePositiveID(req.Record.ImageID, "snapshot"); err != nil {
		return err
	}
	if req.Record.Metadata[checkpointMetadataSourceType] != "server" {
		return core.Exit(2, "Hetzner checkpoint source type is missing or unsupported")
	}
	providerArch := strings.TrimSpace(req.Record.Architecture)
	if providerArch == "" || req.Record.Metadata[checkpointMetadataSourceArchitecture] != providerArch {
		return core.Exit(2, "Hetzner checkpoint architecture metadata is missing or mismatched")
	}
	architecture, err := coreArchitectureFromHetzner(providerArch)
	if err != nil {
		return err
	}
	if core.IsArchitectureExplicit(*req.Config) {
		explicit, err := core.NormalizeArchitecture(req.Config.Architecture)
		if err != nil {
			return err
		}
		if explicit != architecture {
			return core.Exit(2, "Hetzner snapshot architecture=%s cannot be forked as %s", architecture, explicit)
		}
	} else {
		req.Config.Architecture = architecture
		core.MarkArchitectureExplicit(req.Config)
	}
	req.Config.Image = req.Record.ImageID
	if req.Record.Region == "" || req.Record.Metadata[checkpointMetadataSourceLocation] != req.Record.Region {
		return core.Exit(2, "Hetzner checkpoint source location is missing or mismatched")
	}
	req.Config.Location = req.Record.Region
	return nil
}
func (Provider) RegisterFlags(*flag.FlagSet, core.Config) any { return core.NoProviderFlags() }
func (Provider) ApplyFlags(*core.Config, *flag.FlagSet, any) error {
	return nil
}

func (Provider) ServerTypeForConfig(cfg core.Config) string {
	if cfg.ServerTypeExplicit && strings.TrimSpace(cfg.ServerType) != "" {
		return strings.TrimSpace(cfg.ServerType)
	}
	if candidates, matched := core.ProviderClassCandidatesForProfiles(classProfiles, cfg); matched {
		return candidates[0]
	}
	if core.IsCanonicalProviderClass(cfg.Class) {
		return ""
	}
	return cfg.Class
}

func (Provider) ServerTypeForClass(class string) string {
	cfg := core.Config{Provider: "hetzner", TargetOS: core.TargetLinux, Architecture: core.ArchitectureAMD64, Class: class}
	if candidates, matched := core.ProviderClassCandidatesForProfiles(classProfiles, cfg); matched {
		return candidates[0]
	}
	return class
}

func (Provider) ClassProfiles() []core.ProviderClassProfile {
	return classProfiles
}

func (Provider) ClassSpecs() []core.ClassSpec {
	return core.ProviderClassSpecsFromProfiles(classProfiles)
}

func buildClassProfiles() []core.ProviderClassProfile {
	profiles := make([]core.ProviderClassProfile, 0, len(core.CanonicalProviderClasses()))
	for _, class := range core.CanonicalProviderClasses() {
		candidates := serverTypeCandidatesForClass(class)
		machines := make([]core.ProviderClassMachine, 0, len(candidates))
		for _, serverType := range candidates {
			machines = append(machines, hetznerClassMachine(serverType))
		}
		profiles = append(profiles, core.ProviderClassProfileFromMachines(
			class, core.TargetLinux, "", core.ProviderClassArchitectureAMD64, machines,
		))
	}
	return profiles
}

func hetznerClassMachine(serverType string) core.ProviderClassMachine {
	vcpus, memoryGB := serverShape(serverType)
	machine := core.ProviderClassMachine{Type: serverType, Architecture: core.ProviderClassArchitectureAMD64}
	if vcpus > 0 {
		machine.VCPU = &vcpus
	}
	if memoryGB > 0 {
		machine.Memory = &core.ProviderMemory{Value: float64(memoryGB), Unit: core.ProviderMemoryUnitGB}
	}
	return machine
}

func serverShape(serverType string) (int, int) {
	shape, ok := serverShapes[strings.ToLower(strings.TrimSpace(serverType))]
	if !ok {
		return 0, 0
	}
	return shape.vcpus, shape.memoryGB
}

func serverTypeCandidatesForClass(class string) []string {
	switch class {
	case "tiny":
		return []string{"ccx13", "cpx22", "cx23"}
	case "small":
		return []string{"ccx23", "cpx32", "cx33"}
	case "standard":
		return []string{"ccx33", "cpx62", "cx53"}
	case "fast":
		return []string{"ccx43", "cpx62", "cx53"}
	case "large":
		return []string{"ccx53", "ccx43", "cpx62", "cx53"}
	case "beast":
		return []string{"ccx63", "ccx53", "ccx43", "cpx62", "cx53"}
	default:
		return []string{class}
	}
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return NewHetznerLeaseBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	return shared.ConfigureDoctor("hetzner", func() (core.Backend, error) { return p.Configure(cfg, rt) })
}
