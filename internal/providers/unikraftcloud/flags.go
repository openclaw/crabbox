package unikraftcloud

import (
	"flag"
	"strings"
)

type unikraftCloudFlagValues struct {
	APIURL   *string
	Metro    *string
	Image    *string
	MemoryMB *int
}

// registerUnikraftCloudProviderFlags exposes only non-secret provider flags.
// The API key is sourced from CRABBOX_UNIKRAFT_CLOUD_API_KEY /
// UNIKRAFT_CLOUD_API_KEY / UKC_API_KEY / UKC_TOKEN or the unikraftCloud.apiKey
// config key so it is never passed as a command-line argument.
func registerUnikraftCloudProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return unikraftCloudFlagValues{
		APIURL:   fs.String("unikraft-cloud-url", defaults.UnikraftCloud.APIURL, "Unikraft Cloud API URL override (default derived from the metro)"),
		Metro:    fs.String("unikraft-cloud-metro", defaults.UnikraftCloud.Metro, "Unikraft Cloud metro (fra, dal, sin, was, sfo)"),
		Image:    fs.String("unikraft-cloud-image", defaults.UnikraftCloud.Image, "OCI image reference for warmup-created instances"),
		MemoryMB: fs.Int("unikraft-cloud-memory", defaults.UnikraftCloud.MemoryMB, "instance memory in MB for warmup-created instances"),
	}
}

func applyUnikraftCloudProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if isUnikraftCloudProviderName(cfg.Provider) {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=%s", providerName)
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=%s", providerName)
		}
	}
	v, ok := values.(unikraftCloudFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "unikraft-cloud-url") {
		cfg.UnikraftCloud.APIURL = *v.APIURL
	}
	if flagWasSet(fs, "unikraft-cloud-metro") {
		cfg.UnikraftCloud.Metro = *v.Metro
	}
	if flagWasSet(fs, "unikraft-cloud-image") {
		cfg.UnikraftCloud.Image = *v.Image
	}
	if flagWasSet(fs, "unikraft-cloud-memory") {
		cfg.UnikraftCloud.MemoryMB = *v.MemoryMB
	}
	return nil
}

func isUnikraftCloudProviderName(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case providerName, "unikraftcloud", "ukc":
		return true
	default:
		return false
	}
}
