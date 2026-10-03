package digitalocean

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) NormalizeConfigForShow(cfg core.Config) core.Config {
	core.ApplyConfigShowSSHDefaults(&cfg, "root")
	return cfg
}

var _ core.ProviderClassProfileProvider = Provider{}

var classProfiles = buildClassProfiles()

func buildClassProfiles() []core.ProviderClassProfile {
	// DigitalOcean size slugs and memory are published at https://slugs.do-api.dev/.
	machine := func(serverType string, vcpu int, memoryGiB float64) core.ProviderClassMachine {
		return core.ProviderClassMachine{
			Type: serverType, Architecture: core.ProviderClassArchitectureAMD64, VCPU: &vcpu,
			Memory: &core.ProviderMemory{Value: memoryGiB, Unit: core.ProviderMemoryUnitGiB},
		}
	}
	machines := map[string][]core.ProviderClassMachine{
		"tiny":     {machine("s-1vcpu-1gb", 1, 1)},
		"small":    {machine("s-2vcpu-4gb", 2, 4)},
		"standard": {machine("s-4vcpu-8gb", 4, 8)},
		"fast":     {machine("s-8vcpu-16gb", 8, 16), machine("c-8", 8, 16)},
		"large":    {machine("g-16vcpu-64gb", 16, 64), machine("c-16", 16, 32)},
		"beast":    {machine("g-32vcpu-128gb", 32, 128), machine("c-32", 32, 64)},
	}
	profiles := make([]core.ProviderClassProfile, 0, len(core.CanonicalProviderClasses()))
	for _, class := range core.CanonicalProviderClasses() {
		profiles = append(profiles, core.ProviderClassProfileFromMachines(
			class, core.TargetLinux, "", core.ProviderClassArchitectureAMD64, machines[class],
		))
	}
	return profiles
}

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Authentication:   core.DirectProviderAuthentication(core.ProviderAuthenticationAPIToken),
		Name:             providerName,
		Family:           providerName,
		Kind:             core.ProviderKindSSHLease,
		Targets:          []core.TargetSpec{{OS: core.TargetLinux}},
		Features:         core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup, core.FeatureTailscale},
		Coordinator:      core.CoordinatorNever,
		ClassDisposition: core.ProviderClassDispositionMapped,
	}
}

func (Provider) ClassProfiles() []core.ProviderClassProfile {
	return classProfiles
}

func (Provider) RegisterFlags(*flag.FlagSet, core.Config) any { return core.NoProviderFlags() }
func (Provider) ApplyFlags(*core.Config, *flag.FlagSet, any) error {
	return nil
}

func (Provider) PrepareLeaseClaimEndpoint(existing core.LeaseClaim, provider, slug string, server core.Server, allowProviderMetadata bool) (core.Server, error) {
	if existing.FixedCreateIntent != nil {
		if err := validateFixedDroplet(existing, droplet{ID: server.ID, Name: server.Name, Tags: tagsFromLabels(server.Labels)}); err != nil {
			return core.Server{}, err
		}
	}
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
	if allowProviderMetadata {
		return server, nil
	}
	labels := shared.CloneLabels(server.Labels)
	for _, key := range []string{
		digitalOceanAccountLabel,
		digitalOceanRecoveryKeyIDLabel,
		digitalOceanKeyOwnedLabel,
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
	return core.ProviderClassPrimaryTypeForProfiles(classProfiles, cfg, digitalOceanServerTypeForClass(cfg.Class))
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return NewDigitalOceanLeaseBackend(p.Spec(), cfg, rt), nil
}

func digitalOceanServerTypeForClass(class string) string {
	for _, profile := range classProfiles {
		if profile.Class == class {
			return profile.Primary.Type
		}
	}
	return "s-1vcpu-1gb"
}

func (Provider) ApplyConfigDefaults(cfg *core.Config) error {
	applyNativeDefaults(&cfg.DigitalOcean)
	if core.OSImageWasExplicit(*cfg) && !core.DigitalOceanImageWasExplicit(*cfg) {
		if cfg.OSImage == "ubuntu:24.04" {
			cfg.DigitalOcean.Image = "ubuntu-24-04-x64"
		} else {
			// Leave unsupported intent unresolved until acquisition validation.
			cfg.DigitalOcean.Image = ""
		}
	}
	base := core.BaseConfig()
	core.ApplyLinuxConnectionDefaults(cfg, base.SSHUser, base.SSHPort)
	return nil
}

func applyNativeDefaults(cfg *core.DigitalOceanConfig) {
	if cfg.Region == "" {
		cfg.Region = core.DigitalOceanRegionFallback
	}
	if cfg.Image == "" {
		cfg.Image = core.DigitalOceanImageFallback
	}
}
