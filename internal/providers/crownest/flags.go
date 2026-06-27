package crownest

import (
	"flag"
	"net"
	"net/url"
	"strings"
)

type flagValues struct {
	APIURL        *string
	ProjectID     *string
	Template      *string
	TimeoutSecs   *int
	ForgetMissing *bool
}

func registerFlags(fs *flag.FlagSet, defaults Config) any {
	return flagValues{
		APIURL:        fs.String("crownest-url", defaults.Crownest.APIURL, "Trusted CrowNest API base URL"),
		ProjectID:     fs.String("crownest-project-id", defaults.Crownest.ProjectID, "CrowNest project ID"),
		Template:      fs.String("crownest-template", defaults.Crownest.Template, "CrowNest Workspace Run template"),
		TimeoutSecs:   fs.Int("crownest-timeout-secs", defaults.Crownest.TimeoutSecs, "CrowNest Workspace Run timeout in seconds"),
		ForgetMissing: fs.Bool("crownest-forget-missing", defaults.Crownest.ForgetMissing, "remove the local claim when stop gets 404"),
	}
}

func applyFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if strings.EqualFold(strings.TrimSpace(cfg.Provider), providerName) {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=crownest; use --crownest-template")
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=crownest; use --crownest-template")
		}
	}
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "crownest-url") {
		cfg.Crownest.APIURL = *v.APIURL
	}
	if flagWasSet(fs, "crownest-project-id") {
		cfg.Crownest.ProjectID = *v.ProjectID
	}
	if flagWasSet(fs, "crownest-template") {
		cfg.Crownest.Template = *v.Template
	}
	if flagWasSet(fs, "crownest-timeout-secs") {
		cfg.Crownest.TimeoutSecs = *v.TimeoutSecs
	}
	if flagWasSet(fs, "crownest-forget-missing") {
		cfg.Crownest.ForgetMissing = *v.ForgetMissing
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
		raw = "https://api.crownest.dev"
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", exit(2, "provider=crownest base URL must be an absolute URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", exit(2, "provider=crownest base URL must not contain userinfo, query parameters, or a fragment")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname())) {
		return "", exit(2, "provider=crownest base URL must use HTTPS except for loopback development endpoints")
	}
	parsed.Host = canonicalHostPort(parsed)
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed.String(), nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
