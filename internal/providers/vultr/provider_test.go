package vultr

import (
	"context"
	"flag"
	"io"
	"reflect"
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
	for _, class := range []string{"tiny", "small", "standard", "fast", "large", "beast", "unknown"} {
		if got := provider.ServerTypeForConfig(core.Config{Class: class}); got != "vc2-1c-1gb" {
			t.Fatalf("ServerTypeForConfig(%q)=%q", class, got)
		}
	}
}

func TestConfigureReturnsLeaseAndDoctorBackend(t *testing.T) {
	backend, err := Provider{}.Configure(core.Config{}, core.Runtime{Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	leaseBackend, ok := backend.(core.SSHLeaseBackend)
	if !ok {
		t.Fatalf("backend=%T, want SSHLeaseBackend", backend)
	}
	cleanup, ok := backend.(core.CleanupBackend)
	if !ok {
		t.Fatalf("backend=%T, want CleanupBackend", backend)
	}
	if _, err := leaseBackend.Acquire(context.Background(), core.AcquireRequest{}); err == nil || !strings.Contains(err.Error(), "VULTR_API_KEY is required") {
		t.Fatalf("Acquire err=%v", err)
	}
	if err := cleanup.Cleanup(context.Background(), core.CleanupRequest{}); err == nil || !strings.Contains(err.Error(), "VULTR_API_KEY is required") {
		t.Fatalf("Cleanup err=%v", err)
	}
	doctor, ok := backend.(core.DoctorBackend)
	if !ok {
		t.Fatalf("backend=%T, want DoctorBackend", backend)
	}
	result, err := doctor.Doctor(context.Background(), core.DoctorRequest{})
	if err == nil || !strings.Contains(err.Error(), "VULTR_API_KEY is required") {
		t.Fatalf("doctor result=%#v err=%v", result, err)
	}
}

func TestVultrBindingNoFlags(t *testing.T) {
	for _, name := range []string{"vultr", "other", ""} {
		cfg := core.Config{Provider: name, Vultr: core.VultrConfig{Region: "prior", UserScheme: "prior", VPCIDs: []string{"prior"}}}
		before := cfg
		fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
		v := (Provider{}).RegisterFlags(fs, cfg)
		n := 0
		fs.VisitAll(func(*flag.Flag) { n++ })
		if n != 0 || !reflect.DeepEqual(v, core.NoProviderFlags()) {
			t.Fatalf("flags=%d values=%#v", n, v)
		}
		for _, values := range []any{v, nil, struct{}{}} {
			if err := (Provider{}).ApplyFlags(&cfg, fs, values); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg, before) {
				t.Fatal("no-op changed cfg")
			}
		}
	}
}

func TestVultrBindingRuntime(t *testing.T) {
	for _, tc := range []struct{ raw, generic, region, user string }{{"", "", "ewr", "root"}, {"", "generic", "generic", "root"}, {"  ", "generic", "  ", "  "}, {"custom", "generic", "custom", "custom"}} {
		cfg := core.Config{Location: tc.generic, Vultr: core.VultrConfig{Region: tc.raw, UserScheme: tc.raw}}
		if vultrRegion(cfg) != tc.region || cfg.Vultr.WithRuntimeDefaults().UserScheme != tc.user {
			t.Fatal("lower fallback mismatch")
		}
		applyVultrDefaults(&cfg)
		r, u := tc.raw, tc.raw
		if r == "" {
			r = "ewr"
			u = "root"
		}
		if cfg.Vultr.Region != r || cfg.Vultr.UserScheme != u || cfg.SSHUser != "root" || cfg.SSHPort != "22" || cfg.WorkRoot != "/work/crabbox" || cfg.ServerType != "vc2-1c-1gb" || cfg.TargetOS != core.TargetLinux || cfg.Vultr.OS != "" || cfg.Vultr.Image != "" || cfg.Vultr.Snapshot != "" {
			t.Fatalf("defaults=%#v ssh=%q/%q", cfg.Vultr, cfg.SSHUser, cfg.SSHPort)
		}
	}
	for _, tc := range []struct{ scheme, user string }{{"limited", "limited"}, {"LIMITED", "limited"}, {" limited ", "root"}, {"root", "root"}} {
		cfg := core.Config{Vultr: core.VultrConfig{UserScheme: tc.scheme}}
		applyVultrDefaults(&cfg)
		if cfg.SSHUser != tc.user || cfg.Vultr.UserScheme != tc.scheme {
			t.Fatalf("scheme=%q ssh=%q", tc.scheme, cfg.SSHUser)
		}
		cfg = core.Config{SSHUser: "alice", SSHPort: "2200", WorkRoot: "/srv/project", ServerType: "custom-type", Vultr: core.VultrConfig{UserScheme: tc.scheme}}
		core.MarkSSHUserExplicit(&cfg)
		core.MarkSSHPortExplicit(&cfg)
		applyVultrDefaults(&cfg)
		if cfg.SSHUser != "alice" || cfg.SSHPort != "2200" || cfg.WorkRoot != "/srv/project" || cfg.ServerType != "custom-type" {
			t.Fatal("explicit generic fields changed")
		}
	}
}

func TestVultrRuntimeTransformProvider(t *testing.T) {
	for _, region := range []string{"", "custom-region", "  "} {
		for _, scheme := range []string{"", "custom-scheme", "  ", "limited", "LIMITED", " limited "} {
			for _, lists := range []string{"nil", "empty", "shared"} {
				for _, explicit := range []bool{false, true} {
					cfg := core.Config{Provider: "vultr", Location: "generic-region", Class: "standard", Vultr: core.VultrConfig{Region: region, UserScheme: scheme, OS: "raw-os", Image: "raw-image", Snapshot: "raw-snapshot", FirewallGroup: "raw-group"}}
					switch lists {
					case "empty":
						cfg.Vultr.VPCIDs = []string{}
						cfg.Vultr.SSHCIDRs = []string{}
					case "shared":
						cfg.Vultr.VPCIDs = []string{"vpc-a", "vpc-a"}
						cfg.Vultr.SSHCIDRs = []string{" 192.0.2.0/24 ", ""}
					}
					before := cfg.Vultr
					want := before
					if region == "" {
						want.Region = "ewr"
					}
					if scheme == "" {
						want.UserScheme = "root"
					}
					if got := cfg.Vultr.WithRuntimeDefaults().UserScheme; got != want.UserScheme {
						t.Fatalf("getter=%q want=%q", got, want.UserScheme)
					}
					if !reflect.DeepEqual(cfg.Vultr, before) {
						t.Fatal("read-only getter changed config")
					}
					lowerRegion := region
					if lowerRegion == "" {
						lowerRegion = "generic-region"
					}
					if got := vultrRegion(cfg); got != lowerRegion {
						t.Fatalf("lower region=%q want=%q", got, lowerRegion)
					}
					user, port, root := "root", "22", "/work/crabbox"
					if scheme == "limited" || scheme == "LIMITED" {
						user = "limited"
					}
					if explicit {
						user, port, root = "alice", "2200", "/srv/project"
						cfg.SSHUser = user
						cfg.SSHPort = port
						cfg.WorkRoot = root
						core.MarkSSHUserExplicit(&cfg)
						core.MarkSSHPortExplicit(&cfg)
						core.MarkWorkRootExplicit(&cfg)
					}
					applyVultrDefaults(&cfg)
					if !reflect.DeepEqual(cfg.Vultr, want) {
						t.Fatalf("region=%q scheme=%q lists=%s got=%#v want=%#v", region, scheme, lists, cfg.Vultr, want)
					}
					if reflect.ValueOf(cfg.Vultr.VPCIDs).Pointer() != reflect.ValueOf(before.VPCIDs).Pointer() || reflect.ValueOf(cfg.Vultr.SSHCIDRs).Pointer() != reflect.ValueOf(before.SSHCIDRs).Pointer() {
						t.Fatal("backend changed slice backing")
					}
					if cfg.SSHUser != user || cfg.SSHPort != port || cfg.WorkRoot != root || cfg.Class != "standard" || cfg.Location != "generic-region" || cfg.ServerType != "vc2-1c-1gb" || cfg.TargetOS != core.TargetLinux || cfg.SSHFallbackPorts != nil {
						t.Fatalf("generic effects user=%q port=%q root=%q class=%q location=%q type=%q", cfg.SSHUser, cfg.SSHPort, cfg.WorkRoot, cfg.Class, cfg.Location, cfg.ServerType)
					}
				}
			}
		}
	}
	if got := vultrRegion(core.Config{}); got != "ewr" {
		t.Fatalf("lower raw fallback=%q", got)
	}
}
