package fal

import (
	"context"
	"flag"
	"io"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderSpecAndAlias(t *testing.T) {
	spec := Provider{}.Spec()
	if spec.Name != providerName || spec.Family != providerName || spec.Kind != core.ProviderKindSSHLease || spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("unexpected spec: %#v", spec)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("targets=%#v, want linux only", spec.Targets)
	}
	for _, feature := range []core.Feature{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup} {
		if !spec.Features.Has(feature) {
			t.Fatalf("features=%v missing %s", spec.Features, feature)
		}
	}
	aliases := Provider{}.Aliases()
	if len(aliases) != 1 || aliases[0] != "fal-ai" {
		t.Fatalf("aliases=%#v, want [fal-ai]", aliases)
	}
}

func TestIsFalProviderNameAcceptsAlias(t *testing.T) {
	for _, name := range []string{"fal", "FAL", " fal-ai "} {
		if !isFalProviderName(name) {
			t.Fatalf("isFalProviderName(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", "falai", "runpod"} {
		if isFalProviderName(name) {
			t.Fatalf("isFalProviderName(%q) = true, want false", name)
		}
	}
}

func TestFalTokenFlagIsNotRegistered(t *testing.T) {
	cfg := Config{}
	cfg.Fal.APIKey = "secret-key"
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	Provider{}.RegisterFlags(fs, cfg)
	for _, name := range []string{"fal-key", "fal-api-key", "fal-token", "fal-api-token"} {
		if fs.Lookup(name) != nil {
			t.Fatalf("fal API key surfaced as a flag --%s", name)
		}
	}
	for _, name := range []string{"fal-api-url", "fal-instance-type", "fal-sector", "fal-user", "fal-work-root"} {
		if fs.Lookup(name) == nil {
			t.Fatalf("%s flag missing", name)
		}
	}
}

func TestFalFlagsApplyNonSecretConfig(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	values := Provider{}.RegisterFlags(fs, Config{})
	if err := fs.Parse([]string{
		"--fal-api-url", "https://api.example.test/v1",
		"--fal-instance-type", string(InstanceTypeH100x8),
		"--fal-sector", string(Sector3),
		"--fal-user", "ubuntu",
		"--fal-work-root", "/srv/crabbox",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Provider: providerName}
	if err := (Provider{}).ApplyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Fal.APIURL != "https://api.example.test/v1" ||
		cfg.Fal.InstanceType != string(InstanceTypeH100x8) ||
		cfg.Fal.Sector != string(Sector3) ||
		cfg.Fal.User != "ubuntu" ||
		cfg.Fal.WorkRoot != "/srv/crabbox" {
		t.Fatalf("fal flags not applied: %#v", cfg.Fal)
	}
	if cfg.Fal.APIKey != "" {
		t.Fatalf("fal API key should stay env-only, got %q", cfg.Fal.APIKey)
	}
}

func TestConfigureRejectsUnsupportedTargetAndTailscale(t *testing.T) {
	for name, cfg := range map[string]Config{
		"macos target": {TargetOS: "macos"},
		"tailscale":    {TargetOS: targetLinux, Tailscale: core.TailscaleConfig{Enabled: true}},
		"network":      {TargetOS: targetLinux, Network: "tailscale"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Provider{}.Configure(cfg, newDiscardRuntime())
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestConfigureReturnsSSHLeaseBackend(t *testing.T) {
	gotBackend, err := Provider{}.Configure(Config{TargetOS: targetLinux}, newDiscardRuntime())
	if err != nil {
		t.Fatal(err)
	}
	sshBackend, ok := gotBackend.(core.SSHLeaseBackend)
	if !ok {
		t.Fatalf("backend %T does not implement SSHLeaseBackend", gotBackend)
	}
	cleanup, ok := gotBackend.(core.CleanupBackend)
	if !ok {
		t.Fatalf("backend %T does not implement CleanupBackend", gotBackend)
	}
	_ = sshBackend
	_ = cleanup
	if got := gotBackend.(*backend).Spec(); got.Name != providerName {
		t.Fatalf("backend spec=%#v", got)
	}
	if _, err := (Provider{}).ConfigureDoctor(Config{TargetOS: targetLinux}, newDiscardRuntime()); err != nil {
		t.Fatalf("ConfigureDoctor: %v", err)
	}
}

func TestBackendTimingAndClaimTargetHelpers(t *testing.T) {
	b := &backend{pollInterval: 25 * time.Millisecond}
	if got := b.effectivePollInterval(); got != 25*time.Millisecond {
		t.Fatalf("poll interval=%s", got)
	}
	b.pollInterval = 0
	if got := b.effectivePollInterval(); got != falPollInterval {
		t.Fatalf("default poll interval=%s", got)
	}

	t.Setenv("XDG_STATE_HOME", t.TempDir())
	claim := core.LeaseClaim{
		LeaseID:  "falbx_helper",
		Slug:     "helper",
		Provider: providerName,
		CloudID:  "inst_helper",
		SSHHost:  "192.0.2.10",
		SSHPort:  2222,
		Labels: map[string]string{
			"name":        "helper",
			"state":       "ready",
			"server_type": string(InstanceTypeH100x1),
		},
	}
	target, err := leaseTargetFromClaim(claim, Config{SSHUser: "root"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if target.LeaseID != claim.LeaseID || target.Server.CloudID != claim.CloudID || target.SSH.Host != claim.SSHHost || target.SSH.Port != "2222" {
		t.Fatalf("target=%#v", target)
	}
	claim.Provider = "runpod"
	if _, err := leaseTargetFromClaim(claim, Config{}, false); err == nil {
		t.Fatal("expected provider mismatch")
	}
}

func TestDoctorReportsMissingAuthWithoutTokenNames(t *testing.T) {
	gotBackend, err := Provider{}.Configure(Config{TargetOS: targetLinux}, newDiscardRuntime())
	if err != nil {
		t.Fatal(err)
	}
	_, err = gotBackend.(*backend).Doctor(t.Context(), DoctorRequest{})
	if err == nil {
		t.Fatal("expected missing auth error")
	}
	message := err.Error()
	if !strings.Contains(message, "requires fal credentials in environment") {
		t.Fatalf("missing auth error=%q", message)
	}
	for _, forbidden := range []string{"FAL_KEY", "CRABBOX_FAL_KEY", "secret"} {
		if strings.Contains(message, forbidden) {
			t.Fatalf("missing auth error leaked %q: %q", forbidden, message)
		}
	}
}

func TestDoctorReadyMessageDoesNotClaimLimitedInventoryCount(t *testing.T) {
	gotBackend, err := Provider{}.Configure(Config{
		TargetOS: targetLinux,
		Fal:      FalConfig{APIKey: "test-key", APIURL: "https://api.example.test/v1"},
	}, newDiscardRuntime())
	if err != nil {
		t.Fatal(err)
	}
	backend := gotBackend.(*backend)
	backend.clientFactory = func(Config, Runtime) (computeAPI, error) {
		return stubComputeAPI{list: ListInstancesResponse{
			HasMore:   true,
			Instances: []ComputeInstance{{ID: "inst_1"}},
		}}, nil
	}
	result, err := backend.Doctor(t.Context(), DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Message, "leases=") {
		t.Fatalf("doctor message claimed partial inventory count: %q", result.Message)
	}
	if !strings.Contains(result.Message, "auth=ready") || !strings.Contains(result.Message, "api=list") {
		t.Fatalf("doctor message missing readiness details: %q", result.Message)
	}
}

type stubComputeAPI struct {
	list ListInstancesResponse
}

func (s stubComputeAPI) ListInstances(_ context.Context, _ int, _ string) (ListInstancesResponse, error) {
	return s.list, nil
}

func (stubComputeAPI) GetInstance(context.Context, string) (ComputeInstance, error) {
	return ComputeInstance{}, nil
}

func (stubComputeAPI) CreateInstance(context.Context, CreateInstanceRequest, string) (ComputeInstance, error) {
	return ComputeInstance{}, nil
}

func (stubComputeAPI) DeleteInstance(context.Context, string) error {
	return nil
}
