package linode

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
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

var tagSchema = shared.LeaseTagSchema(shared.TailscaleTagFields()...)

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
	for _, key := range tagSchema.Keys() {
		if value := labels[key]; value != "" {
			tags = append(tags, encodeTagKV(key, value)...)
		}
	}
	return normalizeTags(tags)
}

func encodeTagKV(key, value string) []string {
	key = sanitizeTagPart(key)
	plain := tagPrefix + key + ":" + sanitizeTagPart(value)
	if !tagSchema.Exact(key) && len(plain) <= maxLinodeTagLength {
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

func versionedExactTagValueKey(key string) (string, bool) {
	logical := strings.TrimSuffix(key, "_v1")
	return logical, logical != key && tagSchema.Exact(logical)
}

func chunkedTagValueKey(key string) (string, bool) {
	logical := strings.TrimSuffix(key, tagChunkSuffix)
	if logical == key {
		return "", false
	}
	for _, candidate := range tagSchema.Keys() {
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
	return shared.EncodeExactTagValue(value, maxLen)
}

func decodeExactTagValue(value string) string {
	return shared.DecodeExactTagValue(value)
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
	reducer := tagSchema.Reducer()
	chunkedExact := map[string]*tagChunkSet{}
	var versionedExact, legacyExact shared.TagValueSet
	for _, tag := range tags {
		lowerTag := strings.ToLower(tag)
		switch {
		case lowerTag == tagCrabbox:
			reducer.MarkOwned()
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
				versionedExact.Record(logical, decodeExactTagValue(parts[1]))
				continue
			}
			if tagSchema.Exact(key) {
				value := parts[1]
				if legacyEncodedExactTagValueKey(key) {
					value = decodeExactTagValue(value)
				}
				legacyExact.Record(key, value)
				continue
			}
			reducer.Apply(key, parts[1])
		}
	}
	for key, chunks := range chunkedExact {
		if chunks.conflict || chunks.total == 0 || len(chunks.parts) != chunks.total {
			versionedExact.Reject(key)
			continue
		}
		var encoded strings.Builder
		for index := 0; index < chunks.total; index++ {
			part, ok := chunks.parts[index]
			if !ok {
				versionedExact.Reject(key)
				break
			}
			encoded.WriteString(part)
		}
		if _, _, conflict := versionedExact.Get(key); !conflict {
			versionedExact.Record(key, decodeExactTagValue(encoded.String()))
		}
	}
	for _, key := range tagSchema.Keys() {
		if value, present, conflict := versionedExact.Get(key); present {
			reducer.Overlay(key, value, conflict)
			continue
		}
		if value, present, conflict := legacyExact.Get(key); present {
			reducer.Overlay(key, value, conflict)
		}
	}
	return reducer.Finish(ownershipTagConflictLabel)
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
