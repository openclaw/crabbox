package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

const FixedMachine0CreateIntentVersion = 1

type FixedMachine0CreateIntentRequest struct {
	RequestedSlug string
	Keep          bool
}

type fixedMachine0CreateIntent struct {
	Version       int                                `json:"version"`
	RequestedSlug string                             `json:"requestedSlug"`
	Provider      string                             `json:"provider"`
	Profile       string                             `json:"profile"`
	Machine       fixedMachine0CreateIntentMachine   `json:"machine"`
	Capabilities  fixedMachine0CreateIntentFeatures  `json:"capabilities"`
	Networking    fixedMachine0CreateIntentNetwork   `json:"networking"`
	Lifecycle     fixedMachine0CreateIntentLifecycle `json:"lifecycle"`
	Workload      fixedMachine0CreateIntentWorkload  `json:"workload"`
	Cache         fixedAWSCreateIntentCache          `json:"cache"`
}

type fixedMachine0CreateIntentMachine struct {
	TargetOS           string `json:"targetOS"`
	Architecture       string `json:"architecture"`
	OSImage            string `json:"osImage"`
	Class              string `json:"class"`
	ServerType         string `json:"serverType"`
	ServerTypeExplicit bool   `json:"serverTypeExplicit"`
	Size               string `json:"size"`
	Region             string `json:"region"`
	Image              string `json:"image"`
	ImageVersion       int    `json:"imageVersion"`
	DesktopImage       string `json:"desktopImage"`
	Key                string `json:"key"`
	WorkRoot           string `json:"workRoot"`
	ReleasePolicy      string `json:"releasePolicy"`
}

type fixedMachine0CreateIntentFeatures struct {
	Desktop           bool              `json:"desktop"`
	DesktopEnv        string            `json:"desktopEnv"`
	Browser           bool              `json:"browser"`
	Code              bool              `json:"code"`
	ImageRequirements imageRequirements `json:"imageRequirements"`
}

type fixedMachine0CreateIntentNetwork struct {
	Mode NetworkMode                  `json:"mode"`
	SSH  fixedMachine0CreateIntentSSH `json:"ssh"`
}

type fixedMachine0CreateIntentSSH struct {
	User          string   `json:"user"`
	Port          string   `json:"port"`
	FallbackPorts []string `json:"fallbackPorts,omitempty"`
}

type fixedMachine0CreateIntentLifecycle struct {
	Keep            bool  `json:"keep"`
	TTLNanoseconds  int64 `json:"ttlNanoseconds"`
	IdleNanoseconds int64 `json:"idleNanoseconds"`
}

type fixedMachine0CreateIntentWorkload struct {
	Pond         string   `json:"pond,omitempty"`
	ExposedPorts []string `json:"exposedPorts,omitempty"`
	WorkRoot     string   `json:"workRoot"`
}

func FixedMachine0CreateIntentFingerprint(cfg Config, req FixedMachine0CreateIntentRequest) (string, error) {
	intent, err := fixedMachine0CreateIntentForConfig(cfg, req)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(intent)
	if err != nil {
		return "", fmt.Errorf("encode fixed Machine0 create intent: %w", err)
	}
	digest := sha256.Sum256(append([]byte("crabbox-fixed-machine0-create-intent-v1\x00"), data...))
	return hex.EncodeToString(digest[:]), nil
}

func fixedMachine0CreateIntentForConfig(cfg Config, req FixedMachine0CreateIntentRequest) (fixedMachine0CreateIntent, error) {
	network, err := parseNetworkMode(string(cfg.Network))
	if err != nil {
		return fixedMachine0CreateIntent{}, err
	}
	osImage, err := normalizeOSImage(cfg.OSImage)
	if err != nil {
		return fixedMachine0CreateIntent{}, err
	}
	exposedPorts, err := requestedExposedPorts(cfg.ExposedPorts)
	if err != nil {
		return fixedMachine0CreateIntent{}, err
	}
	cacheVolumes, err := fixedAWSCreateIntentCacheVolumes(cfg.Cache.Volumes)
	if err != nil {
		return fixedMachine0CreateIntent{}, err
	}
	return fixedMachine0CreateIntent{
		Version:       FixedMachine0CreateIntentVersion,
		RequestedSlug: normalizeLeaseSlug(req.RequestedSlug),
		Provider:      strings.TrimSpace(cfg.Provider),
		Profile:       strings.TrimSpace(cfg.Profile),
		Machine: fixedMachine0CreateIntentMachine{
			TargetOS:           strings.TrimSpace(cfg.TargetOS),
			Architecture:       effectiveArchitectureForConfig(cfg),
			OSImage:            osImage,
			Class:              strings.TrimSpace(cfg.Class),
			ServerType:         strings.TrimSpace(cfg.ServerType),
			ServerTypeExplicit: cfg.ServerTypeExplicit,
			Size:               strings.TrimSpace(cfg.Machine0.Size),
			Region:             strings.TrimSpace(cfg.Machine0.Region),
			Image:              strings.TrimSpace(cfg.Machine0.Image),
			ImageVersion:       cfg.Machine0.ImageVersion,
			DesktopImage:       strings.TrimSpace(cfg.Machine0.DesktopImage),
			Key:                strings.TrimSpace(cfg.Machine0.Key),
			WorkRoot:           strings.TrimSpace(cfg.Machine0.WorkRoot),
			ReleasePolicy:      fixedMachine0ReleasePolicy(cfg.Machine0.ReleasePolicy),
		},
		Capabilities: fixedMachine0CreateIntentFeatures{
			Desktop:           cfg.Desktop,
			DesktopEnv:        normalizedDesktopEnv(cfg.DesktopEnv),
			Browser:           cfg.Browser,
			Code:              cfg.Code,
			ImageRequirements: cfg.imageRequirements,
		},
		Networking: fixedMachine0CreateIntentNetwork{
			Mode: network,
			SSH: fixedMachine0CreateIntentSSH{
				User:          strings.TrimSpace(cfg.SSHUser),
				Port:          strings.TrimSpace(cfg.SSHPort),
				FallbackPorts: fixedAWSCanonicalOrderedStrings(cfg.SSHFallbackPorts),
			},
		},
		Lifecycle: fixedMachine0CreateIntentLifecycle{
			Keep:            req.Keep,
			TTLNanoseconds:  fixedAWSCanonicalDuration(cfg.TTL),
			IdleNanoseconds: fixedAWSCanonicalDuration(cfg.IdleTimeout),
		},
		Workload: fixedMachine0CreateIntentWorkload{
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

func fixedMachine0ReleasePolicy(value string) string {
	if strings.EqualFold(strings.TrimSpace(value), "suspend") {
		return "suspend"
	}
	return "destroy"
}
