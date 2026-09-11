package proxmox

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string      { return "proxmox" }
func (Provider) Aliases() []string { return nil }
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

type flagValues struct {
	APIURL      *string
	Node        *string
	TemplateID  *int
	Storage     *string
	Pool        *string
	Bridge      *string
	User        *string
	WorkRoot    *string
	FullClone   *bool
	InsecureTLS *bool
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return flagValues{
		APIURL:      fs.String("proxmox-api-url", defaults.Proxmox.APIURL, "Proxmox VE API URL"),
		Node:        fs.String("proxmox-node", defaults.Proxmox.Node, "Proxmox VE node name"),
		TemplateID:  fs.Int("proxmox-template-id", defaults.Proxmox.TemplateID, "Proxmox QEMU template VMID"),
		Storage:     fs.String("proxmox-storage", defaults.Proxmox.Storage, "Proxmox clone storage"),
		Pool:        fs.String("proxmox-pool", defaults.Proxmox.Pool, "Proxmox pool for cloned VMs"),
		Bridge:      fs.String("proxmox-bridge", defaults.Proxmox.Bridge, "Proxmox bridge for net0 override"),
		User:        fs.String("proxmox-user", defaults.Proxmox.User, "cloud-init SSH user for cloned VMs"),
		WorkRoot:    fs.String("proxmox-work-root", defaults.Proxmox.WorkRoot, "remote work root for Proxmox VMs"),
		FullClone:   fs.Bool("proxmox-full-clone", defaults.Proxmox.FullClone, "create full Proxmox clones"),
		InsecureTLS: fs.Bool("proxmox-insecure-tls", defaults.Proxmox.InsecureTLS, "allow self-signed Proxmox TLS certificates"),
	}
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if core.FlagWasSet(fs, "proxmox-api-url") {
		cfg.Proxmox.APIURL = *v.APIURL
		core.RecordProviderFlagInputs(cfg, true, "proxmox")
	}
	if core.FlagWasSet(fs, "proxmox-node") {
		cfg.Proxmox.Node = *v.Node
		core.RecordProviderFlagInputs(cfg, true, "proxmox")
	}
	if core.FlagWasSet(fs, "proxmox-template-id") {
		cfg.Proxmox.TemplateID = *v.TemplateID
		core.RecordProviderFlagInputs(cfg, true, "proxmox")
		cfg.ServerType = core.ProxmoxServerTypeForConfig(*cfg)
	}
	if core.FlagWasSet(fs, "proxmox-storage") {
		cfg.Proxmox.Storage = *v.Storage
		core.RecordProviderFlagInputs(cfg, true, "proxmox")
	}
	if core.FlagWasSet(fs, "proxmox-pool") {
		cfg.Proxmox.Pool = *v.Pool
		core.RecordProviderFlagInputs(cfg, true, "proxmox")
	}
	if core.FlagWasSet(fs, "proxmox-bridge") {
		cfg.Proxmox.Bridge = *v.Bridge
		core.RecordProviderFlagInputs(cfg, true, "proxmox")
	}
	if core.FlagWasSet(fs, "proxmox-user") {
		cfg.Proxmox.User = *v.User
		core.RecordProviderFlagInputs(cfg, true, "proxmox")
		cfg.SSHUser = *v.User
	}
	if core.FlagWasSet(fs, "proxmox-work-root") {
		cfg.Proxmox.WorkRoot = *v.WorkRoot
		core.RecordProviderFlagInputs(cfg, true, "proxmox")
		cfg.WorkRoot = *v.WorkRoot
	}
	if core.FlagWasSet(fs, "proxmox-full-clone") {
		cfg.Proxmox.FullClone = *v.FullClone
		core.RecordProviderFlagInputs(cfg, true, "proxmox")
	}
	if core.FlagWasSet(fs, "proxmox-insecure-tls") {
		cfg.Proxmox.InsecureTLS = *v.InsecureTLS
		core.RecordProviderFlagInputs(cfg, true, "proxmox")
	}
	return nil
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return NewLeaseBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	return shared.ConfigureDoctor("proxmox", func() (core.Backend, error) { return p.Configure(cfg, rt) })
}
