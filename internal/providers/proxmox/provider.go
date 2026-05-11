package proxmox

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string      { return "proxmox" }
func (Provider) Aliases() []string { return nil }
func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:        "proxmox",
		Kind:        core.ProviderKindSSHLease,
		Targets:     []core.TargetSpec{{OS: core.TargetLinux}},
		Features:    core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup},
		Coordinator: core.CoordinatorNever,
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
	}
	if core.FlagWasSet(fs, "proxmox-node") {
		cfg.Proxmox.Node = *v.Node
	}
	if core.FlagWasSet(fs, "proxmox-template-id") {
		cfg.Proxmox.TemplateID = *v.TemplateID
		cfg.ServerType = core.ProxmoxServerTypeForConfig(*cfg)
	}
	if core.FlagWasSet(fs, "proxmox-storage") {
		cfg.Proxmox.Storage = *v.Storage
	}
	if core.FlagWasSet(fs, "proxmox-pool") {
		cfg.Proxmox.Pool = *v.Pool
	}
	if core.FlagWasSet(fs, "proxmox-bridge") {
		cfg.Proxmox.Bridge = *v.Bridge
	}
	if core.FlagWasSet(fs, "proxmox-user") {
		cfg.Proxmox.User = *v.User
		cfg.SSHUser = *v.User
	}
	if core.FlagWasSet(fs, "proxmox-work-root") {
		cfg.Proxmox.WorkRoot = *v.WorkRoot
		cfg.WorkRoot = *v.WorkRoot
	}
	if core.FlagWasSet(fs, "proxmox-full-clone") {
		cfg.Proxmox.FullClone = *v.FullClone
	}
	if core.FlagWasSet(fs, "proxmox-insecure-tls") {
		cfg.Proxmox.InsecureTLS = *v.InsecureTLS
	}
	return nil
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return NewLeaseBackend(p.Spec(), cfg, rt), nil
}
