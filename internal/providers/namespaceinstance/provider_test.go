package namespaceinstance

import (
	"flag"
	"io"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderSpecAndAliases(t *testing.T) {
	provider := Provider{}
	if provider.Name() != providerName {
		t.Fatalf("Name=%q", provider.Name())
	}
	if aliases := provider.Aliases(); len(aliases) != 1 || aliases[0] != providerAlias {
		t.Fatalf("Aliases=%v", aliases)
	}
	spec := provider.Spec()
	if spec.Name != providerName || spec.Family != "namespace" || spec.Kind != core.ProviderKindSSHLease || spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("spec=%#v", spec)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("targets=%#v", spec.Targets)
	}
	for _, feature := range []core.Feature{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup} {
		if !spec.Features.Has(feature) {
			t.Fatalf("spec missing feature %s: %#v", feature, spec.Features)
		}
	}
}

func TestProviderForResolvesCanonicalAndComputeAlias(t *testing.T) {
	for _, name := range []string{providerName, providerAlias} {
		provider, err := core.ProviderFor(name)
		if err != nil {
			t.Fatalf("ProviderFor(%q): %v", name, err)
		}
		if provider.Name() != providerName {
			t.Fatalf("ProviderFor(%q).Name=%q", name, provider.Name())
		}
	}
}

func TestServerTypeMapping(t *testing.T) {
	provider := Provider{}
	if got := provider.ServerTypeForClass("standard"); got != defaultMachineType {
		t.Fatalf("ServerTypeForClass=%q", got)
	}
	if got := provider.ServerTypeForConfig(core.Config{ServerType: "ns/custom", ServerTypeExplicit: true}); got != "ns/custom" {
		t.Fatalf("explicit ServerTypeForConfig=%q", got)
	}
	if got := provider.ServerTypeForConfig(core.Config{NamespaceInstance: core.NamespaceInstanceConfig{MachineType: "linux-large"}}); got != "linux-large" {
		t.Fatalf("machine type ServerTypeForConfig=%q", got)
	}
	if got := provider.ServerTypeForConfig(core.Config{ServerType: "cpx51"}); got != defaultMachineType {
		t.Fatalf("implicit cross-provider ServerTypeForConfig=%q", got)
	}
}

func TestApplyFlagsSetsNamespaceInstanceConfig(t *testing.T) {
	defaults := core.Config{NamespaceInstance: core.NamespaceInstanceConfig{Ephemeral: true, WorkRoot: defaultWorkRoot}}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	values := Provider{}.RegisterFlags(fs, defaults)
	if fs.Lookup("namespace-instance-token") != nil {
		t.Fatal("namespace-instance token must not be registered as an argv flag")
	}
	args := []string{
		"--namespace-instance-machine-type", "linux-large",
		"--namespace-instance-duration", "2h",
		"--namespace-instance-ephemeral=false",
		"--namespace-instance-region", "us-west",
		"--namespace-instance-endpoint", "https://namespace.example.test",
		"--namespace-instance-keychain", "test-keychain",
		"--namespace-instance-work-root", "/work/crabbox-test",
		"--namespace-instance-volume", "cache:/cache,tmp:/tmp/cache",
	}
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	cfg := core.Config{TTL: 90 * time.Minute}
	if err := (Provider{}).ApplyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.NamespaceInstance.MachineType != "linux-large" || cfg.ServerType != "linux-large" || !cfg.ServerTypeExplicit {
		t.Fatalf("machine/server type not applied: cfg=%#v", cfg.NamespaceInstance)
	}
	if cfg.NamespaceInstance.Duration != 2*time.Hour || cfg.NamespaceInstance.Ephemeral || cfg.NamespaceInstance.Region != "us-west" || cfg.NamespaceInstance.Endpoint != "https://namespace.example.test" || cfg.NamespaceInstance.Keychain != "test-keychain" || cfg.NamespaceInstance.WorkRoot != "/work/crabbox-test" || cfg.WorkRoot != "/work/crabbox-test" {
		t.Fatalf("flags not applied: %#v", cfg.NamespaceInstance)
	}
	if len(cfg.NamespaceInstance.Volumes) != 2 || cfg.NamespaceInstance.Volumes[1] != "tmp:/tmp/cache" {
		t.Fatalf("volumes=%#v", cfg.NamespaceInstance.Volumes)
	}
}

func TestValidateRejectsUnsupportedTargetAndBroadWorkRoot(t *testing.T) {
	cfg := core.Config{TargetOS: core.TargetWindows, NamespaceInstance: core.NamespaceInstanceConfig{WorkRoot: defaultWorkRoot}}
	err := validateNamespaceInstanceConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "target=windows") {
		t.Fatalf("target err=%v", err)
	}
	cfg = core.Config{TargetOS: core.TargetLinux, NamespaceInstance: core.NamespaceInstanceConfig{WorkRoot: "/work"}}
	err = validateNamespaceInstanceConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "too broad") {
		t.Fatalf("work root err=%v", err)
	}
}

func TestConfigureReturnsDoctorAndSSHLeaseScaffold(t *testing.T) {
	gotBackend, err := Provider{}.Configure(core.Config{}, core.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := gotBackend.(core.SSHLeaseBackend); !ok {
		t.Fatalf("backend=%T, want SSHLeaseBackend scaffold", gotBackend)
	}
	doctor, err := Provider{}.ConfigureDoctor(core.Config{}, core.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := doctor.(*backend); !ok {
		t.Fatalf("doctor=%T", doctor)
	}
}
