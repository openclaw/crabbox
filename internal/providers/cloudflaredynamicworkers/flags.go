package cloudflaredynamicworkers

import (
	"flag"
	"strings"
)

type flagValues struct {
	URL                *string
	CompatibilityDate  *string
	CompatibilityFlags *string
	CacheMode          *string
	Egress             *string
	CPUMs              *int
	Subrequests        *int
	TimeoutSecs        *int
}

func RegisterProviderFlags(fs *flag.FlagSet, defaults Config) any {
	cfg := defaults.CloudflareDynamicWorkers
	return flagValues{
		URL:                fs.String("cloudflare-dynamic-workers-url", cfg.LoaderURL, "Cloudflare Dynamic Workers loader URL"),
		CompatibilityDate:  fs.String("cloudflare-dynamic-workers-compatibility-date", cfg.CompatibilityDate, "Cloudflare Workers compatibility date"),
		CompatibilityFlags: fs.String("cloudflare-dynamic-workers-compatibility-flags", strings.Join(cfg.CompatibilityFlags, ","), "comma-separated Cloudflare Workers compatibility flags"),
		CacheMode:          fs.String("cloudflare-dynamic-workers-cache", cfg.CacheMode, "Dynamic Workers cache mode: one-shot, stable, or explicit"),
		Egress:             fs.String("cloudflare-dynamic-workers-egress", cfg.Egress, "Dynamic Workers egress mode: blocked or intercept"),
		CPUMs:              fs.Int("cloudflare-dynamic-workers-cpu-ms", cfg.CPUMs, "Dynamic Workers CPU limit in milliseconds"),
		Subrequests:        fs.Int("cloudflare-dynamic-workers-subrequests", cfg.Subrequests, "Dynamic Workers subrequest limit"),
		TimeoutSecs:        fs.Int("cloudflare-dynamic-workers-timeout-secs", cfg.TimeoutSecs, "loader request timeout in seconds"),
	}
}

func ApplyProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if cfg.Provider == providerName || cfg.Provider == "cf-dynamic" || cfg.Provider == "cfdw" {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=%s", providerName)
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=%s", providerName)
		}
		if flagWasSet(fs, "expose") {
			return exit(2, "--expose is not supported for provider=%s", providerName)
		}
	}
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "cloudflare-dynamic-workers-url") {
		cfg.CloudflareDynamicWorkers.LoaderURL = *v.URL
	}
	if flagWasSet(fs, "cloudflare-dynamic-workers-compatibility-date") {
		cfg.CloudflareDynamicWorkers.CompatibilityDate = *v.CompatibilityDate
	}
	if flagWasSet(fs, "cloudflare-dynamic-workers-compatibility-flags") {
		cfg.CloudflareDynamicWorkers.CompatibilityFlags = splitCommaList(*v.CompatibilityFlags)
	}
	if flagWasSet(fs, "cloudflare-dynamic-workers-cache") {
		cfg.CloudflareDynamicWorkers.CacheMode = *v.CacheMode
	}
	if flagWasSet(fs, "cloudflare-dynamic-workers-egress") {
		cfg.CloudflareDynamicWorkers.Egress = *v.Egress
	}
	if flagWasSet(fs, "cloudflare-dynamic-workers-cpu-ms") {
		cfg.CloudflareDynamicWorkers.CPUMs = *v.CPUMs
	}
	if flagWasSet(fs, "cloudflare-dynamic-workers-subrequests") {
		cfg.CloudflareDynamicWorkers.Subrequests = *v.Subrequests
	}
	if flagWasSet(fs, "cloudflare-dynamic-workers-timeout-secs") {
		cfg.CloudflareDynamicWorkers.TimeoutSecs = *v.TimeoutSecs
	}
	return validateProviderConfig(*cfg)
}

func validateProviderConfig(cfg Config) error {
	switch normalizeCacheMode(cfg.CloudflareDynamicWorkers.CacheMode) {
	case "one-shot", "stable", "explicit":
	default:
		return exit(2, "invalid %s cache mode %q", providerName, cfg.CloudflareDynamicWorkers.CacheMode)
	}
	switch normalizeEgress(cfg.CloudflareDynamicWorkers.Egress) {
	case "blocked", "intercept":
	default:
		return exit(2, "invalid %s egress mode %q", providerName, cfg.CloudflareDynamicWorkers.Egress)
	}
	if cfg.CloudflareDynamicWorkers.CPUMs < 0 {
		return exit(2, "%s cpu-ms must be non-negative", providerName)
	}
	if cfg.CloudflareDynamicWorkers.Subrequests < 0 {
		return exit(2, "%s subrequests must be non-negative", providerName)
	}
	if cfg.CloudflareDynamicWorkers.TimeoutSecs < 0 {
		return exit(2, "%s timeout-secs must be non-negative", providerName)
	}
	return nil
}

func splitCommaList(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
