package all

import (
	"flag"
	"reflect"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

var dedupD4Providers = []string{
	"anthropic-sandbox-runtime", "azure-dynamic-sessions", "blaxel", "cloudflare", "cloudflare-sandbox", "cloud-run-sandbox", "codesandbox", "crownest", "cua", "docker-sandbox", "exe-dev", "fastapi-cloud", "hyperv", "lume", "modal", "mxc", "namespace-instance", "nebius", "nomad", "opencomputer", "opensandbox", "orgo", "railway", "runpod", "smolvm", "superserve", "tensorlake", "unikraft-cloud", "upstash-box", "vercel-sandbox", "wandb",
}

func dedupD4FlagField(t *testing.T, cfg *core.Config, name string) reflect.Value {
	t.Helper()
	root := reflect.ValueOf(cfg).Elem()
	for i := 0; i < root.NumField(); i++ {
		group := root.Field(i)
		if group.Kind() != reflect.Struct {
			continue
		}
		for j := 0; j < group.NumField(); j++ {
			if group.Type().Field(j).Tag.Get("flag") == name {
				return group.Field(j)
			}
		}
	}
	t.Fatalf("missing config field for %s", name)
	return reflect.Value{}
}

func TestDedupD4ProviderFlagInputs(t *testing.T) {
	for _, name := range dedupD4Providers {
		provider, err := core.ProviderFor(name)
		if err != nil {
			t.Fatal(err)
		}
		owner := provider.Spec().Name
		defaults := core.BaseConfig()
		defaults.Provider = ""
		catalog := flag.NewFlagSet("catalog", flag.ContinueOnError)
		provider.RegisterFlags(catalog, defaults)
		catalog.VisitAll(func(entry *flag.Flag) {
			t.Run(owner+"/"+entry.Name, func(t *testing.T) {
				cfg := defaults
				fs := flag.NewFlagSet("test", flag.ContinueOnError)
				values := provider.RegisterFlags(fs, cfg)
				field := dedupD4FlagField(t, &cfg, entry.Name)
				switch field.Kind() {
				case reflect.String:
					field.SetString("prior")
				case reflect.Int, reflect.Int64:
					field.SetInt(7)
				case reflect.Float64:
					field.SetFloat(7)
				case reflect.Bool:
					field.SetBool(true)
				case reflect.Slice:
					field.Set(reflect.ValueOf([]string{"prior"}))
				}
				raw := ""
				switch entry.Value.(flag.Getter).Get().(type) {
				case bool:
					raw = "false"
				case int, int64, float64:
					raw = "0"
				case time.Duration:
					raw = "0s"
				}
				if field.Type() == reflect.TypeFor[time.Duration]() {
					raw = "5m"
				}
				if err := fs.Set(entry.Name, raw); err != nil {
					t.Fatal(err)
				}
				for _, wrong := range []any{nil, struct{}{}, &values} {
					before := cfg
					if err := provider.ApplyFlags(&cfg, fs, wrong); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(cfg, before) {
						t.Fatal("wrong values type changed config")
					}
				}
				// Validation can reject an empty value after it has been accepted.
				_ = provider.ApplyFlags(&cfg, fs, values)
				if raw == "5m" {
					if field.Int() != int64(5*time.Minute) {
						t.Fatal("duration was not applied")
					}
				} else if entry.Name == "namespace-instance-volume" {
					if !reflect.DeepEqual(field.Interface(), []string{""}) {
						t.Fatal("append-list flag lost its empty entry")
					}
				} else if field.Kind() == reflect.Slice {
					if field.Len() != 0 {
						t.Fatal("empty list did not clear prior input")
					}
				} else if !field.IsZero() {
					t.Fatal("empty/zero input did not clear prior value")
				}
				if got := manualBatchBFlagMask(cfg, owner); got != 1<<3 {
					t.Fatalf("flag source=%d, want 8", got)
				}
				if got := manualBatchBFlagMask(cfg, "$generic"); got != 0 {
					t.Fatalf("unexpected generic input=%d", got)
				}
			})
		})
		t.Run(owner+"/unrelated", func(t *testing.T) {
			cfg := defaults
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			values := provider.RegisterFlags(fs, cfg)
			fs.String("unrelated-provider-option", "", "")
			if err := fs.Set("unrelated-provider-option", "x"); err != nil {
				t.Fatal(err)
			}
			_ = provider.ApplyFlags(&cfg, fs, values)
			if got := manualBatchBFlagMask(cfg, owner); got != 0 {
				t.Fatalf("unrelated input=%d", got)
			}
		})
	}
}

func TestDedupD4WrongValuesSkipPostprocessing(t *testing.T) {
	for _, name := range dedupD4Providers {
		provider, err := core.ProviderFor(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, selected := range append([]string{provider.Spec().Name}, provider.Spec().Aliases...) {
			t.Run(selected, func(t *testing.T) {
				cfg := core.Config{Provider: selected}
				// Cloudflare normalizes generic sizing before checking the values type.
				if name == "cloudflare" {
					cfg.ServerType = "basic"
				}
				before := cfg
				fs := flag.NewFlagSet("test", flag.ContinueOnError)
				if err := provider.ApplyFlags(&cfg, fs, struct{}{}); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(cfg, before) {
					t.Fatal("wrong values type ran postprocessing")
				}
			})
		}
	}
}

func TestDedupD4PartialFlagErrorOrder(t *testing.T) {
	provider, err := core.ProviderFor("nomad")
	if err != nil {
		t.Fatal(err)
	}
	for _, earlier := range []bool{false, true} {
		cfg := core.BaseConfig()
		cfg.Provider = ""
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		values := provider.RegisterFlags(fs, cfg)
		args := []string{"--nomad-exec-timeout-secs=7", "--nomad-alloc-ready-timeout=invalid"}
		if earlier {
			args = append(args, "--nomad-region=earlier")
		}
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		beforeTimeout, beforeExecTimeout := cfg.Nomad.AllocReadyTimeout, cfg.Nomad.ExecTimeoutSecs
		if err := provider.ApplyFlags(&cfg, fs, values); err == nil {
			t.Fatal("expected duration error")
		}
		if cfg.Nomad.AllocReadyTimeout != beforeTimeout || cfg.Nomad.ExecTimeoutSecs != beforeExecTimeout {
			t.Fatal("applied fields after duration error")
		}
		if earlier && cfg.Nomad.Region != "earlier" {
			t.Fatal("lost earlier assignment")
		}
		if got := manualBatchBFlagMask(cfg, "nomad") != 0; got != earlier {
			t.Fatalf("accepted input=%t, want %t", got, earlier)
		}
	}
}

func TestDedupD4DurationEmptyAndZero(t *testing.T) {
	for _, tc := range []struct {
		provider, flag, raw string
		want                time.Duration
		accepted, wantError bool
	}{
		{"namespace-instance", "namespace-instance-duration", "", time.Minute, false, false},
		{"namespace-instance", "namespace-instance-duration", "0s", 0, true, false},
		{"namespace-instance", "namespace-instance-duration", " 0s ", 0, true, false},
		{"nomad", "nomad-alloc-ready-timeout", "", time.Minute, false, true},
		{"nomad", "nomad-alloc-ready-timeout", "0s", time.Minute, false, true},
	} {
		t.Run(tc.provider+"/"+tc.raw, func(t *testing.T) {
			provider, err := core.ProviderFor(tc.provider)
			if err != nil {
				t.Fatal(err)
			}
			cfg := core.BaseConfig()
			cfg.Provider = ""
			field := dedupD4FlagField(t, &cfg, tc.flag)
			field.SetInt(int64(time.Minute))
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			values := provider.RegisterFlags(fs, cfg)
			if err := fs.Set(tc.flag, tc.raw); err != nil {
				t.Fatal(err)
			}
			err = provider.ApplyFlags(&cfg, fs, values)
			if (err != nil) != tc.wantError {
				t.Fatalf("error=%v, want error=%t", err, tc.wantError)
			}
			if field.Int() != int64(tc.want) {
				t.Fatalf("duration=%d, want %v", field.Int(), tc.want)
			}
			if got := manualBatchBFlagMask(cfg, tc.provider) != 0; got != tc.accepted {
				t.Fatalf("accepted=%t, want %t", got, tc.accepted)
			}
		})
	}
}
