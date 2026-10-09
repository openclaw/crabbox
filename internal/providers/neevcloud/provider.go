package neevcloud

import (
	"flag"
	"os"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func init() {
	core.RegisterProvider(Provider{})
}

// Provider registers the NeevCloud delegated-run sandbox backend.
type Provider struct{}

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Authentication:             core.DirectProviderAuthentication(core.ProviderAuthenticationAPIKey),
		SyncGuardrailFullCandidate: true,
		Name:                       providerName,
		Family:                     providerName,
		Kind:                       core.ProviderKindDelegatedRun,
		Targets:                    []core.TargetSpec{{OS: core.TargetLinux}},
		Features:                   core.FeatureSet{core.FeatureArchiveSync, core.FeatureRunSession},
		Coordinator:                core.CoordinatorNever,
		ClassDisposition:           core.ProviderClassDispositionUnmapped,
	}
}

func (Provider) ServerTypeForConfig(cfg core.Config) string {
	return core.Blank(cfg.Neevcloud.Template, core.NeevcloudConfigDefaultTemplate)
}

// DiagnosticSecrets returns the env-only API key sources for final redaction.
func (Provider) DiagnosticSecrets(core.Config) []string {
	secrets := make([]string, 0, len(apiKeyEnvNames))
	for _, name := range apiKeyEnvNames {
		secrets = append(secrets, os.Getenv(name))
	}
	return secrets
}

// ClaimScope binds local claims to one API endpoint, organization and project.
func (Provider) ClaimScope(cfg core.Config) string {
	return claimScope(cfg)
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterNeevcloudConfigFlags(fs, defaults.Neevcloud)
}

// ApplyFlags applies explicit --neevcloud-* flags and rejects machine sizing flags.
func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	if cfg.Provider == providerName {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "use --neevcloud-template", "use --neevcloud-template"); err != nil {
			return err
		}
	}
	if ok, err := core.ApplyProviderConfigFlags[core.NeevcloudConfigFlagValues](cfg, fs, values, &cfg.Neevcloud, providerName); !ok || err != nil {
		return err
	}
	return validateConfig(*cfg)
}

func (Provider) ValidateConfig(cfg core.Config) error {
	return validateConfig(cfg)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	cfg.Provider = providerName
	return &backend{spec: p.Spec(), cfg: cfg, rt: rt}, nil
}
