package tencentcloud

import (
	"sort"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	tencentTagValueLimit = 255
	accountLabel         = "provider_account"
)

type tag struct {
	Key   string `json:"Key"`
	Value string `json:"Value"`
}

type tagUpdate struct {
	TagKey   string `json:"TagKey"`
	TagValue string `json:"TagValue"`
}

type tagDelete struct {
	TagKey string `json:"TagKey"`
}

func tagUpdateSet(tags []tag) []tagUpdate {
	out := make([]tagUpdate, 0, len(tags))
	for _, item := range tags {
		if item.Key == "" {
			continue
		}
		out = append(out, tagUpdate{TagKey: item.Key, TagValue: item.Value})
	}
	return out
}

func tagDeleteSet(currentTags, desiredTags []tag) []tagDelete {
	desired := make(map[string]bool, len(desiredTags))
	for _, item := range desiredTags {
		key := normalizeTagKey(item.Key)
		if key != "" {
			desired[key] = true
		}
	}
	seen := map[string]bool{}
	out := make([]tagDelete, 0)
	for _, item := range currentTags {
		key := normalizeTagKey(item.Key)
		if key == "" || seen[key] || desired[key] || !crabboxManagedTagKey(key) {
			continue
		}
		seen[key] = true
		out = append(out, tagDelete{TagKey: key})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TagKey < out[j].TagKey })
	return out
}

func crabboxManagedTagKey(key string) bool {
	if strings.HasPrefix(key, "tailscale_") {
		return true
	}
	switch key {
	case "browser",
		"class",
		"code",
		"crabbox",
		"crabbox_exposed_ports",
		"created_at",
		"created_by",
		"desktop",
		"desktop_env",
		"expires_at",
		"idle_timeout",
		"idle_timeout_secs",
		"keep",
		"last_touched_at",
		"lease",
		"market",
		"pond",
		"profile",
		"provider",
		"provider_account",
		"provider_key",
		"server_type",
		"slug",
		"state",
		"tailscale",
		"target",
		"ttl_secs",
		"windows_mode":
		return true
	default:
		return false
	}
}

func leaseTags(cfg core.Config, leaseID, slug, state string, keep bool, now time.Time) []tag {
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", keep, now)
	labels["state"] = state
	if cfg.Tailscale.Enabled && len(cfg.Tailscale.Tags) > 0 {
		labels["tailscale_tags"] = strings.Join(cfg.Tailscale.Tags, ",")
	}
	return tagsFromLabels(labels)
}

func tagsFromLabels(labels map[string]string) []tag {
	keys := make([]string, 0, len(labels))
	for key, value := range labels {
		key = normalizeTagKey(key)
		if key == "" || strings.TrimSpace(value) == "" {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]tag, 0, len(keys))
	seen := map[string]bool{}
	for _, key := range keys {
		if seen[key] {
			continue
		}
		seen[key] = true
		value := labels[key]
		if value == "" {
			value = labels[strings.ToLower(key)]
		}
		out = append(out, tag{Key: key, Value: truncateTagValue(value)})
	}
	return out
}

func labelsFromTags(tags []tag) map[string]string {
	labels := map[string]string{}
	for _, item := range tags {
		key := normalizeTagKey(item.Key)
		if key == "" {
			continue
		}
		labels[key] = strings.TrimSpace(item.Value)
	}
	return labels
}

func normalizeTagKey(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return ""
	}
	var out strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			out.WriteRune(r)
		} else {
			out.WriteByte('_')
		}
	}
	value = strings.Trim(out.String(), "_-")
	if value == "" {
		return ""
	}
	if len(value) > 127 {
		value = value[:127]
	}
	return value
}

func truncateTagValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= tencentTagValueLimit {
		return value
	}
	return value[:tencentTagValueLimit]
}

func ownedLabels(labels map[string]string) bool {
	return labels["crabbox"] == "true" && labels["provider"] == providerName
}

func applyTailscaleMetadata(labels map[string]string, meta core.TailscaleMetadata) {
	if meta.Enabled {
		labels["tailscale"] = "true"
	}
	if meta.Hostname != "" {
		labels["tailscale_hostname"] = meta.Hostname
	}
	if meta.FQDN != "" {
		labels["tailscale_fqdn"] = meta.FQDN
	}
	if meta.IPv4 != "" {
		labels["tailscale_ipv4"] = meta.IPv4
	}
	if len(meta.Tags) > 0 {
		labels["tailscale_tags"] = strings.Join(meta.Tags, ",")
	}
	if meta.State != "" {
		labels["tailscale_state"] = meta.State
	}
	if meta.Error != "" {
		labels["tailscale_error"] = meta.Error
	} else {
		delete(labels, "tailscale_error")
	}
	if meta.ExitNode != "" {
		labels["tailscale_exit_node"] = meta.ExitNode
	}
	if meta.ExitNodeAllowLANAccess {
		labels["tailscale_exit_node_allow_lan_access"] = "true"
	}
}
