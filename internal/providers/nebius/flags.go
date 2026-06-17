package nebius

import (
	"flag"
	"strings"
)

type nebiusFlagValues struct {
	CLI              *string
	Profile          *string
	ParentID         *string
	SubnetID         *string
	Platform         *string
	Preset           *string
	ImageFamily      *string
	DiskType         *string
	DiskSizeGiB      *int
	User             *string
	PublicIP         *string
	SecurityGroupIDs *string
	ServiceAccountID *string
	RecoveryPolicy   *string
}

// RegisterNebiusProviderFlags exposes only non-secret Nebius settings.
// Authentication is owned by Nebius CLI profiles, not Crabbox argv.
func RegisterNebiusProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return nebiusFlagValues{
		CLI:              fs.String("nebius-cli", defaults.Nebius.CLI, "Nebius CLI path"),
		Profile:          fs.String("nebius-profile", defaults.Nebius.Profile, "Nebius CLI profile name"),
		ParentID:         fs.String("nebius-parent-id", defaults.Nebius.ParentID, "Nebius parent/project ID"),
		SubnetID:         fs.String("nebius-subnet-id", defaults.Nebius.SubnetID, "Nebius subnet ID"),
		Platform:         fs.String("nebius-platform", defaults.Nebius.Platform, "Nebius compute platform"),
		Preset:           fs.String("nebius-preset", defaults.Nebius.Preset, "Nebius compute preset"),
		ImageFamily:      fs.String("nebius-image-family", defaults.Nebius.ImageFamily, "Nebius boot image family"),
		DiskType:         fs.String("nebius-disk-type", defaults.Nebius.DiskType, "Nebius boot disk type"),
		DiskSizeGiB:      fs.Int("nebius-disk-size-gib", defaults.Nebius.DiskSizeGiB, "Nebius boot disk size in GiB"),
		User:             fs.String("nebius-user", defaults.Nebius.User, "SSH user for Nebius VMs"),
		PublicIP:         fs.String("nebius-public-ip", defaults.Nebius.PublicIP, "Nebius public IP mode: dynamic or none"),
		SecurityGroupIDs: fs.String("nebius-security-group-ids", "", "comma-separated Nebius security group IDs"),
		ServiceAccountID: fs.String("nebius-service-account-id", defaults.Nebius.ServiceAccountID, "Nebius service account ID for VMs"),
		RecoveryPolicy:   fs.String("nebius-recovery-policy", defaults.Nebius.RecoveryPolicy, "Nebius create recovery policy: fail"),
	}
}

func ApplyNebiusProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(nebiusFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "nebius-cli") {
		cfg.Nebius.CLI = *v.CLI
	}
	if flagWasSet(fs, "nebius-profile") {
		cfg.Nebius.Profile = *v.Profile
	}
	if flagWasSet(fs, "nebius-parent-id") {
		cfg.Nebius.ParentID = *v.ParentID
	}
	if flagWasSet(fs, "nebius-subnet-id") {
		cfg.Nebius.SubnetID = *v.SubnetID
	}
	if flagWasSet(fs, "nebius-platform") {
		cfg.Nebius.Platform = *v.Platform
	}
	if flagWasSet(fs, "nebius-preset") {
		cfg.Nebius.Preset = *v.Preset
	}
	if flagWasSet(fs, "nebius-image-family") {
		cfg.Nebius.ImageFamily = *v.ImageFamily
	}
	if flagWasSet(fs, "nebius-disk-type") {
		cfg.Nebius.DiskType = *v.DiskType
	}
	if flagWasSet(fs, "nebius-disk-size-gib") {
		cfg.Nebius.DiskSizeGiB = *v.DiskSizeGiB
	}
	if flagWasSet(fs, "nebius-user") {
		cfg.Nebius.User = *v.User
	}
	if flagWasSet(fs, "nebius-public-ip") {
		cfg.Nebius.PublicIP = *v.PublicIP
	}
	if flagWasSet(fs, "nebius-security-group-ids") {
		cfg.Nebius.SecurityGroupIDs = splitCommaList(*v.SecurityGroupIDs)
	}
	if flagWasSet(fs, "nebius-service-account-id") {
		cfg.Nebius.ServiceAccountID = *v.ServiceAccountID
	}
	if flagWasSet(fs, "nebius-recovery-policy") {
		cfg.Nebius.RecoveryPolicy = *v.RecoveryPolicy
	}
	if cfg.Provider == providerName {
		return Provider{}.ValidateConfig(*cfg)
	}
	return nil
}

func splitCommaList(value string) []string {
	var out []string
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}
