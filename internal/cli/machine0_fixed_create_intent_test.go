package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestFixedMachine0CreateIntentFingerprintStabilityAndCoverage(t *testing.T) {
	cfg := BaseConfig()
	cfg.Provider = "machine0"
	cfg.Profile = "ci"
	cfg.TargetOS = TargetLinux
	cfg.Architecture = "amd64"
	cfg.OSImage = "ubuntu:24.04"
	cfg.Class = "standard"
	cfg.ServerType = "large"
	cfg.ServerTypeExplicit = true
	cfg.Machine0.Image = "ubuntu-24-04-loaded"
	cfg.Machine0.ImageVersion = 2
	cfg.Machine0.DesktopImage = "ubuntu-desktop"
	cfg.Machine0.Size = "large"
	cfg.Machine0.Region = "eu"
	cfg.Machine0.Key = "ci-key"
	cfg.Machine0.WorkRoot = "/home/ubuntu/crabbox"
	cfg.Machine0.ReleasePolicy = "destroy"
	cfg.Desktop = true
	cfg.DesktopEnv = "gnome"
	cfg.Browser = true
	cfg.Code = true
	cfg.imageRequirements = imageRequirements{MinOS: "24.04", SDKs: map[string]string{"go": "1.26"}}
	cfg.Network = NetworkPublic
	cfg.SSHUser = "ubuntu"
	cfg.SSHPort = "22"
	cfg.SSHFallbackPorts = []string{"2222", "2200"}
	cfg.TTL = 4 * time.Hour
	cfg.IdleTimeout = 45 * time.Minute
	cfg.Pond = "default"
	cfg.ExposedPorts = []string{"8080", "3000"}
	cfg.WorkRoot = "/home/ubuntu/crabbox"
	cfg.Cache = CacheConfig{
		Pnpm: true, Npm: true, Docker: true, Git: true, MaxGB: 80, PurgeOnRelease: true,
		Volumes: []CacheVolumeConfig{{Name: "npm", Key: "npm-key", Path: "/var/cache/npm", SizeGB: 20, Required: true}},
	}
	req := FixedMachine0CreateIntentRequest{RequestedSlug: "fixed-worker", Keep: true}

	want, err := FixedMachine0CreateIntentFingerprint(cfg, req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := FixedMachine0CreateIntentFingerprint(cfg, req)
	if err != nil || again != want {
		t.Fatalf("stable fingerprint=%q want=%q err=%v", again, want, err)
	}

	tests := []struct {
		name   string
		mutate func(*Config, *FixedMachine0CreateIntentRequest)
	}{
		{name: "requested slug", mutate: func(_ *Config, req *FixedMachine0CreateIntentRequest) { req.RequestedSlug = "other-worker" }},
		{name: "keep", mutate: func(_ *Config, req *FixedMachine0CreateIntentRequest) { req.Keep = false }},
		{name: "provider", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) { cfg.Provider = "other" }},
		{name: "profile", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) { cfg.Profile = "other" }},
		{name: "target OS", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) { cfg.TargetOS = TargetMacOS }},
		{name: "architecture", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) { cfg.Architecture = "arm64" }},
		{name: "OS image", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) { cfg.OSImage = "ubuntu:26.04" }},
		{name: "class", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) { cfg.Class = "fast" }},
		{name: "server type", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) { cfg.ServerType = "xlarge" }},
		{name: "server type explicit", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) { cfg.ServerTypeExplicit = false }},
		{name: "size", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) { cfg.Machine0.Size = "xlarge" }},
		{name: "region", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) { cfg.Machine0.Region = "us-east" }},
		{name: "image", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) { cfg.Machine0.Image = "nixos-loaded" }},
		{name: "image version", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) { cfg.Machine0.ImageVersion = 3 }},
		{name: "desktop image", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) { cfg.Machine0.DesktopImage = "other-desktop" }},
		{name: "key", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) { cfg.Machine0.Key = "other-key" }},
		{name: "machine work root", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) { cfg.Machine0.WorkRoot = "/work/machine0" }},
		{name: "release policy", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) { cfg.Machine0.ReleasePolicy = "suspend" }},
		{name: "desktop", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) { cfg.Desktop = false }},
		{name: "desktop env", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) { cfg.DesktopEnv = "wayland" }},
		{name: "browser", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) { cfg.Browser = false }},
		{name: "code", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) { cfg.Code = false }},
		{name: "image requirements min OS", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) {
			cfg.imageRequirements = imageRequirements{MinOS: "26.04", SDKs: map[string]string{"go": "1.26"}}
		}},
		{name: "image requirements SDKs", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) {
			cfg.imageRequirements = imageRequirements{MinOS: "24.04", SDKs: map[string]string{"go": "1.27"}}
		}},
		{name: "image requirements runtimes", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) {
			cfg.imageRequirements = imageRequirements{MinOS: "24.04", SDKs: map[string]string{"go": "1.26"}, Runtimes: map[string]string{"node": "24"}}
		}},
		{name: "image requirements browser", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) {
			cfg.imageRequirements = imageRequirements{MinOS: "24.04", SDKs: map[string]string{"go": "1.26"}, Browser: true}
		}},
		{name: "image requirements WebView2", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) {
			cfg.imageRequirements = imageRequirements{MinOS: "24.04", SDKs: map[string]string{"go": "1.26"}, WebView2: true}
		}},
		{name: "image requirements desktop", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) {
			cfg.imageRequirements = imageRequirements{MinOS: "24.04", SDKs: map[string]string{"go": "1.26"}, Desktop: true}
		}},
		{name: "network mode", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) { cfg.Network = NetworkTailscale }},
		{name: "SSH user", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) { cfg.SSHUser = "nix" }},
		{name: "SSH port", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) { cfg.SSHPort = "2222" }},
		{name: "SSH fallback ports", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) { cfg.SSHFallbackPorts = []string{"2022"} }},
		{name: "TTL", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) { cfg.TTL = 5 * time.Hour }},
		{name: "idle timeout", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) { cfg.IdleTimeout = time.Hour }},
		{name: "pond", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) { cfg.Pond = "other" }},
		{name: "exposed ports", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) { cfg.ExposedPorts = []string{"9090"} }},
		{name: "work root", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) { cfg.WorkRoot = "/work/repo" }},
		{name: "cache pnpm", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) { cfg.Cache.Pnpm = false }},
		{name: "cache npm", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) { cfg.Cache.Npm = false }},
		{name: "cache docker", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) { cfg.Cache.Docker = false }},
		{name: "cache git", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) { cfg.Cache.Git = false }},
		{name: "cache max", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) { cfg.Cache.MaxGB = 81 }},
		{name: "cache purge", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) { cfg.Cache.PurgeOnRelease = false }},
		{name: "cache volume name", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) {
			cfg.Cache.Volumes = []CacheVolumeConfig{{Name: "npm-other", Key: "npm-key", Path: "/var/cache/npm", SizeGB: 20, Required: true}}
		}},
		{name: "cache volume key", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) {
			cfg.Cache.Volumes = []CacheVolumeConfig{{Name: "npm", Key: "npm-other", Path: "/var/cache/npm", SizeGB: 20, Required: true}}
		}},
		{name: "cache volume path", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) {
			cfg.Cache.Volumes = []CacheVolumeConfig{{Name: "npm", Key: "npm-key", Path: "/var/cache/npm-other", SizeGB: 20, Required: true}}
		}},
		{name: "cache volume size", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) {
			cfg.Cache.Volumes = []CacheVolumeConfig{{Name: "npm", Key: "npm-key", Path: "/var/cache/npm", SizeGB: 21, Required: true}}
		}},
		{name: "cache volume required", mutate: func(cfg *Config, _ *FixedMachine0CreateIntentRequest) {
			cfg.Cache.Volumes = []CacheVolumeConfig{{Name: "npm", Key: "npm-key", Path: "/var/cache/npm", SizeGB: 20}}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			changedCfg, changedReq := cfg, req
			tc.mutate(&changedCfg, &changedReq)
			got, err := FixedMachine0CreateIntentFingerprint(changedCfg, changedReq)
			if err != nil {
				t.Fatal(err)
			}
			if got == want {
				t.Fatalf("covered field did not change fingerprint %s", want)
			}
		})
	}
}

func TestFixedMachine0CreateIntentNormalizesAndExcludesTuningAndSecrets(t *testing.T) {
	const secret = "machine0-fixed-secret-sentinel"
	left := BaseConfig()
	left.Provider = "machine0"
	left.Machine0.ReleasePolicy = " SUSPEND "
	left.SSHFallbackPorts = []string{"22", "2222", "22"}
	left.ExposedPorts = []string{"8080", "80", "8080"}
	left.Cache.Volumes = []CacheVolumeConfig{
		{Name: " npm ", Key: " npm-key ", Path: " /var/cache/npm ", SizeGB: 20},
		{Name: "", Key: "pnpm-key", Path: "/var/cache/pnpm", Required: true},
	}
	left.Machine0.CLIPath = "/first/machine0"
	left.Machine0.CreateTimeout = time.Minute
	left.Machine0.PollInterval = time.Second
	left.CoordToken = secret
	left.CoordAdminToken = secret
	left.Access.ClientSecret = secret
	left.SSHKey = secret
	left.Tailscale.AuthKey = secret

	right := left
	right.Machine0.ReleasePolicy = "suspend"
	right.SSHFallbackPorts = []string{"22", "2222"}
	right.ExposedPorts = []string{"80", "8080"}
	right.Cache.Volumes = []CacheVolumeConfig{
		{Name: "pnpm-key", Key: "pnpm-key", Path: "/var/cache/pnpm", Required: true},
		{Name: "npm", Key: "npm-key", Path: "/var/cache/npm", SizeGB: 20},
	}
	right.Machine0.CLIPath = "/other/machine0"
	right.Machine0.CreateTimeout = 20 * time.Minute
	right.Machine0.PollInterval = 30 * time.Second
	right.CoordToken = "rotated"
	right.CoordAdminToken = "rotated"
	right.Access.ClientSecret = "rotated"
	right.SSHKey = "rotated"
	right.Tailscale.AuthKey = "rotated"

	req := FixedMachine0CreateIntentRequest{RequestedSlug: " Fixed__Worker ", Keep: true}
	leftIntent, err := fixedMachine0CreateIntentForConfig(left, req)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(leftIntent)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secret) {
		t.Fatalf("fixed intent persisted secret material: %s", data)
	}
	leftFingerprint, err := FixedMachine0CreateIntentFingerprint(left, req)
	if err != nil {
		t.Fatal(err)
	}
	rightFingerprint, err := FixedMachine0CreateIntentFingerprint(right, FixedMachine0CreateIntentRequest{RequestedSlug: "fixed-worker", Keep: true})
	if err != nil {
		t.Fatal(err)
	}
	if leftFingerprint != rightFingerprint {
		t.Fatalf("normalized or excluded fields changed fingerprint: left=%s right=%s", leftFingerprint, rightFingerprint)
	}
}
