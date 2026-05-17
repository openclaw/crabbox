package exedev

import "flag"

type exeDevFlagValues struct {
	APIURL *string
}

// RegisterExeDevProviderFlags exposes exe.dev-specific flags. The API key is
// intentionally not surfaced as a flag because secrets must not be passed as
// command-line arguments; it is sourced from EXE_API_KEY / CRABBOX_EXE_API_KEY.
func RegisterExeDevProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return exeDevFlagValues{
		APIURL: fs.String("exe-dev-url", defaults.ExeDev.APIURL, "exe.dev API URL"),
	}
}

func ApplyExeDevProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if cfg.Provider == providerName {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=%s", providerName)
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=%s", providerName)
		}
	}
	v, ok := values.(exeDevFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "exe-dev-url") {
		cfg.ExeDev.APIURL = *v.APIURL
	}
	return nil
}
