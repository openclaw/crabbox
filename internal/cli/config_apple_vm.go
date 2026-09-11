package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type AppleVMConfig struct {
	HelperPath  string
	Image       string
	ImageSHA256 string
	User        string
	WorkRoot    string
	CPUs        int
	MemoryMiB   int
	DiskGiB     int
}

type fileAppleVMConfig struct {
	HelperPath  string `yaml:"helperPath,omitempty"`
	Image       string `yaml:"image,omitempty"`
	ImageSHA256 string `yaml:"imageSHA256,omitempty"`
	User        string `yaml:"user,omitempty"`
	WorkRoot    string `yaml:"workRoot,omitempty"`
	CPUs        *int   `yaml:"cpus,omitempty"`
	MemoryMiB   *int   `yaml:"memoryMiB,omitempty"`
	DiskGiB     *int   `yaml:"diskGiB,omitempty"`
}

func initialAppleVMConfig(image, checksum string) AppleVMConfig {
	return AppleVMConfig{
		Image:       image,
		ImageSHA256: checksum,
		User:        "crabbox",
		WorkRoot:    "/work/crabbox",
		CPUs:        4,
		MemoryMiB:   8192,
		DiskGiB:     30,
	}
}

// ApplyAppleVMImage records an already-accepted image and clears the prior checksum.
func ApplyAppleVMImage(cfg *Config, image string) {
	cfg.AppleVM.Image = image
	cfg.AppleVM.ImageSHA256 = ""
	MarkAppleVMImageExplicit(cfg)
}

// ApplyAppleVMImageSHA256 records an already-accepted checksum.
func ApplyAppleVMImageSHA256(cfg *Config, checksum string) {
	cfg.AppleVM.ImageSHA256 = checksum
	MarkAppleVMImageSHA256Explicit(cfg)
}

type AppleVMConfigApplied struct {
	InputAccepted bool
}

func applyAppleVMFile(cfg *Config, file *fileAppleVMConfig) AppleVMConfigApplied {
	var applied AppleVMConfigApplied
	if file == nil {
		return applied
	}
	if file.HelperPath != "" {
		cfg.AppleVM.HelperPath = file.HelperPath
		applied.InputAccepted = true
	}
	if file.Image != "" {
		ApplyAppleVMImage(cfg, file.Image)
		applied.InputAccepted = true
	}
	if file.ImageSHA256 != "" {
		ApplyAppleVMImageSHA256(cfg, file.ImageSHA256)
		applied.InputAccepted = true
	}
	if file.User != "" {
		cfg.AppleVM.User = file.User
		applied.InputAccepted = true
	}
	if file.WorkRoot != "" {
		cfg.AppleVM.WorkRoot = file.WorkRoot
		applied.InputAccepted = true
	}
	if file.CPUs != nil {
		cfg.AppleVM.CPUs = *file.CPUs
		MarkAppleVMCPUsExplicit(cfg)
		applied.InputAccepted = true
	}
	if file.MemoryMiB != nil {
		cfg.AppleVM.MemoryMiB = *file.MemoryMiB
		MarkAppleVMMemoryExplicit(cfg)
		applied.InputAccepted = true
	}
	if file.DiskGiB != nil {
		cfg.AppleVM.DiskGiB = *file.DiskGiB
		MarkAppleVMDiskExplicit(cfg)
		applied.InputAccepted = true
	}
	return applied
}

// appleVMEnv reads a CRABBOX_APPLE_VM_* variable, falling back to the
// deprecated CRABBOX_APPLE_VZ_* spelling from before the provider rename.
func appleVMEnv(name string) string {
	if value := os.Getenv("CRABBOX_APPLE_VM_" + name); value != "" {
		return value
	}
	return os.Getenv("CRABBOX_APPLE_VZ_" + name)
}

func applyAppleVMEnv(cfg *Config) (AppleVMConfigApplied, error) {
	var applied AppleVMConfigApplied
	if value := appleVMEnv("HELPER"); value != "" {
		cfg.AppleVM.HelperPath = value
		applied.InputAccepted = true
	}
	if image := appleVMEnv("IMAGE"); image != "" {
		ApplyAppleVMImage(cfg, image)
		applied.InputAccepted = true
	}
	if checksum := appleVMEnv("IMAGE_SHA256"); checksum != "" {
		ApplyAppleVMImageSHA256(cfg, checksum)
		applied.InputAccepted = true
	}
	if value := appleVMEnv("USER"); value != "" {
		cfg.AppleVM.User = value
		applied.InputAccepted = true
	}
	if value := appleVMEnv("WORK_ROOT"); value != "" {
		cfg.AppleVM.WorkRoot = value
		applied.InputAccepted = true
	}
	if rawCPUs := appleVMEnv("CPUS"); rawCPUs != "" {
		cpus, err := strconv.Atoi(strings.TrimSpace(rawCPUs))
		if err != nil {
			return applied, fmt.Errorf("CRABBOX_APPLE_VM_CPUS must be an integer: %w", err)
		}
		cfg.AppleVM.CPUs = cpus
		MarkAppleVMCPUsExplicit(cfg)
		applied.InputAccepted = true
	}
	if rawMemory := appleVMEnv("MEMORY"); rawMemory != "" {
		memoryMiB, err := strconv.Atoi(strings.TrimSpace(rawMemory))
		if err != nil {
			return applied, fmt.Errorf("CRABBOX_APPLE_VM_MEMORY must be an integer: %w", err)
		}
		cfg.AppleVM.MemoryMiB = memoryMiB
		MarkAppleVMMemoryExplicit(cfg)
		applied.InputAccepted = true
	}
	if rawDisk := appleVMEnv("DISK"); rawDisk != "" {
		diskGiB, err := strconv.Atoi(strings.TrimSpace(rawDisk))
		if err != nil {
			return applied, fmt.Errorf("CRABBOX_APPLE_VM_DISK must be an integer: %w", err)
		}
		cfg.AppleVM.DiskGiB = diskGiB
		MarkAppleVMDiskExplicit(cfg)
		applied.InputAccepted = true
	}
	return applied, nil
}
