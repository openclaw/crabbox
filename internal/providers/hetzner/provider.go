package hetzner

import (
	"flag"
	"strconv"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string      { return "hetzner" }
func (Provider) Aliases() []string { return nil }
func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:        "hetzner",
		Family:      "hetzner",
		Kind:        core.ProviderKindSSHLease,
		Targets:     []core.TargetSpec{{OS: core.TargetLinux}},
		Features:    core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup, core.FeatureDesktop, core.FeatureBrowser, core.FeatureCode, core.FeatureTailscale, core.FeatureCheckpoint, core.FeatureFork, core.FeatureSnapshot},
		Coordinator: core.CoordinatorSupported,
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
	capability := core.NativeCheckpointCapability{Kind: core.CheckpointKindHetzner, Direct: true}
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
func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return NewHetznerLeaseBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	doctor, ok := backend.(core.DoctorBackend)
	if !ok {
		return nil, core.Exit(2, "hetzner doctor backend unavailable")
	}
	return doctor, nil
}
