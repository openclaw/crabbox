package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

const FixedAWSCreateIntentVersion = 2

type FixedAWSCreateIntentRequest struct {
	AccountID     string
	RequestedSlug string
	SSHPublicKey  string
	Keep          bool
}

type fixedAWSCreateIntent struct {
	Version       int                          `json:"version"`
	AccountID     string                       `json:"accountID"`
	RequestedSlug string                       `json:"requestedSlug"`
	Provider      string                       `json:"provider" configFields:"Provider"`
	Profile       string                       `json:"profile" configFields:"Profile"`
	Machine       fixedAWSCreateIntentMachine  `json:"machine"`
	Capabilities  fixedCreateIntentFeatures    `json:"capabilities"`
	Networking    fixedAWSCreateIntentNetwork  `json:"networking"`
	Capacity      fixedAWSCreateIntentCapacity `json:"capacity" configFields:"Capacity"`
	Lifecycle     fixedCreateIntentLifecycle   `json:"lifecycle"`
	Workload      fixedCreateIntentWorkload    `json:"workload"`
	Cache         fixedCreateIntentCache       `json:"cache" configFields:"Cache"`
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

type fixedAWSCreateIntentNetwork struct {
	Mode            NetworkMode                   `json:"mode" configFields:"Network"`
	SecurityGroupID string                        `json:"securityGroupID,omitempty" configFields:"AWSSGID"`
	SSHCIDRsPinned  bool                          `json:"sshCIDRsPinned" configFields:"AWSSSHCIDRsPinned"`
	SSHCIDRs        []string                      `json:"sshCIDRs,omitempty" configFields:"AWSSSHCIDRs"`
	SSH             fixedAWSCreateIntentSSH       `json:"ssh"`
	Tailscale       fixedAWSCreateIntentTailscale `json:"tailscale" configFields:"Tailscale"`
}

// fixedAWSCreateIntentSSH stays separate because its identity fields must not enter Machine0's durable JSON.
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
	cacheVolumes, err := fixedCreateIntentCacheVolumes(cfg.Cache.Volumes)
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
		sshCIDRs = fixedCanonicalStringSet(cfg.AWSSSHCIDRs)
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
		Capabilities: fixedCreateIntentFeatures{
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
				FallbackPorts:   fixedCanonicalOrderedStrings(cfg.SSHFallbackPorts),
				ProviderKey:     strings.TrimSpace(cfg.ProviderKey),
				PublicKeySHA256: hex.EncodeToString(publicKeyHash[:]),
			},
			Tailscale: fixedAWSCreateIntentTailscale{
				Enabled:                cfg.Tailscale.Enabled,
				Tags:                   fixedCanonicalStringSet(cfg.Tailscale.Tags),
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
			Regions:           fixedCanonicalOrderedStrings(cfg.Capacity.Regions),
			AvailabilityZones: fixedCanonicalOrderedStrings(cfg.Capacity.AvailabilityZones),
			Hints:             cfg.Capacity.Hints,
		},
		Lifecycle: fixedCreateIntentLifecycle{
			Keep:            req.Keep,
			TTLNanoseconds:  fixedCanonicalDuration(cfg.TTL),
			IdleNanoseconds: fixedCanonicalDuration(cfg.IdleTimeout),
		},
		Workload: fixedCreateIntentWorkload{
			Pond:         normalizePondName(cfg.Pond),
			ExposedPorts: exposedPorts,
			WorkRoot:     strings.TrimSpace(cfg.WorkRoot),
		},
		Cache: fixedCreateIntentCache{
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
