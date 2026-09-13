package crownest

import (
	"context"
	"flag"
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestCrownestConfigShowSection(t *testing.T) {
	for _, selected := range []string{"", "crownest"} {
		for _, forget := range []bool{false, true} {
			cfg := core.Config{Provider: selected, Crownest: core.CrownestConfig{APIURL: "https://api.example.test/path?debug=1#hint", ProjectID: "", Template: " raw-template ", TimeoutSecs: 0, ForgetMissing: forget}}
			before := cfg.Crownest
			section := (Provider{}).ConfigShowSection(cfg)
			values := map[string]any{}
			var fields []string
			for _, field := range section.Fields {
				values[field.JSONName] = field.JSONValue
				fields = append(fields, field.TextName+"="+field.TextValue)
			}
			want := map[string]any{"apiUrl": "https://api.example.test/path", "projectId": "", "template": " raw-template ", "timeoutSecs": 0, "forgetMissing": forget}
			if section.JSONKey != "crownest" || section.TextLabel != "crownest" || !reflect.DeepEqual(section.Providers, []string{"crownest"}) || !reflect.DeepEqual(values, want) {
				t.Fatal("unexpected Crownest display projection")
			}
			if strings.Join(fields, " ") != "api_url=https://api.example.test/path project_id=- template= raw-template  timeout_secs=0 forget_missing="+strconv.FormatBool(forget) {
				t.Fatal("text projection changed raw values or field order")
			}
			if cfg.Crownest != before {
				t.Fatal("display mutated configuration")
			}
		}
	}
}

func TestCrownestOrdinaryFlagOrdering(t *testing.T) {
	for _, provider := range []string{" CROWNEST ", "other"} {
		for _, sizing := range []string{"", "class", "type"} {
			cfg := core.Config{Provider: provider}
			before := cfg
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.String("class", "", "")
			fs.String("type", "", "")
			if sizing != "" {
				if err := fs.Parse([]string{"--" + sizing + "=ordinary"}); err != nil {
					t.Fatal(err)
				}
			}
			for _, values := range []any{nil, struct{}{}} {
				err := (Provider{}).ApplyFlags(&cfg, fs, values)
				if (err != nil) != (provider != "other" && sizing != "") || !reflect.DeepEqual(cfg, before) {
					t.Fatalf("early guard %q/%q: %v", provider, sizing, err)
				}
			}
		}
	}
	for _, provider := range []string{"crownest", "other"} {
		for _, tc := range []struct {
			url, template string
			timeout       int
			diagnostic    string
		}{{" https://example.invalid ", "same", 0, ""}, {"", "same", 7, ""}, {"ordinary", "", -1, "provider=crownest base URL must be an absolute URL"}, {"https://example.invalid", "", -1, "crownest timeoutSecs must be non-negative"}, {"https://example.invalid", "", 0, "crownest template must not be empty"}} {
			cfg := core.Config{Provider: provider, Crownest: core.CrownestConfig{APIURL: "https://example.invalid", ProjectID: "same", Template: "same", TimeoutSecs: 7}}
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			values := (Provider{}).RegisterFlags(fs, cfg)
			before := cfg
			if err := (Provider{}).ApplyFlags(&cfg, fs, values); err != nil || !reflect.DeepEqual(cfg, before) {
				t.Fatal("unvisited values changed")
			}
			if err := fs.Parse([]string{"--crownest-url=" + tc.url, "--crownest-project-id=first", "--crownest-project-id=", "--crownest-template=" + tc.template, "--crownest-timeout-secs=" + strconv.Itoa(tc.timeout), "--crownest-forget-missing=true"}); err != nil {
				t.Fatal(err)
			}
			err := (Provider{}).ApplyFlags(&cfg, fs, values)
			if (err != nil) != (tc.diagnostic != "") || (err != nil && err.Error() != tc.diagnostic) {
				t.Fatalf("validation %v", err)
			}
			want := before
			want.Crownest = core.CrownestConfig{APIURL: tc.url, ProjectID: "", Template: tc.template, TimeoutSecs: tc.timeout, ForgetMissing: true}
			core.RecordProviderFlagInputs(&want, true, "crownest")
			if !reflect.DeepEqual(cfg, want) {
				t.Fatalf("all flags must precede validation: %#v", cfg)
			}
		}
	}
}

func TestManualConfigInputFlags(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = "fixture-other"
	cfg.Crownest.APIURL = "https://fixture.invalid"
	cfg.Crownest.Template = "fixture"
	fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
	values := (Provider{}).RegisterFlags(fs, cfg)
	before := cfg
	if err := (Provider{}).ApplyFlags(&cfg, fs, struct{}{}); err != nil || !reflect.DeepEqual(cfg, before) {
		t.Fatalf("foreign values changed configuration: %v", err)
	}
	if err := (Provider{}).ApplyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	want := cfg
	core.RecordProviderFlagInputs(&want, true, "crownest")
	if reflect.DeepEqual(cfg, want) {
		t.Fatal("unvisited flags recorded input")
	}
	for repeat := 0; repeat < 2; repeat++ {
		if err := fs.Set("crownest-project-id", "fixture"); err != nil {
			t.Fatal(err)
		}
		if err := (Provider{}).ApplyFlags(&cfg, fs, values); err != nil {
			t.Fatal(err)
		}
		want = cfg
		core.RecordProviderFlagInputs(&want, true, "crownest")
		if !reflect.DeepEqual(cfg, want) {
			t.Fatal("accepted/equal flag value was not recorded")
		}
	}
}

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
	configured, err := provider.Configure(testConfig(), core.Runtime{Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatalf("Configure err=%v", err)
	}
	delegated, ok := configured.(core.DelegatedRunBackend)
	if !ok {
		t.Fatalf("configured backend does not implement DelegatedRunBackend: %T", configured)
	}
	if _, err := delegated.Run(context.Background(), core.RunRequest{}); err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("Run err=%v, want API key requirement", err)
	}
	if _, ok := configured.(core.CleanupBackend); !ok {
		t.Fatalf("configured backend does not implement CleanupBackend: %T", configured)
	}
	doctor, err := provider.ConfigureDoctor(testConfig(), core.Runtime{Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatalf("ConfigureDoctor err=%v", err)
	}
	if _, err := doctor.Doctor(context.Background(), core.DoctorRequest{}); err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("Doctor err=%v, want API key requirement", err)
	}
}

func testConfig() core.Config {
	cfg := core.BaseConfig()
	cfg.Crownest.APIURL = "https://api.crownest.dev"
	cfg.Crownest.Template = "python-node"
	cfg.Crownest.TimeoutSecs = 600
	return cfg
}
