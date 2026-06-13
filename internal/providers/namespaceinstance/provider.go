package namespaceinstance

import (
	"context"
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string { return providerName }
func (Provider) Aliases() []string {
	return []string{providerAlias}
}
func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:        providerName,
		Family:      "namespace",
		Kind:        core.ProviderKindSSHLease,
		Targets:     []core.TargetSpec{{OS: core.TargetLinux}},
		Features:    core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup},
		Coordinator: core.CoordinatorNever,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return RegisterNamespaceInstanceProviderFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return ApplyNamespaceInstanceProviderFlags(cfg, fs, values)
}

func (Provider) ServerTypeForConfig(cfg core.Config) string {
	return namespaceInstanceServerTypeForConfig(cfg)
}

func (Provider) ServerTypeForClass(class string) string {
	return namespaceInstanceServerTypeForClass(class)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return newBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	return newBackend(p.Spec(), cfg, rt), nil
}

type backend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

func newBackend(spec ProviderSpec, cfg Config, rt Runtime) *backend {
	applyNamespaceInstanceDefaults(&cfg)
	return &backend{spec: spec, cfg: cfg, rt: rt}
}

func (b *backend) Spec() ProviderSpec { return b.spec }

func (b *backend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	client, err := newNSCClient(b.cfg, b.rt)
	if err != nil {
		return DoctorResult{}, err
	}
	count, err := client.CheckReadiness(ctx)
	if err != nil {
		return DoctorResult{}, err
	}
	return DoctorResult{
		Provider: providerName,
		Status:   "ok",
		Message:  "nsc=ready auth=ready list=ready mutation=false",
		Checks: []DoctorCheck{
			{Status: "ok", Check: "nsc", Message: "nsc=ready", Details: map[string]string{"mutation": "false"}},
			{Status: "ok", Check: "auth", Message: "auth=ready", Details: map[string]string{"mutation": "false"}},
			{Status: "ok", Check: "inventory", Message: "list=ready", Details: map[string]string{"leases": count, "mutation": "false"}},
		},
	}, nil
}

func (b *backend) Acquire(context.Context, AcquireRequest) (LeaseTarget, error) {
	return LeaseTarget{}, exit(2, "provider=%s acquire is deferred to the namespace-instance lifecycle plan", providerName)
}

func (b *backend) Resolve(_ context.Context, req ResolveRequest) (LeaseTarget, error) {
	return LeaseTarget{LeaseID: req.ID}, exit(2, "provider=%s resolve is deferred to the namespace-instance lifecycle plan", providerName)
}

func (b *backend) List(context.Context, ListRequest) ([]LeaseView, error) {
	return nil, exit(2, "provider=%s list is deferred to the namespace-instance lifecycle plan", providerName)
}

func (b *backend) ReleaseLease(context.Context, ReleaseLeaseRequest) error {
	return exit(2, "provider=%s release is deferred to the namespace-instance lifecycle plan", providerName)
}

func (b *backend) Touch(_ context.Context, req TouchRequest) (Server, error) {
	return req.Lease.Server, nil
}
