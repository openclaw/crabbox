package lambda

import (
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	lambdaKeyIDLabel       = "lambda_ssh_key_id"
	lambdaKeyNameLabel     = "lambda_ssh_key_name"
	lambdaKeyOwnedLabel    = "lambda_ssh_key_owned"
	lambdaTouchLocalLabel  = "lambda_touch_local_only"
	lambdaRecoveryKeyLabel = "recovery"
)

func leaseTags(cfg core.Config, leaseID, slug, state string, keep bool, now time.Time) map[string]string {
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", keep, now)
	labels["state"] = state
	if cfg.Tailscale.Enabled && len(cfg.Tailscale.Tags) > 0 {
		labels["tailscale_tags"] = strings.Join(cfg.Tailscale.Tags, ",")
	}
	return labels
}

func isOwnedInstance(item Instance) bool {
	return validateLambdaLabels(item.Tags) == nil
}

func validateLambdaLabels(labels map[string]string) error {
	if labels == nil ||
		labels["crabbox"] != "true" ||
		labels["created_by"] != "crabbox" ||
		labels["provider"] != providerName ||
		labels["lease"] == "" ||
		labels["slug"] == "" ||
		labels["target"] != core.TargetLinux {
		return core.Exit(2, "refusing to operate on non-Crabbox Lambda instance")
	}
	return nil
}

func normalizeLambdaLabels(labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels))
	for key, value := range labels {
		out[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	if out["provider_key"] == "" && out["lease"] != "" {
		out["provider_key"] = providerKeyForLease(out["lease"])
	}
	return out
}

func applyTailscaleMetadata(labels map[string]string, meta core.TailscaleMetadata) {
	if !meta.Enabled {
		delete(labels, "tailscale_state")
		delete(labels, "tailscale_hostname")
		delete(labels, "tailscale_tags")
		delete(labels, "tailscale_ipv4")
		delete(labels, "tailscale_fqdn")
		delete(labels, "tailscale_error")
		delete(labels, "tailscale_exit_node")
		delete(labels, "tailscale_exit_node_allow_lan_access")
		return
	}
	labels["tailscale"] = "true"
	if meta.State != "" {
		labels["tailscale_state"] = meta.State
	}
	if meta.Hostname != "" {
		labels["tailscale_hostname"] = meta.Hostname
	}
	if len(meta.Tags) > 0 {
		labels["tailscale_tags"] = strings.Join(meta.Tags, ",")
	}
	if meta.IPv4 != "" {
		labels["tailscale_ipv4"] = meta.IPv4
	}
	if meta.FQDN != "" {
		labels["tailscale_fqdn"] = meta.FQDN
	}
	if meta.Error != "" {
		labels["tailscale_error"] = meta.Error
	}
	if meta.ExitNode != "" {
		labels["tailscale_exit_node"] = meta.ExitNode
		labels["tailscale_exit_node_allow_lan_access"] = "false"
		if meta.ExitNodeAllowLANAccess {
			labels["tailscale_exit_node_allow_lan_access"] = "true"
		}
	}
}
