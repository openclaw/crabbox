package cli

import (
	"runtime/debug"
	"strings"
)

var version = "0.13.0-dev"

func currentVersion() string {
	buildInfoVersion := ""
	if info, ok := debug.ReadBuildInfo(); ok {
		buildInfoVersion = info.Main.Version
	}
	return resolveVersion(version, buildInfoVersion)
}

func resolveVersion(injected, buildInfoVersion string) string {
	if normalized := normalizeBuildVersion(injected); normalized != "" && !strings.HasSuffix(normalized, "-dev") {
		return normalized
	}
	if normalized := normalizeTaggedBuildInfoVersion(buildInfoVersion); normalized != "" {
		return normalized
	}
	if normalized := normalizeBuildVersion(injected); normalized != "" {
		return normalized
	}
	return "dev"
}

func normalizeBuildVersion(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || value == "(devel)" {
		return ""
	}
	return strings.TrimPrefix(value, "v")
}

func normalizeTaggedBuildInfoVersion(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || value == "(devel)" || !strings.HasPrefix(value, "v") || strings.Contains(value, "+") {
		return ""
	}
	value = strings.TrimPrefix(value, "v")
	if isPseudoVersion(value) {
		return ""
	}
	return value
}

func isPseudoVersion(value string) bool {
	parts := strings.Split(value, "-")
	if len(parts) < 3 {
		return false
	}
	candidate := parts[len(parts)-2]
	if dot := strings.LastIndex(candidate, "."); dot >= 0 {
		candidate = candidate[dot+1:]
	}
	if len(candidate) != 14 {
		return false
	}
	for _, ch := range candidate {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}
