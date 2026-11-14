package cli

import (
	"encoding/base32"
	"strconv"
	"strings"
)

var gcpExactTailscaleKeys = []string{
	"tailscale_hostname", "tailscale_fqdn", "tailscale_ipv4", "tailscale_exit_node",
	"tailscale_version", "tailscale_device_id", "tailscale_error",
}

var gcpTailscaleEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// Numbered chunks preserve endpoints longer than GCP's 63-byte label limit.
func encodeGCPTailscaleLabels(out, labels map[string]string) {
	for _, key := range gcpExactTailscaleKeys {
		value, ok := labels[key]
		if !ok || value == "" {
			continue
		}
		encoded := gcpTailscaleEncoding.EncodeToString([]byte(value))
		count := (len(encoded) + 62) / 63
		out[key+"_enc"] = "b32-" + strconv.Itoa(count)
		for i := 0; i < count; i++ {
			end := (i + 1) * 63
			if end > len(encoded) {
				end = len(encoded)
			}
			chunkKey := key
			if i > 0 {
				chunkKey += "_" + strconv.Itoa(i)
			}
			out[chunkKey] = encoded[i*63 : end]
		}
	}
}

func decodeGCPTailscaleLabels(labels map[string]string) map[string]string {
	out := cloneStringMap(labels)
	for _, key := range gcpExactTailscaleKeys {
		marker := labels[key+"_enc"]
		if !strings.HasPrefix(marker, "b32-") {
			continue
		}
		count, err := strconv.Atoi(strings.TrimPrefix(marker, "b32-"))
		if err != nil || count < 1 || count > 64 {
			delete(out, key)
			continue
		}
		var encoded strings.Builder
		complete := true
		for i := 0; i < count; i++ {
			chunkKey := key
			if i > 0 {
				chunkKey += "_" + strconv.Itoa(i)
			}
			chunk, ok := labels[chunkKey]
			if !ok {
				complete = false
			}
			encoded.WriteString(chunk)
			delete(out, chunkKey)
		}
		delete(out, key+"_enc")
		decoded, err := gcpTailscaleEncoding.DecodeString(encoded.String())
		if complete && err == nil {
			out[key] = string(decoded)
		}
	}
	return out
}
