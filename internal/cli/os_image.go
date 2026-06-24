package cli

import "strings"

const defaultOSImage = "ubuntu:26.04"

const (
	ArchitectureAMD64 = "amd64"
	ArchitectureARM64 = "arm64"
)

type osImageSpec struct {
	Selector        string
	AWSName         string
	AWSArm64Name    string
	AWSLabel        string
	AzureImage      string
	AzureArm64Image string
	GCPImage        string
	HetznerImage    string
	LinodeImage     string
	DockerImage     string
	ContainerName   string
	AppleVZImage    string
	AppleVZSHA256   string
}

var osImageSpecs = map[string]osImageSpec{
	"ubuntu:24.04": {
		Selector:        "ubuntu:24.04",
		AWSName:         "ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*",
		AWSArm64Name:    "ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-arm64-server-*",
		AWSLabel:        "Ubuntu 24.04",
		AzureImage:      "Canonical:ubuntu-24_04-lts:server:latest",
		AzureArm64Image: "Canonical:ubuntu-24_04-lts:server-arm64:latest",
		GCPImage:        "projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-amd64",
		HetznerImage:    "ubuntu-24.04",
		LinodeImage:     "linode/ubuntu24.04",
		DockerImage:     "docker.io/library/ubuntu:24.04",
		ContainerName:   "ubuntu:24.04",
		AppleVZImage:    "https://cloud-images.ubuntu.com/releases/noble/release-20260518/ubuntu-24.04-server-cloudimg-arm64.img",
		AppleVZSHA256:   "6a61b967ba4a27dd1966f835a67643073ed55c2860ce3dc1cb0517282e6b8bec",
	},
	"ubuntu:26.04": {
		Selector:        "ubuntu:26.04",
		AWSName:         "ubuntu/images/hvm-ssd-gp3/ubuntu-resolute-26.04-amd64-server-*",
		AWSArm64Name:    "ubuntu/images/hvm-ssd-gp3/ubuntu-resolute-26.04-arm64-server-*",
		AWSLabel:        "Ubuntu 26.04",
		AzureImage:      "Canonical:ubuntu-26_04-lts:server:latest",
		AzureArm64Image: "Canonical:ubuntu-26_04-lts:server-arm64:latest",
		GCPImage:        "projects/ubuntu-os-cloud/global/images/family/ubuntu-2604-lts-amd64",
		HetznerImage:    "ubuntu-24.04",
		DockerImage:     "docker.io/library/ubuntu:26.04",
		ContainerName:   "ubuntu:26.04",
		AppleVZImage:    "https://cloud-images.ubuntu.com/releases/resolute/release-20260520/ubuntu-26.04-server-cloudimg-arm64.img",
		AppleVZSHA256:   "5e091e27d60116efbb0c743b8dd5cb2d15618e414ef04db0817ed43c8e2d7c7b",
	},
}

func normalizeOSImage(value string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if normalized == "" {
		normalized = defaultOSImage
	}
	normalized = strings.ReplaceAll(normalized, "_", ".")
	normalized = strings.ReplaceAll(normalized, "-", ":")
	switch normalized {
	case "ubuntu2404":
		normalized = "ubuntu:24.04"
	case "ubuntu:2404":
		normalized = "ubuntu:24.04"
	case "ubuntu2604":
		normalized = "ubuntu:26.04"
	case "ubuntu:2604":
		normalized = "ubuntu:26.04"
	}
	if _, ok := osImageSpecs[normalized]; !ok {
		return "", exit(2, "unsupported os %q; supported: ubuntu:26.04, ubuntu:24.04", value)
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
	if cfg.Provider == "apple-vz" || cfg.Provider == "applevz" || cfg.Provider == "aws-lambda-microvm" {
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

func osImageDefaultAppleVZImage(value string) (string, error) {
	spec, err := osImageSpecFor(value)
	if err != nil {
		return "", err
	}
	return spec.AppleVZImage, nil
}

func OSImageDefaultAppleVZImage(value string) (string, error) {
	return osImageDefaultAppleVZImage(value)
}

func osImageDefaultAppleVZSHA256(value string) (string, error) {
	spec, err := osImageSpecFor(value)
	if err != nil {
		return "", err
	}
	return spec.AppleVZSHA256, nil
}

func OSImageDefaultAppleVZSHA256(value string) (string, error) {
	return osImageDefaultAppleVZSHA256(value)
}
