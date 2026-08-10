package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

const FixedAWSCreateIntentVersion = 2

type FixedAWSCreateIntentRequest struct {
	AccountID     string
	RequestedSlug string
	SSHPublicKey  string
	Keep          bool
}

type fixedAWSCreateIntent struct {
	Version       int                           `json:"version"`
	AccountID     string                        `json:"accountID"`
	RequestedSlug string                        `json:"requestedSlug"`
	Provider      string                        `json:"provider" configFields:"Provider"`
	Profile       string                        `json:"profile" configFields:"Profile"`
	Machine       fixedAWSCreateIntentMachine   `json:"machine"`
	Capabilities  fixedAWSCreateIntentFeatures  `json:"capabilities"`
	Networking    fixedAWSCreateIntentNetwork   `json:"networking"`
	Capacity      fixedAWSCreateIntentCapacity  `json:"capacity" configFields:"Capacity"`
	Lifecycle     fixedAWSCreateIntentLifecycle `json:"lifecycle"`
	Workload      fixedAWSCreateIntentWorkload  `json:"workload"`
	Cache         fixedAWSCreateIntentCache     `json:"cache" configFields:"Cache"`
}

type fixedAWSCreateIntentMachine struct {
	TargetOS           string `json:"targetOS" configFields:"TargetOS"`
	Architecture       string `json:"architecture" configFields:"Architecture"`
	OSImage            string `json:"osImage" configFields:"OSImage"`
	WindowsMode        string `json:"windowsMode" configFields:"WindowsMode"`
	Class              string `json:"class" configFields:"Class"`
	ServerType         string `json:"serverType" configFields:"ServerType"`
	ServerTypeExplicit bool   `json:"serverTypeExplicit" configFields:"ServerTypeExplicit"`
	HostID             string `json:"hostID,omitempty" configFields:"HostID,AWSMacHostID"`
	Region             string `json:"region" configFields:"AWSRegion"`
	AMI                string `json:"ami,omitempty" configFields:"AWSAMI"`
	Snapshot           string `json:"snapshot,omitempty" configFields:"AWSSnapshot"`
	SubnetID           string `json:"subnetID,omitempty" configFields:"AWSSubnetID"`
	InstanceProfile    string `json:"instanceProfile,omitempty" configFields:"AWSProfile"`
	RootGB             int32  `json:"rootGB" configFields:"AWSRootGB"`
}

type fixedAWSCreateIntentFeatures struct {
	Desktop           bool              `json:"desktop" configFields:"Desktop"`
	DesktopEnv        string            `json:"desktopEnv" configFields:"DesktopEnv"`
	Browser           bool              `json:"browser" configFields:"Browser"`
	Code              bool              `json:"code" configFields:"Code"`
	ImageRequirements imageRequirements `json:"imageRequirements"`
}

type fixedAWSCreateIntentNetwork struct {
	Mode            NetworkMode                   `json:"mode" configFields:"Network"`
	SecurityGroupID string                        `json:"securityGroupID,omitempty" configFields:"AWSSGID"`
	SSHCIDRsPinned  bool                          `json:"sshCIDRsPinned" configFields:"AWSSSHCIDRsPinned"`
	SSHCIDRs        []string                      `json:"sshCIDRs,omitempty" configFields:"AWSSSHCIDRs"`
	SSH             fixedAWSCreateIntentSSH       `json:"ssh"`
	Tailscale       fixedAWSCreateIntentTailscale `json:"tailscale" configFields:"Tailscale"`
}

type fixedAWSCreateIntentSSH struct {
	User            string   `json:"user" configFields:"SSHUser"`
	Port            string   `json:"port" configFields:"SSHPort"`
	FallbackPorts   []string `json:"fallbackPorts,omitempty" configFields:"SSHFallbackPorts"`
	ProviderKey     string   `json:"providerKey" configFields:"ProviderKey"`
	PublicKeySHA256 string   `json:"publicKeySHA256"`
}

type fixedAWSCreateIntentTailscale struct {
	Enabled                bool     `json:"enabled"`
	Tags                   []string `json:"tags,omitempty"`
	HostnameTemplate       string   `json:"hostnameTemplate,omitempty"`
	Hostname               string   `json:"hostname,omitempty"`
	ExitNode               string   `json:"exitNode,omitempty"`
	ExitNodeAllowLANAccess bool     `json:"exitNodeAllowLANAccess,omitempty"`
}

type fixedAWSCreateIntentCapacity struct {
	Market            string   `json:"market"`
	Strategy          string   `json:"strategy"`
	Fallback          string   `json:"fallback"`
	Regions           []string `json:"regions,omitempty"`
	AvailabilityZones []string `json:"availabilityZones,omitempty"`
	Hints             bool     `json:"hints"`
}

type fixedAWSCreateIntentLifecycle struct {
	Keep            bool  `json:"keep"`
	TTLNanoseconds  int64 `json:"ttlNanoseconds" configFields:"TTL"`
	IdleNanoseconds int64 `json:"idleNanoseconds" configFields:"IdleTimeout"`
}

type fixedAWSCreateIntentWorkload struct {
	Pond         string   `json:"pond,omitempty" configFields:"Pond"`
	ExposedPorts []string `json:"exposedPorts,omitempty" configFields:"ExposedPorts"`
	WorkRoot     string   `json:"workRoot" configFields:"WorkRoot"`
}

type fixedAWSCreateIntentCache struct {
	Pnpm           bool                              `json:"pnpm"`
	Npm            bool                              `json:"npm"`
	Docker         bool                              `json:"docker"`
	Git            bool                              `json:"git"`
	MaxGB          int                               `json:"maxGB"`
	PurgeOnRelease bool                              `json:"purgeOnRelease"`
	Volumes        []fixedAWSCreateIntentCacheVolume `json:"volumes,omitempty"`
}

type fixedAWSCreateIntentCacheVolume struct {
	Name     string `json:"name"`
	Key      string `json:"key"`
	Path     string `json:"path"`
	SizeGB   int    `json:"sizeGB"`
	Required bool   `json:"required"`
}

func FixedAWSCreateIntentFingerprint(cfg Config, req FixedAWSCreateIntentRequest) (string, error) {
	intent, err := fixedAWSCreateIntentForConfig(cfg, req)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(intent)
	if err != nil {
		return "", fmt.Errorf("encode fixed AWS create intent: %w", err)
	}
	digest := sha256.Sum256(append([]byte("crabbox-fixed-aws-create-intent-v2\x00"), data...))
	return hex.EncodeToString(digest[:]), nil
}

func fixedAWSCreateIntentForConfig(cfg Config, req FixedAWSCreateIntentRequest) (fixedAWSCreateIntent, error) {
	network, err := parseNetworkMode(string(cfg.Network))
	if err != nil {
		return fixedAWSCreateIntent{}, err
	}
	osImage, err := normalizeOSImage(cfg.OSImage)
	if err != nil {
		return fixedAWSCreateIntent{}, err
	}
	exposedPorts, err := requestedExposedPorts(cfg.ExposedPorts)
	if err != nil {
		return fixedAWSCreateIntent{}, err
	}
	cacheVolumes, err := fixedAWSCreateIntentCacheVolumes(cfg.Cache.Volumes)
	if err != nil {
		return fixedAWSCreateIntent{}, err
	}
	publicKeyHash := sha256.Sum256([]byte(strings.TrimSpace(req.SSHPublicKey)))
	rootGB := cfg.AWSRootGB
	if rootGB <= 0 {
		rootGB = 400
	}
	hostID := strings.TrimSpace(cfg.HostID)
	if hostID == "" {
		hostID = strings.TrimSpace(cfg.AWSMacHostID)
	}
	sshCIDRs := []string(nil)
	if cfg.AWSSSHCIDRsPinned {
		sshCIDRs = fixedAWSCanonicalStringSet(cfg.AWSSSHCIDRs)
	}
	return fixedAWSCreateIntent{
		Version:       FixedAWSCreateIntentVersion,
		AccountID:     strings.TrimSpace(req.AccountID),
		RequestedSlug: normalizeLeaseSlug(req.RequestedSlug),
		Provider:      strings.TrimSpace(cfg.Provider),
		Profile:       strings.TrimSpace(cfg.Profile),
		Machine: fixedAWSCreateIntentMachine{
			TargetOS:           strings.TrimSpace(cfg.TargetOS),
			Architecture:       effectiveArchitectureForConfig(cfg),
			OSImage:            osImage,
			WindowsMode:        strings.TrimSpace(cfg.WindowsMode),
			Class:              strings.TrimSpace(cfg.Class),
			ServerType:         strings.TrimSpace(cfg.ServerType),
			ServerTypeExplicit: cfg.ServerTypeExplicit,
			HostID:             hostID,
			Region:             strings.TrimSpace(cfg.AWSRegion),
			AMI:                strings.TrimSpace(cfg.AWSAMI),
			Snapshot:           strings.TrimSpace(cfg.AWSSnapshot),
			SubnetID:           strings.TrimSpace(cfg.AWSSubnetID),
			InstanceProfile:    strings.TrimSpace(cfg.AWSProfile),
			RootGB:             rootGB,
		},
		Capabilities: fixedAWSCreateIntentFeatures{
			Desktop:           cfg.Desktop,
			DesktopEnv:        normalizedDesktopEnv(cfg.DesktopEnv),
			Browser:           cfg.Browser,
			Code:              cfg.Code,
			ImageRequirements: cfg.imageRequirements,
		},
		Networking: fixedAWSCreateIntentNetwork{
			Mode:            network,
			SecurityGroupID: strings.TrimSpace(cfg.AWSSGID),
			SSHCIDRsPinned:  cfg.AWSSSHCIDRsPinned,
			SSHCIDRs:        sshCIDRs,
			SSH: fixedAWSCreateIntentSSH{
				User:            strings.TrimSpace(cfg.SSHUser),
				Port:            strings.TrimSpace(cfg.SSHPort),
				FallbackPorts:   fixedAWSCanonicalOrderedStrings(cfg.SSHFallbackPorts),
				ProviderKey:     strings.TrimSpace(cfg.ProviderKey),
				PublicKeySHA256: hex.EncodeToString(publicKeyHash[:]),
			},
			Tailscale: fixedAWSCreateIntentTailscale{
				Enabled:                cfg.Tailscale.Enabled,
				Tags:                   fixedAWSCanonicalStringSet(cfg.Tailscale.Tags),
				HostnameTemplate:       strings.TrimSpace(cfg.Tailscale.HostnameTemplate),
				Hostname:               strings.TrimSpace(cfg.Tailscale.Hostname),
				ExitNode:               strings.TrimSpace(cfg.Tailscale.ExitNode),
				ExitNodeAllowLANAccess: cfg.Tailscale.ExitNodeAllowLANAccess,
			},
		},
		Capacity: fixedAWSCreateIntentCapacity{
			Market:            strings.TrimSpace(cfg.Capacity.Market),
			Strategy:          strings.TrimSpace(cfg.Capacity.Strategy),
			Fallback:          strings.TrimSpace(cfg.Capacity.Fallback),
			Regions:           fixedAWSCanonicalOrderedStrings(cfg.Capacity.Regions),
			AvailabilityZones: fixedAWSCanonicalOrderedStrings(cfg.Capacity.AvailabilityZones),
			Hints:             cfg.Capacity.Hints,
		},
		Lifecycle: fixedAWSCreateIntentLifecycle{
			Keep:            req.Keep,
			TTLNanoseconds:  fixedAWSCanonicalDuration(cfg.TTL),
			IdleNanoseconds: fixedAWSCanonicalDuration(cfg.IdleTimeout),
		},
		Workload: fixedAWSCreateIntentWorkload{
			Pond:         normalizePondName(cfg.Pond),
			ExposedPorts: exposedPorts,
			WorkRoot:     strings.TrimSpace(cfg.WorkRoot),
		},
		Cache: fixedAWSCreateIntentCache{
			Pnpm:           cfg.Cache.Pnpm,
			Npm:            cfg.Cache.Npm,
			Docker:         cfg.Cache.Docker,
			Git:            cfg.Cache.Git,
			MaxGB:          cfg.Cache.MaxGB,
			PurgeOnRelease: cfg.Cache.PurgeOnRelease,
			Volumes:        cacheVolumes,
		},
	}, nil
}

func fixedAWSCreateIntentCacheVolumes(volumes []CacheVolumeConfig) ([]fixedAWSCreateIntentCacheVolume, error) {
	canonical := make([]fixedAWSCreateIntentCacheVolume, 0, len(volumes))
	for _, raw := range volumes {
		volume := CacheVolumeConfig{
			Name:     strings.TrimSpace(raw.Name),
			Key:      strings.TrimSpace(raw.Key),
			Path:     strings.TrimSpace(raw.Path),
			SizeGB:   raw.SizeGB,
			Required: raw.Required,
		}
		if volume.Name == "" {
			volume.Name = volume.Key
		}
		if err := validateCacheVolume(volume); err != nil {
			return nil, err
		}
		canonical = append(canonical, fixedAWSCreateIntentCacheVolume(volume))
	}
	sort.Slice(canonical, func(i, j int) bool {
		left, right := canonical[i], canonical[j]
		if left.Name != right.Name {
			return left.Name < right.Name
		}
		if left.Key != right.Key {
			return left.Key < right.Key
		}
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		if left.SizeGB != right.SizeGB {
			return left.SizeGB < right.SizeGB
		}
		return !left.Required && right.Required
	})
	if len(canonical) == 0 {
		return nil, nil
	}
	return canonical, nil
}

func fixedAWSCanonicalStringSet(values []string) []string {
	canonical := fixedAWSCanonicalOrderedStrings(values)
	sort.Strings(canonical)
	return canonical
}

func fixedAWSCanonicalOrderedStrings(values []string) []string {
	canonical := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		canonical = append(canonical, value)
	}
	if len(canonical) == 0 {
		return nil
	}
	return canonical
}

func fixedAWSCanonicalDuration(value time.Duration) int64 {
	return value.Nanoseconds()
}
