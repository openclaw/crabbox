package codesandbox

import (
	"context"
	"flag"
	"io"
	"reflect"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderSpecAndAliases(t *testing.T) {
	provider := Provider{}
	spec := provider.Spec()
	if spec.Name != providerName || spec.Family != providerFamily {
		t.Fatalf("spec identity=%#v", spec)
	}
	if spec.Kind != core.ProviderKindDelegatedRun || spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("spec kind/coordinator=%#v", spec)
	}
	for _, feature := range []core.Feature{core.FeatureArchiveSync, core.FeatureCleanup, core.FeaturePauseResume, core.FeatureRunSession} {
		if !spec.Features.Has(feature) {
			t.Fatalf("features=%v missing %s", spec.Features, feature)
		}
	}
	if spec.Features.Has(core.FeatureURLBridge) {
		t.Fatalf("features=%v must not advertise the pond URL bridge without BridgeProvider support", spec.Features)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("targets=%#v", spec.Targets)
	}
	if aliases := provider.Aliases(); !reflect.DeepEqual(aliases, []string{"csb", "code-sandbox"}) {
		t.Fatalf("aliases=%v", aliases)
	}
}

func TestProviderFlagsApplyAndValidateNonSecretFields(t *testing.T) {
	cfg := newTestConfig()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	values := RegisterCodeSandboxProviderFlags(fs, cfg)
	args := []string{
		"--codesandbox-template-id", "tmpl_123",
		"--codesandbox-workdir", "/project/workspace/my-app",
		"--codesandbox-vm-tier", "micro",
		"--codesandbox-privacy", "public-hosts",
		"--codesandbox-hibernation-timeout-secs", "900",
		"--codesandbox-automatic-wakeup-http=false",
		"--codesandbox-automatic-wakeup-websocket",
		"--codesandbox-bridge-command", "/opt/node",
		"--codesandbox-sdk-package", "@codesandbox/sdk@2.4.2",
		"--codesandbox-doctor-list-limit", "2",
		"--codesandbox-operation-timeout-secs", "45",
	}
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	if err := ApplyCodeSandboxProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	got := cfg.CodeSandbox
	if got.TemplateID != "tmpl_123" || got.Workdir != "/project/workspace/my-app" || got.VMTier != "micro" || got.Privacy != "public-hosts" {
		t.Fatalf("codesandbox config=%#v", got)
	}
	if got.HibernationTimeoutSecs != 900 || got.AutomaticWakeupHTTP || !got.AutomaticWakeupWebSocket || got.BridgeCommand != "/opt/node" || got.SDKPackage != "@codesandbox/sdk@2.4.2" || got.DoctorListLimit != 2 || got.OperationTimeoutSecs != 45 {
		t.Fatalf("codesandbox config=%#v", got)
	}
	if _, ok := reflect.TypeOf(CodeSandboxConfig{}).FieldByName("APIKey"); ok {
		t.Fatal("CodeSandboxConfig must not persist API keys")
	}
}

func TestProviderFlagsRejectGenericSizingForAliases(t *testing.T) {
	for _, provider := range []string{"codesandbox", "csb", "code-sandbox", " CodeSandbox "} {
		for _, flagName := range []string{"class", "type"} {
			t.Run(strings.TrimSpace(provider)+"/"+flagName, func(t *testing.T) {
				cfg := newTestConfig()
				cfg.Provider = provider
				fs := flag.NewFlagSet("test", flag.ContinueOnError)
				fs.SetOutput(io.Discard)
				fs.String("class", "", "")
				fs.String("type", "", "")
				values := RegisterCodeSandboxProviderFlags(fs, cfg)
				if err := fs.Parse([]string{"--" + flagName, "large"}); err != nil {
					t.Fatal(err)
				}
				err := ApplyCodeSandboxProviderFlags(&cfg, fs, values)
				if err == nil || !strings.Contains(err.Error(), "--codesandbox-vm-tier") {
					t.Fatalf("provider=%q flag=%s err=%v", provider, flagName, err)
				}
			})
		}
	}
}

func TestValidateCodeSandboxConfigRejectsUnsafeValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{name: "workdir outside project workspace", mutate: func(cfg *Config) { cfg.CodeSandbox.Workdir = "/tmp/app" }, want: "under /project/workspace"},
		{name: "empty bridge command", mutate: func(cfg *Config) { cfg.CodeSandbox.BridgeCommand = " " }, want: "bridgeCommand"},
		{name: "invalid privacy", mutate: func(cfg *Config) { cfg.CodeSandbox.Privacy = "team-only" }, want: "privacy"},
		{name: "invalid vm tier", mutate: func(cfg *Config) { cfg.CodeSandbox.VMTier = "huge" }, want: "vmTier"},
		{name: "negative timeout", mutate: func(cfg *Config) { cfg.CodeSandbox.OperationTimeoutSecs = -1 }, want: "operationTimeoutSecs"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := newTestConfig()
			tc.mutate(&cfg)
			err := validateCodeSandboxConfig(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validate err=%v, want %q", err, tc.want)
			}
		})
	}
}

func TestAuthFromEnvPrecedence(t *testing.T) {
	t.Setenv(codesandboxPrimaryAPIKeyEnv, "primary-token")
	t.Setenv(codesandboxFallbackAPIKeyEnv, "fallback-token")
	token, source, ok := authFromEnv()
	if !ok || token != "primary-token" || source != codesandboxPrimaryAPIKeyEnv {
		t.Fatalf("auth=%q source=%q ok=%v", token, source, ok)
	}
	t.Setenv(codesandboxPrimaryAPIKeyEnv, "")
	token, source, ok = authFromEnv()
	if !ok || token != "fallback-token" || source != codesandboxFallbackAPIKeyEnv {
		t.Fatalf("fallback auth=%q source=%q ok=%v", token, source, ok)
	}
}

func TestDoctorRequiresEnvOnlyAuthBeforeBridge(t *testing.T) {
	t.Setenv(codesandboxPrimaryAPIKeyEnv, "")
	t.Setenv(codesandboxFallbackAPIKeyEnv, "")
	calls := 0
	restore := replaceClientFactory(func(Config, Runtime) (codeSandboxAPI, error) {
		calls++
		return &fakeSandboxLister{}, nil
	})
	defer restore()
	backend := newTestBackend(newTestConfig())
	_, err := backend.Doctor(context.Background(), DoctorRequest{})
	if err == nil || !strings.Contains(err.Error(), codesandboxPrimaryAPIKeyEnv) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("Doctor err=%v", err)
	}
	if calls != 0 {
		t.Fatalf("doctor called bridge without auth; calls=%d", calls)
	}
}

func TestDoctorIsNonMutatingListReadiness(t *testing.T) {
	t.Setenv(codesandboxPrimaryAPIKeyEnv, "secret-token")
	fake := &fakeSandboxLister{
		result: ListSandboxesResult{
			Sandboxes:  []SandboxSummary{{ID: "csb_1", Title: "my-app"}},
			TotalCount: 1,
		},
	}
	restore := replaceClientFactory(func(Config, Runtime) (codeSandboxAPI, error) {
		return fake, nil
	})
	defer restore()
	backend := newTestBackend(newTestConfig())
	result, err := backend.Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatalf("Doctor err=%v", err)
	}
	if result.Provider != providerName || result.Status != "ok" || !strings.Contains(result.Message, "mutation=false") {
		t.Fatalf("result=%#v", result)
	}
	if !reflect.DeepEqual(fake.calls, []ListSandboxesRequest{{Limit: 1}}) {
		t.Fatalf("calls=%#v", fake.calls)
	}
}

func newTestConfig() Config {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	return cfg
}

func newTestBackend(cfg Config) *codeSandboxBackend {
	backend, err := Provider{}.Configure(cfg, discardRuntime())
	if err != nil {
		panic(err)
	}
	return backend.(*codeSandboxBackend)
}

func replaceClientFactory(fn func(Config, Runtime) (codeSandboxAPI, error)) func() {
	prev := newCodeSandboxClient
	newCodeSandboxClient = fn
	return func() { newCodeSandboxClient = prev }
}

type fakeSandboxLister struct {
	calls  []ListSandboxesRequest
	result ListSandboxesResult
	err    error
}

func (f *fakeSandboxLister) ListSandboxes(_ context.Context, req ListSandboxesRequest) (ListSandboxesResult, error) {
	f.calls = append(f.calls, req)
	if f.err != nil {
		return ListSandboxesResult{}, f.err
	}
	return f.result, nil
}

func (f *fakeSandboxLister) CreateSandbox(context.Context, CreateSandboxRequest) (SandboxSummary, error) {
	return SandboxSummary{}, nil
}

func (f *fakeSandboxLister) GetSandbox(context.Context, string) (SandboxSummary, error) {
	return SandboxSummary{}, nil
}

func (f *fakeSandboxLister) DeleteSandbox(context.Context, string) error {
	return nil
}

func (f *fakeSandboxLister) HibernateSandbox(context.Context, string) error {
	return nil
}

func (f *fakeSandboxLister) ResumeSandbox(context.Context, string) (SandboxSummary, error) {
	return SandboxSummary{}, nil
}

func (f *fakeSandboxLister) RunCommand(context.Context, string, CommandRequest) (CommandResult, error) {
	return CommandResult{}, nil
}

func (f *fakeSandboxLister) UploadFile(context.Context, string, string, io.Reader) error {
	return nil
}

func (f *fakeSandboxLister) ListPorts(context.Context, string) ([]PortInfo, error) {
	return nil, nil
}

func (f *fakeSandboxLister) WaitForPortURL(context.Context, string, int) (PortInfo, error) {
	return PortInfo{}, nil
}
