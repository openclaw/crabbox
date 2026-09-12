package modal

import (
	"flag"
	"reflect"
	"strings"
	"testing"
)

func TestModalSecretFlagsReplaceConfiguredDefaults(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{name: "replace and repeat", args: []string{"--modal-secret", "example,sample", "--modal-secret", "dummy"}, want: []string{"example", "sample", "dummy"}},
		{name: "clear", args: []string{"--modal-secret="}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newTestConfig()
			cfg.Provider = providerName
			cfg.Modal.Secrets = []string{"sample"}
			fs := flag.NewFlagSet("modal", flag.ContinueOnError)
			values := RegisterModalProviderFlags(fs, cfg)
			if err := fs.Parse(tt.args); err != nil {
				t.Fatal(err)
			}
			if err := ApplyModalProviderFlags(&cfg, fs, values); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.Modal.Secrets, tt.want) {
				t.Fatalf("Modal secrets=%v want %v", cfg.Modal.Secrets, tt.want)
			}
		})
	}
}

func TestModalConfigFlagContract(t *testing.T) {
	cfg := newTestConfig()
	cfg.Modal.Environment = "prior-env"
	cfg.Modal.Secrets = []string{"prior"}
	fs := flag.NewFlagSet("contract", flag.ContinueOnError)
	values := RegisterModalProviderFlags(fs, cfg)
	count := 0
	fs.VisitAll(func(*flag.Flag) { count++ })
	if count != 6 {
		t.Fatalf("flags=%d", count)
	}
	for _, tc := range []struct{ name, want string }{{"modal-app", "crabbox"}, {"modal-image", "python:3.13-slim"}, {"modal-workdir", "/workspace/crabbox"}, {"modal-python", "python3"}, {"modal-environment", "prior-env"}, {"modal-secret", "prior"}} {
		f := fs.Lookup(tc.name)
		if f == nil || f.DefValue != tc.want || f.Usage == "" {
			t.Fatalf("flag %s=%#v", tc.name, f)
		}
	}
	cfg.Modal = ModalConfig{App: "layered-app", Image: "layered-image", Workdir: "/workspace/layered", Python: "layered-python", Environment: "layered-env", Secrets: []string{"layered"}}
	want := cfg.Modal
	if err := ApplyModalProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Modal, want) {
		t.Fatal("unvisited flags overwrite later config")
	}
	if err := fs.Parse([]string{"--modal-app=", "--modal-image=  ", "--modal-workdir=/workspace/flag", "--modal-python=flag-python", "--modal-environment=flag-env", "--modal-secret=alpha,beta,alpha"}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyModalProviderFlags(&cfg, fs, struct{}{}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Modal, want) {
		t.Fatal("wrong values type changed config")
	}
	if err := ApplyModalProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	want = ModalConfig{Image: "  ", Workdir: "/workspace/flag", Python: "flag-python", Environment: "flag-env", Secrets: []string{"alpha", "beta", "alpha"}}
	if !reflect.DeepEqual(cfg.Modal, want) {
		t.Fatalf("flags=%#v want=%#v", cfg.Modal, want)
	}
}

func TestModalConfigListFlagCopies(t *testing.T) {
	for _, tc := range []struct {
		name         string
		values, want []string
	}{{"empty-first", []string{"", " alpha, ,beta ", "alpha"}, []string{"alpha", "beta", "alpha"}}, {"later-empty", []string{"alpha", "", " beta,alpha "}, []string{"alpha", "beta", "alpha"}}, {"clear", []string{""}, nil}, {"literal-none", []string{"none"}, []string{"none"}}} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := newTestConfig()
			cfg.Modal.Secrets = []string{"initial"}
			fs := flag.NewFlagSet("contract", flag.ContinueOnError)
			values := RegisterModalProviderFlags(fs, cfg)
			f := fs.Lookup("modal-secret")
			getter, ok := f.Value.(flag.Getter)
			if !ok {
				t.Fatal("list does not implement flag.Getter")
			}
			cfg.Modal.Secrets[0] = "mutated"
			if f.Value.String() != "initial" {
				t.Fatal("registration aliases configured list")
			}
			copy := getter.Get().([]string)
			copy[0] = "getter-mutated"
			if f.Value.String() != "initial" {
				t.Fatal("Get aliases registered list")
			}
			for _, raw := range tc.values {
				if err := fs.Set("modal-secret", raw); err != nil {
					t.Fatal(err)
				}
			}
			if f.Value.String() != strings.Join(tc.want, ",") {
				t.Fatalf("String=%q", f.Value.String())
			}
			if getter.Get().([]string) == nil {
				t.Fatal("Get must return nonnil empty copy")
			}
			if err := ApplyModalProviderFlags(&cfg, fs, values); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.Modal.Secrets, tc.want) {
				t.Fatalf("applied=%#v want=%#v", cfg.Modal.Secrets, tc.want)
			}
			if len(cfg.Modal.Secrets) > 0 {
				cfg.Modal.Secrets[0] = "config-mutated"
				if f.Value.String() != strings.Join(tc.want, ",") {
					t.Fatal("Apply aliases registered list")
				}
				if err := ApplyModalProviderFlags(&cfg, fs, values); err != nil {
					t.Fatal(err)
				}
			}
			if err := fs.Set("modal-secret", "later"); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.Modal.Secrets, tc.want) {
				t.Fatal("later Set mutated applied config")
			}
		})
	}
}

func TestModalConfigFlagGuardOrder(t *testing.T) {
	for _, provider := range []string{"modal", " MODAL ", "aws"} {
		for _, args := range [][]string{{"--class=large", "--type=machine"}, {"--type=machine"}} {
			cfg := Config{Provider: provider}
			fs := flag.NewFlagSet("contract", flag.ContinueOnError)
			fs.String("class", "", "")
			fs.String("type", "", "")
			RegisterModalProviderFlags(fs, cfg)
			if err := fs.Parse(args); err != nil {
				t.Fatal(err)
			}
			err := ApplyModalProviderFlags(&cfg, fs, struct{}{})
			if provider != "modal" {
				if err != nil {
					t.Fatal(err)
				}
				continue
			}
			want := "--type is not supported"
			if len(args) == 2 {
				want = "--class is not supported"
			}
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("err=%v want=%s", err, want)
			}
		}
	}
}
