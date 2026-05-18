package railway

import (
	"flag"
	"strings"
)

type railwayFlagValues struct {
	APIURL        *string
	ProjectID     *string
	EnvironmentID *string
}

// RegisterRailwayProviderFlags exposes railway-specific flags. The API token is
// intentionally not surfaced as a flag because secrets must not be passed as
// command-line arguments; it is sourced from RAILWAY_API_TOKEN /
// CRABBOX_RAILWAY_API_TOKEN.
func RegisterRailwayProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return railwayFlagValues{
		APIURL:        fs.String("railway-url", defaults.Railway.APIURL, "Railway GraphQL API URL"),
		ProjectID:     fs.String("railway-project", defaults.Railway.ProjectID, "Railway project ID containing the target service"),
		EnvironmentID: fs.String("railway-environment", defaults.Railway.EnvironmentID, "Railway environment ID to deploy into"),
	}
}

func ApplyRailwayProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if isRailwayProviderName(cfg.Provider) {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=%s", providerName)
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=%s", providerName)
		}
	}
	v, ok := values.(railwayFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "railway-url") {
		cfg.Railway.APIURL = *v.APIURL
	}
	if flagWasSet(fs, "railway-project") {
		cfg.Railway.ProjectID = *v.ProjectID
	}
	if flagWasSet(fs, "railway-environment") {
		cfg.Railway.EnvironmentID = *v.EnvironmentID
	}
	return nil
}

func isRailwayProviderName(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case providerName, "rail", "railwayapp":
		return true
	default:
		return false
	}
}
