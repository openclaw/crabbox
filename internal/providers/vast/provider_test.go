package vast

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
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
			name: "query url",
			mutate: func(cfg *core.Config) {
				cfg.Vast.APIURL = "https://vast.example.test/api/v0?token=secret"
			},
			want: "query strings or fragments",
		},
		{
			name: "fragment url",
			mutate: func(cfg *core.Config) {
				cfg.Vast.APIURL = "https://vast.example.test/api/v0#secret"
			},
			want: "query strings or fragments",
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

func TestConfigureRejectsTailscaleBeforeBackend(t *testing.T) {
	base := core.Config{
		TargetOS: core.TargetLinux,
		Vast: core.VastConfig{
			APIURL:        "https://console.vast.ai/api/v0",
			InstanceType:  "ondemand",
			Runtype:       "ssh_direct",
			DiskGB:        20,
			ReleaseAction: "destroy",
		},
	}
	tests := []struct {
		name   string
		mutate func(*core.Config)
	}{
		{
			name: "enabled",
			mutate: func(cfg *core.Config) {
				cfg.Tailscale.Enabled = true
			},
		},
		{
			name: "network",
			mutate: func(cfg *core.Config) {
				cfg.Network = core.NetworkTailscale
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := base
			test.mutate(&cfg)
			backend, err := (Provider{}).Configure(cfg, core.Runtime{})
			if err == nil || backend != nil || !strings.Contains(err.Error(), "does not support Tailscale") {
				t.Fatalf("backend=%T err=%v, want Tailscale rejection", backend, err)
			}
		})
	}
}

func TestVastBindingFlagContract(t *testing.T) {
	for _, selector := range []string{"vast", " VAST-AI ", "vastai", "aws"} {
		cfg := core.BaseConfig()
		cfg.Provider = selector
		cfg.Vast.InstanceType = " On_Demand "
		p := Provider{}
		fs := flag.NewFlagSet("contract", flag.ContinueOnError)
		values := p.RegisterFlags(fs, cfg)
		count := 0
		fs.VisitAll(func(*flag.Flag) { count++ })
		if count != 14 || fs.Lookup("vast-api-key") != nil {
			t.Fatal("flag surface changed")
		}
		if err := p.ApplyFlags(&cfg, fs, values); err != nil {
			t.Fatal(err)
		}
		if cfg.Vast.InstanceType != " On_Demand " || core.IsVastWorkRootExplicit(&cfg) || core.DeleteOnReleaseExplicit(cfg, "vast") {
			t.Fatal("unvisited values normalized/marked")
		}
		args := []string{"--vast-api-url=https://console.vast.ai/api/v0", "--vast-instance-type= On_Demand ", "--vast-gpu-name=fixture-gpu", "--vast-gpu-count=2", "--vast-image=fixture-image", "--vast-template-id=fixture-template", "--vast-runtype=ssh_direct", "--vast-disk-gb=37", "--vast-max-dph-total=0.25", "--vast-min-reliability=0.75", "--vast-order=reliability desc", "--vast-user=alice", "--vast-work-root=/work/fixture", "--vast-release-action=keep"}
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		before := fmt.Sprintf("%#v", cfg)
		if err := p.ApplyFlags(&cfg, fs, struct{}{}); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprintf("%#v", cfg) != before {
			t.Fatal("wrong values type changed config")
		}
		if err := p.ApplyFlags(&cfg, fs, values); err != nil {
			t.Fatal(err)
		}
		want := core.VastConfig{APIURL: "https://console.vast.ai/api/v0", InstanceType: "ondemand", GPUName: "fixture-gpu", GPUCount: 2, Image: "fixture-image", TemplateID: "fixture-template", Runtype: "ssh_direct", DiskGB: 37, MaxDphTotal: .25, MinReliability: .75, Order: "reliability desc", User: "alice", WorkRoot: "/work/fixture", ReleaseAction: "keep"}
		if cfg.Vast != want || !core.IsVastWorkRootExplicit(&cfg) || !core.DeleteOnReleaseExplicit(cfg, "vast") {
			t.Fatalf("selector=%q flags=%#v want=%#v", selector, cfg.Vast, want)
		}
	}
	for _, value := range []string{"same", "empty"} {
		cfg := core.BaseConfig()
		cfg.Provider = "vast"
		p := Provider{}
		fs := flag.NewFlagSet("contract", flag.ContinueOnError)
		values := p.RegisterFlags(fs, cfg)
		root, action := "/work/crabbox", "destroy"
		if value == "empty" {
			root = ""
			action = ""
		}
		if err := fs.Parse([]string{"--vast-work-root=" + root, "--vast-release-action=" + action, "--vast-image=", "--vast-disk-gb=0"}); err != nil {
			t.Fatal(err)
		}
		if err := p.ApplyFlags(&cfg, fs, values); err != nil {
			t.Fatal(err)
		}
		if cfg.Vast.Image != "" || cfg.Vast.DiskGB != 0 || cfg.Vast.WorkRoot != root || cfg.Vast.ReleaseAction != action || !core.IsVastWorkRootExplicit(&cfg) || !core.DeleteOnReleaseExplicit(cfg, "vast") {
			t.Fatal("empty/same-value flag acceptance changed")
		}
		route := p.CommandRouting(cfg, core.CommandRoutingRequest{}).Args
		found := false
		for i, arg := range route {
			if arg == "--vast-release-action" && i+1 < len(route) && route[i+1] == action {
				found = true
			}
		}
		if !found {
			t.Fatal("explicit release routing missing")
		}
	}
}

func TestVastBindingGuardAndValidationOrder(t *testing.T) {
	for _, args := range [][]string{{"--type=fixture", "--class="}, {"--type="}} {
		cfg := core.BaseConfig()
		cfg.Provider = " VASTAI "
		p := Provider{}
		fs := flag.NewFlagSet("contract", flag.ContinueOnError)
		fs.String("class", "", "")
		fs.String("type", "", "")
		p.RegisterFlags(fs, cfg)
		if err := fs.Parse(append(args, "--vast-work-root=/work/fixture")); err != nil {
			t.Fatal(err)
		}
		before := fmt.Sprintf("%#v", cfg)
		err := p.ApplyFlags(&cfg, fs, struct{}{})
		want := "--type is not supported for provider=vast; use --vast-image"
		if len(args) == 2 {
			want = "--class is not supported for provider=vast; use --vast-gpu-name or --vast-gpu-count"
		}
		var exitErr core.ExitError
		if err == nil || err.Error() != want || !errors.As(err, &exitErr) || exitErr.Code != 2 || fmt.Sprintf("%#v", cfg) != before {
			t.Fatalf("sizing phase err=%v want=%q", err, want)
		}
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--vast-api-url=", "--vast-instance-type=unknown"}, "vast.apiUrl is required"},
		{[]string{"--vast-instance-type=unknown", "--vast-runtype=unknown"}, "vast.instanceType must be ondemand or interruptible"},
		{[]string{"--vast-instance-type= On_Demand ", "--vast-runtype=unknown", "--vast-gpu-count=-2"}, "vast.runtype must be ssh_direct"},
		{[]string{"--vast-gpu-count=-2", "--vast-disk-gb=-2"}, "vast.gpuCount must be non-negative"},
		{[]string{"--vast-disk-gb=-2", "--vast-max-dph-total=-2"}, "vast.diskGB must be non-negative"},
		{[]string{"--vast-max-dph-total=-2", "--vast-min-reliability=2"}, "vast.maxDphTotal must be non-negative"},
		{[]string{"--vast-min-reliability=2", "--vast-release-action=unknown"}, "vast.minReliability must be between 0 and 1"},
		{[]string{"--vast-release-action=unknown"}, "vast.releaseAction must be destroy, delete, stop, or keep"},
	} {
		for _, selected := range []bool{false, true} {
			cfg := core.BaseConfig()
			cfg.Provider = "aws"
			if selected {
				cfg.Provider = "vast-ai"
			}
			p := Provider{}
			fs := flag.NewFlagSet("contract", flag.ContinueOnError)
			values := p.RegisterFlags(fs, cfg)
			if err := fs.Parse(append(tc.args, "--vast-work-root=/work/fixture")); err != nil {
				t.Fatal(err)
			}
			err := p.ApplyFlags(&cfg, fs, values)
			if selected {
				var exitErr core.ExitError
				if err == nil || err.Error() != tc.want || !errors.As(err, &exitErr) || exitErr.Code != 2 {
					t.Fatalf("validation err=%v want=%q", err, tc.want)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if !core.IsVastWorkRootExplicit(&cfg) || cfg.Vast.WorkRoot != "/work/fixture" {
				t.Fatal("validation moved before copy/marker")
			}
		}
	}
}

func TestVastBindingBackendDefaultsAndPayload(t *testing.T) {
	for _, explicitUser := range []bool{false, true} {
		cfg := core.Config{SSHUser: "generic-user", WorkRoot: "/generic/root", Vast: core.VastConfig{User: "provider-user", WorkRoot: "/provider/root"}}
		if explicitUser {
			core.MarkSSHUserExplicit(&cfg)
		}
		applyVastDefaults(&cfg)
		wantUser := "provider-user"
		if explicitUser {
			wantUser = "generic-user"
		}
		if cfg.SSHUser != wantUser || cfg.WorkRoot != "/provider/root" || cfg.Vast.APIURL != "" || cfg.Vast.Image != "" || cfg.Vast.DiskGB != 0 || cfg.Vast.InstanceType != "ondemand" || cfg.Vast.Runtype != "ssh_direct" || cfg.Vast.Order != "dlperf_per_dphtotal desc" || cfg.Vast.ReleaseAction != "destroy" {
			t.Fatal("backend fallback/projection phase changed")
		}
	}
	cfg := core.Config{}
	applyVastDefaults(&cfg)
	if cfg.SSHUser != "root" || cfg.WorkRoot != "/work/crabbox" {
		t.Fatal("backend raw-empty user/root fallback changed")
	}
	for _, tc := range []struct{ instance, want string }{{" On_Demand ", "ondemand"}, {"interruptible", "bid"}} {
		cfg := core.BaseConfig()
		cfg.Provider = "vast"
		cfg.Vast.APIKey = "inert-configured-key"
		cfg.Vast.APIURL = "https://console.vast.ai/api/v0/"
		cfg.Vast.InstanceType = tc.instance
		cfg.Vast.Image = ""
		cfg.Vast.DiskGB = 0
		configured, err := (Provider{}).Configure(cfg, core.Runtime{})
		if err != nil {
			t.Fatal(err)
		}
		b := configured.(*backend)
		if b.cfg.Vast.Image != "" || b.cfg.Vast.DiskGB != 0 || b.cfg.Vast.InstanceType != tc.instance {
			t.Fatal("validated backend changed post-flag values")
		}
		api, err := b.api()
		if err != nil {
			t.Fatal(err)
		}
		if api.(*vastClient).apiURL != "https://console.vast.ai/api/v0" {
			t.Fatal("validated constructor endpoint changed")
		}
		search := buildVastOfferSearchPayload(b.cfg.Vast)
		data, err := json.Marshal(search)
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded["type"] != tc.want {
			t.Fatalf("search type=%v want=%s", decoded["type"], tc.want)
		}
		create := buildVastCreatePayload(vastCreateInstanceInput{Config: b.cfg.Vast})
		if _, ok := create["image"]; ok {
			t.Fatal("empty image no longer omitted")
		}
		if _, ok := create["disk"]; ok {
			t.Fatal("zero disk no longer omitted")
		}
	}
}
