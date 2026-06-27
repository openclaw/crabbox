package crownest

import (
	"context"
	"flag"
	"io"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderSpecIsDelegatedLinuxAliasFree(t *testing.T) {
	provider := Provider{}
	if provider.Name() != providerName {
		t.Fatalf("Name=%q want %q", provider.Name(), providerName)
	}
	if aliases := provider.Aliases(); len(aliases) != 0 {
		t.Fatalf("aliases=%v want none", aliases)
	}
	spec := provider.Spec()
	if spec.Kind != core.ProviderKindDelegatedRun {
		t.Fatalf("kind=%q want delegated-run", spec.Kind)
	}
	if spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("coordinator=%q want never", spec.Coordinator)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("targets=%#v want linux only", spec.Targets)
	}
	for _, feature := range []core.Feature{core.FeatureArchiveSync, core.FeatureCleanup, core.FeatureRunSession} {
		if !spec.Features.Has(feature) {
			t.Fatalf("features=%v missing %s", spec.Features, feature)
		}
	}
	for _, feature := range []core.Feature{core.FeatureRunArtifacts, core.FeatureRunDownloads, core.FeatureRunProof, core.FeatureSSH} {
		if spec.Features.Has(feature) {
			t.Fatalf("features=%v should not include %s", spec.Features, feature)
		}
	}
}

func TestApplyFlagsUpdatesConfigWithoutAPIKey(t *testing.T) {
	cfg := testConfig()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String("class", "", "")
	fs.String("type", "", "")
	values := (Provider{}).RegisterFlags(fs, cfg)
	if err := fs.Parse([]string{
		"--crownest-url", "https://api.example.test/",
		"--crownest-project-id", "prj_test",
		"--crownest-template", "python-node",
		"--crownest-timeout-secs", "300",
		"--crownest-forget-missing",
	}); err != nil {
		t.Fatal(err)
	}
	if err := (Provider{}).ApplyFlags(&cfg, fs, values); err != nil {
		t.Fatalf("ApplyFlags err=%v", err)
	}
	if cfg.Crownest.APIURL != "https://api.example.test/" || cfg.Crownest.ProjectID != "prj_test" || cfg.Crownest.Template != "python-node" || cfg.Crownest.TimeoutSecs != 300 || !cfg.Crownest.ForgetMissing {
		t.Fatalf("cfg=%#v", cfg.Crownest)
	}
}

func TestValidateBaseURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr string
	}{
		{name: "default", raw: "", want: "https://api.crownest.dev"},
		{name: "https default port", raw: "HTTPS://API.EXAMPLE.TEST:443/", want: "https://api.example.test"},
		{name: "loopback http", raw: "http://localhost:8787/", want: "http://localhost:8787"},
		{name: "userinfo", raw: "https://user:pass@api.example.test", wantErr: "must not contain userinfo"},
		{name: "query", raw: "https://api.example.test?token=secret", wantErr: "must not contain userinfo"},
		{name: "plain http", raw: "http://api.example.test", wantErr: "must use HTTPS"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateBaseURL(tt.raw)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err=%v want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err=%v", err)
			}
			if got != tt.want {
				t.Fatalf("got=%q want %q", got, tt.want)
			}
		})
	}
}

func TestConfigureReturnsDelegatedCleanupAndDoctor(t *testing.T) {
	t.Setenv("CRABBOX_CROWNEST_API_KEY", "")
	t.Setenv("CROWNEST_API_KEY", "")
	provider := Provider{}
	configured, err := provider.Configure(testConfig(), Runtime{Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatalf("Configure err=%v", err)
	}
	delegated, ok := configured.(core.DelegatedRunBackend)
	if !ok {
		t.Fatalf("configured backend does not implement DelegatedRunBackend: %T", configured)
	}
	if _, err := delegated.Run(context.Background(), RunRequest{}); err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("Run err=%v, want API key requirement", err)
	}
	if _, ok := configured.(core.CleanupBackend); !ok {
		t.Fatalf("configured backend does not implement CleanupBackend: %T", configured)
	}
	doctor, err := provider.ConfigureDoctor(testConfig(), Runtime{Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatalf("ConfigureDoctor err=%v", err)
	}
	if _, err := doctor.Doctor(context.Background(), DoctorRequest{}); err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("Doctor err=%v, want API key requirement", err)
	}
}

func testConfig() Config {
	cfg := core.BaseConfig()
	cfg.Crownest.APIURL = "https://api.crownest.dev"
	cfg.Crownest.Template = "python-node"
	cfg.Crownest.TimeoutSecs = 600
	return cfg
}
