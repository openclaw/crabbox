package cli

import (
	"encoding/hex"
	"net/url"
	"regexp"
	"testing"
)

func TestUbuntu2604AppleVMImageSourceContract(t *testing.T) {
	t.Parallel()
	const (
		approvedURL    = "https://cloud-images.ubuntu.com/releases/resolute/release-20260731/ubuntu-26.04-server-cloudimg-arm64.img"
		approvedSHA256 = "3e113fdd41f39e13729375173bb2ae793f87dc6db4294e5251ff2476971788ba"
	)

	spec := osImageSpecs["ubuntu:26.04"]
	if spec.AppleVMImage != approvedURL || spec.AppleVMSHA256 != approvedSHA256 {
		t.Fatalf("Ubuntu 26.04 Apple VM source=(%q, %q), want approved immutable source", spec.AppleVMImage, spec.AppleVMSHA256)
	}
	imageURL, err := url.Parse(spec.AppleVMImage)
	if err != nil {
		t.Fatal(err)
	}
	if imageURL.Scheme != "https" || imageURL.Host != "cloud-images.ubuntu.com" || imageURL.User != nil || imageURL.RawQuery != "" || imageURL.Fragment != "" {
		t.Fatalf("Ubuntu 26.04 Apple VM source is not a credential-free canonical HTTPS URL: %q", spec.AppleVMImage)
	}
	if !regexp.MustCompile(`^/releases/resolute/release-[0-9]{8}/ubuntu-26[.]04-server-cloudimg-arm64[.]img$`).MatchString(imageURL.Path) {
		t.Fatalf("Ubuntu 26.04 Apple VM source is not pinned to an immutable dated release: %q", spec.AppleVMImage)
	}
	if len(spec.AppleVMSHA256) != 64 {
		t.Fatalf("Ubuntu 26.04 Apple VM digest length=%d, want 64", len(spec.AppleVMSHA256))
	}
	if _, err := hex.DecodeString(spec.AppleVMSHA256); err != nil {
		t.Fatalf("Ubuntu 26.04 Apple VM digest is not canonical hex: %v", err)
	}
}

func TestNormalizeOSImage(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":             "ubuntu:26.04",
		"ubuntu:26.04": "ubuntu:26.04",
		"ubuntu-26.04": "ubuntu:26.04",
		"ubuntu2604":   "ubuntu:26.04",
		"ubuntu:24.04": "ubuntu:24.04",
		"ubuntu-24.04": "ubuntu:24.04",
		"ubuntu2404":   "ubuntu:24.04",
	}
	for input, want := range cases {
		input, want := input, want
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			got, err := normalizeOSImage(input)
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("normalizeOSImage(%q)=%q want %q", input, got, want)
			}
		})
	}
}

func TestAWSLinuxAMIQueryForOS(t *testing.T) {
	t.Parallel()
	name, label, err := awsLinuxAMIQueryForOS("ubuntu:26.04", ArchitectureAMD64)
	if err != nil {
		t.Fatal(err)
	}
	if name != "ubuntu/images/hvm-ssd-gp3/ubuntu-resolute-26.04-amd64-server-*" || label != "Ubuntu 26.04" {
		t.Fatalf("query name=%q label=%q", name, label)
	}
	name, label, err = awsLinuxAMIQueryForOS("ubuntu:24.04", ArchitectureARM64)
	if err != nil {
		t.Fatal(err)
	}
	if name != "ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-arm64-server-*" || label != "Ubuntu 24.04" {
		t.Fatalf("arm query name=%q label=%q", name, label)
	}
}
