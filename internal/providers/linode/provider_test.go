package linode

import (
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
	if got := linodeRegionForConfig(cfg); got != defaultRegion {
		t.Fatalf("region=%q", got)
	}
	if got := linodeImageForConfig(cfg); got != defaultImage {
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
