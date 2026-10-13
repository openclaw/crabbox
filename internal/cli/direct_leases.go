package cli

import (
	"fmt"
	"strings"
)

func normalizeDirectLeases(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "allow":
		return "allow", nil
	case "deny":
		return "deny", nil
	default:
		return "", fmt.Errorf("directLeases must be allow or deny")
	}
}

func normalizeDirectLeaseAllow(values []string) ([]string, error) {
	var result []string
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		provider, err := ProviderFor(value)
		if err != nil {
			return nil, err
		}
		name := provider.Spec().Name
		if !seen[name] {
			result = append(result, name)
			seen[name] = true
		}
	}
	return result, nil
}

func applyDirectLeaseFileConfig(cfg *Config, file fileConfig, trusted bool) error {
	if file.DirectLeases != "" {
		policy, err := normalizeDirectLeases(file.DirectLeases)
		if err != nil {
			return err
		}
		if trusted || policy == "deny" {
			cfg.DirectLeases = policy
		}
	}
	// Repository automation cannot grant a machine-policy exception, even if
	// it also sets deny in this layer. Only the operator owns the allowlist.
	if trusted && file.DirectLeaseAllow != nil {
		allow, err := normalizeDirectLeaseAllow(*file.DirectLeaseAllow)
		if err != nil {
			return err
		}
		cfg.DirectLeaseAllow = allow
	}
	return nil
}

// Local-only backends never allocate remote infrastructure. Backends capable
// of selecting a remote host (for example parallels/incus) are not exempt.
func directLeaseExempt(provider string) bool {
	switch provider {
	case "ssh", "local-container", "docker-sandbox", "multipass", "firecracker", "lume", "tart", "hyperv", "windows-sandbox", "apple-container", "apple-machine", "apple-vm", "anthropic-sandbox-runtime", "mxc":
		return true
	}
	return false
}

func validateDirectLeaseCreation(cfg Config) error {
	policy, err := normalizeDirectLeases(cfg.DirectLeases)
	if err != nil {
		return err
	}
	if policy != "deny" {
		return nil
	}
	provider, err := ProviderFor(cfg.Provider)
	if err != nil {
		return err
	}
	name := provider.Spec().Name
	if directLeaseExempt(name) || ShouldUseCoordinator(cfg, provider.Spec()) {
		return nil
	}
	for _, allowed := range cfg.DirectLeaseAllow {
		if allowed == name {
			return nil
		}
	}
	return Exit(2, "direct-provider leases are disabled (directLeases: deny); configure a coordinator or add %s to directLeaseAllow", name)
}
