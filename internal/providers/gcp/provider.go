package gcp

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string { return "gcp" }
func (Provider) Aliases() []string {
	return []string{"google", "google-cloud"}
}
func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:   "gcp",
		Family: "gcp",
		Kind:   core.ProviderKindSSHLease,
		Targets: []core.TargetSpec{
			{OS: core.TargetLinux},
		},
		Features:    core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup, core.FeatureTailscale},
		Coordinator: core.CoordinatorSupported,
	}
}
func (Provider) RegisterFlags(*flag.FlagSet, core.Config) any { return core.NoProviderFlags() }
func (Provider) ApplyFlags(*core.Config, *flag.FlagSet, any) error {
	return nil
}

func (Provider) PrepareLeaseClaimEndpoint(existing core.LeaseClaim, provider, slug string, server core.Server, allowProviderMetadata bool) (core.Server, error) {
	_ = allowProviderMetadata
	if provider != "gcp" {
		return core.Server{}, core.Exit(2, "refusing to rewrite GCP lease=%s as provider=%s", existing.LeaseID, provider)
	}
	if slug != existing.Slug || server.Labels["lease"] != existing.LeaseID || server.Labels["slug"] != existing.Slug {
		return core.Server{}, core.Exit(2, "refusing to rewrite GCP lease=%s with mismatched label identity", existing.LeaseID)
	}
	if existing.CloudID != "" && server.CloudID != "" && existing.CloudID != server.CloudID {
		return core.Server{}, core.Exit(2, "refusing to rewrite GCP lease=%s with stale instance name", existing.LeaseID)
	}
	if existing.CloudNumericID != 0 && server.ID != 0 && existing.CloudNumericID != server.ID {
		return core.Server{}, core.Exit(2, "refusing to rewrite GCP lease=%s with stale numeric instance identity", existing.LeaseID)
	}
	if existing.CloudNumericID == 0 {
		server.ID = 0
	}
	labels := make(map[string]string, len(server.Labels))
	for key, value := range server.Labels {
		labels[key] = value
	}
	for _, key := range []string{"zone", "provider_key"} {
		existingValue := existing.Labels[key]
		if cloudValue := labels[key]; existingValue != "" && cloudValue != "" && cloudValue != existingValue {
			return core.Server{}, core.Exit(2, "refusing to rewrite GCP lease=%s with mismatched %s", existing.LeaseID, key)
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
	return core.GCPMachineTypeCandidatesForClass(cfg.Class)[0]
}

func (Provider) ServerTypeForClass(class string) string {
	return core.GCPMachineTypeCandidatesForClass(class)[0]
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return NewGCPLeaseBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	doctor, ok := backend.(core.DoctorBackend)
	if !ok {
		return nil, core.Exit(2, "gcp doctor backend unavailable")
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
		return core.NativeCheckpointCapability{Kind: core.CheckpointKindGCP}, true
	}
	return core.NativeCheckpointCapability{Kind: core.CheckpointKindGCPDisk}, true
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
	case core.CheckpointKindGCP:
		cfg.GCPMachineImage = firstNonBlank(req.Record.Resource, req.Record.ImageID)
	case core.CheckpointKindGCPDisk:
		cfg.GCPSnapshot = firstNonBlank(req.Record.Resource, req.Record.ImageID)
	default:
		return core.Exit(2, "provider=gcp does not support checkpoint kind=%s", req.Record.Kind)
	}
	if req.Record.Region != "" {
		cfg.GCPZone = req.Record.Region
	}
	if req.Record.Project != "" {
		core.SetGCPProjectExplicit(cfg, req.Record.Project)
	}
	return nil
}
