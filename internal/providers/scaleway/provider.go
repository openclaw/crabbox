package scaleway

import (
	"context"
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

const providerName = "scaleway"

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

type flagValues struct {
	Region         *string
	Zone           *string
	Image          *string
	Type           *string
	ProjectID      *string
	OrganizationID *string
	SecurityGroup  *string
	SSHCIDRs       *string
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return flagValues{
		Region:         fs.String("scaleway-region", defaults.Scaleway.Region, "Scaleway region"),
		Zone:           fs.String("scaleway-zone", defaults.Scaleway.Zone, "Scaleway zone"),
		Image:          fs.String("scaleway-image", defaults.Scaleway.Image, "Scaleway image label or ID"),
		Type:           fs.String("scaleway-type", defaults.Scaleway.Type, "Scaleway Instances commercial type"),
		ProjectID:      fs.String("scaleway-project-id", defaults.Scaleway.ProjectID, "Scaleway project ID"),
		OrganizationID: fs.String("scaleway-organization-id", defaults.Scaleway.OrganizationID, "Scaleway organization ID"),
		SecurityGroup:  fs.String("scaleway-security-group", defaults.Scaleway.SecurityGroup, "Scaleway security group ID"),
		SSHCIDRs:       fs.String("scaleway-ssh-cidrs", "", "comma-separated Scaleway SSH source CIDRs"),
	}
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if core.FlagWasSet(fs, "scaleway-region") {
		cfg.Scaleway.Region = *v.Region
	}
	if core.FlagWasSet(fs, "scaleway-zone") {
		cfg.Scaleway.Zone = *v.Zone
	}
	if core.FlagWasSet(fs, "scaleway-image") {
		cfg.Scaleway.Image = *v.Image
		core.SetScalewayImageExplicit(cfg)
	}
	if core.FlagWasSet(fs, "scaleway-type") {
		cfg.Scaleway.Type = *v.Type
		core.SetScalewayTypeExplicit(cfg)
	}
	if core.FlagWasSet(fs, "scaleway-project-id") {
		cfg.Scaleway.ProjectID = *v.ProjectID
	}
	if core.FlagWasSet(fs, "scaleway-organization-id") {
		cfg.Scaleway.OrganizationID = *v.OrganizationID
	}
	if core.FlagWasSet(fs, "scaleway-security-group") {
		cfg.Scaleway.SecurityGroup = *v.SecurityGroup
	}
	if core.FlagWasSet(fs, "scaleway-ssh-cidrs") {
		cfg.Scaleway.SSHCIDRs = splitCommaList(*v.SSHCIDRs)
	}
	return nil
}

func (Provider) ValidateConfig(cfg core.Config) error {
	return validateFoundationConfig(cfg)
}

func (Provider) ServerTypeForConfig(cfg core.Config) string {
	if cfg.ServerTypeExplicit && cfg.ServerType != "" {
		return cfg.ServerType
	}
	if cfg.Scaleway.Type != "" {
		return cfg.Scaleway.Type
	}
	return scalewayServerTypeForClass(cfg.Class)
}

func (Provider) ServerTypeForClass(class string) string {
	return scalewayServerTypeForClass(class)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return &Backend{spec: p.Spec(), cfg: cfg, rt: rt, newClient: newClient}, nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	doctor, ok := backend.(core.DoctorBackend)
	if !ok {
		return nil, core.Exit(2, "scaleway doctor backend unavailable")
	}
	return doctor, nil
}

type Backend struct {
	spec      core.ProviderSpec
	cfg       core.Config
	rt        core.Runtime
	newClient func(core.Config, core.Runtime) (Client, error)
}

func (b *Backend) Spec() core.ProviderSpec { return b.spec }

func (b *Backend) Doctor(context.Context, core.DoctorRequest) (core.DoctorResult, error) {
	if _, err := b.newClient(b.cfg, b.rt); err != nil {
		return core.DoctorResult{Provider: providerName, Message: err.Error(), Status: "failed", Checks: []core.DoctorCheck{{
			Status:  "failed",
			Check:   "auth",
			Message: err.Error(),
			Details: map[string]string{"mutation": "false"},
		}}}, nil
	}
	return core.DoctorResult{Provider: providerName, Message: "auth=ready mutation=false", Status: "ok", Checks: []core.DoctorCheck{{
		Status:  "ok",
		Check:   "auth",
		Message: "auth=ready mutation=false",
		Details: map[string]string{"mutation": "false"},
	}}}, nil
}

func (b *Backend) Acquire(context.Context, core.AcquireRequest) (core.LeaseTarget, error) {
	return core.LeaseTarget{}, core.Exit(2, "provider=scaleway acquire is not implemented yet")
}

func (b *Backend) Resolve(context.Context, core.ResolveRequest) (core.LeaseTarget, error) {
	return core.LeaseTarget{}, core.Exit(2, "provider=scaleway resolve is not implemented yet")
}

func (b *Backend) List(context.Context, core.ListRequest) ([]core.LeaseView, error) {
	return nil, core.Exit(2, "provider=scaleway list is not implemented yet")
}

func (b *Backend) ReleaseLease(context.Context, core.ReleaseLeaseRequest) error {
	return core.Exit(2, "provider=scaleway release is not implemented yet")
}

func (b *Backend) Touch(context.Context, core.TouchRequest) (core.Server, error) {
	return core.Server{}, core.Exit(2, "provider=scaleway touch is not implemented yet")
}

func scalewayServerTypeForClass(class string) string {
	switch class {
	case "standard", "fast", "large", "beast":
		return "DEV1-S"
	default:
		return "DEV1-S"
	}
}

func splitCommaList(value string) []string {
	if value == "" {
		return nil
	}
	var out []string
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}
