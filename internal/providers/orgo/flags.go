package orgo

import (
	"flag"
	"strings"
)

type orgoFlagValues struct {
	APIBase     *string
	WorkspaceID *string
	RAMGB       *int
	CPUs        *int
	DiskGB      *int
	Resolution  *string
}

// RegisterOrgoProviderFlags exposes non-secret Orgo settings. The API key is
// intentionally not a flag; secrets are read from env/config only.
func RegisterOrgoProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return orgoFlagValues{
		APIBase:     fs.String("orgo-api-base", defaults.Orgo.APIBase, "Orgo API base URL"),
		WorkspaceID: fs.String("orgo-workspace-id", defaults.Orgo.WorkspaceID, "Existing Orgo workspace ID to create computers in"),
		RAMGB:       fs.Int("orgo-ram", defaults.Orgo.RAMGB, "Orgo computer RAM in GB"),
		CPUs:        fs.Int("orgo-cpu", defaults.Orgo.CPUs, "Orgo computer CPU count"),
		DiskGB:      fs.Int("orgo-disk", defaults.Orgo.DiskGB, "Orgo computer disk size in GB"),
		Resolution:  fs.String("orgo-resolution", defaults.Orgo.Resolution, "Orgo desktop resolution, for example 1280x720x24"),
	}
}

func ApplyOrgoProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case providerName, "orgo-ai":
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=%s", providerName)
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=%s", providerName)
		}
	}
	v, ok := values.(orgoFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "orgo-api-base") {
		cfg.Orgo.APIBase = *v.APIBase
	}
	if flagWasSet(fs, "orgo-workspace-id") {
		cfg.Orgo.WorkspaceID = *v.WorkspaceID
	}
	if flagWasSet(fs, "orgo-ram") {
		cfg.Orgo.RAMGB = *v.RAMGB
	}
	if flagWasSet(fs, "orgo-cpu") {
		cfg.Orgo.CPUs = *v.CPUs
	}
	if flagWasSet(fs, "orgo-disk") {
		cfg.Orgo.DiskGB = *v.DiskGB
	}
	if flagWasSet(fs, "orgo-resolution") {
		cfg.Orgo.Resolution = *v.Resolution
	}
	return nil
}
