package machine0

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

type flagValues struct {
	CLIPath       *string
	Image         *string
	ImageVersion  *int
	DesktopImage  *string
	Size          *string
	Region        *string
	Key           *string
	WorkRoot      *string
	ReleasePolicy *string
	CreateTimeout *string
	PollInterval  *string
}

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	m := defaults.Machine0
	return flagValues{
		CLIPath:       fs.String("machine0-cli", m.CLIPath, "Machine0 CLI path"),
		Image:         fs.String("machine0-image", m.Image, "Machine0 image name"),
		ImageVersion:  fs.Int("machine0-image-version", m.ImageVersion, "Machine0 image version; 0 uses the active version"),
		DesktopImage:  fs.String("machine0-desktop-image", m.DesktopImage, "optional prepared Machine0 image for --desktop leases"),
		Size:          fs.String("machine0-size", m.Size, "Machine0 live-catalog size slug"),
		Region:        fs.String("machine0-region", m.Region, "Machine0 region"),
		Key:           fs.String("machine0-key", m.Key, "Machine0 registered SSH key name; empty uses the Machine0 default"),
		WorkRoot:      fs.String("machine0-work-root", m.WorkRoot, "remote Crabbox work root"),
		ReleasePolicy: fs.String("machine0-release-policy", m.ReleasePolicy, "release policy: destroy or explicit suspend"),
		CreateTimeout: fs.String("machine0-create-timeout", m.CreateTimeout.String(), "Machine0 creation timeout"),
		PollInterval:  fs.String("machine0-poll-interval", m.PollInterval.String(), "Machine0 status polling interval"),
	}
}

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "machine0-cli") {
		cfg.Machine0.CLIPath = *v.CLIPath
	}
	if flagWasSet(fs, "machine0-image") {
		cfg.Machine0.Image = *v.Image
	}
	if flagWasSet(fs, "machine0-image-version") {
		cfg.Machine0.ImageVersion = *v.ImageVersion
	}
	if flagWasSet(fs, "machine0-desktop-image") {
		cfg.Machine0.DesktopImage = *v.DesktopImage
	}
	if flagWasSet(fs, "machine0-size") {
		cfg.Machine0.Size = *v.Size
		cfg.Machine0.SizeExplicit = true
		cfg.ServerType = *v.Size
		cfg.ServerTypeExplicit = true
	}
	if flagWasSet(fs, "machine0-region") {
		cfg.Machine0.Region = *v.Region
	}
	if flagWasSet(fs, "machine0-key") {
		cfg.Machine0.Key = *v.Key
	}
	if flagWasSet(fs, "machine0-work-root") {
		cfg.Machine0.WorkRoot = *v.WorkRoot
		cfg.WorkRoot = *v.WorkRoot
	}
	if flagWasSet(fs, "machine0-release-policy") {
		cfg.Machine0.ReleasePolicy = *v.ReleasePolicy
	}
	if flagWasSet(fs, "machine0-create-timeout") {
		if err := core.ApplyLeaseDuration(&cfg.Machine0.CreateTimeout, *v.CreateTimeout); err != nil {
			return err
		}
	}
	if flagWasSet(fs, "machine0-poll-interval") {
		if err := core.ApplyLeaseDuration(&cfg.Machine0.PollInterval, *v.PollInterval); err != nil {
			return err
		}
	}
	if strings.EqualFold(strings.TrimSpace(cfg.Provider), providerName) {
		applyDefaults(cfg)
	}
	return nil
}
