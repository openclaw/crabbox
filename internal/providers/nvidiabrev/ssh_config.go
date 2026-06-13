package nvidiabrev

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

type brevSSHConfigEntry struct {
	Aliases        []string
	HostName       string
	Port           string
	User           string
	IdentityFile   string
	KnownHostsFile string
	ProxyCommand   string
}

func defaultBrevSSHConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".brev", "ssh_config")
	}
	// brev refresh writes generated hosts here and includes this file from ~/.ssh/config.
	return filepath.Join(home, ".brev", "ssh_config")
}

func parseBrevSSHConfig(data string) ([]brevSSHConfigEntry, error) {
	var entries []brevSSHConfigEntry
	var current *brevSSHConfigEntry
	scanner := bufio.NewScanner(strings.NewReader(data))
	for scanner.Scan() {
		line := stripSSHConfigComment(scanner.Text())
		if strings.TrimSpace(line) == "" {
			continue
		}
		key, value := splitSSHConfigDirective(line)
		if key == "" {
			continue
		}
		if strings.EqualFold(key, "Host") {
			aliases := splitSSHConfigFields(value)
			if len(aliases) == 0 {
				current = nil
				continue
			}
			entry := brevSSHConfigEntry{Aliases: aliases}
			entries = append(entries, entry)
			current = &entries[len(entries)-1]
			continue
		}
		if current == nil {
			continue
		}
		switch strings.ToLower(key) {
		case "hostname":
			current.HostName = unquoteSSHConfigValue(value)
		case "port":
			current.Port = unquoteSSHConfigValue(value)
		case "user":
			current.User = unquoteSSHConfigValue(value)
		case "identityfile":
			current.IdentityFile = unquoteSSHConfigValue(value)
		case "userknownhostsfile":
			current.KnownHostsFile = unquoteSSHConfigValue(value)
		case "proxycommand":
			current.ProxyCommand = strings.TrimSpace(value)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return entries, nil
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

func splitSSHConfigFields(value string) []string {
	var out []string
	for _, field := range strings.Fields(value) {
		field = unquoteSSHConfigValue(field)
		if field != "" {
			out = append(out, field)
		}
	}
	return out
}

func unquoteSSHConfigValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}

func selectBrevSSHTarget(cfg Config, data, alias string) (SSHTarget, error) {
	entries, err := parseBrevSSHConfig(data)
	if err != nil {
		return SSHTarget{}, err
	}
	var matches []brevSSHConfigEntry
	for _, entry := range entries {
		for _, candidate := range entry.Aliases {
			if candidate == alias {
				matches = append(matches, entry)
				break
			}
		}
	}
	if len(matches) == 0 {
		return SSHTarget{}, exit(4, "nvidia-brev SSH config entry not found for host %q", alias)
	}
	if len(matches) > 1 {
		return SSHTarget{}, exit(2, "nvidia-brev SSH config entry for host %q is ambiguous", alias)
	}
	entry := matches[0]
	user := firstNonEmpty(cfg.NvidiaBrev.User, entry.User, cfg.SSHUser)
	if strings.TrimSpace(user) == "" {
		return SSHTarget{}, exit(2, "nvidia-brev SSH config entry %q is missing User", alias)
	}
	if !validBrevSSHUser(user) {
		return SSHTarget{}, exit(2, "nvidia-brev SSH config entry %q has invalid User %q", alias, user)
	}
	if strings.TrimSpace(entry.IdentityFile) == "" {
		return SSHTarget{}, exit(2, "nvidia-brev SSH config entry %q is missing IdentityFile", alias)
	}
	host := strings.TrimSpace(entry.HostName)
	proxy := strings.TrimSpace(entry.ProxyCommand)
	if host == "" && proxy == "" {
		return SSHTarget{}, exit(2, "nvidia-brev SSH config entry %q is missing HostName or ProxyCommand", alias)
	}
	if host == "" {
		host = alias
	}
	port := strings.TrimSpace(entry.Port)
	if port == "" {
		port = defaultSSHPort
	}
	if _, err := strconv.Atoi(port); err != nil {
		return SSHTarget{}, exit(2, "nvidia-brev SSH config entry %q has invalid Port %q", alias, port)
	}
	target := SSHTarget{
		User:           user,
		Host:           host,
		Key:            entry.IdentityFile,
		KnownHostsFile: entry.KnownHostsFile,
		Port:           port,
		TargetOS:       targetLinux,
		NetworkKind:    networkPublic,
	}
	if proxy != "" {
		target.SSHConfigProxy = true
		target.ProxyCommand = proxy
	}
	return target, nil
}

func validBrevSSHUser(user string) bool {
	if user == "" || strings.HasPrefix(user, "-") || strings.Contains(user, "@") {
		return false
	}
	return strings.IndexFunc(user, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) == -1
}

func brevSSHConfigAlias(workspaceName, target string) string {
	name := strings.TrimSpace(workspaceName)
	if strings.EqualFold(strings.TrimSpace(target), "host") {
		return name + "-host"
	}
	return name
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
