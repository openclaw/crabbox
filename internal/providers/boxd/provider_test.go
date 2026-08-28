package boxd

import (
	"flag"
	core "github.com/openclaw/crabbox/internal/cli"
	"io"
	"strings"
	"testing"
)

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
