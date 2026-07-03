package vast

import "flag"

type vastFlagValues struct {
	APIURL         *string
	InstanceType   *string
	GPUName        *string
	GPUCount       *int
	Image          *string
	TemplateID     *string
	Runtype        *string
	DiskGB         *int
	MaxDphTotal    *float64
	MinReliability *float64
	Order          *string
	User           *string
	WorkRoot       *string
	ReleaseAction  *string
}

// RegisterVastProviderFlags exposes only non-secret Vast settings. API keys
// are sourced from CRABBOX_VAST_API_KEY / VAST_API_KEY and never argv.
func RegisterVastProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return vastFlagValues{
		APIURL:         fs.String("vast-api-url", defaults.Vast.APIURL, "Vast.ai REST API URL"),
		InstanceType:   fs.String("vast-instance-type", defaults.Vast.InstanceType, "Vast.ai offer type: ondemand or interruptible"),
		GPUName:        fs.String("vast-gpu-name", defaults.Vast.GPUName, "Vast.ai GPU name selector"),
		GPUCount:       fs.Int("vast-gpu-count", defaults.Vast.GPUCount, "Vast.ai minimum GPU count"),
		Image:          fs.String("vast-image", defaults.Vast.Image, "Docker image to deploy on the instance"),
		TemplateID:     fs.String("vast-template-id", defaults.Vast.TemplateID, "Optional Vast.ai template ID"),
		Runtype:        fs.String("vast-runtype", defaults.Vast.Runtype, "Vast.ai runtime type: ssh_direct"),
		DiskGB:         fs.Int("vast-disk-gb", defaults.Vast.DiskGB, "Instance disk size in GB"),
		MaxDphTotal:    fs.Float64("vast-max-dph-total", defaults.Vast.MaxDphTotal, "Maximum total dollars per hour"),
		MinReliability: fs.Float64("vast-min-reliability", defaults.Vast.MinReliability, "Minimum reliability score from 0 to 1"),
		Order:          fs.String("vast-order", defaults.Vast.Order, "Vast.ai offer ordering expression"),
		User:           fs.String("vast-user", defaults.Vast.User, "SSH user for Vast.ai instances"),
		WorkRoot:       fs.String("vast-work-root", defaults.Vast.WorkRoot, "remote Crabbox work root on Vast.ai instances"),
		ReleaseAction:  fs.String("vast-release-action", defaults.Vast.ReleaseAction, "Vast.ai release action: destroy, stop, or keep"),
	}
}

func ApplyVastProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if isVastProviderName(cfg.Provider) {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=%s; use --vast-gpu-name or --vast-gpu-count", providerName)
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=%s; use --vast-image", providerName)
		}
	}
	v, ok := values.(vastFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "vast-api-url") {
		cfg.Vast.APIURL = *v.APIURL
	}
	if flagWasSet(fs, "vast-instance-type") {
		cfg.Vast.InstanceType = normalizeInstanceType(*v.InstanceType)
	}
	if flagWasSet(fs, "vast-gpu-name") {
		cfg.Vast.GPUName = *v.GPUName
	}
	if flagWasSet(fs, "vast-gpu-count") {
		cfg.Vast.GPUCount = *v.GPUCount
	}
	if flagWasSet(fs, "vast-image") {
		cfg.Vast.Image = *v.Image
	}
	if flagWasSet(fs, "vast-template-id") {
		cfg.Vast.TemplateID = *v.TemplateID
	}
	if flagWasSet(fs, "vast-runtype") {
		cfg.Vast.Runtype = *v.Runtype
	}
	if flagWasSet(fs, "vast-disk-gb") {
		cfg.Vast.DiskGB = *v.DiskGB
	}
	if flagWasSet(fs, "vast-max-dph-total") {
		cfg.Vast.MaxDphTotal = *v.MaxDphTotal
	}
	if flagWasSet(fs, "vast-min-reliability") {
		cfg.Vast.MinReliability = *v.MinReliability
	}
	if flagWasSet(fs, "vast-order") {
		cfg.Vast.Order = *v.Order
	}
	if flagWasSet(fs, "vast-user") {
		cfg.Vast.User = *v.User
	}
	if flagWasSet(fs, "vast-work-root") {
		cfg.Vast.WorkRoot = *v.WorkRoot
		markVastWorkRootExplicit(cfg)
	}
	if flagWasSet(fs, "vast-release-action") {
		cfg.Vast.ReleaseAction = *v.ReleaseAction
		markReleaseActionExplicit(cfg)
	}
	if isVastProviderName(cfg.Provider) {
		return Provider{}.ValidateConfig(*cfg)
	}
	return nil
}
