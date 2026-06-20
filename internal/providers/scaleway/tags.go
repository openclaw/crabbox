package scaleway

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	tagCrabbox                = "crabbox"
	tagPrefix                 = "crabbox:"
	ownershipTagConflictLabel = "_scaleway_ownership_tag_conflict"
)

var tagSafeRe = regexp.MustCompile(`[^A-Za-z0-9_:\-]`)

func leaseTags(cfg core.Config, leaseID, slug, state string, keep bool, now time.Time) []string {
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", keep, now)
	labels["state"] = state
	if cfg.Tailscale.Enabled && len(cfg.Tailscale.Tags) > 0 {
		labels["tailscale_tags"] = strings.Join(cfg.Tailscale.Tags, ",")
	}
	return tagsFromLabels(labels)
}

func tagsFromLabels(labels map[string]string) []string {
	tags := []string{
		tagCrabbox,
		"crabbox:provider:" + providerName,
		"crabbox:target:" + core.TargetLinux,
	}
	for _, key := range tagLabelKeys() {
		if value := labels[key]; value != "" {
			tags = append(tags, encodeTagKV(key, value))
		}
	}
	return normalizeTags(tags)
}

func tagLabelKeys() []string {
	return []string{
		"lease", "slug", "state", "keep", "target", "class", "server_type", "provider_key",
		"ttl_secs", "idle_timeout", "idle_timeout_secs", "expires_at", "created_at", "last_touched_at", "updated_at",
		"profile", "market", "desktop", "desktop_env", "browser", "code", "pond", "crabbox_exposed_ports",
		"tailscale", "tailscale_state", "tailscale_hostname", "tailscale_tags", "tailscale_ipv4", "tailscale_fqdn", "tailscale_error",
		"tailscale_exit_node", "tailscale_exit_node_allow_lan_access",
		"recovery", "scaleway_project", "scaleway_organization", "scaleway_region", "scaleway_zone", "scaleway_ssh_key_id", "scaleway_ssh_key_name",
	}
}

func encodeTagKV(key, value string) string {
	key = sanitizeTagPart(key)
	if exactTagValueKey(key) {
		key += "_v1"
		return tagPrefix + key + ":" + encodeExactTagValue(value, 255-len(tagPrefix)-len(key)-1)
	}
	return tagPrefix + key + ":" + sanitizeTagPart(value)
}

func sanitizeTagPart(value string) string {
	value = strings.TrimSpace(value)
	value = tagSafeRe.ReplaceAllString(value, "-")
	value = strings.Trim(value, "-")
	if value == "" {
		return "unknown"
	}
	if len(value) > 64 {
		return value[:64]
	}
	return value
}

func exactTagValueKey(key string) bool {
	switch key {
	case "tailscale_hostname", "tailscale_tags", "tailscale_ipv4", "tailscale_fqdn", "tailscale_error", "tailscale_exit_node":
		return true
	default:
		return false
	}
}

func versionedExactTagValueKey(key string) (string, bool) {
	logical := strings.TrimSuffix(key, "_v1")
	return logical, logical != key && exactTagValueKey(logical)
}

func encodeExactTagValue(value string, maxLen int) string {
	value = strings.TrimSpace(value)
	const hex = "0123456789abcdef"
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == ':' {
			if out.Len()+1 > maxLen {
				break
			}
			out.WriteByte(ch)
			continue
		}
		if out.Len()+3 > maxLen {
			break
		}
		out.WriteByte('_')
		out.WriteByte(hex[ch>>4])
		out.WriteByte(hex[ch&0x0f])
	}
	if out.Len() == 0 {
		return "unknown"
	}
	return out.String()
}

func decodeExactTagValue(value string) string {
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] == '_' && i+2 < len(value) {
			decoded, err := strconv.ParseUint(value[i+1:i+3], 16, 8)
			if err == nil {
				out.WriteByte(byte(decoded))
				i += 2
				continue
			}
		}
		out.WriteByte(value[i])
	}
	return out.String()
}

func normalizeTags(tags []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" || seen[tag] {
			continue
		}
		seen[tag] = true
		out = append(out, tag)
	}
	sort.Strings(out)
	return out
}

func labelsFromTags(tags []string) map[string]string {
	labels := map[string]string{}
	ownershipConflicts := map[string]bool{}
	for _, tag := range tags {
		lowerTag := strings.ToLower(tag)
		switch {
		case lowerTag == tagCrabbox:
			labels["crabbox"] = "true"
			labels["created_by"] = "crabbox"
		case strings.HasPrefix(lowerTag, tagPrefix):
			parts := strings.SplitN(tag[len(tagPrefix):], ":", 2)
			if len(parts) != 2 {
				continue
			}
			key := strings.ToLower(parts[0])
			value := parts[1]
			if logical, ok := versionedExactTagValueKey(key); ok {
				labels[logical] = decodeExactTagValue(value)
				continue
			}
			switch key {
			case "provider", "lease", "slug", "target":
				if prior := labels[key]; prior != "" && prior != value {
					ownershipConflicts[key] = true
				}
				if key == "provider" || key == "target" {
					value = strings.ToLower(value)
				}
				labels[key] = value
			case "state":
				labels[key] = strings.ToLower(value)
			default:
				labels[key] = value
			}
		}
	}
	if len(ownershipConflicts) > 0 {
		keys := make([]string, 0, len(ownershipConflicts))
		for key := range ownershipConflicts {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		labels[ownershipTagConflictLabel] = strings.Join(keys, ",")
	}
	return labels
}
