package digitalocean

import (
	"flag"
	"reflect"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderSpec(t *testing.T) {
	spec := Provider{}.Spec()
	if spec.Name != providerName || spec.Family != providerName || spec.Kind != core.ProviderKindSSHLease || spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("spec=%#v", spec)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("targets=%v", spec.Targets)
	}
	for _, feature := range []core.Feature{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup, core.FeatureTailscale} {
		if !spec.Features.Has(feature) {
			t.Fatalf("features=%v missing %s", spec.Features, feature)
		}
	}
}

func TestProviderServerTypeDefaults(t *testing.T) {
	if got := (Provider{}).ServerTypeForConfig(core.Config{Class: "standard"}); got != "s-1vcpu-1gb" {
		t.Fatalf("ServerTypeForConfig standard=%q", got)
	}
	if got := (Provider{}).ServerTypeForConfig(core.Config{ServerType: "s-2vcpu-2gb", ServerTypeExplicit: true}); got != "s-2vcpu-2gb" {
		t.Fatalf("explicit ServerTypeForConfig=%q", got)
	}
	if got := (Provider{}).ServerTypeForConfig(core.Config{ServerType: "cpx51"}); got != "s-1vcpu-1gb" {
		t.Fatalf("implicit cross-provider ServerTypeForConfig=%q", got)
	}
}

func TestDigitalOceanBindingNoFlags(t *testing.T) {
	for _, name := range []string{"digitalocean", "other", ""} {
		cfg := core.Config{Provider: name, DigitalOcean: core.DigitalOceanConfig{Region: "prior", Image: "prior", VPCUUID: "prior", SSHCIDRs: []string{"prior"}}}
		before := cfg
		fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
		values := (Provider{}).RegisterFlags(fs, cfg)
		n := 0
		fs.VisitAll(func(*flag.Flag) { n++ })
		if n != 0 || !reflect.DeepEqual(values, core.NoProviderFlags()) {
			t.Fatalf("flags=%d values=%#v", n, values)
		}
		for _, v := range []any{values, nil, struct{}{}} {
			if err := (Provider{}).ApplyFlags(&cfg, fs, v); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg, before) {
				t.Fatal("no-op changed config")
			}
		}
	}
}

func TestDigitalOceanBindingRuntime(t *testing.T) {
	for _, tc := range []struct{ provider, generic, region, image string }{{"", "", "nyc3", "ubuntu-24-04-x64"}, {"", "generic", "generic", "generic"}, {"  ", "generic", "  ", "  "}, {"custom", "generic", "custom", "custom"}} {
		cfg := core.Config{Location: tc.generic, Image: tc.generic, SSHUser: "alice", SSHPort: "2222", WorkRoot: "/srv/project", DigitalOcean: core.DigitalOceanConfig{Region: tc.provider, Image: tc.provider}}
		if digitalOceanRegion(cfg) != tc.region || digitalOceanImage(cfg) != tc.image {
			t.Fatalf("lower=%q/%q", digitalOceanRegion(cfg), digitalOceanImage(cfg))
		}
		core.MarkSSHUserExplicit(&cfg)
		core.MarkSSHPortExplicit(&cfg)
		applyDigitalOceanDefaults(&cfg)
		r, i := tc.provider, tc.provider
		if r == "" {
			r = "nyc3"
			i = "ubuntu-24-04-x64"
		}
		if cfg.DigitalOcean.Region != r || cfg.DigitalOcean.Image != i || cfg.SSHUser != "alice" || cfg.SSHPort != "2222" || cfg.WorkRoot != "/srv/project" || cfg.ServerType != "s-1vcpu-1gb" || cfg.TargetOS != core.TargetLinux {
			t.Fatalf("runtime=%#v", cfg)
		}
	}
	cfg := core.Config{OSImage: "ubuntu:26.04"}
	core.SetOSImageExplicit(&cfg)
	b := NewDigitalOceanLeaseBackend((Provider{}).Spec(), cfg, core.Runtime{}).(*digitalOceanLeaseBackend)
	if b.acquireConfigErr == nil || !strings.Contains(b.acquireConfigErr.Error(), "does not support --os ubuntu:26.04") {
		t.Fatalf("pre-default capture=%v", b.acquireConfigErr)
	}
	if b.Cfg.DigitalOcean.Image != "ubuntu-24-04-x64" || b.clientFactory == nil || cfg.DigitalOcean.Image != "" {
		t.Fatal("lazy constructor/default state")
	}
	cfg.DigitalOcean.Image = "custom"
	b = NewDigitalOceanLeaseBackend((Provider{}).Spec(), cfg, core.Runtime{}).(*digitalOceanLeaseBackend)
	if b.acquireConfigErr != nil || b.Cfg.DigitalOcean.Image != "custom" {
		t.Fatalf("custom image=%q err=%v", b.Cfg.DigitalOcean.Image, b.acquireConfigErr)
	}
}
