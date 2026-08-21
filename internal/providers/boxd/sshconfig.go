package boxd

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

// The boxd CLI maintains a marker-delimited block in ~/.ssh/config with one
// entry per machine (Host aliases, HostName, the machine's per-VM SSH Port,
// User boxd, and the CLI-managed IdentityFile), refreshed as a side effect of
// `boxd machine new`/`list`/`remove`. It also pre-trusts the proxy's host key
// in ~/.ssh/known_hosts for every machine host:port. The provider reads that
// entry back to build an explicit SSHTarget — the same vendor-CLI-managed
// connection model the nvidia-brev and tenki providers use.

type boxdSSHConfigEntry struct {
	Aliases        []string
	HostName       string
	Port           string
	User           string
	IdentityFile   string
	KnownHostsFile string
}

func defaultSSHConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".ssh", "config")
	}
	return filepath.Join(home, ".ssh", "config")
}

// defaultKnownHostsPath is where the boxd CLI pre-trusts the proxy host key
// for every machine, as `[<host>]:<port> <keytype> <key>` lines.
func defaultKnownHostsPath() string {
	return filepath.Join(filepath.Dir(defaultSSHConfigPath()), "known_hosts")
}

// pinnedHostKey returns the "<keytype> <key>" the boxd CLI pinned for exactly
// `[host]:port`, or "" when no pin is present (or the file is unreadable).
func pinnedHostKey(data, host, port string) string {
	want := "[" + strings.TrimSpace(host) + "]:" + strings.TrimSpace(port)
	scanner := bufio.NewScanner(strings.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "@") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		for _, pattern := range strings.Split(fields[0], ",") {
			if pattern == want {
				return fields[1] + " " + fields[2]
			}
		}
	}
	return ""
}

func parseSSHConfigEntries(data string) []boxdSSHConfigEntry {
	var entries []boxdSSHConfigEntry
	var current *boxdSSHConfigEntry
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
			entries = append(entries, boxdSSHConfigEntry{Aliases: aliases})
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
		}
	}
	return entries
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

// selectBoxdSSHEntry finds the CLI-written entry whose aliases contain the
// machine's fully-qualified host (`<name>.<zone>`). An entry without a Port is
// returned as found-but-incomplete: the machine's per-VM SSH port was not yet
// allocated when the CLI last synced, and the caller re-syncs and retries.
func selectBoxdSSHEntry(data, host string) (boxdSSHConfigEntry, bool, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return boxdSSHConfigEntry{}, false, core.Exit(2, "boxd ssh-config lookup requires a machine host")
	}
	var matches []boxdSSHConfigEntry
	for _, entry := range parseSSHConfigEntries(data) {
		for _, alias := range entry.Aliases {
			if alias == host {
				matches = append(matches, entry)
				break
			}
		}
	}
	if len(matches) == 0 {
		return boxdSSHConfigEntry{}, false, nil
	}
	if len(matches) > 1 {
		return boxdSSHConfigEntry{}, false, core.Exit(2, "boxd ssh-config entry for host %q is ambiguous", host)
	}
	return matches[0], true, nil
}

// sshTargetFromEntry validates a complete CLI-written entry into an explicit
// SSHTarget. Multiplexing is disabled: the boxd proxy keeps one interactive
// session's forwarding state per TCP connection, so ControlMaster-shared
// connections would clobber each other (the boxd CLI's own ssh-config block
// documents the same constraint).
//
// Host trust is the vendor-managed pin, REQUIRED: the target carries the
// CLI-maintained known_hosts file, and that file's exact `[host]:port` pin is
// surfaced as SSHHostKey — which flips the shared SSH transport to
// StrictHostKeyChecking=yes with GlobalKnownHostsFile/KnownHostsCommand
// disabled, so a mismatched edge key is rejected instead of trusted on first
// use. A missing pin fails closed (errHostKeyPinMissing); the caller retries
// behind a CLI re-sync, so a first connection is never trusted blind.
func sshTargetFromEntry(entry boxdSSHConfigEntry, host, knownHostsPath string) (core.SSHTarget, error) {
	port := strings.TrimSpace(entry.Port)
	if port == "" {
		return core.SSHTarget{}, core.Exit(5, "boxd ssh-config entry for %q has no Port (machine SSH port not yet allocated)", host)
	}
	if _, err := strconv.Atoi(port); err != nil {
		return core.SSHTarget{}, core.Exit(2, "boxd ssh-config entry for %q has invalid Port %q", host, entry.Port)
	}
	key := strings.TrimSpace(entry.IdentityFile)
	if key == "" {
		return core.SSHTarget{}, core.Exit(2, "boxd ssh-config entry for %q is missing IdentityFile; run any boxd command (e.g. `boxd machine list`) to link this device's SSH key", host)
	}
	hostname := firstNonBlank(entry.HostName, host)
	user := firstNonBlank(entry.User, boxdSSHUser)
	knownHosts := firstNonBlank(strings.TrimSpace(entry.KnownHostsFile), strings.TrimSpace(knownHostsPath), defaultKnownHostsPath())
	pin := ""
	if data, err := os.ReadFile(knownHosts); err == nil {
		pin = pinnedHostKey(string(data), hostname, port)
	}
	if pin == "" {
		// The vendor pin is the host-trust contract: never fall back to
		// trusting a first public-edge connection. The caller retries behind a
		// CLI re-sync (which rewrites the pin) before this becomes fatal.
		return core.SSHTarget{}, fmt.Errorf("%w: no [%s]:%s host-key pin in %s; run any boxd command (e.g. `boxd machine list`) to refresh it, or update the boxd CLI", errHostKeyPinMissing, hostname, port, knownHosts)
	}
	return core.SSHTarget{
		User:            user,
		Host:            hostname,
		Port:            port,
		Key:             key,
		KnownHostsFile:  knownHosts,
		SSHHostKey:      pin,
		TargetOS:        core.TargetLinux,
		NetworkKind:     "public",
		ReadyCheck:      boxdReadyCheck,
		NoControlMaster: true,
	}, nil
}

// errHostKeyPinMissing marks a target whose vendor host-key pin is not (yet)
// present: retryable behind a CLI re-sync, fatal after the bounded wait.
var errHostKeyPinMissing = errors.New("boxd host-key pin missing")
