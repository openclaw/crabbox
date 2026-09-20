package shared

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

// SandboxRepositoryMetadataScope preserves the opaque repository marker used by
// sandbox adapters: local root first, then name, without path normalization.
func SandboxRepositoryMetadataScope(repo core.Repo) string {
	value := strings.TrimSpace(repo.Root)
	if value == "" {
		value = strings.TrimSpace(repo.Name)
	}
	sum := sha256.Sum256([]byte(value))
	return "repo-sha256:" + hex.EncodeToString(sum[:8])
}

// NormalizedSandboxClaimEndpoint preserves the existing E2B-compatible claim key.
func NormalizedSandboxClaimEndpoint(raw string) string {
	endpoint := strings.TrimSpace(core.ClaimScopeURL(raw))
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return strings.TrimRight(endpoint, "/")
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	return canonicalEndpointAddress(parsed)
}
