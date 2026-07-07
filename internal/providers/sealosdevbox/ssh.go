package sealosdevbox

import (
	"context"
	"encoding/json"
	"fmt"
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
	if network, ok := value.(map[string]any); ok {
		if port, ok := numericPort(network["nodePort"]); ok {
			return port, true
		}
	}
	if port, ok := findSSHNodePort(value); ok {
		return port, true
	}
	return 0, false
}

func findSSHNodePort(value any) (int, bool) {
	switch typed := value.(type) {
	case map[string]any:
		if port, ok := numericPort(typed["sshNodePort"]); ok {
			return port, true
		}
		if sshPortEntry(typed) {
			for _, key := range []string{"nodePort", "sshNodePort", "port"} {
				if port, ok := numericPort(typed[key]); ok {
					return port, true
				}
			}
		}
		for _, nested := range typed {
			if port, ok := findSSHNodePort(nested); ok {
				return port, true
			}
		}
	case []any:
		for _, nested := range typed {
			if port, ok := findSSHNodePort(nested); ok {
				return port, true
			}
		}
	}
	return 0, false
}

func sshPortEntry(value map[string]any) bool {
	for _, key := range []string{"name", "protocol", "app", "service"} {
		if strings.EqualFold(strings.TrimSpace(stringValue(value[key])), "ssh") {
			return true
		}
	}
	for _, key := range []string{"port", "targetPort", "containerPort"} {
		if port, ok := numericPort(value[key]); ok && port == 22 {
			return true
		}
	}
	return false
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	default:
		return ""
	}
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

func (b *backend) prepareSSH(ctx context.Context, target *core.SSHTarget, phase string) error {
	transport := *target
	transport.ReadyCheck = "true"
	if err := b.waitForSSH(ctx, &transport, phase); err != nil {
		return err
	}
	target.Port = transport.Port
	if b.rt.Stderr != nil {
		fmt.Fprintln(b.rt.Stderr, "bootstrapping Sealos DevBox tools")
	}
	run := b.sshRun
	if run == nil {
		run = core.RunSSHQuiet
	}
	if err := run(ctx, *target, sealosBootstrapToolsCommand()); err != nil {
		return core.Exit(5, "Sealos DevBox tool bootstrap failed: %v", err)
	}
	return b.waitForSSH(ctx, target, phase)
}

func sealosBootstrapToolsCommand() string {
	return strings.Join([]string{
		"set -e",
		"if command -v git >/dev/null 2>&1 && command -v rsync >/dev/null 2>&1 && command -v tar >/dev/null 2>&1; then exit 0; fi",
		"SUDO=",
		"if [ \"$(id -u)\" != 0 ]; then command -v sudo >/dev/null 2>&1 && sudo -n true || { echo 'sealos-devbox tool bootstrap requires root or passwordless sudo' >&2; exit 1; }; SUDO='sudo -n'; fi",
		"if command -v apt-get >/dev/null 2>&1; then",
		"  $SUDO apt-get update >/tmp/crabbox-sealos-apt-update.log 2>&1",
		"  $SUDO env DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends git rsync tar >/tmp/crabbox-sealos-apt-install.log 2>&1",
		"elif command -v dnf >/dev/null 2>&1; then",
		"  $SUDO dnf install -y git rsync tar >/tmp/crabbox-sealos-dnf-install.log 2>&1",
		"elif command -v yum >/dev/null 2>&1; then",
		"  $SUDO yum install -y git rsync tar >/tmp/crabbox-sealos-yum-install.log 2>&1",
		"elif command -v apk >/dev/null 2>&1; then",
		"  $SUDO apk add --no-cache git rsync tar >/tmp/crabbox-sealos-apk-install.log 2>&1",
		"else",
		"  echo 'sealos-devbox tool bootstrap requires apt-get, dnf, yum, or apk' >&2; exit 1",
		"fi",
		sealosSSHReadyCheck,
	}, "\n")
}
