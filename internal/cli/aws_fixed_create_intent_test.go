package cli

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestFixedAWSCreateIntentFingerprintGolden(t *testing.T) {
	// This literal pins durable on-disk state and must never be "updated" to match new behavior.
	cfg := BaseConfig()
	cfg.Provider = "aws-golden"
	cfg.Profile = "profile-golden"
	cfg.TargetOS = TargetLinux
	cfg.Architecture = "arm64"
	cfg.OSImage = "ubuntu:24.04"
	cfg.WindowsMode = "normal"
	cfg.Class = "class-golden"
	cfg.ServerType = "m7g.2xlarge"
	cfg.ServerTypeExplicit = true
	cfg.HostID = "h-aws-golden"
	cfg.AWSMacHostID = "h-aws-fallback-golden"
	cfg.AWSRegion = "eu-west-2"
	cfg.AWSAMI = "ami-aws-golden"
	cfg.AWSSnapshot = "snap-aws-golden"
	cfg.AWSSGID = "sg-aws-golden"
	cfg.AWSSubnetID = "subnet-aws-golden"
	cfg.AWSProfile = "instance-profile-golden"
	cfg.AWSRootGB = 317
	cfg.AWSSSHCIDRsPinned = true
	cfg.AWSSSHCIDRs = []string{"203.0.113.17/32", "198.51.100.29/32"}
	cfg.Desktop = true
	cfg.DesktopEnv = "gnome"
	cfg.Browser = true
	cfg.Code = true
	cfg.imageRequirements = imageRequirements{
		MinOS: "24.04", SDKs: map[string]string{"go": "1.26.3"}, Runtimes: map[string]string{"node": "24.1"},
		Browser: true, WebView2: true, Desktop: true,
	}
	cfg.Network = NetworkPublic
	cfg.SSHUser = "aws-golden-user"
	cfg.SSHPort = "2022"
	cfg.SSHFallbackPorts = []string{"2023", "2024"}
	cfg.ProviderKey = "aws-golden-provider-key"
	cfg.Tailscale = TailscaleConfig{
		Enabled: true, Tags: []string{"tag:golden-b", "tag:golden-a"},
		HostnameTemplate: "golden-{{.Slug}}", Hostname: "aws-golden-host",
		ExitNode: "aws-golden-exit", ExitNodeAllowLANAccess: true,
	}
	cfg.Capacity = CapacityConfig{
		Market: "spot", Strategy: "capacity-optimized", Fallback: "on-demand",
		Regions: []string{"eu-west-2", "eu-central-1"}, AvailabilityZones: []string{"eu-west-2b", "eu-west-2a"}, Hints: true,
	}
	cfg.TTL = 7*time.Hour + 23*time.Minute
	cfg.IdleTimeout = 41*time.Minute + 19*time.Second
	cfg.Pond = "aws-golden-pond"
	cfg.ExposedPorts = []string{"8443", "9090"}
	cfg.WorkRoot = "/srv/aws-golden-work"
	cfg.Cache = CacheConfig{
		Pnpm: true, Npm: true, Docker: true, Git: true, MaxGB: 73, PurgeOnRelease: true,
		Volumes: []CacheVolumeConfig{{Name: "aws-golden-volume", Key: "aws-golden-key", Path: "/var/cache/aws-golden", SizeGB: 37, Required: true}},
	}

	got, err := FixedAWSCreateIntentFingerprint(cfg, FixedAWSCreateIntentRequest{
		AccountID: "123456789017", RequestedSlug: "aws-golden-lease", SSHPublicKey: "ssh-ed25519 AAAAawsGolden", Keep: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	const want = "b9da70cdeb1422676ab434ac324571652e617fc5a533226b02f7e34b737c7d00"
	if got != want {
		t.Fatalf("fingerprint=%s want=%s", got, want)
	}
}

func TestFixedAWSCreateIntentEquivalentNormalizedConfig(t *testing.T) {
	left := BaseConfig()
	left.Provider = "aws"
	left.HostID = ""
	left.AWSMacHostID = " host-123 "
	left.ExposedPorts = []string{"8080", "80", "8080"}
	left.AWSSSHCIDRsPinned = true
	left.AWSSSHCIDRs = []string{"203.0.113.8/32", "198.51.100.4/32", "203.0.113.8/32"}
	left.SSHFallbackPorts = []string{"22", "2222", "22"}
	left.Tailscale.Tags = []string{"tag:zeta", "tag:alpha", "tag:zeta"}
	left.Cache.Volumes = []CacheVolumeConfig{
		{Name: " npm ", Key: " npm-key ", Path: " /var/cache/npm ", SizeGB: 20},
		{Name: "", Key: "pnpm-key", Path: "/var/cache/pnpm", Required: true},
	}

	right := left
	right.HostID = "host-123"
	right.AWSMacHostID = ""
	right.ExposedPorts = []string{"80", "8080"}
	right.AWSSSHCIDRs = []string{"198.51.100.4/32", "203.0.113.8/32"}
	right.SSHFallbackPorts = []string{"22", "2222"}
	right.Tailscale.Tags = []string{"tag:alpha", "tag:zeta"}
	right.Cache.Volumes = []CacheVolumeConfig{
		{Name: "pnpm-key", Key: "pnpm-key", Path: "/var/cache/pnpm", Required: true},
		{Name: "npm", Key: "npm-key", Path: "/var/cache/npm", SizeGB: 20},
	}

	leftFingerprint, err := FixedAWSCreateIntentFingerprint(left, FixedAWSCreateIntentRequest{
		AccountID: " 123456789012 ", RequestedSlug: " Fixed__Lease ", SSHPublicKey: " ssh-ed25519 test ", Keep: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	rightFingerprint, err := FixedAWSCreateIntentFingerprint(right, FixedAWSCreateIntentRequest{
		AccountID: "123456789012", RequestedSlug: "fixed-lease", SSHPublicKey: "ssh-ed25519 test", Keep: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if leftFingerprint != rightFingerprint {
		t.Fatalf("equivalent normalized intents differ: left=%s right=%s", leftFingerprint, rightFingerprint)
	}
}

func TestFixedAWSCreateIntentExcludesSecrets(t *testing.T) {
	const firstSecret = "sentinel-first-secret"
	const secondSecret = "sentinel-second-secret"
	cfg := BaseConfig()
	cfg.Provider = "aws"
	cfg.CoordToken = firstSecret
	cfg.CoordAdminToken = firstSecret
	cfg.CoordPromotionToken = firstSecret
	cfg.Access.ClientID = firstSecret
	cfg.Access.ClientSecret = firstSecret
	cfg.Access.Token = firstSecret
	cfg.SSHKey = firstSecret
	cfg.Tailscale.AuthKey = firstSecret
	cfg.Tailscale.AuthKeyEnv = firstSecret
	cfg.Profiles = map[string]ProfileConfig{"secret": {Env: map[string]string{"TOKEN": firstSecret}}}

	request := FixedAWSCreateIntentRequest{
		AccountID: "123456789012", RequestedSlug: "secret-free", SSHPublicKey: "ssh-ed25519 public", Keep: true,
	}
	intent, err := fixedAWSCreateIntentForConfig(cfg, request)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), firstSecret) {
		t.Fatalf("fixed intent persisted secret material: %s", data)
	}
	firstFingerprint, err := FixedAWSCreateIntentFingerprint(cfg, request)
	if err != nil {
		t.Fatal(err)
	}
	cfg.CoordToken = secondSecret
	cfg.CoordAdminToken = secondSecret
	cfg.CoordPromotionToken = secondSecret
	cfg.Access.ClientID = secondSecret
	cfg.Access.ClientSecret = secondSecret
	cfg.Access.Token = secondSecret
	cfg.SSHKey = secondSecret
	cfg.Tailscale.AuthKey = secondSecret
	cfg.Tailscale.AuthKeyEnv = secondSecret
	cfg.Profiles = map[string]ProfileConfig{"secret": {Env: map[string]string{"TOKEN": secondSecret}}}
	secondFingerprint, err := FixedAWSCreateIntentFingerprint(cfg, request)
	if err != nil {
		t.Fatal(err)
	}
	if firstFingerprint != secondFingerprint {
		t.Fatalf("credential rotation changed immutable intent: first=%s second=%s", firstFingerprint, secondFingerprint)
	}
}

func TestFixedAWSCreateIntentClassifiesEveryExportedConfigField(t *testing.T) {
	included := map[string]bool{}
	collectFixedAWSConfigFields(reflect.TypeOf(fixedAWSCreateIntent{}), included)
	excluded := map[string]string{}
	classify := func(reason, fields string) {
		for _, name := range strings.Fields(fields) {
			if previous := excluded[name]; previous != "" {
				t.Fatalf("Config field %s classified twice as %s and %s", name, previous, reason)
			}
			excluded[name] = reason
		}
	}
	classify("coordinator transport or credential", `
		Coordinator BrokerMode BrokerLoginRedirectOrigins BrokerAutoWebVNC
		CoordToken CoordTokenCommand CoordAdminToken CoordPromotionToken Access SSHKey
	`)
	classify("non-AWS provider selection", `
		Location Image AWSLambdaMicroVM
		AzureSubscription AzureTenant AzureClientID AzureLocation AzureBackend AzureResourceGroup
		AzureImage AzureSnapshot AzureSnapshotSKU AzureOSDisk AzureOSDiskExplicit AzureOSDiskSKU
		AzureVNet AzureSubnet AzureNSG AzureSSHCIDRs AzureNetwork AzureDynamicSessions
		GCPProject GCPZone GCPImage GCPMachineImage GCPSnapshot GCPNetwork GCPSubnet GCPTags
		GCPSSHCIDRs GCPRootGB GCPServiceAccount DigitalOcean Vultr Linode GitHubCodespaces
		Lambda Nebius OVH Scaleway TencentCloud Incus Proxmox Firecracker XCPNg Parallels
		Blacksmith KubeVirt SealosDevbox AgentSandbox External Namespace NamespaceInstance
		Phala Coder Morph Daytona E2B CubeSandbox ExeDev Railway FastAPICloud UnikraftCloud
		Runpod Vast NvidiaBrev Hostinger Wandb Orgo Islo Freestyle Tenki Tensorlake Cua
		OpenComputer CodeSandbox OpenSandbox Nomad Blaxel VercelSandbox CloudflareSandbox
		Superserve Crownest DockerSandbox AnthropicSRT CloudRunSandbox Modal UpstashBox
		Smolvm AsciiBox Cloudflare CloudflareDynamicWorkers Semaphore Sprites LocalContainer
		AppleContainer AppleVM MXC Multipass Machine0 Tart Lume HyperV WindowsSandbox Static
	`)
	classify("post-acquisition command, transport, or reporting behavior", `
		Sync Run EnvAllow Actions Results Shard Profiles Presets ProofTemplates Jobs
	`)

	configType := reflect.TypeOf(Config{})
	known := map[string]bool{}
	for index := 0; index < configType.NumField(); index++ {
		field := configType.Field(index)
		if !field.IsExported() {
			continue
		}
		known[field.Name] = true
		_, isIncluded := included[field.Name]
		_, isExcluded := excluded[field.Name]
		if isIncluded == isExcluded {
			t.Errorf("exported Config field %s must be classified exactly once (included=%t excluded=%t)", field.Name, isIncluded, isExcluded)
		}
	}
	for name := range included {
		if !known[name] {
			t.Errorf("fixed intent configFields tag references unknown or unexported Config field %s", name)
		}
	}
	for name := range excluded {
		if !known[name] {
			t.Errorf("excluded fixed intent classification references unknown Config field %s", name)
		}
	}
}

func collectFixedAWSConfigFields(value reflect.Type, fields map[string]bool) {
	for index := 0; index < value.NumField(); index++ {
		field := value.Field(index)
		for _, name := range strings.Split(field.Tag.Get("configFields"), ",") {
			if name = strings.TrimSpace(name); name != "" {
				fields[name] = true
			}
		}
		if field.Type.Kind() == reflect.Struct &&
			(strings.HasPrefix(field.Type.Name(), "fixedAWSCreateIntent") || strings.HasPrefix(field.Type.Name(), "fixedCreateIntent")) {
			collectFixedAWSConfigFields(field.Type, fields)
		}
	}
}
