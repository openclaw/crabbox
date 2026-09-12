package nvidiabrev

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func defaultBrevSSHConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".brev", "ssh_config")
	}
	// brev refresh writes generated hosts here and includes this file from ~/.ssh/config.
	return filepath.Join(home, ".brev", "ssh_config")
}

func parseBrevSSHConfig(data string) ([]shared.GeneratedSSHConfigEntry, error) {
	return shared.ParseGeneratedSSHConfig(data, func(line string) (string, string) {
		return splitSSHConfigDirective(stripSSHConfigComment(line))
	})
}

func stripSSHConfigComment(line string) string {
	var quoted byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		if quoted != 0 {
			if c == quoted {
				quoted = 0
			}
			continue
		}
		if c == '\'' || c == '"' {
			quoted = c
			continue
		}
		if c == '#' {
			return line[:i]
		}
	}
	return line
}

func splitSSHConfigDirective(line string) (string, string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", ""
	}
	for i, r := range line {
		if r == ' ' || r == '\t' {
			return strings.TrimSpace(line[:i]), strings.TrimSpace(line[i:])
		}
	}
	return line, ""
}

func selectBrevSSHTarget(cfg core.Config, data, alias string) (core.SSHTarget, error) {
	entries, err := parseBrevSSHConfig(data)
	if err != nil {
		return core.SSHTarget{}, err
	}
	var matches []shared.GeneratedSSHConfigEntry
	for _, entry := range entries {
		for _, candidate := range entry.Aliases {
			if candidate == alias {
				matches = append(matches, entry)
				break
			}
		}
	}
	if len(matches) == 0 {
		return core.SSHTarget{}, core.Exit(4, "nvidia-brev SSH config entry not found for host %q", alias)
	}
	if len(matches) > 1 {
		return core.SSHTarget{}, core.Exit(2, "nvidia-brev SSH config entry for host %q is ambiguous", alias)
	}
	entry := matches[0]
	user := shared.FirstNonBlankTrimmed(cfg.NvidiaBrev.User, entry.User, cfg.SSHUser)
	if strings.TrimSpace(user) == "" {
		return core.SSHTarget{}, core.Exit(2, "nvidia-brev SSH config entry %q is missing User", alias)
	}
	if !shared.ValidSSHConfigUser(user) {
		return core.SSHTarget{}, core.Exit(2, "nvidia-brev SSH config entry %q has invalid User %q", alias, user)
	}
	if strings.TrimSpace(entry.IdentityFile) == "" {
		return core.SSHTarget{}, core.Exit(2, "nvidia-brev SSH config entry %q is missing IdentityFile", alias)
	}
	host := strings.TrimSpace(entry.HostName)
	proxy := strings.TrimSpace(entry.ProxyCommand)
	if host == "" && proxy == "" {
		return core.SSHTarget{}, core.Exit(2, "nvidia-brev SSH config entry %q is missing HostName or ProxyCommand", alias)
	}
	if host == "" {
		host = alias
	}
	port := strings.TrimSpace(entry.Port)
	if port == "" {
		port = defaultSSHPort
	}
	if _, err := strconv.Atoi(port); err != nil {
		return core.SSHTarget{}, core.Exit(2, "nvidia-brev SSH config entry %q has invalid Port %q", alias, port)
	}
	target := core.SSHTarget{
		User:           user,
		Host:           host,
		Key:            entry.IdentityFile,
		KnownHostsFile: entry.KnownHostsFile,
		Port:           port,
		TargetOS:       targetLinux,
		ReadyCheck:     "command -v git >/dev/null && command -v rsync >/dev/null && command -v tar >/dev/null",
		NetworkKind:    networkPublic,
	}
	if proxy != "" {
		target.SSHConfigProxy = true
		target.ProxyCommand = proxy
	}
	return target, nil
}

func brevSSHConfigAlias(workspaceName, target string) string {
	name := strings.TrimSpace(workspaceName)
	if strings.EqualFold(strings.TrimSpace(target), "host") {
		return name + "-host"
	}
	return name
}
