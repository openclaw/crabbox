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
