package cli

import (
	"os"
	"slices"
)

// GCPConfig keeps provider settings and their explicit-input intent together.
// File, environment, and coordinator policies intentionally remain distinct.
type GCPConfig struct {
	Project         string
	projectExplicit bool
	Zone            string
	zoneExplicit    bool
	Image           string
	imageExplicit   bool
	MachineImage    string
	Snapshot        string
	Network         string
	networkExplicit bool
	Subnet          string
	Tags            []string
	tagsExplicit    bool
	SSHCIDRs        []string
	RootGB          int64
	rootGBExplicit  bool
	ServiceAccount  string
}

type fileGCPConfig struct {
	Project        string   `yaml:"project,omitempty"`
	Zone           string   `yaml:"zone,omitempty"`
	Image          string   `yaml:"image,omitempty"`
	Network        string   `yaml:"network,omitempty"`
	Subnet         string   `yaml:"subnet,omitempty"`
	Tags           []string `yaml:"tags,omitempty"`
	SSHCIDRs       []string `yaml:"sshCIDRs,omitempty"`
	RootGB         int64    `yaml:"rootGB,omitempty"`
	ServiceAccount string   `yaml:"serviceAccount,omitempty"`
}

func initialGCPConfig(image string) GCPConfig {
	return GCPConfig{Zone: "europe-west2-a", Image: image, Network: "default", Tags: []string{"crabbox-ssh"}, RootGB: 400}
}

func (cfg *GCPConfig) applyOSImageDefault(image, baseImage string, force, wasOSDefault bool) {
	if force || cfg.Image == "" || (!cfg.imageExplicit && (cfg.Image == baseImage || wasOSDefault)) {
		cfg.Image = image
	}
}

func (cfg *Config) applyGCPFileConfig(file *fileGCPConfig, inputSource configInputSource) {
	if file != nil {
		if file.Project != "" {
			cfg.GCP.Project = file.Project
			recordConfigInput(cfg, "gcp", inputSource, true)
			cfg.GCP.projectExplicit = true
		}
		if file.Zone != "" {
			cfg.GCP.Zone = file.Zone
			recordConfigInput(cfg, "gcp", inputSource, true)
			cfg.GCP.zoneExplicit = true
		}
		if file.Image != "" {
			cfg.GCP.Image = file.Image
			recordConfigInput(cfg, "gcp", inputSource, true)
			cfg.GCP.imageExplicit = true
		}
		if file.Network != "" {
			cfg.GCP.Network = file.Network
			recordConfigInput(cfg, "gcp", inputSource, true)
			cfg.GCP.networkExplicit = true
		}
		configInputFileString(cfg, "gcp", inputSource, &cfg.GCP.Subnet, file.Subnet)
		if len(file.Tags) > 0 {
			cfg.GCP.Tags = file.Tags
			recordConfigInput(cfg, "gcp", inputSource, true)
			cfg.GCP.tagsExplicit = true
		}
		if len(file.SSHCIDRs) > 0 {
			cfg.GCP.SSHCIDRs = file.SSHCIDRs
			recordConfigInput(cfg, "gcp", inputSource, true)
		}
		if file.RootGB > 0 {
			cfg.GCP.RootGB = file.RootGB
			recordConfigInput(cfg, "gcp", inputSource, true)
			cfg.GCP.rootGBExplicit = true
		}
		configInputFileString(cfg, "gcp", inputSource, &cfg.GCP.ServiceAccount, file.ServiceAccount)
	}
}

func (cfg *Config) applyGCPEnvironmentPrefix() {
	if project := os.Getenv("CRABBOX_GCP_PROJECT"); project != "" {
		cfg.GCP.Project = project
		recordConfigInput(cfg, "gcp", configInputEnvironment, true)
		cfg.GCP.projectExplicit = true
	} else if cfg.GCP.Project == "" {
		if project := os.Getenv("GOOGLE_CLOUD_PROJECT"); project != "" {
			cfg.GCP.Project = project
			recordConfigInput(cfg, "gcp", configInputEnvironment, true)
			cfg.GCP.projectExplicit = false
		} else if project := os.Getenv("GCP_PROJECT_ID"); project != "" {
			cfg.GCP.Project = project
			recordConfigInput(cfg, "gcp", configInputEnvironment, true)
			cfg.GCP.projectExplicit = false
		}
	}
	if zone := os.Getenv("CRABBOX_GCP_ZONE"); zone != "" {
		cfg.GCP.Zone = zone
		recordConfigInput(cfg, "gcp", configInputEnvironment, true)
		cfg.GCP.zoneExplicit = true
	}
	if image := os.Getenv("CRABBOX_GCP_IMAGE"); image != "" {
		cfg.GCP.Image = image
		recordConfigInput(cfg, "gcp", configInputEnvironment, true)
		cfg.GCP.imageExplicit = true
	}
	if network := os.Getenv("CRABBOX_GCP_NETWORK"); network != "" {
		cfg.GCP.Network = network
		recordConfigInput(cfg, "gcp", configInputEnvironment, true)
		cfg.GCP.networkExplicit = true
	}
	cfg.GCP.Subnet = configInputEnvString(cfg, "gcp", cfg.GCP.Subnet, "CRABBOX_GCP_SUBNET")
	if rootGB := os.Getenv("CRABBOX_GCP_ROOT_GB"); rootGB != "" {
		cfg.GCP.RootGB = int64(configInputEnvInt(cfg, "gcp", int(cfg.GCP.RootGB), "CRABBOX_GCP_ROOT_GB"))
		cfg.GCP.rootGBExplicit = true
		recordConfigInputIntent(cfg, "gcp", configInputEnvironment, true)
	}
	cfg.GCP.ServiceAccount = configInputEnvString(cfg, "gcp", cfg.GCP.ServiceAccount, "CRABBOX_GCP_SERVICE_ACCOUNT")
}

// Preserve the historical position after Incus environment application.
func (cfg *Config) applyGCPEnvironmentLists() {
	if tags := os.Getenv("CRABBOX_GCP_TAGS"); tags != "" {
		cfg.GCP.Tags = splitCommaList(tags)
		recordConfigInput(cfg, "gcp", configInputEnvironment, true)
		cfg.GCP.tagsExplicit = true
	}
	if cidrs := os.Getenv("CRABBOX_GCP_SSH_CIDRS"); cidrs != "" {
		cfg.GCP.SSHCIDRs = splitCommaList(cidrs)
		recordConfigInput(cfg, "gcp", configInputEnvironment, true)
	}
}

func SetGCPProjectExplicit(cfg *Config, project string) {
	cfg.GCP.Project = project
	cfg.GCP.projectExplicit = true
}

func addCoordinatorGCPFields(req map[string]any, cfg Config) {
	if cfg.Provider != "gcp" {
		return
	}
	base := baseConfig()
	if cfg.GCP.Project != "" && cfg.GCP.projectExplicit {
		req["gcpProject"] = cfg.GCP.Project
	}
	if cfg.GCP.Zone != "" && (cfg.GCP.zoneExplicit || cfg.GCP.Zone != base.GCP.Zone) {
		req["gcpZone"] = cfg.GCP.Zone
	}
	if cfg.GCP.Image != "" && (cfg.GCP.imageExplicit || cfg.GCP.Image != base.GCP.Image) {
		req["gcpImage"] = cfg.GCP.Image
	}
	if cfg.GCP.MachineImage != "" {
		req["gcpMachineImage"] = cfg.GCP.MachineImage
	}
	if cfg.GCP.Snapshot != "" {
		req["gcpSnapshot"] = cfg.GCP.Snapshot
	}
	if cfg.GCP.Network != "" && (cfg.GCP.networkExplicit || cfg.GCP.Network != base.GCP.Network) {
		req["gcpNetwork"] = cfg.GCP.Network
	}
	if cfg.GCP.Subnet != "" {
		req["gcpSubnet"] = cfg.GCP.Subnet
	}
	if len(cfg.GCP.Tags) > 0 && (cfg.GCP.tagsExplicit || !slices.Equal(cfg.GCP.Tags, base.GCP.Tags)) {
		req["gcpTags"] = cfg.GCP.Tags
	}
	if len(cfg.GCP.SSHCIDRs) > 0 {
		req["gcpSSHCIDRs"] = cfg.GCP.SSHCIDRs
	}
	if cfg.GCP.RootGB > 0 && (cfg.GCP.rootGBExplicit || cfg.GCP.RootGB != base.GCP.RootGB) {
		req["gcpRootGB"] = cfg.GCP.RootGB
	}
	if cfg.GCP.ServiceAccount != "" {
		req["gcpServiceAccount"] = cfg.GCP.ServiceAccount
	}
}
