package cli

import (
	"fmt"
	"sort"
	"strings"
)

type imageCapabilities struct {
	OSVersion string            `json:"osVersion,omitempty"`
	SDKs      map[string]string `json:"sdks,omitempty"`
	Runtimes  map[string]string `json:"runtimes,omitempty"`
	Browser   bool              `json:"browser,omitempty"`
	WebView2  bool              `json:"webview2,omitempty"`
	Desktop   bool              `json:"desktop,omitempty"`
}

type imageRequirements struct {
	MinOS    string            `json:"minOS,omitempty"`
	SDKs     map[string]string `json:"sdks,omitempty"`
	Runtimes map[string]string `json:"runtimes,omitempty"`
	Browser  bool              `json:"browser,omitempty"`
	WebView2 bool              `json:"webview2,omitempty"`
	Desktop  bool              `json:"desktop,omitempty"`
}

func parseImageVersions(values []string, flagName string) (map[string]string, error) {
	if len(values) > 32 {
		return nil, exit(2, "--%s supports at most 32 entries", flagName)
	}
	parsed := map[string]string{}
	for _, value := range values {
		name, version, ok := strings.Cut(value, "=")
		name = strings.ToLower(strings.TrimSpace(name))
		version = strings.TrimSpace(version)
		if !ok || !validImageCapabilityName(name) || !validImageVersion(version) {
			return nil, exit(2, "--%s must use name=dot.separated.numeric.version", flagName)
		}
		parsed[name] = version
	}
	if len(parsed) == 0 {
		return nil, nil
	}
	return parsed, nil
}

func validImageCapabilityName(value string) bool {
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	for index, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') {
			continue
		}
		if index > 0 && (char == '.' || char == '_' || char == '-') {
			continue
		}
		return false
	}
	return true
}

func validImageVersion(value string) bool {
	if value == "" || len(value) > 128 || strings.HasPrefix(value, ".") || strings.HasSuffix(value, ".") {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && char != '.' {
			return false
		}
	}
	return !strings.Contains(value, "..")
}

func validateImageVersion(value, flagName string) error {
	if value != "" && !validImageVersion(value) {
		return exit(2, "--%s must be a dot-separated numeric version", flagName)
	}
	return nil
}

func imageRequirementsEmpty(value imageRequirements) bool {
	return value.MinOS == "" && len(value.SDKs) == 0 && len(value.Runtimes) == 0 &&
		!value.Browser && !value.WebView2 && !value.Desktop
}

func validateReadyPoolImageRequirements(value imageRequirements, pool string) error {
	if strings.TrimSpace(pool) != "" && !imageRequirementsEmpty(value) {
		return exit(2, "--pool cannot verify image capability requirements; omit --pool or the --image-* flags")
	}
	return nil
}

func addSortedImageVersions(add func(string), values map[string]string) {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		add(fmt.Sprintf("%s=%s", name, values[name]))
	}
}
