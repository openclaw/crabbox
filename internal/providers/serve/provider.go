package serve

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/localcontainer"
)

func init() { core.RegisterProvider(Provider{}) }

type Provider struct{}

func (Provider) Spec() core.ProviderSpec {
	// The coordinator resolves the four served sizes, including dynamic whole-offer
	// capacity; this adapter does not expose a static canonical class profile catalog.
	return core.ProviderSpec{Authentication: core.ProviderAuthentication{{Route: "brokered", Methods: []core.ProviderAuthenticationMethod{core.ProviderAuthenticationCoordinator}, Description: "Owner-scoped coordinator login for donated machines."}}, Name: "serve", Family: "serve", Kind: core.ProviderKindSSHLease, ClassDisposition: core.ProviderClassDispositionUnmapped, Targets: []core.TargetSpec{{OS: core.TargetLinux}}, Features: core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync}, BrokeredOnly: true, Coordinator: core.CoordinatorSupported, ActionsRunnerUnsupported: true}
}

type flags struct{ machine, fallback *string }

func (Provider) RegisterFlags(fs *flag.FlagSet, cfg core.Config) any {
	return flags{fs.String("serve-machine", cfg.Serve.Machine, "Pin a served machine by name or UUID"), fs.String("serve-fallback", cfg.Serve.Fallback, "Retry capacity-unavailable acquisition once on this provider")}
}
func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, v any) error {
	f := v.(flags)
	if core.FlagWasSet(fs, "serve-machine") {
		cfg.Serve.Machine = *f.machine
	}
	if core.FlagWasSet(fs, "serve-fallback") {
		cfg.Serve.Fallback = *f.fallback
	}
	return nil
}
func (Provider) CreationOnlyFlagNames() []string { return []string{"serve-machine", "serve-fallback"} }
func (Provider) ApplyConfigDefaults(cfg *core.Config) error {
	if cfg.Class != "" && cfg.Class != "beast" && !cfg.ServerTypeExplicit {
		found := false
		for _, spec := range classSizes() {
			if cfg.Class == spec.Class {
				found = true
			}
		}
		if !found {
			return core.Exit(2, "serve class must be standard, fast, large, or beast")
		}
	}

	core.ApplyLinuxConnectionDefaults(cfg, "crabbox", "22")
	cfg.SSHFallbackPorts = []string{}
	cfg.SSHUser = "crabbox"
	cfg.WorkRoot = "/work/crabbox"
	return nil
}
func (Provider) SupportsArchitecture(_ core.Config, a string) bool {
	return a == core.ArchitectureAMD64 || a == core.ArchitectureARM64
}
func (Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if cfg.Desktop || cfg.Browser || cfg.Code || cfg.Tailscale.Enabled {
		return nil, core.Exit(2, "provider=serve does not support desktop, WebVNC, browser, code or guest Tailscale; use the machine's advertised address")
	}
	return &backend{cfg: cfg, rt: rt}, nil
}
func (Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	return &backend{cfg: cfg, rt: rt}, nil
}

type backend struct {
	cfg core.Config
	rt  core.Runtime
}

func (b *backend) Spec() core.ProviderSpec { return Provider{}.Spec() }
func (b *backend) Acquire(context.Context, core.AcquireRequest) (core.LeaseTarget, error) {
	return core.LeaseTarget{}, directError()
}
func (b *backend) Resolve(context.Context, core.ResolveRequest) (core.LeaseTarget, error) {
	return core.LeaseTarget{}, directError()
}
func (b *backend) List(context.Context, core.ListRequest) ([]core.Server, error) {
	return nil, directError()
}
func (b *backend) ReleaseLease(context.Context, core.ReleaseLeaseRequest) error { return directError() }
func (b *backend) Touch(context.Context, core.TouchRequest) (core.Server, error) {
	return core.Server{}, directError()
}
func directError() error {
	return core.Exit(2, "provider=serve requires a coordinator in brokered mode; configure broker.url and log in")
}
func (b *backend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	if strings.TrimSpace(b.cfg.Coordinator) == "" {
		return core.DoctorResult{}, directError()
	}
	c, err := core.NewCoordinatorClient(b.cfg)
	if err != nil {
		return core.DoctorResult{}, err
	}
	var result struct {
		Machines []machine `json:"machines"`
	}
	err = c.AgentRequest(ctx, "GET", "/v1/serve/machines", nil, &result)
	if err != nil {
		return core.DoctorResult{}, err
	}
	count := 0
	for _, m := range result.Machines {
		if m.Online {
			count++
		}
	}
	return core.DoctorResult{Provider: "serve", Status: "ready", Message: fmt.Sprintf("coordinator=ready online_machines=%d mutation=false", count)}, nil
}

func (b *backend) AcquisitionFallback(err error) (string, bool) {
	var response core.CoordinatorHTTPError
	if b.cfg.Serve.Fallback == "" || b.cfg.Serve.Fallback == "serve" || !errors.As(err, &response) || response.StatusCode != 409 {
		return "", false
	}
	var body struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(response.Message), &body) != nil || body.Error != "serve_capacity_unavailable" {
		return "", false
	}
	return b.cfg.Serve.Fallback, true
}

func classSizes() []core.ClassSpec {
	return []core.ClassSpec{{Class: "standard", Type: "cpu8", VCPUs: 8, MemoryGB: 16}, {Class: "fast", Type: "cpu16", VCPUs: 16, MemoryGB: 32}, {Class: "large", Type: "cpu32", VCPUs: 32, MemoryGB: 64}, {Class: "beast", Type: "whole-offer"}}
}

func (Provider) CoordinatorSelectsArchitecture() bool            { return true }
func (Provider) DescribeImplicitArchitecture(core.Config) string { return "native" }

func (Provider) ConfigureSSHTarget(target *core.SSHTarget, _ string) {
	target.ReadyCheck = localcontainer.ServedReadyCheck()
	target.HostKeyAlias = target.Host
	valid := core.IsCanonicalLeaseID(target.Host) && target.Port == "22" && target.User == "crabbox" && target.SSHHostKey != "" && target.SSHJump != nil
	target.PrepareConnection = func(context.Context) error {
		if !valid {
			return core.Exit(2, "serve lease is missing its pinned SSH front-door route; refresh the lease")
		}
		return nil
	}
	target.NoControlMaster = true
	target.SSHConfigProxy = true
	target.FallbackPorts = []string{}
}
