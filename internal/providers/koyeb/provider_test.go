package koyeb

import (
	"context"
	"slices"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

const wantCoordinatorRequiredError = "provider=koyeb requires a configured coordinator; direct lifecycle is not supported"

func TestProviderSpec(t *testing.T) {
	p := Provider{}
	if p.Name() != providerName {
		t.Fatalf("Name()=%q want %q", p.Name(), providerName)
	}
	if aliases := p.Aliases(); len(aliases) != 0 {
		t.Fatalf("Aliases()=%v want none", aliases)
	}
	spec := p.Spec()
	if spec.Name != providerName || spec.Family != providerName || spec.Kind != core.ProviderKindSSHLease {
		t.Fatalf("spec identity=%#v", spec)
	}
	if spec.Coordinator != core.CoordinatorSupported {
		t.Fatalf("Coordinator=%q want %q", spec.Coordinator, core.CoordinatorSupported)
	}
	if spec.ClassDisposition != core.ProviderClassDispositionUnmapped {
		t.Fatalf("ClassDisposition=%q want %q", spec.ClassDisposition, core.ProviderClassDispositionUnmapped)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux || spec.Targets[0].WindowsMode != "" {
		t.Fatalf("Targets=%#v want linux only", spec.Targets)
	}
	wantFeatures := core.FeatureSet{
		core.FeatureSSH,
		core.FeatureCrabboxSync,
		core.FeatureCleanup,
		core.FeatureDesktop,
		core.FeatureBrowser,
		core.FeatureCode,
		core.FeatureTailscale,
	}
	if !slices.Equal(spec.Features, wantFeatures) {
		t.Fatalf("Features=%v want %v", spec.Features, wantFeatures)
	}
}

func TestProviderUsesCoordinatorWhenConfigured(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Coordinator = "https://broker.example.test"
	if !core.ShouldUseCoordinator(cfg, (Provider{}).Spec()) {
		t.Fatal("ShouldUseCoordinator=false with a configured coordinator")
	}
	cfg.Coordinator = ""
	if core.ShouldUseCoordinator(cfg, (Provider{}).Spec()) {
		t.Fatal("ShouldUseCoordinator=true without a configured coordinator")
	}
}

func TestProviderSupportsLinuxAMD64Only(t *testing.T) {
	p := Provider{}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	if !p.SupportsArchitecture(cfg, core.ArchitectureAMD64) {
		t.Fatal("amd64 rejected")
	}
	if p.SupportsArchitecture(cfg, core.ArchitectureARM64) {
		t.Fatal("arm64 accepted")
	}
}

func TestProviderConfiguresUserspaceTailscaleSSH(t *testing.T) {
	target := core.SSHTarget{
		Host:     "100.101.102.103",
		Port:     "22",
		TargetOS: core.TargetLinux,
	}
	(Provider{}).ConfigureSSHTarget(&target, "command -v git")
	if target.Host != "100.101.102.103" || target.Port != "22" {
		t.Fatalf("route changed=%#v", target)
	}
	if !target.SSHConfigProxy || target.ProxyCommand != "tailscale nc %h %p" {
		t.Fatalf("userspace Tailscale proxy=%#v", target)
	}
	if target.ReadyCheck != "crabbox-ready" {
		t.Fatalf("ReadyCheck=%q want crabbox-ready", target.ReadyCheck)
	}
}

func TestProviderConfiguresDirectKoyebMeshSSH(t *testing.T) {
	target := core.SSHTarget{
		Host:     "cbx-blue-lobster.my-app.internal",
		Port:     "22",
		TargetOS: core.TargetLinux,
	}
	(Provider{}).ConfigureSSHTarget(&target, "command -v git")
	if target.Host != "cbx-blue-lobster.my-app.internal" || target.Port != "22" {
		t.Fatalf("route changed=%#v", target)
	}
	if target.SSHConfigProxy || target.ProxyCommand != "" {
		t.Fatalf("private mesh must use direct SSH=%#v", target)
	}
	if target.ReadyCheck != "crabbox-ready" {
		t.Fatalf("ReadyCheck=%q want crabbox-ready", target.ReadyCheck)
	}
}

func TestProviderDefaultsRemoveUnrelatedCloudValuesAndPreserveNativeType(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.ServerType = "medium"
	cfg.ServerTypeExplicit = true
	if err := (Provider{}).ApplyConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Location != "" || cfg.Image != "" {
		t.Fatalf("unrelated defaults location=%q image=%q", cfg.Location, cfg.Image)
	}
	if cfg.ServerType != "medium" || (Provider{}).ServerTypeForConfig(cfg) != "medium" {
		t.Fatalf("native type=%q resolved=%q", cfg.ServerType, (Provider{}).ServerTypeForConfig(cfg))
	}
}

func TestBackendFailsClosedWithoutCoordinator(t *testing.T) {
	p := Provider{}
	backend, err := p.Configure(core.BaseConfig(), core.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	ssh, ok := backend.(core.SSHLeaseBackend)
	if !ok {
		t.Fatalf("backend %T does not implement SSHLeaseBackend", backend)
	}
	cleanup, ok := backend.(core.CleanupBackend)
	if !ok {
		t.Fatalf("backend %T does not implement CleanupBackend", backend)
	}
	doctor, ok := backend.(core.DoctorBackend)
	if !ok {
		t.Fatalf("backend %T does not implement DoctorBackend", backend)
	}

	ctx := context.Background()
	tests := map[string]func() error{
		"acquire": func() error {
			_, err := ssh.Acquire(ctx, core.AcquireRequest{})
			return err
		},
		"resolve": func() error {
			_, err := ssh.Resolve(ctx, core.ResolveRequest{})
			return err
		},
		"list": func() error {
			_, err := ssh.List(ctx, core.ListRequest{})
			return err
		},
		"release": func() error {
			return ssh.ReleaseLease(ctx, core.ReleaseLeaseRequest{})
		},
		"touch": func() error {
			_, err := ssh.Touch(ctx, core.TouchRequest{})
			return err
		},
		"cleanup": func() error {
			return cleanup.Cleanup(ctx, core.CleanupRequest{})
		},
		"doctor": func() error {
			_, err := doctor.Doctor(ctx, core.DoctorRequest{})
			return err
		},
	}
	for name, run := range tests {
		t.Run(name, func(t *testing.T) {
			if err := run(); err == nil || err.Error() != wantCoordinatorRequiredError {
				t.Fatalf("error=%v want %q", err, wantCoordinatorRequiredError)
			}
		})
	}
}

func TestProviderConfiguresDoctorBackend(t *testing.T) {
	doctor, err := (Provider{}).ConfigureDoctor(core.BaseConfig(), core.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := doctor.(core.SSHLeaseBackend); !ok {
		t.Fatalf("doctor backend %T does not preserve SSH lease scaffolding", doctor)
	}
}
