package lambda

import (
	"flag"
	"reflect"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderSpecAndDefaults(t *testing.T) {
	spec := (Provider{}).Spec()
	if spec.Name != providerName || spec.Family != providerName || spec.Kind != core.ProviderKindSSHLease || spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("spec=%#v", spec)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("targets=%#v", spec.Targets)
	}
	for _, feature := range []core.Feature{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup, core.FeatureTailscale} {
		if !spec.Features.Has(feature) {
			t.Fatalf("features=%v missing %s", spec.Features, feature)
		}
	}

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	if got := (Provider{}).ServerTypeForConfig(cfg); got != defaultType {
		t.Fatalf("ServerTypeForConfig=%q want %q", got, defaultType)
	}
}

func TestValidateConfigRejectsAmbiguousImage(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Lambda.Image = "img-123"
	cfg.Lambda.ImageFamily = "lambda-stack-24-04"
	if err := (Provider{}).ValidateConfig(cfg); err == nil {
		t.Fatal("ValidateConfig succeeded for image plus imageFamily")
	}
}

func TestLambdaBindingRuntime(t *testing.T) {
	cfg := core.Config{Lambda: core.LambdaConfig{Region: "prior"}}
	before := cfg
	fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
	v := (Provider{}).RegisterFlags(fs, cfg)
	n := 0
	fs.VisitAll(func(*flag.Flag) { n++ })
	if n != 0 || !reflect.DeepEqual(v, core.NoProviderFlags()) {
		t.Fatal("provider flags added")
	}
	for _, value := range []any{v, nil, struct{}{}} {
		if err := (Provider{}).ApplyFlags(&cfg, fs, value); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(cfg, before) {
			t.Fatal("no-op changed config")
		}
	}
	for _, tc := range []struct{ raw, region, typ, family string }{{"", "us-west-1", "gpu_1x_a10", "lambda-stack-24-04"}, {"  ", "us-west-1", "gpu_1x_a10", "lambda-stack-24-04"}, {" custom ", "custom", "custom", "custom"}} {
		cfg := core.Config{Lambda: core.LambdaConfig{Region: tc.raw, Type: tc.raw, ImageFamily: tc.raw}}
		if regionForConfig(cfg) != tc.region || typeForConfig(cfg) != tc.typ || imageFamilyForConfig(cfg) != tc.family || imageForConfig(cfg) != "" {
			t.Fatal("trimmed helpers")
		}
		b := backend{cfg: cfg}
		b.initDirect()
		r, typ, fam := tc.raw, tc.raw, tc.raw
		if tc.raw == "" {
			r = "us-west-1"
			typ = "gpu_1x_a10"
			fam = "lambda-stack-24-04"
		}
		if b.cfg.Lambda.Region != r || b.cfg.Lambda.Type != typ || b.cfg.Lambda.ImageFamily != fam || b.cfg.SSHUser != "ubuntu" || b.cfg.SSHPort != "22" || b.cfg.TargetOS != core.TargetLinux {
			t.Fatalf("init=%#v", b.cfg.Lambda)
		}
	}
	cfg = core.Config{ServerType: " explicit ", ServerTypeExplicit: true, Lambda: core.LambdaConfig{Type: "configured", Image: " image ", ImageFamily: "family"}}
	if typeForConfig(cfg) != "explicit" || imageForConfig(cfg) != "image" || imageFamilyForConfig(cfg) != "" {
		t.Fatal("explicit type/image precedence")
	}
	for _, class := range []string{"standard", " FAST ", "custom"} {
		if serverTypeForClass(class) != "gpu_1x_a10" {
			t.Fatal("class default")
		}
	}
	b := backend{cfg: core.Config{SSHUser: "alice", SSHPort: "2200", WorkRoot: "/srv/project", Lambda: core.LambdaConfig{Image: "image"}}}
	b.initDirect()
	if b.cfg.SSHUser != "alice" || b.cfg.SSHPort != "2200" || b.cfg.WorkRoot != "/srv/project" || b.cfg.Lambda.ImageFamily != "" {
		t.Fatal("init preserved fields")
	}
	for _, raw := range []string{"", "  ", "custom", "us-west-1"} {
		for _, pair := range []struct{ image, family, wantFamily string }{{"", "", "lambda-stack-24-04"}, {"image", "", ""}, {"", "family", "family"}, {"image", "family", "family"}, {"  ", "  ", "  "}} {
			cfg := core.Config{Class: "standard", SSHUser: "alice", SSHPort: "2200", WorkRoot: "/srv/project", ServerType: "explicit-type", Lambda: core.LambdaConfig{Region: raw, Type: raw, Image: pair.image, ImageFamily: pair.family, FirewallRuleset: "rule", SSHCIDRs: []string{" raw ", ""}, FilesystemNames: []string{"data", "data"}, FilesystemMounts: []core.LambdaFilesystemMount{{Name: " data ", MountPath: " /mnt/data "}}}}
			cidrs, names, mounts := cfg.Lambda.SSHCIDRs, cfg.Lambda.FilesystemNames, cfg.Lambda.FilesystemMounts
			r, typ := raw, raw
			if raw == "" {
				r = "us-west-1"
				typ = "gpu_1x_a10"
			}
			want := core.LambdaConfig{Region: r, Type: typ, Image: pair.image, ImageFamily: pair.wantFamily, FirewallRuleset: "rule", SSHCIDRs: []string{" raw ", ""}, FilesystemNames: []string{"data", "data"}, FilesystemMounts: []core.LambdaFilesystemMount{{Name: " data ", MountPath: " /mnt/data "}}}
			b := backend{cfg: cfg}
			b.initDirect()
			if !reflect.DeepEqual(b.cfg.Lambda, want) || &b.cfg.Lambda.SSHCIDRs[0] != &cidrs[0] || &b.cfg.Lambda.FilesystemNames[0] != &names[0] || &b.cfg.Lambda.FilesystemMounts[0] != &mounts[0] {
				t.Fatalf("raw=%q pair=%#v cfg=%#v", raw, pair, b.cfg.Lambda)
			}
			if b.cfg.SSHUser != "alice" || b.cfg.SSHPort != "2200" || b.cfg.WorkRoot != "/srv/project" || b.cfg.ServerType != "explicit-type" || b.cfg.Class != "standard" || b.cfg.TargetOS != core.TargetLinux {
				t.Fatal("backend generic effects changed")
			}
		}
	}
	zero := backend{}
	zero.initDirect()
	if zero.cfg.WorkRoot != "" || zero.cfg.Lambda.SSHCIDRs != nil || zero.cfg.Lambda.FilesystemNames != nil || zero.cfg.Lambda.FilesystemMounts != nil || zero.cfg.ServerType != "gpu_1x_a10" {
		t.Fatal("raw init nil/generic state changed")
	}

}
