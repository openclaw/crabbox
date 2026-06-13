package vultr

import (
	"context"
	"io"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderSpec(t *testing.T) {
	provider := Provider{}
	if provider.Name() != providerName {
		t.Fatalf("Name=%q", provider.Name())
	}
	if aliases := provider.Aliases(); len(aliases) != 0 {
		t.Fatalf("Aliases=%v want none", aliases)
	}
	spec := provider.Spec()
	if spec.Name != providerName || spec.Family != providerName || spec.Kind != core.ProviderKindSSHLease || spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("unexpected spec: %#v", spec)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("targets=%#v", spec.Targets)
	}
	for _, feature := range []core.Feature{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup} {
		if !spec.Features.Has(feature) {
			t.Fatalf("spec missing feature %s: %#v", feature, spec.Features)
		}
	}
	if spec.Features.Has(core.FeatureTailscale) {
		t.Fatalf("vultr must not advertise Tailscale before lifecycle/user_data proof: %#v", spec.Features)
	}
}

func TestProviderForResolvesCanonicalOnly(t *testing.T) {
	provider, err := core.ProviderFor(providerName)
	if err != nil {
		t.Fatal(err)
	}
	if provider.Name() != providerName {
		t.Fatalf("provider=%s", provider.Name())
	}
	for _, alias := range []string{"vlt", "vultr-cloud"} {
		if got, err := core.ProviderFor(alias); err == nil && got.Name() == providerName {
			t.Fatalf("%q alias unexpectedly resolves to vultr", alias)
		}
	}
}

func TestProviderServerTypeDefaults(t *testing.T) {
	provider := Provider{}
	if got := provider.ServerTypeForConfig(core.Config{}); got != "vc2-1c-1gb" {
		t.Fatalf("ServerTypeForConfig=%q", got)
	}
	if got := provider.ServerTypeForConfig(core.Config{ServerType: "vc2-2c-2gb", ServerTypeExplicit: true}); got != "vc2-2c-2gb" {
		t.Fatalf("explicit ServerTypeForConfig=%q", got)
	}
	for _, class := range []string{"standard", "fast", "large", "beast", "unknown"} {
		if got := provider.ServerTypeForClass(class); got != "vc2-1c-1gb" {
			t.Fatalf("ServerTypeForClass(%q)=%q", class, got)
		}
	}
}

func TestConfigureReturnsDoctorAndClearLifecycleStubs(t *testing.T) {
	backend, err := Provider{}.Configure(core.Config{}, core.Runtime{Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	leaseBackend, ok := backend.(core.SSHLeaseBackend)
	if !ok {
		t.Fatalf("backend=%T, want SSHLeaseBackend skeleton", backend)
	}
	if _, err := leaseBackend.Acquire(context.Background(), core.AcquireRequest{}); err == nil || !strings.Contains(err.Error(), "provider=vultr acquire lifecycle is not implemented yet") {
		t.Fatalf("Acquire err=%v", err)
	}
	cleanup, ok := backend.(core.CleanupBackend)
	if !ok {
		t.Fatalf("backend=%T, want CleanupBackend skeleton", backend)
	}
	if err := cleanup.Cleanup(context.Background(), core.CleanupRequest{}); err == nil || !strings.Contains(err.Error(), "provider=vultr cleanup lifecycle is not implemented yet") {
		t.Fatalf("Cleanup err=%v", err)
	}
	doctor, ok := backend.(core.DoctorBackend)
	if !ok {
		t.Fatalf("backend=%T, want DoctorBackend", backend)
	}
	result, err := doctor.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Provider != providerName || result.Status != "missing" || !strings.Contains(result.Message, "lifecycle=not-implemented") || strings.Contains(result.Message, "VULTR_API_KEY") {
		t.Fatalf("doctor result=%#v", result)
	}
}
