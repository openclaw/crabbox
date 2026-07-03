package linode

import (
	"fmt"
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
	ownershipTagConflictLabel = "_linode_ownership_tag_conflict"
	maxLinodeTagLength        = 50
	maxEncodedTagValueLength  = 255
	tagChunkSuffix            = "_v2"
	tagChunkHeaderLength      = 5
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
			tags = append(tags, encodeTagKV(key, value)...)
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

func encodeTagKV(key, value string) []string {
	key = sanitizeTagPart(key)
	plain := tagPrefix + key + ":" + sanitizeTagPart(value)
	if !exactTagValueKey(key) && len(plain) <= maxLinodeTagLength {
		return []string{plain}
	}
	return encodeChunkedTagKV(key, value)
}

func sanitizeTagPart(value string) string {
	value = strings.TrimSpace(value)
	value = tagSafeRe.ReplaceAllString(value, "-")
	value = strings.Trim(value, "-")
	if value == "" {
		return "unknown"
	}
	return value
}

func encodeChunkedTagKV(key, value string) []string {
	base := tagPrefix + key + tagChunkSuffix + ":"
	chunkSize := maxLinodeTagLength - len(base) - tagChunkHeaderLength
	if chunkSize <= 0 {
		return nil
	}
	encoded := encodeExactTagValue(value, maxEncodedTagValueLength)
	chunkCount := (len(encoded) + chunkSize - 1) / chunkSize
	if chunkCount == 0 {
		chunkCount = 1
	}
	if chunkCount > 99 {
		chunkCount = 99
		encoded = encoded[:chunkSize*chunkCount]
	}
	tags := make([]string, 0, chunkCount)
	for index := 0; index < chunkCount; index++ {
		start := index * chunkSize
		end := min(start+chunkSize, len(encoded))
		tags = append(tags, base+fmt.Sprintf("%02d%02d:", index, chunkCount)+encoded[start:end])
	}
	return tags
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

func chunkedTagValueKey(key string) (string, bool) {
	logical := strings.TrimSuffix(key, tagChunkSuffix)
	if logical == key {
		return "", false
	}
	for _, candidate := range tagLabelKeys() {
		if logical == candidate {
			return logical, true
		}
	}
	return "", false
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

type tagChunkSet struct {
	total    int
	parts    map[int]string
	conflict bool
}

func labelsFromTags(tags []string) map[string]string {
	labels := map[string]string{}
	ownershipConflicts := map[string]bool{}
	chunkedExact := map[string]*tagChunkSet{}
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
			if logical, ok := chunkedTagValueKey(key); ok {
				recordTagChunk(chunkedExact, logical, parts[1])
				continue
			}
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
			applyDecodedTagLabel(labels, ownershipConflicts, key, parts[1])
		}
	}
	for key, chunks := range chunkedExact {
		if chunks.conflict || chunks.total == 0 || len(chunks.parts) != chunks.total {
			versionedExactConflict[key] = true
			continue
		}
		var encoded strings.Builder
		for index := 0; index < chunks.total; index++ {
			part, ok := chunks.parts[index]
			if !ok {
				versionedExactConflict[key] = true
				break
			}
			encoded.WriteString(part)
		}
		if !versionedExactConflict[key] {
			recordExactTagValue(versionedExact, versionedExactConflict, key, decodeExactTagValue(encoded.String()))
		}
	}
	for _, key := range tagLabelKeys() {
		if value, ok := versionedExact[key]; ok || versionedExactConflict[key] {
			delete(labels, key)
			if ok && !versionedExactConflict[key] {
				applyDecodedTagLabel(labels, ownershipConflicts, key, value)
			} else if isOwnershipTagKey(key) {
				ownershipConflicts[key] = true
			}
			continue
		}
		if value, ok := legacyExact[key]; ok || legacyExactConflict[key] {
			delete(labels, key)
			if ok && !legacyExactConflict[key] {
				applyDecodedTagLabel(labels, ownershipConflicts, key, value)
			} else if isOwnershipTagKey(key) {
				ownershipConflicts[key] = true
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

func recordTagChunk(chunks map[string]*tagChunkSet, key, value string) {
	set := chunks[key]
	if set == nil {
		set = &tagChunkSet{parts: map[int]string{}}
		chunks[key] = set
	}
	if len(value) < tagChunkHeaderLength || value[4] != ':' {
		set.conflict = true
		return
	}
	index, indexErr := strconv.Atoi(value[:2])
	total, totalErr := strconv.Atoi(value[2:4])
	if indexErr != nil || totalErr != nil || total < 1 || index >= total {
		set.conflict = true
		return
	}
	if set.total != 0 && set.total != total {
		set.conflict = true
		return
	}
	set.total = total
	part := value[tagChunkHeaderLength:]
	if existing, ok := set.parts[index]; ok && existing != part {
		set.conflict = true
		return
	}
	set.parts[index] = part
}

func applyDecodedTagLabel(labels map[string]string, ownershipConflicts map[string]bool, key, value string) {
	switch key {
	case "provider", "lease", "slug", "target":
		if key == "provider" || key == "target" {
			value = strings.ToLower(value)
		}
		recordOwnershipTagValue(labels, ownershipConflicts, key, value)
	case "keep", "class", "server_type", "provider_key", "ttl_secs", "idle_timeout", "idle_timeout_secs", "created_at", "updated_at", "profile", "market", "desktop", "desktop_env", "browser", "code", "pond", "crabbox_exposed_ports", "tailscale", "tailscale_state", "tailscale_hostname", "tailscale_tags", "tailscale_ipv4", "tailscale_fqdn", "tailscale_error", "tailscale_exit_node", "tailscale_exit_node_allow_lan_access":
		switch key {
		case "keep", "tailscale", "tailscale_state", "tailscale_exit_node_allow_lan_access":
			value = strings.ToLower(value)
		}
		labels[key] = value
	case "state":
		value = strings.ToLower(value)
		if statePriority(value) >= statePriority(labels["state"]) {
			labels["state"] = value
		}
	case "expires_at", "last_touched_at":
		if numericTagValue(value) >= numericTagValue(labels[key]) {
			labels[key] = value
		}
	}
}

func isOwnershipTagKey(key string) bool {
	switch key {
	case "provider", "lease", "slug", "target":
		return true
	default:
		return false
	}
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

func replaceCrabboxTags(existing, desired []string) []string {
	tags := append([]string(nil), desired...)
	for _, tag := range existing {
		lower := strings.ToLower(strings.TrimSpace(tag))
		if lower == tagCrabbox || strings.HasPrefix(lower, tagPrefix) {
			continue
		}
		tags = append(tags, tag)
	}
	return normalizeTags(tags)
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

func isOwnedLinode(item linodeInstance) bool {
	return validateLinodeLabels(labelsFromTags(item.Tags)) == nil
}

func validateLinodeLabels(labels map[string]string) error {
	if labels == nil ||
		labels[ownershipTagConflictLabel] != "" ||
		labels["crabbox"] != "true" ||
		labels["created_by"] != "crabbox" ||
		labels["provider"] != providerName ||
		!core.IsCanonicalLeaseID(labels["lease"]) ||
		labels["slug"] == "" ||
		labels["target"] != core.TargetLinux {
		return core.Exit(2, "refusing to operate on non-Crabbox Linode instance")
	}
	return nil
}
