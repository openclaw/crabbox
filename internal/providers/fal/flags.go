package fal

import "flag"

type falFlagValues struct {
	APIURL       *string
	InstanceType *string
	Sector       *string
	User         *string
	WorkRoot     *string
}

// RegisterFalProviderFlags exposes only non-secret fal settings. The API key is
// intentionally env-only so it cannot leak through shell history or process
// listings.
func RegisterFalProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return falFlagValues{
		APIURL:       fs.String("fal-api-url", defaults.Fal.APIURL, "fal Platform API URL"),
		InstanceType: fs.String("fal-instance-type", defaults.Fal.InstanceType, "fal Compute instance type"),
		Sector:       fs.String("fal-sector", defaults.Fal.Sector, "fal Compute sector for supported multi-node instance types"),
		User:         fs.String("fal-user", defaults.Fal.User, "SSH user for fal Compute instances"),
		WorkRoot:     fs.String("fal-work-root", defaults.Fal.WorkRoot, "remote Crabbox work root on fal Compute instances"),
	}
}

func ApplyFalProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(falFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "fal-api-url") {
		cfg.Fal.APIURL = *v.APIURL
	}
	if flagWasSet(fs, "fal-instance-type") {
		cfg.Fal.InstanceType = *v.InstanceType
	}
	if flagWasSet(fs, "fal-sector") {
		cfg.Fal.Sector = *v.Sector
	}
	if flagWasSet(fs, "fal-user") {
		cfg.Fal.User = *v.User
	}
	if flagWasSet(fs, "fal-work-root") {
		cfg.Fal.WorkRoot = *v.WorkRoot
	}
	if isFalProviderName(cfg.Provider) {
		applyFalDefaults(cfg)
	}
	return nil
}
