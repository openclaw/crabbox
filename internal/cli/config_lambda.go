package cli

import (
	"os"
	"strings"
)

const (
	LambdaConfiguredRegionDefault = "us-west-1"
	LambdaConfiguredTypeDefault   = "gpu_1x_a10"
	LambdaImageFamilyFallback     = "lambda-stack-24-04"
)

// LambdaConfig owns the complete runtime and file field shape. Native mount
// interpretation and portable OS selection remain separate policy.
type LambdaConfig struct {
	Region           string                  `yaml:"region,omitempty"`
	Type             string                  `yaml:"type,omitempty"`
	Image            string                  `yaml:"image,omitempty"`
	ImageFamily      string                  `yaml:"imageFamily,omitempty"`
	FirewallRuleset  string                  `yaml:"firewallRuleset,omitempty"`
	SSHCIDRs         []string                `yaml:"sshCIDRs,omitempty"`
	FilesystemNames  []string                `yaml:"filesystemNames,omitempty"`
	FilesystemMounts []LambdaFilesystemMount `yaml:"filesystemMounts,omitempty"`
}

// Keep the defined file type's identity for existing YAML decoder diagnostics.
type fileLambdaConfig LambdaConfig

type LambdaFilesystemMount struct {
	Name      string `yaml:"name,omitempty" json:"name,omitempty"`
	MountPath string `yaml:"mountPath,omitempty" json:"mountPath,omitempty"`
}

type LambdaConfigApplied struct {
	InputAccepted bool
	Type          bool
	Image         bool
	ImageFamily   bool
}

func initialLambdaConfig() LambdaConfig {
	return (LambdaConfig{}).WithRuntimeDefaults()
}

// WithRuntimeDefaults fills the raw configured defaults on a shallow copy.
// Explicit OS policy and native trim-aware selection remain separate.
func (cfg LambdaConfig) WithRuntimeDefaults() LambdaConfig {
	if cfg.Region == "" {
		cfg.Region = LambdaConfiguredRegionDefault
	}
	if cfg.Type == "" {
		cfg.Type = LambdaConfiguredTypeDefault
	}
	if cfg.Image == "" && cfg.ImageFamily == "" {
		cfg.ImageFamily = LambdaImageFamilyFallback
	}
	return cfg
}

func (cfg *LambdaConfig) applyFile(file *fileLambdaConfig) LambdaConfigApplied {
	var applied LambdaConfigApplied
	if file == nil {
		return applied
	}
	values := LambdaConfig(*file)
	if values.Region != "" {
		cfg.Region = values.Region
		applied.InputAccepted = true
	}
	if values.Type != "" {
		cfg.Type = values.Type
		applied.InputAccepted = true
		applied.Type = true
	}
	if values.Image != "" {
		cfg.Image = values.Image
		applied.InputAccepted = true
		applied.Image = true
	}
	if values.ImageFamily != "" {
		cfg.ImageFamily = values.ImageFamily
		applied.InputAccepted = true
		applied.ImageFamily = true
	}
	if applied.Image && !applied.ImageFamily {
		cfg.ImageFamily = ""
	}
	if values.FirewallRuleset != "" {
		cfg.FirewallRuleset = values.FirewallRuleset
		applied.InputAccepted = true
	}
	if len(values.SSHCIDRs) > 0 {
		cfg.SSHCIDRs = values.SSHCIDRs
		applied.InputAccepted = true
	}
	if len(values.FilesystemNames) > 0 {
		cfg.FilesystemNames = values.FilesystemNames
		applied.InputAccepted = true
	}
	if len(values.FilesystemMounts) > 0 {
		cfg.FilesystemMounts = values.FilesystemMounts
		applied.InputAccepted = true
	}
	return applied
}

func (cfg *LambdaConfig) applyEnv() LambdaConfigApplied {
	var applied LambdaConfigApplied
	if value := os.Getenv("CRABBOX_LAMBDA_REGION"); value != "" {
		cfg.Region = value
		applied.InputAccepted = true
	}
	if lambdaType := os.Getenv("CRABBOX_LAMBDA_TYPE"); lambdaType != "" {
		cfg.Type = lambdaType
		applied.InputAccepted = true
		applied.Type = true
	}
	if image := os.Getenv("CRABBOX_LAMBDA_IMAGE"); image != "" {
		cfg.Image = image
		applied.InputAccepted = true
		applied.Image = true
		cfg.ImageFamily = ""
	}
	if imageFamily := os.Getenv("CRABBOX_LAMBDA_IMAGE_FAMILY"); imageFamily != "" {
		cfg.ImageFamily = imageFamily
		applied.InputAccepted = true
		applied.ImageFamily = true
		cfg.Image = ""
	}
	if value := os.Getenv("CRABBOX_LAMBDA_FIREWALL_RULESET"); value != "" {
		cfg.FirewallRuleset = value
		applied.InputAccepted = true
	}
	if cidrs := os.Getenv("CRABBOX_LAMBDA_SSH_CIDRS"); cidrs != "" {
		cfg.SSHCIDRs = splitCommaList(cidrs)
		applied.InputAccepted = true
	}
	if names := os.Getenv("CRABBOX_LAMBDA_FILESYSTEM_NAMES"); names != "" {
		cfg.FilesystemNames = splitCommaList(names)
		applied.InputAccepted = true
	}
	if mounts := os.Getenv("CRABBOX_LAMBDA_FILESYSTEM_MOUNTS"); mounts != "" {
		cfg.FilesystemMounts = parseLambdaFilesystemMounts(mounts)
		applied.InputAccepted = true
	}
	return applied
}

func parseLambdaFilesystemMounts(value string) []LambdaFilesystemMount {
	parts := splitCommaList(value)
	out := make([]LambdaFilesystemMount, 0, len(parts))
	for _, part := range parts {
		name, mountPath, ok := strings.Cut(part, ":")
		if !ok {
			out = append(out, LambdaFilesystemMount{Name: strings.TrimSpace(part)})
			continue
		}
		out = append(out, LambdaFilesystemMount{Name: strings.TrimSpace(name), MountPath: strings.TrimSpace(mountPath)})
	}
	return out
}
