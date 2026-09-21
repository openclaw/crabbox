package cli

import "os"

//go:generate go run ../../scripts/configgen -source config_tart.go -output config_tart_generated.go -type TartConfig -provider tart

// TartConfig declares bindings; ordered resource validation remains provider-owned.
//
//configgen:flag-application manual
type TartConfig struct {
	Image    string `config:"image" env:"CRABBOX_TART_IMAGE" flag:"tart-image" help:"tart base image to clone from" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	User     string `config:"user" env:"CRABBOX_TART_USER" flag:"tart-user" help:"guest user account for SSH and desktop/VNC" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value" default:"admin"`
	Password string `config:"password" env:"CRABBOX_TART_PASSWORD" sources:"user,repo,env" fileIgnoreEmpty:"true" fileStorage:"value"`
	WorkRoot string `config:"workRoot" env:"CRABBOX_TART_WORK_ROOT" sources:"user,repo,env" fileIgnoreEmpty:"true" fileStorage:"value"`
	CPUs     int    `config:"cpus" env:"CRABBOX_TART_CPUS" flag:"tart-cpu" help:"CPU count for tart VMs" sources:"user,repo,env,flag" nonnegative:"true" fileInt:"present" envInt:"fallback" default:"4"`
	Memory   int    `config:"memory" env:"CRABBOX_TART_MEMORY" flag:"tart-memory" help:"memory in MB for tart VMs" sources:"user,repo,env,flag" nonnegative:"true" fileInt:"present" envInt:"fallback" default:"8192"`
	Disk     int    `config:"disk" env:"CRABBOX_TART_DISK" flag:"tart-disk" help:"disk size in GB for tart VMs (0 = use clone default)" sources:"user,repo,env,flag" nonnegative:"true" fileInt:"present" envInt:"fallback"`
}

func initialTartConfig() TartConfig {
	cfg := defaultTartConfig()
	cfg.Image = DefaultTartImage
	cfg.WorkRoot = "/Users/admin/crabbox"
	return cfg
}

func applyTartFileConfig(cfg *Config, file *fileTartConfig, source configInputSource) error {
	applied, err := cfg.Tart.applyFile(file)
	recordConfigInput(cfg, "tart", source, applied.InputAccepted)
	if applied.Image {
		cfg.tartImageExplicit = true
	}
	if file != nil {
		if file.CPUs != nil {
			cfg.tartCPUsExplicit = true
		}
		if file.Memory != nil {
			cfg.tartMemoryExplicit = true
		}
		if file.Disk != nil {
			cfg.tartDiskExplicit = true
		}
	}
	return err
}

func applyTartEnvConfig(cfg *Config) error {
	cpuPresent := os.Getenv("CRABBOX_TART_CPUS") != ""
	memoryPresent := os.Getenv("CRABBOX_TART_MEMORY") != ""
	diskPresent := os.Getenv("CRABBOX_TART_DISK") != ""
	applied, err := cfg.Tart.applyEnv()
	recordConfigInput(cfg, "tart", configInputEnvironment, applied.InputAccepted)
	if applied.Image {
		cfg.tartImageExplicit = true
	}
	// Raw numeric intent survives rejected parsing; disk uses the resulting value.
	if cpuPresent {
		cfg.tartCPUsExplicit = true
	}
	if memoryPresent {
		cfg.tartMemoryExplicit = true
	}
	if diskPresent {
		cfg.tartDiskExplicit = cfg.Tart.Disk > 0
	}
	recordConfigInputIntent(cfg, "tart", configInputEnvironment, cpuPresent || memoryPresent || diskPresent)
	return err
}
