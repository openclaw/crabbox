package crownest

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterCrownestConfigFlags(fs, defaults.Crownest)
}

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	if strings.EqualFold(strings.TrimSpace(cfg.Provider), providerName) {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "use --crownest-template", "use --crownest-template"); err != nil {
			return err
		}
	}
	v, ok := values.(core.CrownestConfigFlagValues)
	if !ok {
		return nil
	}
	applied, err := v.Apply(&cfg.Crownest, fs)
	core.RecordProviderFlagInputs(cfg, applied.InputAccepted, "crownest")
	if err != nil {
		return err
	}
	return validateConfig(*cfg)
}

func validateConfig(cfg core.Config) error {
	if _, err := validateBaseURL(cfg.Crownest.APIURL); err != nil {
		return err
	}
	if cfg.Crownest.TimeoutSecs < 0 {
		return core.Exit(2, "crownest timeoutSecs must be non-negative")
	}
	if strings.TrimSpace(cfg.Crownest.Template) == "" {
		return core.Exit(2, "crownest template must not be empty")
	}
	return nil
}

func validateBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = core.CrownestConfigDefaultAPIURL
	}
	return shared.NormalizeHTTPSBaseURL(raw, shared.EndpointURLErrors{
		Invalid:    core.Exit(2, "provider=crownest base URL must be an absolute URL"),
		Components: core.Exit(2, "provider=crownest base URL must not contain userinfo, query parameters, or a fragment"),
		Insecure:   core.Exit(2, "provider=crownest base URL must use HTTPS except for loopback development endpoints"),
	})
}
