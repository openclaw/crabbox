package fal

const (
	defaultAPIURL       = "https://api.fal.ai/v1"
	defaultInstanceType = "gpu_1x_h100_sxm5"
	defaultUser         = "root"
	defaultWorkRoot     = "/work/crabbox"
)

func applyFalDefaults(cfg *Config) {
	if cfg == nil {
		return
	}
	if cfg.Fal.APIURL == "" {
		cfg.Fal.APIURL = defaultAPIURL
	}
	if cfg.Fal.InstanceType == "" {
		cfg.Fal.InstanceType = defaultInstanceType
	}
	if cfg.Fal.User == "" {
		cfg.Fal.User = defaultUser
	}
	if cfg.Fal.WorkRoot == "" {
		cfg.Fal.WorkRoot = defaultWorkRoot
	}
	cfg.Provider = providerName
	if cfg.TargetOS == "" {
		cfg.TargetOS = targetLinux
	}
	if cfg.SSHUser == "" {
		cfg.SSHUser = cfg.Fal.User
	}
	if cfg.SSHPort == "" {
		cfg.SSHPort = "22"
	}
	cfg.SSHFallbackPorts = nil
	if cfg.WorkRoot == "" {
		cfg.WorkRoot = cfg.Fal.WorkRoot
	}
	if cfg.ServerType == "" {
		cfg.ServerType = cfg.Fal.InstanceType
	}
}
