package koyeb

import (
	"flag"
	"regexp"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

const providerName = "koyeb"

var privateHostPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.internal$`)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

var (
	_ core.Provider                                 = Provider{}
	_ core.DoctorProvider                           = Provider{}
	_ core.ProviderArchitectureCapability           = Provider{}
	_ core.ProviderConfigDefaulter                  = Provider{}
	_ core.ProviderSSHTargetConfigurer              = Provider{}
	_ core.ProviderServerTypeProvider               = Provider{}
	_ core.ProviderReadyPoolImageIdentityCapability = Provider{}
)

func (Provider) Name() string      { return providerName }
func (Provider) Aliases() []string { return nil }

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Authentication: core.ProviderAuthentication{
			{Route: "brokered", Methods: []core.ProviderAuthenticationMethod{core.ProviderAuthenticationCoordinator}, Description: "The client authenticates to the coordinator; Koyeb credentials remain server-side."},
		},
		Name:             providerName,
		Family:           providerName,
		Kind:             core.ProviderKindSSHLease,
		Targets:          []core.TargetSpec{{OS: core.TargetLinux}},
		Features:         core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup, core.FeatureDesktop, core.FeatureBrowser, core.FeatureCode, core.FeatureTailscale},
		Coordinator:      core.CoordinatorSupported,
		ClassDisposition: core.ProviderClassDispositionUnmapped,
	}
}

func (Provider) RegisterFlags(*flag.FlagSet, core.Config) any { return core.NoProviderFlags() }

func (Provider) ApplyFlags(*core.Config, *flag.FlagSet, any) error { return nil }

func (Provider) ApplyConfigDefaults(cfg *core.Config) error {
	cfg.Provider = providerName
	// Koyeb allocation is coordinator-owned. Do not leak the compiled Hetzner
	// location and image defaults into the coordinator request.
	cfg.Location = ""
	cfg.Image = ""
	core.ApplyTailscaleEnabledDefault(cfg, true)
	if !core.IsWorkRootExplicit(cfg) {
		cfg.WorkRoot = "/workspace/crabbox"
	}
	return nil
}

func (Provider) SupportsArchitecture(_ core.Config, architecture string) bool {
	return architecture == core.ArchitectureAMD64
}

func (Provider) ConfigureSSHTarget(target *core.SSHTarget, _ string) {
	if target.TargetOS != core.TargetLinux {
		return
	}
	if !privateHostPattern.MatchString(target.Host) {
		target.ProxyCommand = "tailscale nc %h %p"
		target.SSHConfigProxy = true
	}
	target.ReadyCheck = "crabbox-ready"
}

func (Provider) ServerTypeForConfig(cfg core.Config) string { return cfg.ServerType }

func (Provider) ServerTypeForClass(string) string { return "" }

func (Provider) ReadyPoolImageIdentityMatchesLease(req core.ProviderReadyPoolImageIdentityRequest) bool {
	image := req.Lease.Image
	if req.Identity.Provider != providerName || req.Lease.Provider != providerName || image == nil ||
		image.Provider != providerName || image.Kind != "koyeb-sandbox-runner" || image.Source != "explicit" ||
		image.ID != req.Identity.ID || image.Scope != req.Identity.Scope || image.Region != req.Lease.Region {
		return false
	}
	parts := strings.Split(image.Scope, ":")
	return len(parts) == 6 && parts[0] == providerName && koyebUUIDPattern.MatchString(parts[1]) &&
		koyebUUIDPattern.MatchString(parts[2]) && parts[3] == req.Lease.Region &&
		parts[4] == req.Lease.ServerType && parts[5] == "clean-runner-v1" &&
		koyebImagePattern.MatchString(image.ID) && req.Lease.Region != "" && req.Lease.ServerType != ""
}

var koyebUUIDPattern = regexp.MustCompile(`^[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}$`)
var koyebImagePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._:-]*(?:/[a-z0-9][a-z0-9._-]*)+(?::[A-Za-z0-9_][A-Za-z0-9._-]{0,127})?@sha256:[a-f0-9]{64}$`)

func (p Provider) Configure(core.Config, core.Runtime) (core.Backend, error) {
	return newBackend(p.Spec()), nil
}

func (p Provider) ConfigureDoctor(core.Config, core.Runtime) (core.DoctorBackend, error) {
	return newBackend(p.Spec()), nil
}
