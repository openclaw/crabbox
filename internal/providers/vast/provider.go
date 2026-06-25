package vast

import (
	"flag"
	"net/url"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string { return providerName }

func (Provider) Aliases() []string {
	return []string{"vast-ai", "vastai"}
}

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:        providerName,
		Family:      "vast",
		Kind:        core.ProviderKindSSHLease,
		Targets:     []core.TargetSpec{{OS: core.TargetLinux}},
		Features:    core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup},
		Coordinator: core.CoordinatorNever,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return RegisterVastProviderFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return ApplyVastProviderFlags(cfg, fs, values)
}

func (Provider) PrepareLeaseClaimEndpoint(existing core.LeaseClaim, provider, slug string, server core.Server, allowProviderMetadata bool) (core.Server, error) {
	if provider != providerName {
		return core.Server{}, core.Exit(2, "refusing to rewrite vast lease=%s as provider=%s", existing.LeaseID, provider)
	}
	if slug != existing.Slug {
		return core.Server{}, core.Exit(2, "refusing to rewrite vast lease=%s with slug=%s", existing.LeaseID, slug)
	}
	if server.Labels["lease"] != existing.LeaseID || server.Labels["slug"] != existing.Slug {
		return core.Server{}, core.Exit(2, "refusing to rewrite vast lease=%s with mismatched label identity", existing.LeaseID)
	}
	if existing.CloudID != "" && server.CloudID != "" && existing.CloudID != server.CloudID {
		return core.Server{}, core.Exit(2, "refusing to rewrite vast lease=%s with stale instance identity", existing.LeaseID)
	}
	if allowProviderMetadata {
		return server, nil
	}
	server.Labels = preserveVastClaimMetadata(server.Labels, existing.Labels)
	return server, nil
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if err := p.ValidateConfig(cfg); err != nil {
		return nil, err
	}
	if cfg.TargetOS != "" && cfg.TargetOS != core.TargetLinux {
		return nil, exit(2, "provider=%s supports target=linux only", providerName)
	}
	if cfg.Tailscale.Enabled || cfg.Network == core.NetworkTailscale {
		return nil, exit(2, "provider=%s does not support Tailscale options", providerName)
	}
	return newBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	doctor, ok := backend.(core.DoctorBackend)
	if !ok {
		return nil, core.Exit(2, "vast doctor backend unavailable")
	}
	return doctor, nil
}

func (Provider) ValidateConfig(cfg core.Config) error {
	apiURL := strings.TrimSpace(cfg.Vast.APIURL)
	if apiURL == "" {
		return exit(2, "vast.apiUrl is required")
	}
	u, err := url.Parse(apiURL)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil {
		return exit(2, "vast.apiUrl must be an absolute URL without credentials")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return exit(2, "vast.apiUrl must not include query strings or fragments")
	}
	switch strings.ToLower(u.Scheme) {
	case "https", "http":
	default:
		return exit(2, "vast.apiUrl must use http or https")
	}
	switch normalizeInstanceType(cfg.Vast.InstanceType) {
	case "ondemand", "interruptible":
	default:
		return exit(2, "vast.instanceType must be ondemand or interruptible")
	}
	switch strings.ToLower(strings.TrimSpace(cfg.Vast.Runtype)) {
	case "ssh_direct":
	default:
		return exit(2, "vast.runtype must be ssh_direct")
	}
	if cfg.Vast.GPUCount < 0 {
		return exit(2, "vast.gpuCount must be non-negative")
	}
	if cfg.Vast.DiskGB < 0 {
		return exit(2, "vast.diskGB must be non-negative")
	}
	if cfg.Vast.MaxDphTotal < 0 {
		return exit(2, "vast.maxDphTotal must be non-negative")
	}
	if cfg.Vast.MinReliability < 0 || cfg.Vast.MinReliability > 1 {
		return exit(2, "vast.minReliability must be between 0 and 1")
	}
	switch strings.ToLower(strings.TrimSpace(cfg.Vast.ReleaseAction)) {
	case "", "destroy", "delete", "stop", "keep":
	default:
		return exit(2, "vast.releaseAction must be destroy, delete, stop, or keep")
	}
	return nil
}
