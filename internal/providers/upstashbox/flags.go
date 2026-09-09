package upstashbox

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func RegisterUpstashBoxProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return core.RegisterUpstashBoxConfigFlags(fs, defaults.UpstashBox)
}

func ApplyUpstashBoxProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if cfg.Provider == providerName || cfg.Provider == "upstash" || cfg.Provider == "box" || cfg.Provider == "upstashbox" {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=%s; use --upstash-box-size", providerName)
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=%s; use --upstash-box-runtime", providerName)
		}
	}
	v, ok := values.(core.UpstashBoxConfigFlagValues)
	if !ok {
		return nil
	}
	v.Apply(&cfg.UpstashBox, fs)
	return validateConfig(*cfg)
}

func validateConfig(cfg Config) error {
	if runtime := strings.TrimSpace(cfg.UpstashBox.Runtime); runtime != "" {
		switch runtime {
		case "node", "python", "golang", "ruby", "rust", "node-alpine", "python-alpine", "golang-alpine", "ruby-alpine", "rust-alpine":
		default:
			return exit(2, "invalid upstash-box runtime %q", runtime)
		}
	}
	if size := strings.TrimSpace(cfg.UpstashBox.Size); size != "" {
		switch size {
		case "small", "medium", "large":
		default:
			return exit(2, "invalid upstash-box size %q", size)
		}
	}
	_, err := cleanWorkdir(workdir(cfg))
	return err
}
