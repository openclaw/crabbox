package nvidiabrev

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func defaultBrevSSHConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".brev", "ssh_config")
	}
	// brev refresh writes generated hosts here and includes this file from ~/.ssh/config.
	return filepath.Join(home, ".brev", "ssh_config")
}

// Brev emits Match exec certificate hooks, including entries with no Host stanza.
// Let OpenSSH interpret its own config, retaining the alias for later renewal.
func (c *brevClient) resolveSSHConfig(ctx context.Context, cfg core.Config, path, alias string, data []byte) (core.SSHTarget, error) {
	if !brevSSHNamePattern.MatchString(alias) {
		return core.SSHTarget{}, core.Exit(2, "invalid nvidia-brev SSH alias %q", alias)
	}
	file, err := os.CreateTemp("", "crabbox-brev-ssh-*")
	if err != nil {
		return core.SSHTarget{}, err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(data)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return core.SSHTarget{}, err
	}
	args := []string{"-G", "-F", file.Name()}
	if user := strings.TrimSpace(cfg.NvidiaBrev.User); user != "" {
		if !brevSSHNamePattern.MatchString(user) {
			return core.SSHTarget{}, core.Exit(2, "invalid nvidia-brev SSH User %q", user)
		}
		args = append(args, "-l", user)
	}
	args = append(args, "--", alias)
	result, err := c.rt.Exec.Run(ctx, core.LocalCommandRequest{Name: "ssh", Args: args})
	if err != nil {
		return core.SSHTarget{}, fmt.Errorf("resolve nvidia-brev OpenSSH config for %q: %w", alias, err)
	}
	values := make(map[string]string)
	for _, line := range strings.Split(result.Stdout, "\n") {
		key, value, ok := strings.Cut(line, " ")
		if ok {
			values[key] = strings.TrimSpace(value)
		}
	}
	// Every Brev route declares IdentitiesOnly and a direct host or proxy. If a
	// mandatory mint hook fails, ssh -G succeeds with defaults: reject that route.
	proxy := values["proxycommand"]
	if values["identitiesonly"] != "yes" ||
		((values["hostname"] == "" || values["hostname"] == alias) && (proxy == "" || proxy == "none")) {
		return core.SSHTarget{}, fmt.Errorf("%w for %q; run `crabbox status --wait --id <lease>` and check certificate authentication", errBrevSSHRouteMissing, alias)
	}
	if !brevSSHNamePattern.MatchString(values["user"]) {
		return core.SSHTarget{}, core.Exit(2, "invalid nvidia-brev SSH User %q", values["user"])
	}
	port, err := strconv.Atoi(values["port"])
	if err != nil || port < 1 || port > 65535 {
		return core.SSHTarget{}, core.Exit(2, "invalid nvidia-brev SSH Port %q", values["port"])
	}
	return core.SSHTarget{
		User: values["user"], Host: alias, Port: values["port"],
		SSHConfigFile: path, SSHConfigData: data, SSHConfigProxy: true, NoControlMaster: true,
		KnownHostsFile: values["userknownhostsfile"],
		TargetOS:       targetLinux, NetworkKind: networkPublic,
		ReadyCheck: "command -v git >/dev/null && command -v rsync >/dev/null && command -v tar >/dev/null",
	}, nil
}

var brevSSHNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]*$`)

func brevSSHConfigAlias(workspaceName, target string) string {
	name := strings.TrimSpace(workspaceName)
	if strings.EqualFold(strings.TrimSpace(target), "host") {
		return name + "-host"
	}
	return name
}

var errBrevSSHRouteMissing = core.Exit(4, "nvidia-brev SSH route not found")

const brevSSHConfigDigestLabel = "brev_ssh_config_sha256"

func brevSSHConfigDigest(data []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(data))
}
