package cli

import "strings"

const (
	ArchitectureAMD64 = "amd64"
	ArchitectureARM64 = "arm64"
)

func normalizeOSImage(value string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if normalized == "" {
		normalized = defaultOSImage
	}
	normalized = strings.ReplaceAll(normalized, "_", ".")
	normalized = strings.ReplaceAll(normalized, "-", ":")
	if alias, ok := osImageAliases[normalized]; ok {
		normalized = alias
	}
	if _, ok := osImageSpecs[normalized]; !ok {
		return "", exit(2, "unsupported os %q; supported: %s", value, supportedOSImages)
	}
	return normalized, nil
}

func normalizeArchitecture(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", ArchitectureAMD64, "x86_64", "x64":
		return ArchitectureAMD64, nil
	case ArchitectureARM64, "aarch64":
		return ArchitectureARM64, nil
	default:
		return "", exit(2, "architecture must be amd64 or arm64")
	}
}

func effectiveArchitectureForConfig(cfg Config) string {
	if cfg.architectureExplicit {
		return cfg.Architecture
	}
	if cfg.Provider == "apple-container" || cfg.Provider == "apple" || cfg.Provider == "applecontainer" || cfg.Provider == "apple-vm" || cfg.Provider == "applevm" || cfg.Provider == "lume" || cfg.Provider == "local-lume" || cfg.Provider == "lume-macos" || cfg.Provider == "aws-lambda-microvm" {
		return ArchitectureARM64
	}
	if cfg.TargetOS == targetLinux || cfg.TargetOS == targetWindows {
		if cfg.Provider == "azure" && azureVMSizeIsARM64(cfg.ServerType) {
			return ArchitectureARM64
		}
	}
	if cfg.TargetOS == targetLinux {
		if cfg.Provider == "aws" && awsInstanceTypeIsARM64(cfg.ServerType) {
			return ArchitectureARM64
		}
	}
	return cfg.Architecture
}

func osImageSpecFor(value string) (osImageSpec, error) {
	normalized, err := normalizeOSImage(value)
	if err != nil {
		return osImageSpec{}, err
	}
	return osImageSpecs[normalized], nil
}

func awsLinuxAMIQueryForOS(value string, architecture string) (name string, label string, err error) {
	spec, err := osImageSpecFor(value)
	if err != nil {
		return "", "", err
	}
	if architecture == ArchitectureARM64 {
		return spec.AWSArm64Name, spec.AWSLabel, nil
	}
	return spec.AWSName, spec.AWSLabel, nil
}

func osImageDefaultProviderImages(value string) (hetzner, azure, gcp, linode, docker, container string, err error) {
	return osImageDefaultProviderImagesForArchitecture(value, ArchitectureAMD64)
}

func osImageDefaultProviderImagesForArchitecture(value string, architecture string) (hetzner, azure, gcp, linode, docker, container string, err error) {
	spec, err := osImageSpecFor(value)
	if err != nil {
		return "", "", "", "", "", "", err
	}
	if architecture == ArchitectureARM64 {
		return spec.HetznerImage, spec.AzureArm64Image, spec.GCPImage, spec.LinodeImage, spec.DockerImage, spec.ContainerName, nil
	}
	return spec.HetznerImage, spec.AzureImage, spec.GCPImage, spec.LinodeImage, spec.DockerImage, spec.ContainerName, nil
}

func osImageDefaultMultipassImage(value string) (string, error) {
	spec, err := osImageSpecFor(value)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(spec.Selector, "ubuntu:"), nil
}

func osImageDefaultAppleVMImage(value string) (string, error) {
	spec, err := osImageSpecFor(value)
	if err != nil {
		return "", err
	}
	return spec.AppleVMImage, nil
}

func OSImageDefaultAppleVMImage(value string) (string, error) {
	return osImageDefaultAppleVMImage(value)
}

func osImageDefaultAppleVMSHA256(value string) (string, error) {
	spec, err := osImageSpecFor(value)
	if err != nil {
		return "", err
	}
	return spec.AppleVMSHA256, nil
}

func OSImageDefaultAppleVMSHA256(value string) (string, error) {
	return osImageDefaultAppleVMSHA256(value)
}
