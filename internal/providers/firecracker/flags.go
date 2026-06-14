package firecracker

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

type flagValues struct {
	BinaryPath      *string
	JailerPath      *string
	KernelPath      *string
	RootFSPath      *string
	User            *string
	WorkRoot        *string
	VCPUs           *int
	MemoryMiB       *int
	DiskMiB         *int
	NetworkMode     *string
	CNINetwork      *string
	CNIConfDir      *string
	CNIBinDir       *string
	LaunchTimeout   *string
	DeleteOnRelease *bool
}

func RegisterFirecrackerProviderFlags(fs *flag.FlagSet, defaults core.Config) any {
	return flagValues{
		BinaryPath:      fs.String("firecracker-binary", defaults.Firecracker.Binary, "Firecracker binary name or path"),
		JailerPath:      fs.String("firecracker-jailer", defaults.Firecracker.Jailer, "Optional Firecracker jailer binary path"),
		KernelPath:      fs.String("firecracker-kernel", defaults.Firecracker.Kernel, "Linux kernel image for Firecracker guests"),
		RootFSPath:      fs.String("firecracker-rootfs", defaults.Firecracker.RootFS, "Root filesystem image for Firecracker guests"),
		User:            fs.String("firecracker-user", defaults.Firecracker.User, "SSH user inside Firecracker guests"),
		WorkRoot:        fs.String("firecracker-work-root", defaults.Firecracker.WorkRoot, "Remote Crabbox work root inside Firecracker guests"),
		VCPUs:           fs.Int("firecracker-cpus", defaults.Firecracker.CPUs, "vCPU count for Firecracker guests"),
		MemoryMiB:       fs.Int("firecracker-memory-mib", defaults.Firecracker.MemoryMiB, "Guest memory in MiB"),
		DiskMiB:         fs.Int("firecracker-disk-mib", defaults.Firecracker.DiskMiB, "Per-lease writable disk size in MiB"),
		NetworkMode:     fs.String("firecracker-network", defaults.Firecracker.Network, "Firecracker network mode (currently cni)"),
		CNINetwork:      fs.String("firecracker-cni-network", defaults.Firecracker.CNINetwork, "CNI network name for Firecracker guests"),
		CNIConfDir:      fs.String("firecracker-cni-conf-dir", defaults.Firecracker.CNIConfDir, "CNI network configuration directory"),
		CNIBinDir:       fs.String("firecracker-cni-bin-dir", defaults.Firecracker.CNIBinDir, "CNI plugin binary directory"),
		LaunchTimeout:   fs.String("firecracker-launch-timeout", defaults.Firecracker.LaunchTimeout.String(), "Firecracker launch timeout"),
		DeleteOnRelease: fs.Bool("firecracker-delete-on-release", defaults.Firecracker.DeleteOnRelease, "Delete owned Firecracker artifacts when releasing a lease"),
	}
}

func ApplyFirecrackerProviderFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	if isFirecrackerProviderName(cfg.Provider) {
		if core.FlagWasSet(fs, "class") {
			return core.Exit(2, "--class is not supported for provider=firecracker; use --firecracker-cpus, --firecracker-memory-mib, and --firecracker-disk-mib")
		}
		if core.FlagWasSet(fs, "type") {
			return core.Exit(2, "--type is not supported for provider=firecracker; use explicit Firecracker kernel, rootfs, and sizing flags")
		}
	}
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if core.FlagWasSet(fs, "firecracker-binary") {
		cfg.Firecracker.Binary = core.ExpandUserPath(*v.BinaryPath)
	}
	if core.FlagWasSet(fs, "firecracker-jailer") {
		cfg.Firecracker.Jailer = core.ExpandUserPath(*v.JailerPath)
	}
	if core.FlagWasSet(fs, "firecracker-kernel") {
		cfg.Firecracker.Kernel = core.ExpandUserPath(*v.KernelPath)
	}
	if core.FlagWasSet(fs, "firecracker-rootfs") {
		cfg.Firecracker.RootFS = core.ExpandUserPath(*v.RootFSPath)
	}
	if core.FlagWasSet(fs, "firecracker-user") {
		cfg.Firecracker.User = *v.User
		cfg.SSHUser = *v.User
	}
	if core.FlagWasSet(fs, "firecracker-work-root") {
		cfg.Firecracker.WorkRoot = *v.WorkRoot
		cfg.WorkRoot = *v.WorkRoot
	}
	if core.FlagWasSet(fs, "firecracker-cpus") {
		cfg.Firecracker.CPUs = *v.VCPUs
	}
	if core.FlagWasSet(fs, "firecracker-memory-mib") {
		cfg.Firecracker.MemoryMiB = *v.MemoryMiB
	}
	if core.FlagWasSet(fs, "firecracker-disk-mib") {
		cfg.Firecracker.DiskMiB = *v.DiskMiB
	}
	if core.FlagWasSet(fs, "firecracker-network") {
		cfg.Firecracker.Network = *v.NetworkMode
	}
	if core.FlagWasSet(fs, "firecracker-cni-network") {
		cfg.Firecracker.CNINetwork = *v.CNINetwork
	}
	if core.FlagWasSet(fs, "firecracker-cni-conf-dir") {
		cfg.Firecracker.CNIConfDir = core.ExpandUserPath(*v.CNIConfDir)
	}
	if core.FlagWasSet(fs, "firecracker-cni-bin-dir") {
		cfg.Firecracker.CNIBinDir = core.ExpandUserPath(*v.CNIBinDir)
	}
	if core.FlagWasSet(fs, "firecracker-launch-timeout") {
		if err := core.ApplyLeaseDuration(&cfg.Firecracker.LaunchTimeout, *v.LaunchTimeout); err != nil {
			return err
		}
	}
	if core.FlagWasSet(fs, "firecracker-delete-on-release") {
		cfg.Firecracker.DeleteOnRelease = *v.DeleteOnRelease
		core.MarkDeleteOnReleaseExplicit(cfg, providerName)
	}
	if isFirecrackerProviderName(cfg.Provider) {
		applyDefaults(cfg)
	}
	return nil
}

func isFirecrackerProviderName(provider string) bool {
	return strings.EqualFold(strings.TrimSpace(provider), providerName)
}
