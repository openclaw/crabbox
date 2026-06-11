package digitalocean

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName = "digitalocean"

	tagCrabbox = "crabbox"
	tagPrefix  = "crabbox:"
)

var tagSafeRe = regexp.MustCompile(`[^A-Za-z0-9_:\-]`)

func leaseTags(cfg core.Config, leaseID, slug, state string, keep bool, now time.Time) []string {
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", keep, now)
	labels["state"] = state
	if cfg.Tailscale.Enabled && len(cfg.Tailscale.Tags) > 0 {
		labels["tailscale_tags"] = strings.Join(cfg.Tailscale.Tags, ",")
	}
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
	}
}

func encodeTagKV(key, value string) string {
	key = sanitizeTagPart(key)
	if exactTagValueKey(key) {
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
	case "tailscale_ipv4", "tailscale_fqdn", "tailscale_error":
		return true
	default:
		return false
	}
}

func encodeExactTagValue(value string, maxLen int) string {
	value = strings.TrimSpace(value)
	const hex = "0123456789abcdef"
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if (ch >= 'a' && ch <= 'z') ||
			(ch >= 'A' && ch <= 'Z') ||
			(ch >= '0' && ch <= '9') ||
			ch == '-' || ch == ':' {
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
	for _, tag := range tags {
		switch {
		case tag == tagCrabbox:
			labels["crabbox"] = "true"
			labels["created_by"] = "crabbox"
		case tag == "crabbox:provider:"+providerName:
			labels["provider"] = providerName
		case strings.HasPrefix(tag, tagPrefix):
			parts := strings.SplitN(strings.TrimPrefix(tag, tagPrefix), ":", 2)
			if len(parts) != 2 {
				continue
			}
			switch parts[0] {
			case "lease", "slug", "keep", "target", "class", "server_type", "provider_key", "ttl_secs", "idle_timeout", "idle_timeout_secs", "created_at", "updated_at", "profile", "market", "desktop", "desktop_env", "browser", "code", "pond", "crabbox_exposed_ports", "tailscale", "tailscale_state", "tailscale_hostname", "tailscale_tags", "tailscale_ipv4", "tailscale_fqdn", "tailscale_error", "tailscale_exit_node", "tailscale_exit_node_allow_lan_access":
				value := parts[1]
				if exactTagValueKey(parts[0]) {
					value = decodeExactTagValue(value)
				}
				labels[parts[0]] = value
			case "state":
				if statePriority(parts[1]) >= statePriority(labels["state"]) {
					labels["state"] = parts[1]
				}
			case "expires_at", "last_touched_at":
				if numericTagValue(parts[1]) >= numericTagValue(labels[parts[0]]) {
					labels[parts[0]] = parts[1]
				}
			}
		}
	}
	return labels
}

func statePriority(state string) int64 {
	switch state {
	case "running":
		return 50
	case "ready", "active":
		return 40
	case "leased":
		return 30
	case "provisioning":
		return 20
	case "":
		return 0
	default:
		return 10
	}
}

func numericTagValue(value string) int64 {
	var n int64
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			return 0
		}
		n = n*10 + int64(ch-'0')
	}
	return n
}

func isOwnedDroplet(d droplet) bool {
	return validateDropletLabels(labelsFromTags(d.Tags)) == nil
}

func validateDropletLabels(labels map[string]string) error {
	if labels == nil ||
		labels["crabbox"] != "true" ||
		labels["created_by"] != "crabbox" ||
		labels["provider"] != providerName ||
		labels["lease"] == "" ||
		labels["slug"] == "" ||
		labels["target"] != core.TargetLinux {
		return core.Exit(2, "refusing to operate on non-Crabbox DigitalOcean Droplet")
	}
	return nil
}
