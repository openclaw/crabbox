package neevcloud

import (
	"regexp"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	maxLabels     = 16
	maxNameLength = 63
)

// labelKeys are the Crabbox labels read back from a sandbox, in keep-priority order.
var labelKeys = []string{
	"crabbox", "provider", "lease", "slug", "state", "keep",
	"created_at", "last_touched_at", "expires_at", "ttl_secs", "idle_timeout_secs",
	"server_type", "target", "class", "profile", "pond",
}

var (
	labelKeyPattern   = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,61}[a-z0-9])?$`)
	labelValuePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{0,63}$`)
)

// sandboxLabels keeps the allow-listed, non-empty, server-valid subset of Crabbox labels.
// Values the API would reject are dropped rather than sent; the result never exceeds 16.
func sandboxLabels(all map[string]string) map[string]string {
	out := make(map[string]string, maxLabels)
	for _, key := range labelKeys {
		value := all[key]
		if value == "" || !labelKeyPattern.MatchString(key) || !labelValuePattern.MatchString(value) {
			continue
		}
		out[key] = value
		if len(out) == maxLabels {
			break
		}
	}
	return out
}

// sandboxName derives a stable DNS-1123 label from the slug and lease so a retried create collides.
func sandboxName(slug, leaseID string) string {
	suffix := strings.TrimPrefix(leaseID, "cbx_")
	if len(suffix) > 12 {
		suffix = suffix[:12]
	}
	slug = core.NormalizeLeaseSlug(slug)
	budget := maxNameLength - len("crabbox-") - len("-") - len(suffix)
	if len(slug) > budget {
		slug = strings.TrimRight(slug[:budget], "-")
	}
	if slug == "" {
		return "crabbox-" + suffix
	}
	return "crabbox-" + slug + "-" + suffix
}
