package runpod

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

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
		APIURL:     fs.String("runpod-url", defaults.Runpod.APIURL, "RunPod REST API URL"),
		CloudType:  fs.String("runpod-cloud-type", defaults.Runpod.CloudType, "RunPod cloud type: SECURE or COMMUNITY"),
		InstanceID: fs.String("runpod-instance-id", defaults.Runpod.InstanceID, "RunPod GPU type ID or CPU flavor ID"),
		Image:      fs.String("runpod-image", defaults.Runpod.Image, "Docker image to deploy on the pod"),
		TemplateID: fs.String("runpod-template-id", defaults.Runpod.TemplateID, "Optional RunPod template ID"),
		DiskGB:     fs.Int("runpod-disk-gb", defaults.Runpod.DiskGB, "Container disk size in GB"),
		User:       fs.String("runpod-user", defaults.Runpod.User, "SSH user for runpod pods"),
		WorkRoot:   fs.String("runpod-work-root", defaults.Runpod.WorkRoot, "remote Crabbox work root on runpod pods"),
	}
}

func ApplyRunpodProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if core.ProviderNameMatches(cfg.Provider, Provider{}) {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "use --runpod-instance-id", "use --runpod-image"); err != nil {
			return err
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
	if core.ProviderNameMatches(cfg.Provider, Provider{}) {
		applyRunpodDefaults(cfg)
	}
	return nil
}
