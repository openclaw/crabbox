package proxmox

import (
	"flag"
	"strconv"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) ServerTypeForConfig(cfg core.Config) string {
	if cfg.Proxmox.TemplateID > 0 {
		return "template-" + strconv.Itoa(cfg.Proxmox.TemplateID)
	}
	return "template"
}

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Authentication:   core.DirectProviderAuthentication(core.ProviderAuthenticationAPIToken),
		Name:             "proxmox",
		Family:           "proxmox",
		Kind:             core.ProviderKindSSHLease,
		Targets:          []core.TargetSpec{{OS: core.TargetLinux}},
		Features:         core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup},
		Coordinator:      core.CoordinatorNever,
		ClassDisposition: core.ProviderClassDispositionUnmapped,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterProxmoxConfigFlags(fs, defaults.Proxmox)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(core.ProxmoxConfigFlagValues)
	if !ok {
		return nil
	}
	applied, err := v.Apply(&cfg.Proxmox, fs)
	core.RecordProviderFlagInputs(cfg, applied.InputAccepted, "proxmox")
	if err != nil {
		return err
	}
	// This projection depends only on TemplateID, including explicit zero/negative values.
	if core.FlagWasSet(fs, "proxmox-template-id") {
		cfg.ServerType = (Provider{}).ServerTypeForConfig(*cfg)
	}
	if applied.User {
		cfg.SSHUser = cfg.Proxmox.User
	}
	if applied.WorkRoot {
		cfg.WorkRoot = cfg.Proxmox.WorkRoot
	}
	return nil
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return NewLeaseBackend(p.Spec(), cfg, rt), nil
}
