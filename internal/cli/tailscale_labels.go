package cli

import (
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"strconv"
	"strings"
)

const tailscaleTagChunkPrefix = "tailscale_tags_"

var tailscaleTagEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// Chunked base32 survives even provider labels restricted to 63 lowercase bytes.
// The manifest detects missing/truncated chunks instead of reporting a partial tag set.
func setTailscaleTagLabels(labels map[string]string, tags []string) {
	for key := range labels {
		if strings.HasPrefix(key, tailscaleTagChunkPrefix) {
			delete(labels, key)
		}
	}
	value := strings.Join(tags, ",")
	encoded := tailscaleTagEncoding.EncodeToString([]byte(value))
	count := 0
	for len(encoded) > 0 {
		n := min(63, len(encoded))
		labels[fmt.Sprintf("%s%d", tailscaleTagChunkPrefix, count)] = encoded[:n]
		encoded = encoded[n:]
		count++
	}
	digest := sha256.Sum256([]byte(value))
	labels["tailscale_tags"] = fmt.Sprintf("v1-%d-%x", count, digest[:16])
}

func tailscaleTagsFromLabels(labels map[string]string) ([]string, error) {
	value := labels["tailscale_tags"]
	if strings.HasPrefix(value, "v1-") {
		parts := strings.Split(value, "-")
		if len(parts) != 3 {
			return nil, fmt.Errorf("stored Tailscale tag metadata is incomplete")
		}
		count, err := strconv.Atoi(parts[1])
		if err != nil || count < 0 || count > len(labels) {
			return nil, fmt.Errorf("stored Tailscale tag metadata is incomplete")
		}
		var encoded strings.Builder
		for i := 0; i < count; i++ {
			chunk := labels[fmt.Sprintf("%s%d", tailscaleTagChunkPrefix, i)]
			if chunk == "" || len(chunk) > 63 {
				return nil, fmt.Errorf("stored Tailscale tag metadata is incomplete")
			}
			encoded.WriteString(chunk)
		}
		decoded, err := tailscaleTagEncoding.DecodeString(encoded.String())
		digest := fmt.Sprintf("%x", sha256.Sum256(decoded))
		if err != nil || len(parts[2]) != 32 || digest[:32] != parts[2] {
			return nil, fmt.Errorf("stored Tailscale tag metadata is incomplete")
		}
		value = string(decoded)
	}
	tags := splitCommaList(value)
	for _, tag := range tags {
		if !validTailscaleTag(tag) {
			return nil, fmt.Errorf("stored Tailscale tags are invalid; recreate the lease to record its exact tags")
		}
	}
	return tags, nil
}
