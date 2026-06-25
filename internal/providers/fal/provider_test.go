package fal

import (
	"context"
	"flag"
	"io"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderSpecAndAlias(t *testing.T) {
	spec := Provider{}.Spec()
	if spec.Name != providerName || spec.Family != providerName || spec.Kind != core.ProviderKindServiceControl || spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("unexpected spec: %#v", spec)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("targets=%#v, want linux only", spec.Targets)
	}
	if len(spec.Features) != 0 {
		t.Fatalf("features=%v, want none until lifecycle backend is implemented", spec.Features)
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
	RegisterFalProviderFlags(fs, cfg)
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
	values := RegisterFalProviderFlags(fs, Config{})
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
	if err := ApplyFalProviderFlags(&cfg, fs, values); err != nil {
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
