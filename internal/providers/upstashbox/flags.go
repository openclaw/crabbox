package upstashbox

import (
	"flag"
	"strings"
)

type flagValues struct {
	BaseURL   *string
	Runtime   *string
	Size      *string
	Workdir   *string
	KeepAlive *bool
}

func RegisterUpstashBoxProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return flagValues{
		BaseURL:   fs.String("upstash-box-base-url", defaults.UpstashBox.BaseURL, "Upstash Box API base URL"),
		Runtime:   fs.String("upstash-box-runtime", defaults.UpstashBox.Runtime, "Upstash Box runtime: node, python, golang, ruby, or rust"),
		Size:      fs.String("upstash-box-size", defaults.UpstashBox.Size, "Upstash Box size: small, medium, or large"),
		Workdir:   fs.String("upstash-box-workdir", defaults.UpstashBox.Workdir, "absolute working directory inside the Upstash Box"),
		KeepAlive: fs.Bool("upstash-box-keep-alive", defaults.UpstashBox.KeepAlive, "create Upstash boxes with keepAlive enabled"),
	}
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
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "upstash-box-base-url") {
		cfg.UpstashBox.BaseURL = *v.BaseURL
	}
	if flagWasSet(fs, "upstash-box-runtime") {
		cfg.UpstashBox.Runtime = *v.Runtime
	}
	if flagWasSet(fs, "upstash-box-size") {
		cfg.UpstashBox.Size = *v.Size
	}
	if flagWasSet(fs, "upstash-box-workdir") {
		cfg.UpstashBox.Workdir = *v.Workdir
	}
	if flagWasSet(fs, "upstash-box-keep-alive") {
		cfg.UpstashBox.KeepAlive = *v.KeepAlive
	}
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
