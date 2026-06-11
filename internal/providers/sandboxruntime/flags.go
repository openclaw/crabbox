package sandboxruntime

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

type flagValues struct {
	CLIPath  *string
	Settings *string
	Debug    *bool
}

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	return flagValues{
		CLIPath:  fs.String("sandbox-runtime-cli", defaults.SandboxRuntime.CLIPath, "path to the srt CLI binary"),
		Settings: fs.String("sandbox-runtime-settings", defaults.SandboxRuntime.Settings, "path to an SRT settings JSON file; empty uses SRT defaults"),
		Debug:    fs.Bool("sandbox-runtime-debug", defaults.SandboxRuntime.Debug, "pass --debug to the srt CLI"),
	}
}

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if core.FlagWasSet(fs, "sandbox-runtime-cli") {
		cfg.SandboxRuntime.CLIPath = *v.CLIPath
	}
	if core.FlagWasSet(fs, "sandbox-runtime-settings") {
		cfg.SandboxRuntime.Settings = *v.Settings
	}
	if core.FlagWasSet(fs, "sandbox-runtime-debug") {
		cfg.SandboxRuntime.Debug = *v.Debug
	}
	return validateConfig(*cfg)
}

func validateConfig(cfg Config) error {
	if strings.TrimSpace(cfg.SandboxRuntime.CLIPath) == "" {
		return exit(2, "sandbox-runtime cliPath must not be empty")
	}
	return nil
}
