package fastapicloud

import (
	"flag"
	"strings"
)

type fastAPICloudFlagValues struct {
	APIURL *string
	AppID  *string
	TeamID *string
}

// RegisterFastAPICloudProviderFlags exposes only non-secret provider flags.
// Deploy tokens are sourced from FASTAPI_CLOUD_TOKEN /
// CRABBOX_FASTAPI_CLOUD_TOKEN so they are not passed as command-line
// arguments.
func RegisterFastAPICloudProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return fastAPICloudFlagValues{
		APIURL: fs.String("fastapi-cloud-url", defaults.FastAPICloud.APIURL, "FastAPI Cloud API URL"),
		AppID:  fs.String("fastapi-cloud-app-id", defaults.FastAPICloud.AppID, "FastAPI Cloud app ID"),
		TeamID: fs.String("fastapi-cloud-team-id", defaults.FastAPICloud.TeamID, "FastAPI Cloud team ID for listing apps"),
	}
}

func ApplyFastAPICloudProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if isFastAPICloudProviderName(cfg.Provider) {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=%s", providerName)
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=%s", providerName)
		}
	}
	v, ok := values.(fastAPICloudFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "fastapi-cloud-url") {
		cfg.FastAPICloud.APIURL = *v.APIURL
	}
	if flagWasSet(fs, "fastapi-cloud-app-id") {
		cfg.FastAPICloud.AppID = *v.AppID
	}
	if flagWasSet(fs, "fastapi-cloud-team-id") {
		cfg.FastAPICloud.TeamID = *v.TeamID
	}
	return nil
}

func isFastAPICloudProviderName(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case providerName, "fastapicloud", "fastapi":
		return true
	default:
		return false
	}
}
