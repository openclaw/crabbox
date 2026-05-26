package cli

import "strings"

const defaultOSImage = "ubuntu:26.04"

type osImageSpec struct {
	Selector      string
	AWSName       string
	AWSLabel      string
	AzureImage    string
	GCPImage      string
	HetznerImage  string
	DockerImage   string
	ContainerName string
}

var osImageSpecs = map[string]osImageSpec{
	"ubuntu:24.04": {
		Selector:      "ubuntu:24.04",
		AWSName:       "ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*",
		AWSLabel:      "Ubuntu 24.04",
		AzureImage:    "Canonical:ubuntu-24_04-lts:server:latest",
		GCPImage:      "projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-amd64",
		HetznerImage:  "ubuntu-24.04",
		DockerImage:   "docker.io/library/ubuntu:24.04",
		ContainerName: "ubuntu:24.04",
	},
	"ubuntu:26.04": {
		Selector:      "ubuntu:26.04",
		AWSName:       "ubuntu/images/hvm-ssd-gp3/ubuntu-resolute-26.04-amd64-server-*",
		AWSLabel:      "Ubuntu 26.04",
		AzureImage:    "Canonical:ubuntu-26_04-lts:server:latest",
		GCPImage:      "projects/ubuntu-os-cloud/global/images/family/ubuntu-2604-lts-amd64",
		HetznerImage:  "ubuntu-24.04",
		DockerImage:   "docker.io/library/ubuntu:26.04",
		ContainerName: "ubuntu:26.04",
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

func osImageSpecFor(value string) (osImageSpec, error) {
	normalized, err := normalizeOSImage(value)
	if err != nil {
		return osImageSpec{}, err
	}
	return osImageSpecs[normalized], nil
}

func awsLinuxAMIQueryForOS(value string) (name string, label string, err error) {
	spec, err := osImageSpecFor(value)
	if err != nil {
		return "", "", err
	}
	return spec.AWSName, spec.AWSLabel, nil
}

func osImageDefaultProviderImages(value string) (hetzner, azure, gcp, docker, container string, err error) {
	spec, err := osImageSpecFor(value)
	if err != nil {
		return "", "", "", "", "", err
	}
	return spec.HetznerImage, spec.AzureImage, spec.GCPImage, spec.DockerImage, spec.ContainerName, nil
}
