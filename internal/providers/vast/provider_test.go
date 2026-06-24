package vast

import (
	"flag"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderSpecAndAliases(t *testing.T) {
	p := Provider{}
	if p.Name() != "vast" {
		t.Fatalf("Name=%q", p.Name())
	}
	aliases := p.Aliases()
	if len(aliases) != 2 || aliases[0] != "vast-ai" || aliases[1] != "vastai" {
		t.Fatalf("aliases=%v", aliases)
	}
	spec := p.Spec()
	if spec.Name != "vast" || spec.Family != "vast" || spec.Kind != core.ProviderKindSSHLease || spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("spec=%#v", spec)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("targets=%#v", spec.Targets)
	}
	for _, feature := range []core.Feature{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup} {
		if !spec.Features.Has(feature) {
			t.Fatalf("features=%v missing %s", spec.Features, feature)
		}
	}
}

func TestProviderFlagsApplyNonSecretConfig(t *testing.T) {
	cfg := core.Config{
		Provider: "vast",
		Vast: core.VastConfig{
			APIURL:        "https://console.vast.ai/api/v0",
			InstanceType:  "ondemand",
			Runtype:       "ssh_direct",
			Image:         "nvidia/cuda:default",
			DiskGB:        20,
			User:          "root",
			WorkRoot:      "/work",
			ReleaseAction: "destroy",
		},
	}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := RegisterVastProviderFlags(fs, cfg)
	if err := fs.Parse([]string{
		"--vast-api-url", "https://approved.example.test/api/v0",
		"--vast-instance-type", "on-demand",
		"--vast-gpu-name", "H100",
		"--vast-gpu-count", "4",
		"--vast-image", "nvidia/cuda:12",
		"--vast-template-id", "tpl-123",
		"--vast-runtype", "ssh_direct",
		"--vast-disk-gb", "80",
		"--vast-max-dph-total", "4.5",
		"--vast-min-reliability", "0.95",
		"--vast-order", "reliability desc",
		"--vast-user", "ubuntu",
		"--vast-work-root", "/work/vast",
		"--vast-release-action", "keep",
	}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyVastProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Vast.APIURL != "https://approved.example.test/api/v0" ||
		cfg.Vast.InstanceType != "ondemand" ||
		cfg.Vast.GPUName != "H100" ||
		cfg.Vast.GPUCount != 4 ||
		cfg.Vast.Image != "nvidia/cuda:12" ||
		cfg.Vast.TemplateID != "tpl-123" ||
		cfg.Vast.Runtype != "ssh_direct" ||
		cfg.Vast.DiskGB != 80 ||
		cfg.Vast.MaxDphTotal != 4.5 ||
		cfg.Vast.MinReliability != 0.95 ||
		cfg.Vast.Order != "reliability desc" ||
		cfg.Vast.User != "ubuntu" ||
		cfg.Vast.WorkRoot != "/work/vast" ||
		cfg.Vast.ReleaseAction != "keep" {
		t.Fatalf("vast config=%#v", cfg.Vast)
	}
}

func TestProviderFlagsDoNotExposeAPIKey(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	RegisterVastProviderFlags(fs, core.Config{})
	fs.VisitAll(func(f *flag.Flag) {
		if strings.Contains(f.Name, "api-key") {
			t.Fatalf("unexpected secret flag --%s", f.Name)
		}
	})
	forbidden := "vast-" + "api-key"
	if fs.Lookup(forbidden) != nil {
		t.Fatalf("unexpected --%s flag", forbidden)
	}
}

func TestValidateConfigRejectsUnsafeValues(t *testing.T) {
	base := core.Config{Vast: core.VastConfig{
		APIURL:        "https://console.vast.ai/api/v0",
		InstanceType:  "ondemand",
		Runtype:       "ssh_direct",
		DiskGB:        20,
		ReleaseAction: "destroy",
	}}
	tests := []struct {
		name   string
		mutate func(*core.Config)
		want   string
	}{
		{
			name: "credential url",
			mutate: func(cfg *core.Config) {
				cfg.Vast.APIURL = "https://user:secret@vast.example.test"
			},
			want: "absolute URL without credentials",
		},
		{
			name: "instance type",
			mutate: func(cfg *core.Config) {
				cfg.Vast.InstanceType = "spot"
			},
			want: "vast.instanceType",
		},
		{
			name: "runtype",
			mutate: func(cfg *core.Config) {
				cfg.Vast.Runtype = "ssh_proxy"
			},
			want: "vast.runtype",
		},
		{
			name: "disk",
			mutate: func(cfg *core.Config) {
				cfg.Vast.DiskGB = -1
			},
			want: "vast.diskGB",
		},
		{
			name: "reliability",
			mutate: func(cfg *core.Config) {
				cfg.Vast.MinReliability = 1.1
			},
			want: "vast.minReliability",
		},
		{
			name: "release",
			mutate: func(cfg *core.Config) {
				cfg.Vast.ReleaseAction = "hibernate"
			},
			want: "vast.releaseAction",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := base
			test.mutate(&cfg)
			err := (Provider{}).ValidateConfig(cfg)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err=%v want %q", err, test.want)
			}
		})
	}
	if err := (Provider{}).ValidateConfig(base); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	base.Vast.InstanceType = "on-demand"
	if err := (Provider{}).ValidateConfig(base); err != nil {
		t.Fatalf("on-demand alias rejected: %v", err)
	}
}
