package digitalocean

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string      { return providerName }
func (Provider) Aliases() []string { return nil }
func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:        providerName,
		Family:      providerName,
		Kind:        core.ProviderKindSSHLease,
		Targets:     []core.TargetSpec{{OS: core.TargetLinux}},
		Features:    core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup, core.FeatureTailscale},
		Coordinator: core.CoordinatorNever,
	}
}

func (Provider) RegisterFlags(*flag.FlagSet, core.Config) any { return core.NoProviderFlags() }
func (Provider) ApplyFlags(*core.Config, *flag.FlagSet, any) error {
	return nil
}

func (Provider) PrepareLeaseClaimEndpoint(existing core.LeaseClaim, provider, slug string, server core.Server, allowProviderMetadata bool) (core.Server, error) {
	if provider != providerName {
		return core.Server{}, core.Exit(2, "refusing to rewrite digitalocean lease=%s as provider=%s", existing.LeaseID, provider)
	}
	if slug != existing.Slug {
		return core.Server{}, core.Exit(2, "refusing to rewrite digitalocean lease=%s with slug=%s", existing.LeaseID, slug)
	}
	leaseID := server.Labels["lease"]
	if err := validateDigitalOceanClaimIdentity(existing, leaseID, server.Labels["slug"]); err != nil {
		return core.Server{}, err
	}
	if existing.CloudID != "" && existing.CloudID != server.CloudID {
		return core.Server{}, core.Exit(2, "refusing to rewrite digitalocean lease=%s with stale Droplet identity", existing.LeaseID)
	}
	expectedAccountID := existing.Labels[digitalOceanAccountLabel]
	if expectedAccountID == "" || expectedAccountID != server.Labels[digitalOceanAccountLabel] {
		return core.Server{}, core.Exit(3, "refusing to rewrite digitalocean lease=%s with mismatched account identity", existing.LeaseID)
	}
	authorizedKeyID := server.Labels[digitalOceanKeyDeleteAuthorizedLabel]
	if authorizedKeyID != "" && authorizedKeyID != server.Labels[digitalOceanRecoveryKeyIDLabel] {
		return core.Server{}, core.Exit(2, "refusing to rewrite digitalocean lease=%s with mismatched key-delete authorization", existing.LeaseID)
	}
	if allowProviderMetadata {
		return server, nil
	}
	labels := make(map[string]string, len(server.Labels)+5)
	for key, value := range server.Labels {
		labels[key] = value
	}
	for _, key := range []string{
		digitalOceanAccountLabel,
		digitalOceanRecoveryKeyIDLabel,
		digitalOceanKeyOwnedLabel,
		digitalOceanKeyDeleteAuthorizedLabel,
		"recovery",
	} {
		if value, ok := existing.Labels[key]; ok {
			labels[key] = value
		} else {
			delete(labels, key)
		}
	}
	server.Labels = labels
	return server, nil
}

func (p Provider) ServerTypeForConfig(cfg core.Config) string {
	if cfg.ServerTypeExplicit && cfg.ServerType != "" {
		return cfg.ServerType
	}
	return digitalOceanServerTypeForClass(cfg.Class)
}

func (Provider) ServerTypeForClass(class string) string {
	return digitalOceanServerTypeForClass(class)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return NewDigitalOceanLeaseBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	doctor, ok := backend.(core.DoctorBackend)
	if !ok {
		return nil, core.Exit(2, "digitalocean doctor backend unavailable")
	}
	return doctor, nil
}

func digitalOceanServerTypeForClass(class string) string {
	switch class {
	case "standard", "fast", "large", "beast":
		return "s-1vcpu-1gb"
	default:
		return "s-1vcpu-1gb"
	}
}
