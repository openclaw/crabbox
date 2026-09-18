package unikraftcloud

import (
	"flag"
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestUnikraftCloudProviderSpec(t *testing.T) {
	spec := Provider{}.Spec()
	if spec.Name != providerName {
		t.Fatalf("spec.Name = %q, want %q", spec.Name, providerName)
	}
	if spec.Family != "unikraft-cloud" {
		t.Fatalf("spec.Family = %q, want unikraft-cloud", spec.Family)
	}
	if spec.Kind != core.ProviderKindServiceControl {
		t.Fatalf("spec.Kind = %q, want service-control", spec.Kind)
	}
	if spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("spec.Coordinator = %q, want never", spec.Coordinator)
	}
	if !spec.Features.Has(core.FeatureCleanup) {
		t.Fatalf("spec.Features = %#v, want cleanup", spec.Features)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("spec.Targets = %#v, want linux only", spec.Targets)
	}
	aliases := Provider{}.Spec().Aliases
	if len(aliases) != 2 || aliases[0] != "unikraftcloud" || aliases[1] != "ukc" {
		t.Fatalf("aliases = %#v, want [unikraftcloud ukc]", aliases)
	}
}

func TestUnikraftCloudAPIKeyFlagIsNotRegistered(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	registerUnikraftCloudProviderFlags(fs, core.Config{})
	for _, name := range []string{"unikraft-cloud-token", "unikraft-cloud-api-key", "unikraft-cloud-key", "ukc-token"} {
		if fs.Lookup(name) != nil {
			t.Fatalf("Unikraft Cloud API key surfaced as a flag --%s", name)
		}
	}
	for _, name := range []string{"unikraft-cloud-url", "unikraft-cloud-metro", "unikraft-cloud-image", "unikraft-cloud-memory"} {
		if fs.Lookup(name) == nil {
			t.Fatalf("--%s flag missing", name)
		}
	}
}

func TestApplyUnikraftCloudProviderFlags(t *testing.T) {
	for _, test := range []struct {
		name    string
		args    []string
		wantErr bool
		check   func(t *testing.T, cfg core.Config)
	}{
		{
			name: "overrides",
			args: []string{"-unikraft-cloud-metro", "dal", "-unikraft-cloud-image", "unikraft.org/nginx:latest", "-unikraft-cloud-memory", "256"},
			check: func(t *testing.T, cfg core.Config) {
				if cfg.UnikraftCloud.Metro != "dal" {
					t.Fatalf("metro = %q", cfg.UnikraftCloud.Metro)
				}
				if cfg.UnikraftCloud.Image != "unikraft.org/nginx:latest" {
					t.Fatalf("image = %q", cfg.UnikraftCloud.Image)
				}
				if cfg.UnikraftCloud.MemoryMB != 256 {
					t.Fatalf("memory = %d", cfg.UnikraftCloud.MemoryMB)
				}
			},
		},
		{
			name:    "class rejected",
			args:    []string{"-class", "small"},
			wantErr: true,
		},
		{
			name:    "type rejected",
			args:    []string{"-type", "cx22"},
			wantErr: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.String("class", "", "")
			fs.String("type", "", "")
			values := registerUnikraftCloudProviderFlags(fs, core.Config{})
			if err := fs.Parse(test.args); err != nil {
				t.Fatalf("parse: %v", err)
			}
			cfg := core.Config{Provider: providerName}
			err := applyUnikraftCloudProviderFlags(&cfg, fs, values)
			if test.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			if test.check != nil {
				test.check(t, cfg)
			}
		})
	}
}

func TestUnikraftBindingFlagsAndAliasGuards(t *testing.T) {
	for _, provider := range []string{providerName, "unikraftcloud", "ukc", " UKC ", "fixture-other"} {
		for _, raw := range []string{"", "  ", "fixture"} {
			for _, memory := range []int{-1, 0, 256} {
				cfg := core.BaseConfig()
				cfg.Provider = provider
				before := cfg
				fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
				values := registerUnikraftCloudProviderFlags(fs, cfg)
				if err := applyUnikraftCloudProviderFlags(&cfg, fs, values); err != nil || !reflect.DeepEqual(cfg, before) {
					t.Fatal("unvisited flags changed configuration")
				}
				for _, name := range []string{"url", "metro", "image"} {
					if err := fs.Set("unikraft-cloud-"+name, raw); err != nil {
						t.Fatal(err)
					}
				}
				if err := fs.Set("unikraft-cloud-memory", strconv.Itoa(memory)); err != nil {
					t.Fatal(err)
				}
				if err := applyUnikraftCloudProviderFlags(&cfg, fs, struct{}{}); err != nil || !reflect.DeepEqual(cfg, before) {
					t.Fatal("foreign values changed configuration")
				}
				want := before
				want.UnikraftCloud.APIURL, want.UnikraftCloud.Metro, want.UnikraftCloud.Image, want.UnikraftCloud.MemoryMB = raw, raw, raw, memory
				core.RecordProviderFlagInputs(&want, true, "unikraft-cloud")
				for repeat := 0; repeat < 2; repeat++ {
					if err := applyUnikraftCloudProviderFlags(&cfg, fs, values); err != nil || !reflect.DeepEqual(cfg, want) {
						t.Fatalf("provider=%q raw=%q memory=%d: assignments changed: %v", provider, raw, memory, err)
					}
				}
			}
		}
		for _, generic := range []string{"class", "type"} {
			for _, foreign := range []bool{false, true} {
				cfg := core.BaseConfig()
				cfg.Provider = provider
				fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
				fs.String(generic, "", "")
				values := registerUnikraftCloudProviderFlags(fs, cfg)
				if err := fs.Set(generic, "fixture"); err != nil {
					t.Fatal(err)
				}
				if err := fs.Set("unikraft-cloud-metro", "fixture"); err != nil {
					t.Fatal(err)
				}
				before := cfg
				if foreign {
					values = struct{}{}
				}
				err := applyUnikraftCloudProviderFlags(&cfg, fs, values)
				if provider != "fixture-other" {
					if err == nil || !strings.Contains(err.Error(), "--"+generic) || !reflect.DeepEqual(cfg, before) {
						t.Fatalf("provider=%q foreign=%v: selected guard changed: %v", provider, foreign, err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	registerUnikraftCloudProviderFlags(fs, core.BaseConfig())
	if err := fs.Parse([]string{"--unikraft-cloud-memory=invalid"}); err == nil {
		t.Fatal("malformed memory flag accepted")
	}
}
