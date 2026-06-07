package xcpng

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string      { return "xcp-ng" }
func (Provider) Aliases() []string { return nil }
func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:        "xcp-ng",
		Family:      "xcp-ng",
		Kind:        core.ProviderKindSSHLease,
		Targets:     []core.TargetSpec{{OS: core.TargetLinux}},
		Features:    core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup},
		Coordinator: core.CoordinatorNever,
	}
}

type flagValues struct {
	APIURL       *string
	Username     *string
	Template     *string
	TemplateUUID *string
	SR           *string
	SRUUID       *string
	Network      *string
	NetworkUUID  *string
	Host         *string
	User         *string
	WorkRoot     *string
	InsecureTLS  *bool
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return flagValues{
		APIURL:       fs.String("xcp-ng-api-url", defaults.XCPNg.APIURL, "XCP-ng pool API URL"),
		Username:     fs.String("xcp-ng-username", defaults.XCPNg.Username, "XCP-ng API username"),
		Template:     fs.String("xcp-ng-template", defaults.XCPNg.Template, "XCP-ng VM template name"),
		TemplateUUID: fs.String("xcp-ng-template-uuid", defaults.XCPNg.TemplateUUID, "XCP-ng VM template UUID"),
		SR:           fs.String("xcp-ng-sr", defaults.XCPNg.SR, "XCP-ng storage repository name"),
		SRUUID:       fs.String("xcp-ng-sr-uuid", defaults.XCPNg.SRUUID, "XCP-ng storage repository UUID"),
		Network:      fs.String("xcp-ng-network", defaults.XCPNg.Network, "XCP-ng network name"),
		NetworkUUID:  fs.String("xcp-ng-network-uuid", defaults.XCPNg.NetworkUUID, "XCP-ng network UUID"),
		Host:         fs.String("xcp-ng-host", defaults.XCPNg.Host, "XCP-ng host name or UUID"),
		User:         fs.String("xcp-ng-user", defaults.XCPNg.User, "cloud-init SSH user for XCP-ng VMs"),
		WorkRoot:     fs.String("xcp-ng-work-root", defaults.XCPNg.WorkRoot, "remote work root for XCP-ng VMs"),
		InsecureTLS:  fs.Bool("xcp-ng-insecure-tls", defaults.XCPNg.InsecureTLS, "allow self-signed XCP-ng TLS certificates"),
	}
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if core.FlagWasSet(fs, "xcp-ng-api-url") {
		cfg.XCPNg.APIURL = *v.APIURL
	}
	if core.FlagWasSet(fs, "xcp-ng-username") {
		cfg.XCPNg.Username = *v.Username
	}
	if core.FlagWasSet(fs, "xcp-ng-template") {
		cfg.XCPNg.Template = *v.Template
		cfg.XCPNg.TemplateUUID = ""
		cfg.ServerType = xcpNgServerTypeForConfig(*cfg)
	}
	if core.FlagWasSet(fs, "xcp-ng-template-uuid") {
		cfg.XCPNg.TemplateUUID = *v.TemplateUUID
		cfg.XCPNg.Template = ""
		cfg.ServerType = xcpNgServerTypeForConfig(*cfg)
	}
	if core.FlagWasSet(fs, "xcp-ng-sr") {
		cfg.XCPNg.SR = *v.SR
		cfg.XCPNg.SRUUID = ""
	}
	if core.FlagWasSet(fs, "xcp-ng-sr-uuid") {
		cfg.XCPNg.SRUUID = *v.SRUUID
		cfg.XCPNg.SR = ""
	}
	if core.FlagWasSet(fs, "xcp-ng-network") {
		cfg.XCPNg.Network = *v.Network
		cfg.XCPNg.NetworkUUID = ""
	}
	if core.FlagWasSet(fs, "xcp-ng-network-uuid") {
		cfg.XCPNg.NetworkUUID = *v.NetworkUUID
		cfg.XCPNg.Network = ""
	}
	if core.FlagWasSet(fs, "xcp-ng-host") {
		cfg.XCPNg.Host = *v.Host
	}
	if core.FlagWasSet(fs, "xcp-ng-user") {
		cfg.XCPNg.User = *v.User
		cfg.SSHUser = *v.User
	}
	if core.FlagWasSet(fs, "xcp-ng-work-root") {
		cfg.XCPNg.WorkRoot = *v.WorkRoot
		cfg.WorkRoot = *v.WorkRoot
	}
	if core.FlagWasSet(fs, "xcp-ng-insecure-tls") {
		cfg.XCPNg.InsecureTLS = *v.InsecureTLS
	}
	return nil
}

func (Provider) ServerTypeForConfig(cfg core.Config) string {
	return xcpNgServerTypeForConfig(cfg)
}

func (Provider) ServerTypeForClass(string) string { return "template" }

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return NewLeaseBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	doctor, ok := backend.(core.DoctorBackend)
	if !ok {
		return nil, core.Exit(2, "xcp-ng doctor backend unavailable")
	}
	return doctor, nil
}
