package shared

import (
	"bufio"
	"strings"
	"unicode"
)

// GeneratedSSHConfigEntry is the connection subset emitted by provider CLIs.
// Includes, Match blocks, wildcard resolution, and host selection stay outside
// this parser; adapters retain their generated format's lexical rules.
type GeneratedSSHConfigEntry struct {
	Aliases        []string
	HostName       string
	Port           string
	User           string
	IdentityFile   string
	KnownHostsFile string
	ProxyCommand   string
}

func ParseGeneratedSSHConfig(data string, directive func(string) (string, string)) ([]GeneratedSSHConfigEntry, error) {
	var entries []GeneratedSSHConfigEntry
	var current *GeneratedSSHConfigEntry
	scanner := bufio.NewScanner(strings.NewReader(data))
	for scanner.Scan() {
		key, value := directive(scanner.Text())
		if key == "" {
			continue
		}
		if strings.EqualFold(key, "Host") {
			aliases := SplitSSHConfigFields(value)
			if len(aliases) == 0 {
				current = nil
				continue
			}
			entries = append(entries, GeneratedSSHConfigEntry{Aliases: aliases})
			current = &entries[len(entries)-1]
			continue
		}
		if current == nil {
			continue
		}
		switch strings.ToLower(key) {
		case "hostname":
			current.HostName = UnquoteSSHConfigValue(value)
		case "port":
			current.Port = UnquoteSSHConfigValue(value)
		case "user":
			current.User = UnquoteSSHConfigValue(value)
		case "identityfile":
			current.IdentityFile = UnquoteSSHConfigValue(value)
		case "userknownhostsfile":
			current.KnownHostsFile = UnquoteSSHConfigValue(value)
		case "proxycommand":
			current.ProxyCommand = strings.TrimSpace(value)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

func SplitSSHConfigFields(value string) []string {
	var out []string
	for _, field := range strings.Fields(value) {
		field = UnquoteSSHConfigValue(field)
		if field != "" {
			out = append(out, field)
		}
	}
	return out
}

func UnquoteSSHConfigValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}

func ValidSSHConfigUser(user string) bool {
	if user == "" || strings.HasPrefix(user, "-") || strings.Contains(user, "@") {
		return false
	}
	return strings.IndexFunc(user, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) == -1
}
