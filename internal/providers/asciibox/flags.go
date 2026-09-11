package asciibox

import core "github.com/openclaw/crabbox/internal/cli"

import (
	"flag"
)

type flagValues struct {
	BaseURL *string
	CLIPath *string
	Workdir *string
}

func RegisterAsciiBoxProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return flagValues{
		BaseURL: fs.String("ascii-box-base-url", defaults.AsciiBox.BaseURL, "trusted ASCII Box API base URL"),
		CLIPath: fs.String("ascii-box-cli", defaults.AsciiBox.CLIPath, "ASCII Box CLI path"),
		Workdir: fs.String("ascii-box-workdir", defaults.AsciiBox.Workdir, "absolute working directory inside the ASCII Box"),
	}
}

func ApplyAsciiBoxProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if cfg.Provider == providerName || cfg.Provider == "ascii" || cfg.Provider == "asciibox" || cfg.Provider == "ascii-box" {
		if core.FlagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=%s", providerName)
		}
		if core.FlagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=%s", providerName)
		}
	}
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if core.FlagWasSet(fs, "ascii-box-base-url") {
		cfg.AsciiBox.BaseURL = *v.BaseURL
		core.RecordProviderFlagInputs(cfg, true, "ascii-box")
	}
	if core.FlagWasSet(fs, "ascii-box-cli") {
		cfg.AsciiBox.CLIPath = *v.CLIPath
		core.RecordProviderFlagInputs(cfg, true, "ascii-box")
	}
	if core.FlagWasSet(fs, "ascii-box-workdir") {
		cfg.AsciiBox.Workdir = *v.Workdir
		core.RecordProviderFlagInputs(cfg, true, "ascii-box")
	}
	if cfg.Provider == providerName || cfg.Provider == "ascii" || cfg.Provider == "asciibox" || cfg.Provider == "ascii-box" {
		cleaned, err := cleanWorkdir(workdir(*cfg))
		if err != nil {
			return err
		}
		cfg.WorkRoot = cleaned
	}
	return nil
}
