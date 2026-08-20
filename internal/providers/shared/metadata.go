package shared

import (
	"strconv"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func ApplyTailscaleMetadata(labels map[string]string, meta core.TailscaleMetadata) {
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

func EncodeExactTagValue(value string, maxLen int) string {
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

func DecodeExactTagValue(value string) string {
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
