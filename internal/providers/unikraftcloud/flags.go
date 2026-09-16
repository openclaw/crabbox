package unikraftcloud

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
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
func registerUnikraftCloudProviderFlags(fs *flag.FlagSet, defaults core.Config) any {
	return unikraftCloudFlagValues{
		APIURL:   fs.String("unikraft-cloud-url", defaults.UnikraftCloud.APIURL, "Unikraft Cloud API URL override (default derived from the metro)"),
		Metro:    fs.String("unikraft-cloud-metro", defaults.UnikraftCloud.Metro, "Unikraft Cloud metro (fra, dal, sin, was, sfo)"),
		Image:    fs.String("unikraft-cloud-image", defaults.UnikraftCloud.Image, "OCI image reference for warmup-created instances"),
		MemoryMB: fs.Int("unikraft-cloud-memory", defaults.UnikraftCloud.MemoryMB, "instance memory in MB for warmup-created instances"),
	}
}

func applyUnikraftCloudProviderFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	if core.ProviderNameMatches(cfg.Provider, Provider{}) {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "", ""); err != nil {
			return err
		}
	}
	v, ok := values.(unikraftCloudFlagValues)
	if !ok {
		return nil
	}
	if core.FlagWasSet(fs, "unikraft-cloud-url") {
		cfg.UnikraftCloud.APIURL = *v.APIURL
		core.RecordProviderFlagInputs(cfg, true, "unikraft-cloud")
	}
	if core.FlagWasSet(fs, "unikraft-cloud-metro") {
		cfg.UnikraftCloud.Metro = *v.Metro
		core.RecordProviderFlagInputs(cfg, true, "unikraft-cloud")
	}
	if core.FlagWasSet(fs, "unikraft-cloud-image") {
		cfg.UnikraftCloud.Image = *v.Image
		core.RecordProviderFlagInputs(cfg, true, "unikraft-cloud")
	}
	if core.FlagWasSet(fs, "unikraft-cloud-memory") {
		cfg.UnikraftCloud.MemoryMB = *v.MemoryMB
		core.RecordProviderFlagInputs(cfg, true, "unikraft-cloud")
	}
	return nil
}
