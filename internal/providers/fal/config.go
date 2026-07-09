package fal

import (
	"regexp"
	"strings"
)

const (
	defaultAPIURL       = "https://api.fal.ai/v1"
	defaultInstanceType = "gpu_1x_h100_sxm5"
	defaultUser         = "ubuntu"
	defaultWorkRoot     = "/home/ubuntu/crabbox"
)

var falSSHUserPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9._-]{0,31}$`)

func applyFalDefaults(cfg *Config) {
	if cfg == nil {
		return
	}
	cfg.Fal.User = strings.TrimSpace(cfg.Fal.User)
	cfg.SSHUser = strings.TrimSpace(cfg.SSHUser)
	if cfg.ServerTypeExplicit && strings.TrimSpace(cfg.ServerType) != "" {
		cfg.Fal.InstanceType = strings.TrimSpace(cfg.ServerType)
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
	cfg.ServerType = cfg.Fal.InstanceType
}

func validateFalSSHUser(user string) error {
	if !falSSHUserPattern.MatchString(user) {
		return exit(2, "provider=%s SSH user must be a valid Linux login name, got %q", providerName, user)
	}
	return nil
}
