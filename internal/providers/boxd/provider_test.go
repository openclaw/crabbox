package boxd

import (
	"flag"
	core "github.com/openclaw/crabbox/internal/cli"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestManualConfigInputFlags(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = "fixture-other"
	fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
	values := registerFlags(fs, cfg)
	before := cfg
	if err := applyFlags(&cfg, fs, struct{}{}); err != nil || !reflect.DeepEqual(cfg, before) {
		t.Fatalf("foreign values changed configuration: %v", err)
	}
	if err := applyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	want := cfg
	core.RecordProviderFlagInputs(&want, true, "boxd")
	if reflect.DeepEqual(cfg, want) {
		t.Fatal("unvisited flags recorded input")
	}
	for repeat := 0; repeat < 2; repeat++ {
		if err := fs.Set("boxd-org", ""); err != nil {
			t.Fatal(err)
		}
		if err := applyFlags(&cfg, fs, values); err != nil {
			t.Fatal(err)
		}
		want = cfg
		core.RecordProviderFlagInputs(&want, true, "boxd")
		if !reflect.DeepEqual(cfg, want) {
			t.Fatal("accepted/equal flag value was not recorded")
		}
	}
}

func TestProviderContractAndFlags(t *testing.T) {
	p := Provider{}
	spec := p.Spec()
	if spec.Kind != core.ProviderKindSSHLease || spec.Coordinator != core.CoordinatorNever || !spec.Features.Has(core.FeatureSSH) || !spec.Features.Has(core.FeatureCleanup) {
		t.Fatalf("spec=%#v", spec)
	}
	for _, invalid := range []string{"http://app.boxd.sh", "https://user:password@app.boxd.sh", "https://app.boxd.sh/path"} {
		cfg := core.BaseConfig()
		cfg.Boxd.APIURL = invalid
		if _, err := p.Configure(cfg, core.Runtime{}); err == nil {
			t.Fatal("invalid origin configured")
		}
	}
	for _, network := range []core.NetworkMode{"private", "tailscale"} {
		cfg := core.BaseConfig()
		cfg.Network = network
		if _, err := p.Configure(cfg, core.Runtime{}); err == nil {
			t.Fatal("invalid network configured")
		}
	}
	for _, work := range []string{"/", "/etc", "relative", "/home/boxd/../boxd"} {
		cfg := core.BaseConfig()
		cfg.Boxd.WorkRoot = work
		if _, err := p.Configure(cfg, core.Runtime{}); err == nil {
			t.Fatalf("accepted work root %q", work)
		}
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	fs := flag.NewFlagSet("boxd", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	values := p.RegisterFlags(fs, cfg)
	if fs.Lookup("boxd-cli") != nil {
		t.Fatal("obsolete CLI flag remains")
	}
	if err := fs.Parse([]string{"--boxd-org=team", "--boxd-work-root=/home/boxd/work", "--boxd-delete-on-release=false"}); err != nil {
		t.Fatal(err)
	}
	if err := p.ApplyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Boxd.Org != "team" || cfg.WorkRoot != "/home/boxd/work" || cfg.Boxd.DeleteOnRelease {
		t.Fatal("flags not applied")
	}
}
func TestScopeCanonicalizationAndDiagnosticTokens(t *testing.T) {
	cfg := core.BaseConfig()
	p := Provider{}
	scope := p.ClaimScope(cfg)
	cfg.Boxd.APIURL = "https://APP.BOXD.SH:443/"
	if p.ClaimScope(cfg) != scope {
		t.Fatal("origin not normalized")
	}
	cfg.Boxd.Org = "explicit"
	if p.ClaimScope(cfg) == scope {
		t.Fatal("org not fenced")
	}
	t.Setenv("CRABBOX_BOXD_TOKEN", "fixture-preferred-session")
	t.Setenv("BOXD_TOKEN", "fixture-secondary-session")
	secrets := p.DiagnosticSecrets(cfg)
	if len(secrets) != 2 || secrets[0] != "fixture-preferred-session" || secrets[1] != "fixture-secondary-session" {
		t.Fatal("diagnostic redaction tokens missing")
	}
	b := newBackend(p.Spec(), cfg, core.Runtime{})
	c, err := b.client()
	if err != nil || c.token != "fixture-preferred-session" {
		t.Fatal("session precedence failed")
	}
	if strings.Contains(p.ClaimScope(cfg), "fixture") {
		t.Fatal("credential leaked into scope")
	}
}

func TestBoxdBindingFlagPhases(t *testing.T) {
	for _, provider := range []string{"boxd", " BoXd ", "fixture-other"} {
		for _, field := range []string{"api-url", "org", "work-root", "delete-on-release"} {
			for _, raw := range []string{"", "prior", "  ", "false", "true"} {
				if field == "delete-on-release" && raw != "false" && raw != "true" {
					continue
				}
				cfg := core.BaseConfig()
				cfg.Provider = provider
				cfg.Boxd = core.BoxdConfig{APIURL: "prior", Org: "prior", WorkRoot: "/provider/prior", DeleteOnRelease: true}
				cfg.WorkRoot = "/generic/prior"
				core.MarkWorkRootExplicit(&cfg)
				want := cfg
				fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
				v := registerFlags(fs, cfg)
				if err := fs.Set("boxd-"+field, raw); err != nil {
					t.Fatal(err)
				}
				switch field {
				case "api-url":
					want.Boxd.APIURL = raw
				case "org":
					want.Boxd.Org = raw
				case "work-root":
					want.Boxd.WorkRoot = raw
					core.MarkBoxdWorkRootExplicit(&want)
				case "delete-on-release":
					want.Boxd.DeleteOnRelease = raw == "true"
					core.MarkDeleteOnReleaseExplicit(&want, "boxd")
				}
				core.RecordProviderFlagInputs(&want, true, "boxd")
				core.RecordProviderFlagIntents(&want, field == "work-root" || field == "delete-on-release", "boxd")
				// The provider's unchanged cross-field defaults remain a separate owner.
				if provider != "fixture-other" {
					applyDefaults(&want)
				}
				if err := applyFlags(&cfg, fs, v); err != nil || !reflect.DeepEqual(cfg, want) {
					t.Fatalf("%s %s %q: %v %+v", provider, field, raw, err, cfg.Boxd)
				}
			}
		}
		cfg := core.BaseConfig()
		cfg.Provider = provider
		cfg.TargetOS = "linux"
		cfg.Boxd.APIURL = ""
		before := cfg
		fs := flag.NewFlagSet("guards", flag.ContinueOnError)
		fs.String("class", "", "")
		fs.String("type", "", "")
		v := registerFlags(fs, cfg)
		if err := applyFlags(&cfg, fs, struct{}{}); err != nil || !reflect.DeepEqual(cfg, before) {
			t.Fatal("wrong type performed defaults")
		}
		if err := applyFlags(&cfg, fs, v); err != nil {
			t.Fatal(err)
		}
		if provider != "fixture-other" && cfg.Boxd.APIURL != "https://app.boxd.sh" {
			t.Fatal("typed unvisited defaults skipped")
		}
		for _, generic := range []string{"type", "class"} {
			if err := fs.Set(generic, "fixture"); err != nil {
				t.Fatal(err)
			}
			cfg = before
			err := applyFlags(&cfg, fs, struct{}{})
			if (err != nil) != (provider != "fixture-other") {
				t.Fatal("guard moved after type assertion")
			}
			if provider != "fixture-other" && !strings.Contains(err.Error(), "--"+generic) {
				t.Fatal("guard order changed")
			}
		}
		cfg = before
		cfg.TargetOS = " Linux "
		empty := flag.NewFlagSet("target", flag.ContinueOnError)
		if err := applyFlags(&cfg, empty, struct{}{}); (err != nil) != (provider != "fixture-other") {
			t.Fatal("target check became normalized")
		}
	}
}

func TestBoxdBindingRegistrationOrder(t *testing.T) {
	order := []string{"api-url", "org", "work-root", "delete-on-release"}
	for i, dup := range order {
		fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fs.String("boxd-"+dup, "", "")
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("duplicate missing")
				}
			}()
			registerFlags(fs, core.BaseConfig())
		}()
		for j, name := range order {
			if (fs.Lookup("boxd-"+name) != nil) != (j <= i) {
				t.Fatal("registration prefix changed")
			}
		}
		if fs.Lookup("boxd-token") != nil || fs.Lookup("boxd-cli") != nil {
			t.Fatal("new argv source")
		}
	}
}
