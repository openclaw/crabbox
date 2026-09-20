package cli

//go:generate go run ../../scripts/configgen -source config_hyperv.go -output config_hyperv_generated.go -type HyperVConfig -provider hyperv

// HyperVConfig declares bindings; target and runtime defaults remain provider-owned.
type HyperVConfig struct {
	Image         string `config:"image" env:"CRABBOX_HYPERV_IMAGE" flag:"hyperv-image" help:"Windows VHDX template path for Hyper-V VM creation" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value"`
	User          string `config:"user" env:"CRABBOX_HYPERV_USER" flag:"hyperv-user" help:"guest administrator account for SSH (password via CRABBOX_HYPERV_GUEST_PASSWORD)" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value" default:"crabbox"`
	WorkRoot      string `config:"workRoot" env:"CRABBOX_HYPERV_WORK_ROOT" flag:"hyperv-work-root" help:"Crabbox work root inside the guest" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value"`
	CPUs          int    `config:"cpus" env:"CRABBOX_HYPERV_CPUS" flag:"hyperv-cpu" help:"CPU count for Hyper-V leases" sources:"user,repo,env,flag" nonnegative:"true" fileInt:"positive" fileStorage:"value" envInt:"fallback" default:"4"`
	Memory        int    `config:"memory" env:"CRABBOX_HYPERV_MEMORY" flag:"hyperv-memory" help:"memory in MB for Hyper-V leases" sources:"user,repo,env,flag" nonnegative:"true" fileInt:"positive" fileStorage:"value" envInt:"fallback" default:"8192"`
	Switch        string `config:"switch" env:"CRABBOX_HYPERV_SWITCH" flag:"hyperv-switch" help:"Hyper-V virtual switch name" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value" default:"Default Switch"`
	GuestPassword string `config:"guestPassword" env:"CRABBOX_HYPERV_GUEST_PASSWORD" sources:"user,repo,env" fileIgnoreEmpty:"true" fileStorage:"value"`
	InitPassword  bool   `config:"initPassword" env:"CRABBOX_HYPERV_INIT_PASSWORD" flag:"hyperv-init-password" help:"set the guest password at first boot via the lease disk (for password-less auto-logon templates, e.g. Windows dev-environment VHDXs)" sources:"user,repo,env,flag"`
}

func initialHyperVConfig() HyperVConfig {
	cfg := defaultHyperVConfig()
	cfg.WorkRoot = defaultWindowsWorkRoot
	return cfg
}
