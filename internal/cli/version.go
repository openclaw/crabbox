package cli

import (
	"runtime/debug"
	"strings"
)

var version = "dev"

func currentVersion() string {
	buildInfoVersion := ""
	if info, ok := debug.ReadBuildInfo(); ok {
		buildInfoVersion = info.Main.Version
	}
	return resolveVersion(version, buildInfoVersion)
}

func resolveVersion(injected, buildInfoVersion string) string {
	if normalized := normalizeBuildVersion(injected); normalized != "" && normalized != "dev" && !strings.HasSuffix(normalized, "-dev") {
		return normalized
	}
	if normalized := normalizeModuleBuildInfoVersion(buildInfoVersion); normalized != "" {
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

func normalizeModuleBuildInfoVersion(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || value == "(devel)" || !strings.HasPrefix(value, "v") || strings.Contains(value, "+") {
		return ""
	}
	return strings.TrimPrefix(value, "v")
}
