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

	tagCrabbox                = "crabbox"
	tagPrefix                 = "crabbox:"
	ownershipTagConflictLabel = "_digitalocean_ownership_tag_conflict"
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

func legacyEncodedExactTagValueKey(key string) bool {
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
	ownershipConflicts := map[string]bool{}
	versionedExact := map[string]string{}
	versionedExactConflict := map[string]bool{}
	legacyExact := map[string]string{}
	legacyExactConflict := map[string]bool{}
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
			if logical, ok := versionedExactTagValueKey(key); ok {
				recordExactTagValue(versionedExact, versionedExactConflict, logical, decodeExactTagValue(parts[1]))
				continue
			}
			if exactTagValueKey(key) {
				value := parts[1]
				if legacyEncodedExactTagValueKey(key) {
					value = decodeExactTagValue(value)
				}
				recordExactTagValue(legacyExact, legacyExactConflict, key, value)
				continue
			}
			switch key {
			case "provider", "lease", "slug", "target":
				value := parts[1]
				if key == "provider" || key == "target" {
					value = strings.ToLower(value)
				}
				recordOwnershipTagValue(labels, ownershipConflicts, key, value)
			case "keep", "class", "server_type", "provider_key", "ttl_secs", "idle_timeout", "idle_timeout_secs", "created_at", "updated_at", "profile", "market", "desktop", "desktop_env", "browser", "code", "pond", "crabbox_exposed_ports", "tailscale", "tailscale_state", "tailscale_exit_node_allow_lan_access":
				value := parts[1]
				switch key {
				case "keep", "tailscale", "tailscale_state", "tailscale_exit_node_allow_lan_access":
					value = strings.ToLower(value)
				}
				labels[key] = value
			case "state":
				value := strings.ToLower(parts[1])
				if statePriority(value) >= statePriority(labels["state"]) {
					labels["state"] = value
				}
			case "expires_at", "last_touched_at":
				if numericTagValue(parts[1]) >= numericTagValue(labels[key]) {
					labels[key] = parts[1]
				}
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
	for _, key := range tagLabelKeys() {
		if !exactTagValueKey(key) {
			continue
		}
		if value, ok := versionedExact[key]; ok || versionedExactConflict[key] {
			if ok && !versionedExactConflict[key] {
				labels[key] = value
			}
			continue
		}
		if value, ok := legacyExact[key]; ok && !legacyExactConflict[key] {
			labels[key] = value
		}
	}
	return labels
}

func recordOwnershipTagValue(labels map[string]string, conflicts map[string]bool, key, value string) {
	if conflicts[key] {
		return
	}
	if existing, ok := labels[key]; ok && existing != value {
		delete(labels, key)
		conflicts[key] = true
		return
	}
	labels[key] = value
}

func recordExactTagValue(values map[string]string, conflicts map[string]bool, key, value string) {
	if conflicts[key] {
		return
	}
	if existing, ok := values[key]; ok && existing != value {
		delete(values, key)
		conflicts[key] = true
		return
	}
	values[key] = value
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
		labels[ownershipTagConflictLabel] != "" ||
		labels["crabbox"] != "true" ||
		labels["created_by"] != "crabbox" ||
		labels["provider"] != providerName ||
		!core.IsCanonicalLeaseID(labels["lease"]) ||
		labels["slug"] == "" ||
		labels["target"] != core.TargetLinux {
		return core.Exit(2, "refusing to operate on non-Crabbox DigitalOcean Droplet")
	}
	return nil
}
