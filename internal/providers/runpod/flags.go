package runpod

import "flag"

type runpodFlagValues struct {
	APIURL     *string
	CloudType  *string
	InstanceID *string
	Image      *string
	TemplateID *string
	DiskGB     *int
	User       *string
	WorkRoot   *string
}

// RegisterRunpodProviderFlags exposes runpod-specific flags. The API key is
// intentionally not surfaced as a flag because secrets must not be passed as
// command-line arguments; it is sourced from RUNPOD_API_KEY /
// CRABBOX_RUNPOD_API_KEY.
func RegisterRunpodProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return runpodFlagValues{
		APIURL:     fs.String("runpod-url", defaults.Runpod.APIURL, "RunPod GraphQL API URL"),
		CloudType:  fs.String("runpod-cloud-type", defaults.Runpod.CloudType, "RunPod cloud type: ALL, SECURE, or COMMUNITY"),
		InstanceID: fs.String("runpod-instance-id", defaults.Runpod.InstanceID, "RunPod CPU instance flavor ID, e.g. cpu3c-2-4"),
		Image:      fs.String("runpod-image", defaults.Runpod.Image, "Docker image to deploy on the pod"),
		TemplateID: fs.String("runpod-template-id", defaults.Runpod.TemplateID, "Optional RunPod template ID"),
		DiskGB:     fs.Int("runpod-disk-gb", defaults.Runpod.DiskGB, "Container disk size in GB"),
		User:       fs.String("runpod-user", defaults.Runpod.User, "SSH user for runpod pods"),
		WorkRoot:   fs.String("runpod-work-root", defaults.Runpod.WorkRoot, "remote Crabbox work root on runpod pods"),
	}
}

func ApplyRunpodProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if isRunpodProviderName(cfg.Provider) {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=%s; use --runpod-instance-id", providerName)
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=%s; use --runpod-image", providerName)
		}
	}
	v, ok := values.(runpodFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "runpod-url") {
		cfg.Runpod.APIURL = *v.APIURL
	}
	if flagWasSet(fs, "runpod-cloud-type") {
		cfg.Runpod.CloudType = *v.CloudType
	}
	if flagWasSet(fs, "runpod-instance-id") {
		cfg.Runpod.InstanceID = *v.InstanceID
	}
	if flagWasSet(fs, "runpod-image") {
		cfg.Runpod.Image = *v.Image
	}
	if flagWasSet(fs, "runpod-template-id") {
		cfg.Runpod.TemplateID = *v.TemplateID
	}
	if flagWasSet(fs, "runpod-disk-gb") {
		cfg.Runpod.DiskGB = *v.DiskGB
	}
	if flagWasSet(fs, "runpod-user") {
		cfg.Runpod.User = *v.User
	}
	if flagWasSet(fs, "runpod-work-root") {
		cfg.Runpod.WorkRoot = *v.WorkRoot
	}
	if isRunpodProviderName(cfg.Provider) {
		applyRunpodDefaults(cfg)
	}
	return nil
}
