package linode

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
	if got := (Provider{}).ServerTypeForClass("standard"); got != defaultType {
		t.Fatalf("ServerTypeForClass standard=%q", got)
	}
	if got := (Provider{}).ServerTypeForConfig(core.Config{ServerType: "g6-standard-2", ServerTypeExplicit: true}); got != "g6-standard-2" {
		t.Fatalf("explicit ServerTypeForConfig=%q", got)
	}
	if got := (Provider{}).ServerTypeForConfig(core.Config{Linode: core.LinodeConfig{Type: "g6-nanode-1"}}); got != "g6-nanode-1" {
		t.Fatalf("linode Type ServerTypeForConfig=%q", got)
	}
	if got := (Provider{}).ServerTypeForConfig(core.Config{ServerType: "cpx51"}); got != defaultType {
		t.Fatalf("implicit cross-provider ServerTypeForConfig=%q", got)
	}
}

func TestProviderForLinode(t *testing.T) {
	provider, err := core.ProviderFor(providerName)
	if err != nil {
		t.Fatal(err)
	}
	if provider.Name() != providerName {
		t.Fatalf("provider=%s", provider.Name())
	}
}

func TestConfigHelpers(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Linode = core.LinodeConfig{}
	if got := linodeRegionForConfig(cfg); got != "us-ord" {
		t.Fatalf("region=%q", got)
	}
	if got := linodeImageForConfig(cfg); got != "linode/ubuntu24.04" {
		t.Fatalf("image=%q", got)
	}
	if got := linodeServerTypeForConfig(cfg); got != defaultType {
		t.Fatalf("type=%q", got)
	}
	cfg.Linode.Region = "us-sea"
	cfg.Linode.Image = "private/123"
	cfg.Linode.Type = "g6-standard-2"
	if err := validateFoundationConfig(cfg); err != nil {
		t.Fatalf("validateFoundationConfig err=%v", err)
	}
}

func TestRequireTokenUsesLinodeTokenOnly(t *testing.T) {
	t.Setenv(tokenEnv, "")
	if _, err := requireToken(); err == nil {
		t.Fatal("requireToken succeeded without LINODE_TOKEN")
	}
	t.Setenv(tokenEnv, " secret ")
	got, err := requireToken()
	if err != nil {
		t.Fatal(err)
	}
	if got != "secret" {
		t.Fatalf("token=%q", got)
	}
}

func TestLinodeBindingNoFlags(t *testing.T) {
	for _, name := range []string{"linode", "other", ""} {
		cfg := core.Config{Provider: name, Linode: core.LinodeConfig{Region: "prior", Image: "prior", Type: "prior", SSHCIDRs: []string{"prior"}}}
		before := cfg
		fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
		v := (Provider{}).RegisterFlags(fs, cfg)
		n := 0
		fs.VisitAll(func(*flag.Flag) { n++ })
		if n != 0 || !reflect.DeepEqual(v, core.NoProviderFlags()) {
			t.Fatalf("flags=%d values=%#v", n, v)
		}
		for _, value := range []any{v, nil, struct{}{}} {
			if err := (Provider{}).ApplyFlags(&cfg, fs, value); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg, before) {
				t.Fatal("no-op changed cfg")
			}
		}
	}
}

func TestLinodeBindingRuntime(t *testing.T) {
	for _, tc := range []struct{ raw, region, image string }{{"", "us-ord", "linode/ubuntu24.04"}, {"  ", "us-ord", "linode/ubuntu24.04"}, {" custom ", "custom", "custom"}} {
		cfg := core.Config{Linode: core.LinodeConfig{Region: tc.raw, Image: tc.raw, Type: tc.raw}}
		if linodeRegionForConfig(cfg) != tc.region || linodeImageForConfig(cfg) != tc.image {
			t.Fatal("trim helper mismatch")
		}
		applyLinodeDefaults(&cfg)
		r, i, typ := tc.raw, tc.raw, tc.raw
		if tc.raw == "" {
			r = "us-ord"
			i = "linode/ubuntu24.04"
			typ = "g6-standard-1"
		}
		if cfg.Linode.Region != r || cfg.Linode.Image != i || cfg.Linode.Type != typ || cfg.SSHUser != "root" || cfg.SSHPort != "22" || cfg.ServerType != "g6-standard-1" || cfg.TargetOS != core.TargetLinux {
			t.Fatalf("defaults=%#v", cfg.Linode)
		}
	}
	for _, tc := range []struct {
		cfg  core.Config
		want string
	}{{core.Config{}, "g6-standard-1"}, {core.Config{Class: " STANDARD "}, "g6-standard-1"}, {core.Config{ServerType: "ignored", Linode: core.LinodeConfig{Type: " g6-nanode-1 "}}, "g6-nanode-1"}, {core.Config{ServerType: " g6-standard-2 ", ServerTypeExplicit: true, Linode: core.LinodeConfig{Type: "g6-nanode-1"}}, "g6-standard-2"}} {
		if got := linodeServerTypeForConfig(tc.cfg); got != tc.want {
			t.Fatalf("type=%q want=%q", got, tc.want)
		}
	}
	cfg := core.Config{SSHUser: "alice", SSHPort: "2222", WorkRoot: "/srv/project"}
	core.MarkSSHUserExplicit(&cfg)
	core.MarkSSHPortExplicit(&cfg)
	applyLinodeDefaults(&cfg)
	if cfg.SSHUser != "alice" || cfg.SSHPort != "2222" || cfg.WorkRoot != "/srv/project" {
		t.Fatal("explicit generic config changed")
	}
	cfg = core.Config{OSImage: "ubuntu:26.04"}
	core.SetOSImageExplicit(&cfg)
	b := newLinodeLeaseBackend((Provider{}).Spec(), cfg, core.Runtime{})
	if b.acquireConfigErr == nil || !strings.Contains(b.acquireConfigErr.Error(), "does not support os") {
		t.Fatalf("saved validation=%v", b.acquireConfigErr)
	}
	if b.Cfg.Linode.Image != "linode/ubuntu24.04" || b.clientFactory == nil || cfg.Linode.Image != "" {
		t.Fatal("validation-before-default/lazy constructor state")
	}
	cfg.Linode.Image = "custom"
	b = newLinodeLeaseBackend((Provider{}).Spec(), cfg, core.Runtime{})
	if b.acquireConfigErr != nil || b.Cfg.Linode.Image != "custom" {
		t.Fatalf("custom image err=%v", b.acquireConfigErr)
	}
}
