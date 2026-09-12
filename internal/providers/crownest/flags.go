package crownest

import core "github.com/openclaw/crabbox/internal/cli"

import (
	"flag"
	"net/url"
	"strings"

	"github.com/openclaw/crabbox/internal/providers/shared"
)

func registerFlags(fs *flag.FlagSet, defaults Config) any {
	return core.RegisterCrownestConfigFlags(fs, defaults.Crownest)
}

func applyFlags(cfg *Config, fs *flag.FlagSet, values any) error {
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

func validateConfig(cfg Config) error {
	if _, err := validateBaseURL(cfg.Crownest.APIURL); err != nil {
		return err
	}
	if cfg.Crownest.TimeoutSecs < 0 {
		return exit(2, "crownest timeoutSecs must be non-negative")
	}
	if strings.TrimSpace(cfg.Crownest.Template) == "" {
		return exit(2, "crownest template must not be empty")
	}
	return nil
}

func validateBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = core.CrownestConfigDefaultAPIURL
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", exit(2, "provider=crownest base URL must be an absolute URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", exit(2, "provider=crownest base URL must not contain userinfo, query parameters, or a fragment")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && shared.IsLoopbackHost(parsed.Hostname())) {
		return "", exit(2, "provider=crownest base URL must use HTTPS except for loopback development endpoints")
	}
	parsed.Host = canonicalHostPort(parsed)
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed.String(), nil
}

func canonicalHostPort(parsed *url.URL) string {
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if (parsed.Scheme == "https" && port == "443") || (parsed.Scheme == "http" && port == "80") {
		port = ""
	}
	if port == "" {
		if strings.Contains(host, ":") {
			return "[" + host + "]"
		}
		return host
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return host + ":" + port
}
