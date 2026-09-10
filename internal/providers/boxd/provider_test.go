package boxd

import (
	"context"
	"flag"
	"io"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
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
	for _, invalid := range []string{"https://boxd.sh:9443", "boxd.sh", "boxd.sh:0"} {
		cfg := core.BaseConfig()
		cfg.Boxd.GRPCURL = invalid
		if _, err := p.Configure(cfg, core.Runtime{}); err == nil {
			t.Fatal("invalid gRPC target configured")
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
	if err := fs.Parse([]string{"--boxd-org=team", "--boxd-grpc-url=boxd.example.test:9443", "--boxd-work-root=/home/boxd/work", "--boxd-delete-on-release=false"}); err != nil {
		t.Fatal(err)
	}
	if err := p.ApplyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Boxd.Org != "team" || cfg.Boxd.GRPCURL != "boxd.example.test:9443" || cfg.WorkRoot != "/home/boxd/work" || cfg.Boxd.DeleteOnRelease {
		t.Fatal("flags not applied")
	}
}
func TestScopeCanonicalizationAndDiagnosticSecrets(t *testing.T) {
	cfg := core.BaseConfig()
	p := Provider{}
	scope := p.ClaimScope(cfg)
	cfg.Boxd.APIURL = "https://APP.BOXD.SH:443/"
	cfg.Boxd.GRPCURL = "BOXD.SH:9443"
	if p.ClaimScope(cfg) != scope {
		t.Fatal("origin not normalized")
	}
	cfg.Boxd.GRPCURL = "another.example.test:9443"
	grpcScope := p.ClaimScope(cfg)
	if grpcScope == scope {
		t.Fatal("gRPC endpoint not fenced")
	}
	cfg.Boxd.Org = "explicit"
	if p.ClaimScope(cfg) == grpcScope {
		t.Fatal("org not fenced")
	}
	preferred := "bxd_" + strings.Repeat("a", 43)
	secondary := "bxd_" + strings.Repeat("b", 43)
	t.Setenv("CRABBOX_BOXD_API_KEY", preferred)
	t.Setenv("BOXD_API_KEY", secondary)
	secrets := p.DiagnosticSecrets(cfg)
	if len(secrets) != 2 || secrets[0] != preferred || secrets[1] != secondary {
		t.Fatal("diagnostic redaction tokens missing")
	}
	b := newBackend(p.Spec(), cfg, core.Runtime{})
	c, err := b.client()
	if err != nil || c.auth.key != preferred {
		t.Fatal("API-key precedence failed")
	}
	if strings.Contains(p.ClaimScope(cfg), "bxd_") {
		t.Fatal("credential leaked into scope")
	}
}
func TestLegacyTokenEnvNeverReachesNetwork(t *testing.T) {
	t.Setenv("CRABBOX_BOXD_API_KEY", "")
	t.Setenv("BOXD_API_KEY", "")
	t.Setenv("BOXD_TOKEN", "fixture-legacy-session")
	t.Setenv("CRABBOX_BOXD_TOKEN", "")
	cfg := core.BaseConfig()
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{})
	_, err := b.Doctor(context.Background(), core.DoctorRequest{})
	if err == nil || !strings.Contains(err.Error(), "no longer used") {
		t.Fatalf("legacy interactive session accepted: %v", err)
	}
	if strings.Contains(err.Error(), "fixture-legacy-session") {
		t.Fatal("echoed legacy credential")
	}
}
