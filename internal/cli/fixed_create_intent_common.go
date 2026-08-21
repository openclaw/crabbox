package cli

import (
	"sort"
	"strings"
	"time"
)

type fixedCreateIntentFeatures struct {
	Desktop           bool              `json:"desktop" configFields:"Desktop"`
	DesktopEnv        string            `json:"desktopEnv" configFields:"DesktopEnv"`
	Browser           bool              `json:"browser" configFields:"Browser"`
	Code              bool              `json:"code" configFields:"Code"`
	ImageRequirements imageRequirements `json:"imageRequirements"`
}

type fixedCreateIntentLifecycle struct {
	Keep            bool  `json:"keep"`
	TTLNanoseconds  int64 `json:"ttlNanoseconds" configFields:"TTL"`
	IdleNanoseconds int64 `json:"idleNanoseconds" configFields:"IdleTimeout"`
}

type fixedCreateIntentWorkload struct {
	Pond         string   `json:"pond,omitempty" configFields:"Pond"`
	ExposedPorts []string `json:"exposedPorts,omitempty" configFields:"ExposedPorts"`
	WorkRoot     string   `json:"workRoot" configFields:"WorkRoot"`
}

type fixedCreateIntentCache struct {
	Pnpm           bool                           `json:"pnpm"`
	Npm            bool                           `json:"npm"`
	Docker         bool                           `json:"docker"`
	Git            bool                           `json:"git"`
	MaxGB          int                            `json:"maxGB"`
	PurgeOnRelease bool                           `json:"purgeOnRelease"`
	Volumes        []fixedCreateIntentCacheVolume `json:"volumes,omitempty"`
	RepoScope      string                         `json:"repoScope,omitempty" configFields:"AWSCacheVolumeRepoScope"`
}

type fixedCreateIntentCacheVolume struct {
	Name     string `json:"name"`
	Key      string `json:"key"`
	Path     string `json:"path"`
	SizeGB   int    `json:"sizeGB"`
	Required bool   `json:"required"`
}

func fixedCreateIntentCacheVolumes(volumes []CacheVolumeConfig) ([]fixedCreateIntentCacheVolume, error) {
	canonical := make([]fixedCreateIntentCacheVolume, 0, len(volumes))
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
		canonical = append(canonical, fixedCreateIntentCacheVolume(volume))
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

func fixedCanonicalStringSet(values []string) []string {
	canonical := fixedCanonicalOrderedStrings(values)
	sort.Strings(canonical)
	return canonical
}

func fixedCanonicalOrderedStrings(values []string) []string {
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

func fixedCanonicalDuration(value time.Duration) int64 {
	return value.Nanoseconds()
}
