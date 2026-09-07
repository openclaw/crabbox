package cli

import (
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
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

func TestOSImageSharedFixtures(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../../testdata/bootstrap/os-images.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct{ Input, Selector string }
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		got, err := normalizeOSImage(fixture.Input)
		if fixture.Selector == "" {
			if err == nil {
				t.Errorf("accepted unsupported OS %q", fixture.Input)
			}
		} else if err != nil || got != fixture.Selector {
			t.Errorf("normalizeOSImage(%q) = %q, %v; want %q", fixture.Input, got, err, fixture.Selector)
		}
	}
	for _, spec := range osImageSpecs {
		for _, architecture := range []string{ArchitectureAMD64, ArchitectureARM64} {
			name, label, err := awsLinuxAMIQueryForOS(spec.Selector, architecture)
			want := spec.AWSName
			if architecture == ArchitectureARM64 {
				want = spec.AWSArm64Name
			}
			if err != nil || name != want || label != spec.AWSLabel {
				t.Errorf("AWS %s/%s: %s %s %v", spec.Selector, architecture, name, label, err)
			}
			_, azure, _, _, _, _, err := osImageDefaultProviderImagesForArchitecture(spec.Selector, architecture)
			want = spec.AzureImage
			if architecture == ArchitectureARM64 {
				want = spec.AzureArm64Image
			}
			if err != nil || azure != want {
				t.Errorf("Azure %s/%s: %s %v", spec.Selector, architecture, azure, err)
			}
		}
	}
}

func TestContainerDefaultsUseReviewedMultiPlatformReferences(t *testing.T) {
	t.Parallel()
	for _, selector := range []string{"ubuntu:24.04", "ubuntu:26.04"} {
		for _, arch := range []string{ArchitectureAMD64, ArchitectureARM64} {
			cfg := baseConfig()
			cfg.OSImage, cfg.Architecture = selector, arch
			cfg.architectureExplicit = true
			applyOSImageProviderDefaults(&cfg, false)
			for _, image := range []string{cfg.LocalContainer.Image, cfg.AppleContainer.Image} {
				if !regexp.MustCompile(`^docker[.]io/library/ubuntu@sha256:[a-f0-9]{64}$`).MatchString(image) {
					t.Fatalf("%s/%s container default is not immutable: %q", selector, arch, image)
				}
				if digest, known := DefaultContainerImageDigest(image); !known || len(digest) != 71 {
					t.Fatalf("%s/%s default has no reviewed digest", selector, arch)
				}
			}
		}
	}
	for _, custom := range []string{"ubuntu:26.04", "docker.io/library/ubuntu:24.04", "example-org/custom:latest"} {
		cfg := baseConfig()
		cfg.LocalContainer.Image, cfg.AppleContainer.Image = custom, custom
		cfg.localContainerImageExplicit, cfg.appleContainerImageExplicit = true, true
		cfg.OSImage = "ubuntu:24.04"
		applyOSImageProviderDefaults(&cfg, false)
		if cfg.LocalContainer.Image != custom || cfg.AppleContainer.Image != custom {
			t.Fatal("explicit custom image replaced by catalog default")
		}
		if _, known := DefaultContainerImageDigest(custom); known {
			t.Fatal("custom image classified as a reviewed default")
		}
	}
}
