package vultr

import (
	"regexp"
	"sort"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	tagCrabbox                = "crabbox"
	tagPrefix                 = "crabbox:"
	ownershipTagConflictLabel = "_vultr_ownership_tag_conflict"
)

var tagSafeRe = regexp.MustCompile(`[^A-Za-z0-9_:\-]`)

func leaseTags(cfg core.Config, leaseID, slug, state string, keep bool, now time.Time) []string {
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", keep, now)
	labels["state"] = state
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
		"provider_key_id", "provider_key_owned",
		"ttl_secs", "idle_timeout", "idle_timeout_secs", "expires_at", "created_at", "last_touched_at", "updated_at",
		"profile", "market", "desktop", "desktop_env", "browser", "code", "pond", "crabbox_exposed_ports",
	}
}

func encodeTagKV(key, value string) string {
	return tagPrefix + sanitizeTagPart(key) + ":" + sanitizeTagPart(value)
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
			switch key {
			case "provider", "lease", "slug", "target":
				value := parts[1]
				if key == "provider" || key == "target" {
					value = strings.ToLower(value)
				}
				recordOwnershipTagValue(labels, ownershipConflicts, key, value)
			case "keep", "class", "server_type", "provider_key", "provider_key_id", "provider_key_owned", "ttl_secs", "idle_timeout", "idle_timeout_secs", "created_at", "updated_at", "profile", "market", "desktop", "desktop_env", "browser", "code", "pond", "crabbox_exposed_ports":
				value := parts[1]
				if key == "keep" || key == "provider_key_owned" {
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

func isOwnedInstance(inst vultrInstance) bool {
	return validateInstanceLabels(labelsFromTags(inst.Tags)) == nil
}

func validateInstanceLabels(labels map[string]string) error {
	if labels == nil ||
		labels[ownershipTagConflictLabel] != "" ||
		labels["crabbox"] != "true" ||
		labels["created_by"] != "crabbox" ||
		labels["provider"] != providerName ||
		!core.IsCanonicalLeaseID(labels["lease"]) ||
		labels["slug"] == "" ||
		labels["target"] != core.TargetLinux {
		return core.Exit(2, "refusing to operate on non-Crabbox Vultr instance")
	}
	return nil
}
