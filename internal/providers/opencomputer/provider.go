package opencomputer

import (
	"flag"
	"os"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string      { return providerName }
func (Provider) Aliases() []string { return []string{"oc", "open-computer"} }

func (Provider) DiagnosticSecrets(core.Config) []string {
	fileConfig := readOCFileConfig()
	return []string{
		os.Getenv("CRABBOX_OPENCOMPUTER_API_KEY"),
		os.Getenv("OPENCOMPUTER_API_KEY"),
		fileConfig.APIKey,
	}
}

func (Provider) ServerTypeForConfig(core.Config) string { return "" }
func (Provider) ServerTypeForClass(string) string       { return "" }

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:             providerName,
		Family:           "opencomputer",
		Kind:             core.ProviderKindDelegatedRun,
		Targets:          []core.TargetSpec{{OS: core.TargetLinux}},
		Features:         core.FeatureSet{core.FeatureArchiveSync, core.FeatureRunSession},
		Coordinator:      core.CoordinatorNever,
		ClassDisposition: core.ProviderClassDispositionUnmapped,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return RegisterOpenComputerProviderFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return ApplyOpenComputerProviderFlags(cfg, fs, values)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return NewOpenComputerBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	return shared.ConfigureDoctor("opencomputer", func() (core.Backend, error) { return p.Configure(cfg, rt) })
}
