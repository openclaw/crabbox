package asciibox

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

type flagValues struct {
	NoEnv   *bool
	BaseURL *string
	CLIPath *string
	Workdir *string
}

func RegisterAsciiBoxProviderFlags(fs *flag.FlagSet, defaults core.Config) any {
	return flagValues{
		NoEnv:   fs.Bool("ascii-box-no-env", defaults.AsciiBox.NoEnv, "create without Boat account environment secrets"),
		BaseURL: fs.String("ascii-box-base-url", defaults.AsciiBox.BaseURL, "trusted ASCII Box API base URL"),
		CLIPath: fs.String("ascii-box-cli", defaults.AsciiBox.CLIPath, "ASCII Box CLI path"),
		Workdir: fs.String("ascii-box-workdir", defaults.AsciiBox.Workdir, "absolute working directory inside the ASCII Box"),
	}
}

func ApplyAsciiBoxProviderFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if core.FlagWasSet(fs, "ascii-box-no-env") {
		cfg.AsciiBox.NoEnv = *v.NoEnv
		core.RecordProviderFlagInputs(cfg, true, "ascii-box")
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
