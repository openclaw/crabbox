package applevm

import (
	"flag"
	"strings"

	"github.com/openclaw/crabbox/internal/applevmhelper"
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

	// Deprecated --apple-vz-* spellings from before the provider rename.
	LegacyHelperPath  *string
	LegacyImage       *string
	LegacyImageSHA256 *string
	LegacyUser        *string
	LegacyWorkRoot    *string
	LegacyCPUs        *int
	LegacyMemoryMiB   *int
	LegacyDiskGiB     *int
}

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	return flagValues{
		HelperPath:  fs.String("apple-vm-helper", defaults.AppleVM.HelperPath, "apple-vm helper binary path"),
		Image:       fs.String("apple-vm-image", applevmhelper.ImageIdentity(defaults.AppleVM.Image, defaults.AppleVM.ImageSHA256), "apple-vm local source image path"),
		ImageSHA256: fs.String("apple-vm-image-sha256", defaults.AppleVM.ImageSHA256, "expected SHA-256 for apple-vm source image downloads"),
		User:        fs.String("apple-vm-user", defaults.AppleVM.User, "SSH user created inside apple-vm leases"),
		WorkRoot:    fs.String("apple-vm-work-root", defaults.AppleVM.WorkRoot, "remote Crabbox work root inside apple-vm leases"),
		CPUs:        fs.Int("apple-vm-cpus", defaults.AppleVM.CPUs, "CPU count for apple-vm leases"),
		MemoryMiB:   fs.Int("apple-vm-memory", defaults.AppleVM.MemoryMiB, "memory in MiB for apple-vm leases"),
		DiskGiB:     fs.Int("apple-vm-disk", defaults.AppleVM.DiskGiB, "disk size in GiB for apple-vm leases"),

		LegacyHelperPath:  fs.String("apple-vz-helper", defaults.AppleVM.HelperPath, "deprecated alias for --apple-vm-helper"),
		LegacyImage:       fs.String("apple-vz-image", applevmhelper.ImageIdentity(defaults.AppleVM.Image, defaults.AppleVM.ImageSHA256), "deprecated alias for --apple-vm-image"),
		LegacyImageSHA256: fs.String("apple-vz-image-sha256", defaults.AppleVM.ImageSHA256, "deprecated alias for --apple-vm-image-sha256"),
		LegacyUser:        fs.String("apple-vz-user", defaults.AppleVM.User, "deprecated alias for --apple-vm-user"),
		LegacyWorkRoot:    fs.String("apple-vz-work-root", defaults.AppleVM.WorkRoot, "deprecated alias for --apple-vm-work-root"),
		LegacyCPUs:        fs.Int("apple-vz-cpus", defaults.AppleVM.CPUs, "deprecated alias for --apple-vm-cpus"),
		LegacyMemoryMiB:   fs.Int("apple-vz-memory", defaults.AppleVM.MemoryMiB, "deprecated alias for --apple-vm-memory"),
		LegacyDiskGiB:     fs.Int("apple-vz-disk", defaults.AppleVM.DiskGiB, "deprecated alias for --apple-vm-disk"),
	}
}

// stringFlag returns the effective value of a renamed flag: the current name
// wins, the deprecated alias applies otherwise.
func stringFlag(fs *flag.FlagSet, name string, value *string, legacyName string, legacyValue *string) (string, bool) {
	if core.FlagWasSet(fs, name) {
		return *value, true
	}
	if core.FlagWasSet(fs, legacyName) {
		return *legacyValue, true
	}
	return "", false
}

func intFlag(fs *flag.FlagSet, name string, value *int, legacyName string, legacyValue *int) (int, bool) {
	if core.FlagWasSet(fs, name) {
		return *value, true
	}
	if core.FlagWasSet(fs, legacyName) {
		return *legacyValue, true
	}
	return 0, false
}

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if helper, set := stringFlag(fs, "apple-vm-helper", v.HelperPath, "apple-vz-helper", v.LegacyHelperPath); set {
		cfg.AppleVM.HelperPath = strings.TrimSpace(helper)
	}
	if image, set := stringFlag(fs, "apple-vm-image", v.Image, "apple-vz-image", v.LegacyImage); set {
		image = strings.TrimSpace(image)
		if applevmhelper.IsRemoteImageRef(image) {
			return exit(2, "--apple-vm-image accepts local paths only; use CRABBOX_APPLE_VM_IMAGE or configuration for remote URLs")
		}
		cfg.AppleVM.Image = image
		cfg.AppleVM.ImageSHA256 = ""
		core.MarkAppleVMImageExplicit(cfg)
	}
	if checksum, set := stringFlag(fs, "apple-vm-image-sha256", v.ImageSHA256, "apple-vz-image-sha256", v.LegacyImageSHA256); set {
		cfg.AppleVM.ImageSHA256 = strings.TrimSpace(checksum)
		core.MarkAppleVMImageSHA256Explicit(cfg)
	}
	if user, set := stringFlag(fs, "apple-vm-user", v.User, "apple-vz-user", v.LegacyUser); set {
		cfg.AppleVM.User = strings.TrimSpace(user)
		cfg.SSHUser = cfg.AppleVM.User
	}
	if workRoot, set := stringFlag(fs, "apple-vm-work-root", v.WorkRoot, "apple-vz-work-root", v.LegacyWorkRoot); set {
		cfg.AppleVM.WorkRoot = strings.TrimSpace(workRoot)
		cfg.WorkRoot = cfg.AppleVM.WorkRoot
	}
	if cpus, set := intFlag(fs, "apple-vm-cpus", v.CPUs, "apple-vz-cpus", v.LegacyCPUs); set {
		if cpus <= 0 {
			return exit(2, "--apple-vm-cpus must be positive (got %d)", cpus)
		}
		cfg.AppleVM.CPUs = cpus
		core.MarkAppleVMCPUsExplicit(cfg)
	}
	if memoryMiB, set := intFlag(fs, "apple-vm-memory", v.MemoryMiB, "apple-vz-memory", v.LegacyMemoryMiB); set {
		if memoryMiB < 1024 {
			return exit(2, "--apple-vm-memory must be at least 1024 MiB (got %d)", memoryMiB)
		}
		cfg.AppleVM.MemoryMiB = memoryMiB
		core.MarkAppleVMMemoryExplicit(cfg)
	}
	if diskGiB, set := intFlag(fs, "apple-vm-disk", v.DiskGiB, "apple-vz-disk", v.LegacyDiskGiB); set {
		if diskGiB <= 0 {
			return exit(2, "--apple-vm-disk must be positive (got %d)", diskGiB)
		}
		cfg.AppleVM.DiskGiB = diskGiB
		core.MarkAppleVMDiskExplicit(cfg)
	}
	if isAppleVMProviderName(cfg.Provider) {
		applyDefaults(cfg)
	}
	return nil
}

func isAppleVMProviderName(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case providerName, "applevm", "apple-vz", "applevz":
		return true
	default:
		return false
	}
}
