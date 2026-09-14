package cli

import "time"

//go:generate go run ../../scripts/configgen -source config_firecracker.go -output config_firecracker_generated.go -type FirecrackerConfig -provider firecracker

// FirecrackerConfig describes ordinary inputs; backend and launch policy stay provider-owned.
type FirecrackerConfig struct {
	Binary          string        `config:"binary" env:"CRABBOX_FIRECRACKER_BINARY" flag:"firecracker-binary" help:"Firecracker binary name or path" sources:"user,env,flag" fileIgnoreEmpty:"true" fileStorage:"value" default:"firecracker" reportApplied:"true"`
	Jailer          string        `config:"jailer" env:"CRABBOX_FIRECRACKER_JAILER" flag:"firecracker-jailer" help:"Optional Firecracker jailer binary path" sources:"user,env,flag" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	Kernel          string        `config:"kernel" env:"CRABBOX_FIRECRACKER_KERNEL" flag:"firecracker-kernel" help:"Linux kernel image for Firecracker guests" sources:"user,env,flag" fileIgnoreEmpty:"true" fileStorage:"value" default:"/var/lib/crabbox/firecracker/vmlinux" reportApplied:"true"`
	RootFS          string        `config:"rootfs" env:"CRABBOX_FIRECRACKER_ROOTFS" flag:"firecracker-rootfs" help:"Root filesystem image for Firecracker guests" sources:"user,env,flag" fileIgnoreEmpty:"true" fileStorage:"value" default:"/var/lib/crabbox/firecracker/rootfs.ext4" reportApplied:"true"`
	User            string        `config:"user" env:"CRABBOX_FIRECRACKER_USER" flag:"firecracker-user" help:"SSH user inside Firecracker guests" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value" default:"crabbox" reportApplied:"true"`
	WorkRoot        string        `config:"workRoot" env:"CRABBOX_FIRECRACKER_WORK_ROOT" flag:"firecracker-work-root" help:"Remote Crabbox work root inside Firecracker guests" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	CPUs            int           `config:"cpus" env:"CRABBOX_FIRECRACKER_CPUS" flag:"firecracker-cpus" help:"vCPU count for Firecracker guests" sources:"user,repo,env,flag" nonnegative:"true" fileInt:"present" envInt:"fallback" default:"4"`
	MemoryMiB       int           `config:"memoryMiB" env:"CRABBOX_FIRECRACKER_MEMORY_MIB" flag:"firecracker-memory-mib" help:"Guest memory in MiB" sources:"user,repo,env,flag" nonnegative:"true" fileInt:"present" envInt:"fallback" default:"4096"`
	DiskMiB         int           `config:"diskMiB" env:"CRABBOX_FIRECRACKER_DISK_MIB" flag:"firecracker-disk-mib" help:"Per-lease writable disk size in MiB" sources:"user,repo,env,flag" nonnegative:"true" fileInt:"present" envInt:"fallback" default:"16384"`
	Network         string        `config:"network" env:"CRABBOX_FIRECRACKER_NETWORK" flag:"firecracker-network" help:"Firecracker network mode (currently cni)" sources:"user,env,flag" fileIgnoreEmpty:"true" fileStorage:"value" default:"cni"`
	CNINetwork      string        `config:"cniNetwork" env:"CRABBOX_FIRECRACKER_CNI_NETWORK" flag:"firecracker-cni-network" help:"CNI network name for Firecracker guests" sources:"user,env,flag" fileIgnoreEmpty:"true" fileStorage:"value" default:"crabbox-firecracker"`
	CNIConfDir      string        `config:"cniConfDir" env:"CRABBOX_FIRECRACKER_CNI_CONF_DIR" flag:"firecracker-cni-conf-dir" help:"CNI network configuration directory" sources:"user,env,flag" fileIgnoreEmpty:"true" fileStorage:"value" default:"/etc/cni/conf.d" reportApplied:"true"`
	CNIBinDir       string        `config:"cniBinDir" env:"CRABBOX_FIRECRACKER_CNI_BIN_DIR" flag:"firecracker-cni-bin-dir" help:"CNI plugin binary directory" sources:"user,env,flag" fileIgnoreEmpty:"true" fileStorage:"value" default:"/opt/cni/bin" reportApplied:"true"`
	LaunchTimeout   time.Duration `config:"launchTimeout" env:"CRABBOX_FIRECRACKER_LAUNCH_TIMEOUT" flag:"firecracker-launch-timeout" help:"Firecracker launch timeout" sources:"user,repo,env,flag" duration:"positive-overlay" fileStorage:"value" flagDuration:"raw-positive" default:"2m"`
	DeleteOnRelease bool          `config:"deleteOnRelease" env:"CRABBOX_FIRECRACKER_DELETE_ON_RELEASE" flag:"firecracker-delete-on-release" help:"Delete owned Firecracker artifacts when releasing a lease" sources:"user,repo,env,flag" default:"true" reportApplied:"true"`
}

const FirecrackerConfigDefaultWorkRoot string = defaultPOSIXWorkRoot

func initialFirecrackerConfig() FirecrackerConfig {
	cfg := defaultFirecrackerConfig()
	cfg.WorkRoot = FirecrackerConfigDefaultWorkRoot
	return cfg
}

func (cfg *FirecrackerConfig) ExpandAppliedLocalPaths(applied FirecrackerConfigApplied) {
	if applied.Binary {
		cfg.Binary = expandUserPath(cfg.Binary)
	}
	if applied.Jailer {
		cfg.Jailer = expandUserPath(cfg.Jailer)
	}
	if applied.Kernel {
		cfg.Kernel = expandUserPath(cfg.Kernel)
	}
	if applied.RootFS {
		cfg.RootFS = expandUserPath(cfg.RootFS)
	}
	if applied.CNIConfDir {
		cfg.CNIConfDir = expandUserPath(cfg.CNIConfDir)
	}
	if applied.CNIBinDir {
		cfg.CNIBinDir = expandUserPath(cfg.CNIBinDir)
	}
}
