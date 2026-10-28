// Package sshhost implements operator-managed native SSH capacity. Static ssh
// remains a separate, direct provider with its existing credential semantics.
package sshhost

import (
	"context"
	"flag"
	"fmt"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() { core.RegisterProvider(Provider{}) }

type Provider struct{}

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{Name: "ssh-host", Family: "ssh", Kind: core.ProviderKindSSHLease,
		Authentication: core.ProviderAuthentication{{Route: "brokered", Methods: []core.ProviderAuthenticationMethod{core.ProviderAuthenticationCoordinator}, Description: "Coordinator login for exclusive operator-managed capacity."}},
		Targets:        []core.TargetSpec{{OS: core.TargetLinux}, {OS: core.TargetWindows, WindowsMode: core.WindowsModeNormal}},
		Features:       core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync}, Coordinator: core.CoordinatorSupported, BrokeredOnly: true,
		ClassDisposition: core.ProviderClassDispositionUnmapped, ActionsRunnerUnsupported: true}
}
func (Provider) CoordinatorSelectsArchitecture() bool { return true }
func (Provider) SupportsArchitecture(_ core.Config, a string) bool {
	return a == "amd64" || a == "arm64"
}

type flags struct{ host, capabilities *string }

func (Provider) RegisterFlags(fs *flag.FlagSet, cfg core.Config) any {
	return flags{fs.String("ssh-host", cfg.ManagedHost, "Select an enrolled host by name or UUID"), fs.String("host-capabilities", strings.Join(cfg.HostCapabilities, ","), "Required operator-advertised capabilities")}
}
func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, v any) error {
	f := v.(flags)
	if core.FlagWasSet(fs, "ssh-host") {
		cfg.ManagedHost = strings.TrimSpace(*f.host)
	}
	if core.FlagWasSet(fs, "host-capabilities") {
		cfg.HostCapabilities = nil
		for _, c := range strings.Split(*f.capabilities, ",") {
			if c = strings.TrimSpace(c); c != "" {
				cfg.HostCapabilities = append(cfg.HostCapabilities, c)
			}
		}
	}
	return nil
}
func (Provider) CoordinatorRequestFields(cfg core.Config) map[string]any {
	fields := map[string]any{"sshHost": cfg.ManagedHost}
	if len(cfg.HostCapabilities) > 0 {
		fields["hostCapabilities"] = cfg.HostCapabilities
	}
	return fields
}
func (Provider) CreationOnlyFlagNames() []string { return []string{"ssh-host", "host-capabilities"} }
func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return &backend{cfg: cfg}, nil
}
func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	return &backend{cfg: cfg}, nil
}

type backend struct{ cfg core.Config }

func (*backend) Spec() core.ProviderSpec { return Provider{}.Spec() }
func directError() error {
	return fmt.Errorf("ssh-host requires a coordinator in brokered mode; use ssh for direct access")
}
func (*backend) Acquire(context.Context, core.AcquireRequest) (core.LeaseTarget, error) {
	return core.LeaseTarget{}, directError()
}
func (*backend) Resolve(context.Context, core.ResolveRequest) (core.LeaseTarget, error) {
	return core.LeaseTarget{}, directError()
}
func (*backend) List(context.Context, core.ListRequest) ([]core.Server, error) {
	return nil, directError()
}
func (*backend) Touch(context.Context, core.TouchRequest) (core.Server, error) {
	return core.Server{}, directError()
}
func (*backend) ReleaseLease(context.Context, core.ReleaseLeaseRequest) error { return directError() }
func (b *backend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	c, err := core.NewCoordinatorClient(b.cfg)
	if err != nil {
		return core.DoctorResult{}, err
	}
	var out map[string]any
	err = c.AgentRequest(ctx, "GET", "/v1/ssh-hosts", nil, &out)
	return core.DoctorResult{Provider: "ssh-host", Message: "Coordinator reachable; host list reports eligible capacity"}, err
}

func (Provider) ConfigureSSHTarget(target *core.SSHTarget, _ string) {
	target.SSHConfigProxy = true
	target.NoControlMaster = true
	target.FallbackPorts = []string{}
	target.PrepareConnection = func(context.Context) error {
		if target.SSHJump == nil || !target.SSHJump.RelayOnly || !target.SSHJump.Relay || target.SSHJump.HostKey == "" || target.SSHHostKey == "" {
			return fmt.Errorf("managed host lease requires pinned relay-only access; refresh the lease")
		}
		return nil
	}
	if target.TargetOS == core.TargetWindows {
		target.ReadyCheck = core.PowershellCommand("$ErrorActionPreference='Stop'; git --version | Out-Null; tar --version | Out-Null; if ($LASTEXITCODE -ne 0) { exit 1 }")
	} else {
		target.ReadyCheck = "git --version >/dev/null && rsync --version >/dev/null"
	}
}

func (Provider) SupportsReadyPoolTarget(target string) bool {
	return target == core.TargetLinux || target == core.TargetWindows
}
func (Provider) ReadyPoolImageIdentityMatchesLease(req core.ProviderReadyPoolImageIdentityRequest) bool {
	image := req.Lease.Image
	return req.Identity.Provider == "ssh-host" && req.Lease.Provider == "ssh-host" && image != nil && image.Provider == "ssh-host" && image.Kind == "host-template" && image.ID == req.Identity.ID && image.Region == req.Identity.Scope && req.Lease.Region == req.Identity.Scope
}
