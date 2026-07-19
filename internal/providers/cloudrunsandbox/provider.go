package cloudrunsandbox

import (
	"flag"
	"os"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string { return providerName }

func (Provider) Aliases() []string {
	return []string{"gcrun-sandbox", "google-cloud-run-sandbox", "cloudrun-sandbox"}
}

func (Provider) DiagnosticSecrets(core.Config) []string {
	return []string{
		os.Getenv("CRABBOX_CLOUD_RUN_SANDBOX_SECRET"),
		os.Getenv("CLOUD_RUN_SANDBOX_SECRET"),
		os.Getenv("CRABBOX_CLOUD_RUN_SANDBOX_AUTH_TOKEN"),
		os.Getenv("CLOUD_RUN_AUTH_TOKEN"),
	}
}

// ServerTypeForConfig / ServerTypeForClass: Cloud Run sandboxes share the
// parent service resources; there is no Crabbox class/type surface.
func (Provider) ServerTypeForConfig(core.Config) string { return "" }
func (Provider) ServerTypeForClass(string) string       { return "" }

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:        providerName,
		Family:      providerFamily,
		Kind:        core.ProviderKindDelegatedRun,
		Targets:     []core.TargetSpec{{OS: core.TargetLinux}},
		Features:    core.FeatureSet{core.FeatureArchiveSync, core.FeatureCleanup, core.FeatureRunSession},
		Coordinator: core.CoordinatorNever,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return RegisterCloudRunSandboxProviderFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return ApplyCloudRunSandboxProviderFlags(cfg, fs, values)
}

func (Provider) ValidateConfig(cfg core.Config) error {
	return validateConfig(cfg)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if err := p.ValidateConfig(cfg); err != nil {
		return nil, err
	}
	return NewBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	doctor, ok := backend.(core.DoctorBackend)
	if !ok {
		return nil, core.Exit(2, "cloud-run-sandbox doctor backend unavailable")
	}
	return doctor, nil
}

func validateConfig(cfg Config) error {
	if workdir := strings.TrimSpace(cfg.CloudRunSandbox.Workdir); workdir != "" && !strings.HasPrefix(workdir, "/") {
		return exit(2, "cloudRunSandbox.workdir must be an absolute path")
	}
	if cli := strings.TrimSpace(cfg.CloudRunSandbox.CLIPath); cli == "" {
		return exit(2, "cloudRunSandbox.cliPath must not be empty")
	}
	return nil
}
