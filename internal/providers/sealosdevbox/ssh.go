package sealosdevbox

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

const sealosSSHReadyCheck = "command -v git >/dev/null && command -v rsync >/dev/null && command -v tar >/dev/null"

func (b *backend) sshTarget(item devboxItem, keyPath string, requireKey bool) (core.SSHTarget, error) {
	network := normalizeNetwork(b.cfg.SealosDevbox.Network)
	switch network {
	case networkSSHGate:
		return b.sshGateTarget(keyPath, requireKey)
	case networkNodePort:
		return b.nodePortTarget(item, keyPath, requireKey)
	default:
		return core.SSHTarget{}, core.Exit(2, "sealos-devbox network must be SSHGate or NodePort")
	}
}

func (b *backend) sshGateTarget(keyPath string, requireKey bool) (core.SSHTarget, error) {
	host := strings.TrimSpace(b.cfg.SealosDevbox.SSHGatewayHost)
	port := strings.TrimSpace(b.cfg.SealosDevbox.SSHGatewayPort)
	target := b.baseSSHTarget(keyPath)
	target.Host = host
	target.Port = port
	if err := validateSealosSSHTarget(target, requireKey); err != nil {
		return core.SSHTarget{}, err
	}
	return target, nil
}

func (b *backend) nodePortTarget(item devboxItem, keyPath string, requireKey bool) (core.SSHTarget, error) {
	host := strings.TrimSpace(b.cfg.SealosDevbox.NodeHost)
	if host == "" {
		host = strings.TrimSpace(item.Status.NodeName)
	}
	port, ok := devboxSSHNodePort(item)
	if !ok {
		return core.SSHTarget{}, core.Exit(5, "Sealos DevBox %s has no SSH NodePort in status.network", strings.TrimSpace(item.Metadata.Name))
	}
	target := b.baseSSHTarget(keyPath)
	target.Host = host
	target.Port = strconv.Itoa(port)
	if err := validateSealosSSHTarget(target, requireKey); err != nil {
		return core.SSHTarget{}, err
	}
	return target, nil
}

func (b *backend) baseSSHTarget(keyPath string) core.SSHTarget {
	return core.SSHTarget{
		User:        strings.TrimSpace(b.cfg.SealosDevbox.SSHUser),
		Key:         keyPath,
		TargetOS:    core.TargetLinux,
		NetworkKind: core.NetworkPublic,
		ReadyCheck:  sealosSSHReadyCheck,
	}
}

func validateSealosSSHTarget(target core.SSHTarget, requireKey bool) error {
	if strings.TrimSpace(target.User) == "" || strings.ContainsAny(target.User, " \t\r\n@") {
		return core.Exit(2, "sealos-devbox SSH user %q is invalid", target.User)
	}
	if strings.TrimSpace(target.Host) == "" {
		return core.Exit(2, "sealos-devbox SSH host is required")
	}
	port, err := strconv.Atoi(strings.TrimSpace(target.Port))
	if err != nil || port < 1 || port > 65535 {
		return core.Exit(2, "sealos-devbox SSH port must be between 1 and 65535")
	}
	if requireKey && strings.TrimSpace(target.Key) == "" {
		return core.Exit(5, "sealos-devbox SSH key path is required")
	}
	return nil
}

func devboxSSHNodePort(item devboxItem) (int, bool) {
	raw, err := json.Marshal(item.Status.Network)
	if err != nil || len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, false
	}
	return findNodePort(value)
}

func findNodePort(value any) (int, bool) {
	switch typed := value.(type) {
	case map[string]any:
		for _, key := range []string{"nodePort", "sshNodePort", "port"} {
			if port, ok := numericPort(typed[key]); ok {
				return port, true
			}
		}
		for _, nested := range typed {
			if port, ok := findNodePort(nested); ok {
				return port, true
			}
		}
	case []any:
		for _, nested := range typed {
			if port, ok := findNodePort(nested); ok {
				return port, true
			}
		}
	}
	return 0, false
}

func numericPort(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		port := int(typed)
		return port, typed == float64(port) && port >= 1 && port <= 65535
	case string:
		port, err := strconv.Atoi(strings.TrimSpace(typed))
		return port, err == nil && port >= 1 && port <= 65535
	default:
		return 0, false
	}
}

func (b *backend) waitForSSH(ctx context.Context, target *core.SSHTarget, phase string) error {
	waiter := b.sshReady
	if waiter == nil {
		waiter = core.WaitForSSHReady
	}
	return waiter(ctx, target, b.rt.Stderr, phase, bootstrapWaitTimeout(b.cfg))
}
