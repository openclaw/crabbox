package applevz

import (
	"flag"
	"strings"

	"github.com/openclaw/crabbox/internal/applevzhelper"
	core "github.com/openclaw/crabbox/internal/cli"
)

type flagValues struct {
	HelperPath  *string
	Image       *string
	ImageSHA256 *string
	User        *string
	WorkRoot    *string
	CPUs        *int
	MemoryMiB   *int
	DiskGiB     *int
}

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	return flagValues{
		HelperPath:  fs.String("apple-vz-helper", defaults.AppleVZ.HelperPath, "apple-vz helper binary path"),
		Image:       fs.String("apple-vz-image", applevzhelper.ImageIdentity(defaults.AppleVZ.Image, defaults.AppleVZ.ImageSHA256), "apple-vz local source image path"),
		ImageSHA256: fs.String("apple-vz-image-sha256", defaults.AppleVZ.ImageSHA256, "expected SHA-256 for apple-vz source image downloads"),
		User:        fs.String("apple-vz-user", defaults.AppleVZ.User, "SSH user created inside apple-vz leases"),
		WorkRoot:    fs.String("apple-vz-work-root", defaults.AppleVZ.WorkRoot, "remote Crabbox work root inside apple-vz leases"),
		CPUs:        fs.Int("apple-vz-cpus", defaults.AppleVZ.CPUs, "CPU count for apple-vz leases"),
		MemoryMiB:   fs.Int("apple-vz-memory", defaults.AppleVZ.MemoryMiB, "memory in MiB for apple-vz leases"),
		DiskGiB:     fs.Int("apple-vz-disk", defaults.AppleVZ.DiskGiB, "disk size in GiB for apple-vz leases"),
	}
}

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if core.FlagWasSet(fs, "apple-vz-helper") {
		cfg.AppleVZ.HelperPath = strings.TrimSpace(*v.HelperPath)
	}
	if core.FlagWasSet(fs, "apple-vz-image") {
		image := strings.TrimSpace(*v.Image)
		if applevzhelper.IsRemoteImageRef(image) {
			return exit(2, "--apple-vz-image accepts local paths only; use CRABBOX_APPLE_VZ_IMAGE or configuration for remote URLs")
		}
		cfg.AppleVZ.Image = image
		cfg.AppleVZ.ImageSHA256 = ""
		core.MarkAppleVZImageExplicit(cfg)
	}
	if core.FlagWasSet(fs, "apple-vz-image-sha256") {
		cfg.AppleVZ.ImageSHA256 = strings.TrimSpace(*v.ImageSHA256)
		core.MarkAppleVZImageSHA256Explicit(cfg)
	}
	if core.FlagWasSet(fs, "apple-vz-user") {
		cfg.AppleVZ.User = strings.TrimSpace(*v.User)
		cfg.SSHUser = cfg.AppleVZ.User
	}
	if core.FlagWasSet(fs, "apple-vz-work-root") {
		cfg.AppleVZ.WorkRoot = strings.TrimSpace(*v.WorkRoot)
		cfg.WorkRoot = cfg.AppleVZ.WorkRoot
	}
	if core.FlagWasSet(fs, "apple-vz-cpus") {
		if *v.CPUs <= 0 {
			return exit(2, "--apple-vz-cpus must be positive (got %d)", *v.CPUs)
		}
		cfg.AppleVZ.CPUs = *v.CPUs
		core.MarkAppleVZCPUsExplicit(cfg)
	}
	if core.FlagWasSet(fs, "apple-vz-memory") {
		if *v.MemoryMiB < 1024 {
			return exit(2, "--apple-vz-memory must be at least 1024 MiB (got %d)", *v.MemoryMiB)
		}
		cfg.AppleVZ.MemoryMiB = *v.MemoryMiB
		core.MarkAppleVZMemoryExplicit(cfg)
	}
	if core.FlagWasSet(fs, "apple-vz-disk") {
		if *v.DiskGiB <= 0 {
			return exit(2, "--apple-vz-disk must be positive (got %d)", *v.DiskGiB)
		}
		cfg.AppleVZ.DiskGiB = *v.DiskGiB
		core.MarkAppleVZDiskExplicit(cfg)
	}
	if isAppleVZProviderName(cfg.Provider) {
		applyDefaults(cfg)
	}
	return nil
}

func isAppleVZProviderName(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case providerName, "applevz":
		return true
	default:
		return false
	}
}
