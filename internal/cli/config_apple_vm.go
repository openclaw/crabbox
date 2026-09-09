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

func applyAppleVMFile(cfg *Config, file *fileAppleVMConfig) {
	if file == nil {
		return
	}
	if file.HelperPath != "" {
		cfg.AppleVM.HelperPath = file.HelperPath
	}
	if file.Image != "" {
		ApplyAppleVMImage(cfg, file.Image)
	}
	if file.ImageSHA256 != "" {
		ApplyAppleVMImageSHA256(cfg, file.ImageSHA256)
	}
	if file.User != "" {
		cfg.AppleVM.User = file.User
	}
	if file.WorkRoot != "" {
		cfg.AppleVM.WorkRoot = file.WorkRoot
	}
	if file.CPUs != nil {
		cfg.AppleVM.CPUs = *file.CPUs
		MarkAppleVMCPUsExplicit(cfg)
	}
	if file.MemoryMiB != nil {
		cfg.AppleVM.MemoryMiB = *file.MemoryMiB
		MarkAppleVMMemoryExplicit(cfg)
	}
	if file.DiskGiB != nil {
		cfg.AppleVM.DiskGiB = *file.DiskGiB
		MarkAppleVMDiskExplicit(cfg)
	}
}

// appleVMEnv reads a CRABBOX_APPLE_VM_* variable, falling back to the
// deprecated CRABBOX_APPLE_VZ_* spelling from before the provider rename.
func appleVMEnv(name string) string {
	if value := os.Getenv("CRABBOX_APPLE_VM_" + name); value != "" {
		return value
	}
	return os.Getenv("CRABBOX_APPLE_VZ_" + name)
}

func applyAppleVMEnv(cfg *Config) error {
	cfg.AppleVM.HelperPath = getenv("CRABBOX_APPLE_VM_HELPER", getenv("CRABBOX_APPLE_VZ_HELPER", cfg.AppleVM.HelperPath))
	if image := appleVMEnv("IMAGE"); image != "" {
		ApplyAppleVMImage(cfg, image)
	}
	if checksum := appleVMEnv("IMAGE_SHA256"); checksum != "" {
		ApplyAppleVMImageSHA256(cfg, checksum)
	}
	cfg.AppleVM.User = getenv("CRABBOX_APPLE_VM_USER", getenv("CRABBOX_APPLE_VZ_USER", cfg.AppleVM.User))
	cfg.AppleVM.WorkRoot = getenv("CRABBOX_APPLE_VM_WORK_ROOT", getenv("CRABBOX_APPLE_VZ_WORK_ROOT", cfg.AppleVM.WorkRoot))
	if rawCPUs := appleVMEnv("CPUS"); rawCPUs != "" {
		cpus, err := strconv.Atoi(strings.TrimSpace(rawCPUs))
		if err != nil {
			return fmt.Errorf("CRABBOX_APPLE_VM_CPUS must be an integer: %w", err)
		}
		cfg.AppleVM.CPUs = cpus
		MarkAppleVMCPUsExplicit(cfg)
	}
	if rawMemory := appleVMEnv("MEMORY"); rawMemory != "" {
		memoryMiB, err := strconv.Atoi(strings.TrimSpace(rawMemory))
		if err != nil {
			return fmt.Errorf("CRABBOX_APPLE_VM_MEMORY must be an integer: %w", err)
		}
		cfg.AppleVM.MemoryMiB = memoryMiB
		MarkAppleVMMemoryExplicit(cfg)
	}
	if rawDisk := appleVMEnv("DISK"); rawDisk != "" {
		diskGiB, err := strconv.Atoi(strings.TrimSpace(rawDisk))
		if err != nil {
			return fmt.Errorf("CRABBOX_APPLE_VM_DISK must be an integer: %w", err)
		}
		cfg.AppleVM.DiskGiB = diskGiB
		MarkAppleVMDiskExplicit(cfg)
	}
	return nil
}
