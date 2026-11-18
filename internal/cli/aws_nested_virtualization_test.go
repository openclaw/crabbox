package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func TestAWSInstanceTypeNestedVirtualizationFamilies(t *testing.T) {
	for _, tc := range []struct {
		instanceType string
		virtual      bool
		kvm          bool
	}{
		{"c8i.2xlarge", true, true},
		{"C8I.2XLARGE", true, true},
		{"m8i-flex.large", true, true},
		{"m7i.large", true, true},
		{"m7i-flex.xlarge", true, true},
		{"c7i.4xlarge", true, true},
		{"c7i-flex.large", true, true},
		{"c8id.4xlarge", true, true},
		{"c8i-flex.large", true, true},
		{"m8id.large", true, true},
		{"r7i.large", true, true},
		{"r7iz.large", true, true},
		{"r8i.large", true, true},
		{"r8id.large", true, true},
		{"r8i-flex.large", true, true},
		{"x8i.large", true, true},
		{"i7i.large", true, true},
		{"i7ie.large", true, true},
		{"c7i.metal-24xl", false, true},
		{"m5.metal", false, true},
		{"c7a.2xlarge", false, false},
		{"m7a.large", false, false},
		{"t3.small", false, false},
		{"c7g.2xlarge", false, false},
		{"c8i", false, false},
		{"", false, false},
	} {
		if got := awsInstanceTypeSupportsLinuxNestedVirtualization(tc.instanceType); got != tc.virtual {
			t.Errorf("awsInstanceTypeSupportsLinuxNestedVirtualization(%q)=%v want %v", tc.instanceType, got, tc.virtual)
		}
		if got := AWSInstanceTypeProvidesLinuxKVM(tc.instanceType); got != tc.kvm {
			t.Errorf("AWSInstanceTypeProvidesLinuxKVM(%q)=%v want %v", tc.instanceType, got, tc.kvm)
		}
	}
}

func TestAWSNestedVirtualizationClassCandidatesStayInSupportedFamilies(t *testing.T) {
	for _, class := range CanonicalProviderClasses() {
		candidates := AWSNestedVirtualizationInstanceTypeCandidatesForClass(class)
		if len(candidates) == 0 {
			t.Fatalf("class=%s has no nested virtualization candidates", class)
		}
		for _, candidate := range candidates {
			if !awsInstanceTypeSupportsLinuxNestedVirtualization(candidate) {
				t.Errorf("class=%s candidate %q lacks nested virtualization", class, candidate)
			}
		}
	}
	if !awsInstanceTypeSupportsLinuxNestedVirtualization(awsNestedVirtualizationFallbackInstanceType) {
		t.Fatalf("fallback %q lacks nested virtualization", awsNestedVirtualizationFallbackInstanceType)
	}
}

func TestAWSLaunchCandidatesWithNestedVirtualization(t *testing.T) {
	base := Config{Provider: "aws", TargetOS: targetLinux, Architecture: ArchitectureAMD64, AWSNestedVirtualization: true}

	beast := base
	beast.Class = "beast"
	// A stored default from the non-nested table is dropped rather than launched.
	beast.ServerType = "c7a.48xlarge"
	got := AWSLaunchCandidates(beast)
	if len(got) == 0 || got[0] != "c8i.48xlarge" {
		t.Fatalf("beast candidates=%v want c8i.48xlarge first", got)
	}
	for _, candidate := range got {
		if !awsInstanceTypeSupportsLinuxNestedVirtualization(candidate) {
			t.Fatalf("candidate %q lacks nested virtualization: %v", candidate, got)
		}
	}

	literal := base
	literal.Class = "c8i.4xlarge"
	if got := AWSLaunchCandidates(literal); strings.Join(got, ",") != "c8i.4xlarge,m7i.large" {
		t.Fatalf("literal class candidates=%v", got)
	}

	exact := base
	exact.Class = "beast"
	exact.ServerType = "c8i.2xlarge"
	exact.ServerTypeExplicit = true
	if got := AWSLaunchCandidates(exact); strings.Join(got, ",") != "c8i.2xlarge" {
		t.Fatalf("exact candidates=%v", got)
	}

	off := base
	off.AWSNestedVirtualization = false
	off.Class = "beast"
	if got := AWSLaunchCandidates(off); got[0] != "c7a.48xlarge" || got[len(got)-1] != "t3.small" {
		t.Fatalf("default candidates changed: %v", got)
	}
}

func TestAWSCapacityRecommendationsWithNestedVirtualization(t *testing.T) {
	cfg := Config{Provider: "aws", TargetOS: targetLinux, Architecture: ArchitectureAMD64, AWSNestedVirtualization: true}
	recommendations := awsCapacityRecommendationCandidates(cfg)
	if len(recommendations) == 0 {
		t.Fatal("no recommendations")
	}
	for _, recommendation := range recommendations {
		if !awsInstanceTypeSupportsLinuxNestedVirtualization(recommendation.serverType) {
			t.Fatalf("recommendation %+v lacks nested virtualization", recommendation)
		}
	}
}

func TestApplyAWSRunInstanceTargetOptionsLinuxNestedVirtualization(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want bool
	}{
		{"linux nested", Config{TargetOS: targetLinux, ServerType: "c8i.2xlarge", AWSNestedVirtualization: true}, true},
		{"linux metal", Config{TargetOS: targetLinux, ServerType: "c7i.metal-24xl", AWSNestedVirtualization: true}, false},
		{"linux default", Config{TargetOS: targetLinux, ServerType: "c8i.2xlarge"}, false},
		{"wsl2", Config{TargetOS: targetWindows, WindowsMode: windowsModeWSL2, ServerType: "m8i.large"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := &ec2.RunInstancesInput{}
			applyAWSRunInstanceTargetOptions(input, tc.cfg)
			got := input.CpuOptions != nil && input.CpuOptions.NestedVirtualization == types.NestedVirtualizationSpecificationEnabled
			if got != tc.want {
				t.Fatalf("nested=%v want %v (CpuOptions=%#v)", got, tc.want, input.CpuOptions)
			}
		})
	}
}

func TestValidateProviderTargetAWSNestedVirtualization(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{name: "class", mutate: func(cfg *Config) { cfg.Class = "small" }},
		{name: "supported type", mutate: func(cfg *Config) { cfg.ServerType, cfg.ServerTypeExplicit = "c8i.2xlarge", true }},
		{name: "metal type", mutate: func(cfg *Config) { cfg.ServerType, cfg.ServerTypeExplicit = "c7i.metal-24xl", true }},
		{name: "wsl2", mutate: func(cfg *Config) { cfg.TargetOS, cfg.WindowsMode = targetWindows, windowsModeWSL2 }},
		{name: "unsupported type", mutate: func(cfg *Config) { cfg.ServerType, cfg.ServerTypeExplicit = "c7a.2xlarge", true }, want: "c7a.2xlarge is not supported"},
		{name: "unsupported class literal", mutate: func(cfg *Config) { cfg.Class = "t3.large" }, want: "class=t3.large is not supported"},
		{name: "arm64", mutate: func(cfg *Config) { cfg.Architecture, cfg.architectureExplicit = ArchitectureARM64, true }, want: "requires architecture=amd64"},
		{name: "native windows", mutate: func(cfg *Config) { cfg.TargetOS = targetWindows }, want: "requires target=linux"},
		{name: "other provider", mutate: func(cfg *Config) { cfg.Provider = "hetzner" }, want: "requires provider=aws"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Provider = "aws"
			cfg.TargetOS = targetLinux
			cfg.Class = "beast"
			cfg.AWSNestedVirtualization = true
			tc.mutate(&cfg)
			err := validateProviderTarget(cfg)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("err=%v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want %q", err, tc.want)
			}
		})
	}
}

func TestCoordinatorAWSNestedVirtualizationInput(t *testing.T) {
	for _, tc := range []struct {
		name, provider, file, env string
		want                      bool
		wantServerType            string
	}{
		{"environment", "aws", "", "1", true, "c8i.48xlarge"},
		{"file", "aws", "aws:\n  nestedVirtualization: true\n", "", true, "c8i.48xlarge"},
		{"environment overrides file", "aws", "aws:\n  nestedVirtualization: true\n", "0", false, "c7a.48xlarge"},
		{"unset", "aws", "", "", false, "c7a.48xlarge"},
		{"other provider", "gcp", "aws:\n  nestedVirtualization: true\n", "1", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
			configPath := filepath.Join(home, "crabbox.yaml")
			t.Setenv("CRABBOX_CONFIG", configPath)
			t.Setenv("CRABBOX_AWS_NESTED_VIRTUALIZATION", tc.env)
			if err := os.WriteFile(configPath, []byte("provider: "+tc.provider+"\nclass: beast\n"+tc.file), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := loadConfig()
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantServerType != "" && cfg.ServerType != tc.wantServerType {
				t.Fatalf("serverType=%q want %q", cfg.ServerType, tc.wantServerType)
			}
			var body map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				_, _ = w.Write([]byte(`{"lease":{"id":"cbx_123"}}`))
			}))
			defer server.Close()
			client := CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
			if _, err := client.CreateLease(context.Background(), cfg, "ssh-ed25519 test", false, "cbx_123", "blue-crab"); err != nil {
				t.Fatal(err)
			}
			value, present := body["awsNestedVirtualization"]
			if tc.want && value != true {
				t.Fatalf("awsNestedVirtualization=%v, want true", value)
			}
			if !tc.want && present {
				t.Fatalf("awsNestedVirtualization must be omitted: %v", value)
			}
		})
	}
}
