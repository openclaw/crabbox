package githubcodespaces

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

type sshConfigEntry struct {
	Aliases        []string
	HostName       string
	Port           string
	User           string
	IdentityFile   string
	KnownHostsFile string
	ProxyCommand   string
}

func parseSSHConfig(data string) ([]sshConfigEntry, error) {
	var entries []sshConfigEntry
	var current *sshConfigEntry
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
			entries = append(entries, sshConfigEntry{Aliases: aliases})
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

func selectSSHTarget(cfg Config, data, alias string) (SSHTarget, error) {
	entry, selectedAlias, err := selectSSHConfigEntry(data, alias)
	if err != nil {
		return SSHTarget{}, err
	}
	user := firstNonEmpty(entry.User, cfg.SSHUser)
	if user == "" {
		return SSHTarget{}, exit(2, "github-codespaces SSH config entry %q is missing User", selectedAlias)
	}
	if !validSSHUser(user) {
		return SSHTarget{}, exit(2, "github-codespaces SSH config entry %q has invalid User %q", selectedAlias, user)
	}
	if strings.TrimSpace(entry.IdentityFile) == "" {
		return SSHTarget{}, exit(2, "github-codespaces SSH config entry %q is missing IdentityFile", selectedAlias)
	}
	host := strings.TrimSpace(entry.HostName)
	proxy := strings.TrimSpace(entry.ProxyCommand)
	if host == "" && proxy == "" {
		return SSHTarget{}, exit(2, "github-codespaces SSH config entry %q is missing HostName or ProxyCommand", selectedAlias)
	}
	if host == "" {
		host = selectedAlias
	}
	port := strings.TrimSpace(entry.Port)
	if port == "" {
		port = defaultSSHPort
	}
	if _, err := strconv.Atoi(port); err != nil {
		return SSHTarget{}, exit(2, "github-codespaces SSH config entry %q has invalid Port %q", selectedAlias, port)
	}
	target := SSHTarget{
		User:           user,
		Host:           host,
		Key:            entry.IdentityFile,
		KnownHostsFile: entry.KnownHostsFile,
		Port:           port,
		TargetOS:       targetLinux,
		ReadyCheck:     githubCodespacesReadyCheck(cfg),
		NetworkKind:    networkPublic,
	}
	if proxy != "" {
		target.SSHConfigProxy = true
		command := rewriteProxyCommandIdentityFile(
			rewriteProxyCommandGHPath(proxy, cfg.GitHubCodespaces.GHPath),
			entry.IdentityFile,
		)
		host, err := (ghRunner{cfg: cfg.GitHubCodespaces}).apiHostname()
		if err != nil {
			return SSHTarget{}, err
		}
		target.ProxyCommand = command
		target.ChildEnv = map[string]string{"GH_HOST": host}
	}
	return target, nil
}

func selectSSHConfigEntry(data, alias string) (sshConfigEntry, string, error) {
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return sshConfigEntry{}, "", exit(2, "github-codespaces SSH config host alias is required")
	}
	entries, err := parseSSHConfig(data)
	if err != nil {
		return sshConfigEntry{}, "", err
	}
	matches := make([]sshConfigEntry, 0, 1)
	matchAliases := make([]string, 0, 1)
	for _, entry := range entries {
		for _, candidate := range entry.Aliases {
			if candidate == alias {
				matches = append(matches, entry)
				matchAliases = append(matchAliases, candidate)
				break
			}
		}
	}
	if len(matches) == 0 {
		for _, entry := range entries {
			if !proxyCommandReferencesCodespace(entry.ProxyCommand, alias) {
				continue
			}
			matches = append(matches, entry)
			matchAliases = append(matchAliases, firstNonEmpty(firstSSHConfigAlias(entry), alias))
		}
	}
	if len(matches) == 0 {
		return sshConfigEntry{}, "", exit(4, "github-codespaces SSH config entry not found for host %q", alias)
	}
	if len(matches) > 1 {
		return sshConfigEntry{}, "", exit(2, "github-codespaces SSH config entry for host %q is ambiguous", alias)
	}
	return matches[0], matchAliases[0], nil
}

func firstSSHConfigAlias(entry sshConfigEntry) string {
	for _, alias := range entry.Aliases {
		if strings.TrimSpace(alias) != "" {
			return strings.TrimSpace(alias)
		}
	}
	return ""
}

func proxyCommandReferencesCodespace(command, name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	fields := splitSSHConfigFields(command)
	for i, field := range fields {
		switch {
		case field == "-c" || field == "--codespace":
			return i+1 < len(fields) && fields[i+1] == name
		case strings.HasPrefix(field, "-c="):
			return strings.TrimPrefix(field, "-c=") == name
		case strings.HasPrefix(field, "--codespace="):
			return strings.TrimPrefix(field, "--codespace=") == name
		}
	}
	return false
}

func rewriteProxyCommandGHPath(command, ghPath string) string {
	ghPath = strings.TrimSpace(ghPath)
	start := len(command) - len(strings.TrimLeftFunc(command, unicode.IsSpace))
	if start == len(command) {
		return command
	}
	body := command[start:]
	executableEnd := proxyCommandGHExecutableEnd(body, ghPath)
	if executableEnd < 0 {
		return command
	}
	executable := unquoteSSHConfigValue(strings.TrimSpace(body[:executableEnd]))
	if executable == defaultGHPath {
		if ghPath == "" || ghPath == defaultGHPath {
			return command
		}
		executable = ghPath
	}
	if executable == "" {
		return command
	}
	return command[:start] + quoteSSHProxyExecutable(executable) + body[executableEnd:]
}

func proxyCommandGHExecutableEnd(command, ghPath string) int {
	markers := []string{" cs ssh ", " codespace ssh "}
	if ghPath = strings.TrimSpace(ghPath); ghPath != "" && ghPath != defaultGHPath && strings.HasPrefix(command, ghPath) {
		for _, marker := range markers {
			if strings.HasPrefix(command[len(ghPath):], marker) {
				return len(ghPath)
			}
		}
	}
	executableEnd := -1
	for _, marker := range markers {
		searchFrom := 0
		for {
			index := strings.Index(command[searchFrom:], marker)
			if index < 0 {
				break
			}
			index += searchFrom
			tail := command[index+len(marker):]
			if strings.HasPrefix(tail, "-c ") || strings.HasPrefix(tail, "--codespace ") {
				if executableEnd < 0 || index < executableEnd {
					executableEnd = index
				}
				break
			}
			searchFrom = index + len(marker)
		}
	}
	return executableEnd
}

func rewriteProxyCommandIdentityFile(command, identityFile string) string {
	identityFile = strings.TrimSpace(identityFile)
	const marker = " -- -i "
	index := strings.LastIndex(command, marker)
	if identityFile == "" || index < 0 {
		return command
	}
	valueStart := index + len(marker)
	if unquoteSSHConfigValue(command[valueStart:]) != identityFile {
		return command
	}
	return command[:valueStart] + quoteSSHProxyExecutable(identityFile)
}

func validatePrivateSSHConfigFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return exit(2, "github-codespaces SSH config path %q must be a regular non-symlink file", path)
	}
	return validatePrivateSSHConfigPermissions(path, info)
}

func storeSSHConfig(leaseID, data string) (string, error) {
	leaseID = strings.TrimSpace(leaseID)
	if leaseID == "" {
		return "", exit(2, "github-codespaces lease id is required for SSH config storage")
	}
	dir, err := sshConfigDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, leaseID+".ssh_config")
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return "", exit(2, "refusing to replace non-regular github-codespaces SSH config path %q", path)
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := securePrivateSSHConfigFile(tmpPath); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if _, err := tmp.Write([]byte(data)); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := replaceSSHConfigFile(tmpPath, path); err != nil {
		return "", err
	}
	removeTemp = false
	if err := syncSSHConfigDirectory(dir); err != nil {
		return "", err
	}
	if err := validatePrivateSSHConfigFile(path); err != nil {
		return "", err
	}
	return path, nil
}

func removeStoredSSHConfig(leaseID string) error {
	if strings.TrimSpace(leaseID) == "" {
		return nil
	}
	dir, err := sshConfigDir()
	if err != nil {
		return err
	}
	err = os.Remove(filepath.Join(dir, strings.TrimSpace(leaseID)+".ssh_config"))
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	return err
}

func sshConfigDir() (string, error) {
	stateDir, err := crabboxStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(stateDir, "github-codespaces"), nil
}

func githubCodespacesReadyCheck(cfg Config) string {
	workRoot := strings.TrimSpace(cfg.GitHubCodespaces.WorkRoot)
	if workRoot == "" {
		workRoot = strings.TrimSpace(cfg.WorkRoot)
	}
	if workRoot == "" {
		workRoot = defaultWorkRoot
	}
	return "command -v git >/dev/null && command -v rsync >/dev/null && command -v tar >/dev/null && test -d " + shellQuote(workRoot)
}

func stripSSHConfigComment(line string) string {
	var quoted byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == '\\' {
			i++
			continue
		}
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
		if c == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t') {
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
		if r == '=' {
			return strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:])
		}
		if r == ' ' || r == '\t' {
			value := strings.TrimSpace(line[i:])
			value = strings.TrimSpace(strings.TrimPrefix(value, "="))
			return strings.TrimSpace(line[:i]), value
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

func validSSHUser(user string) bool {
	if user == "" || strings.HasPrefix(user, "-") || strings.Contains(user, "@") {
		return false
	}
	return strings.IndexFunc(user, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) == -1
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
