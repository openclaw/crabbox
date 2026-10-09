package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// remoteWorkspaceOwnerPayloadPrefix and remoteWorkspaceOwnerPayloadSuffix bracket
// the base64 remote script that remoteWorkspaceOwnerPOSIXEncodedLauncher embeds
// in the command it hands to ssh. Fixtures unwrap it so behavior rules match the
// plaintext remote script instead of an opaque launcher.
const (
	remoteWorkspaceOwnerPayloadPrefix = `payload_b64="`
	remoteWorkspaceOwnerPayloadSuffix = `"; decoded=; if command -v base64`
	remoteWorkspaceOwnerPayloadDepth  = 8
)

// scriptedSSHConfigQueryUnavailable mirrors ssh's transport failure code so a
// configuration query without a real client reports an unsupported option rather
// than a fake success.
const scriptedSSHConfigQueryUnavailable = 255

type scriptedSSHRule struct {
	Contains string `json:"contains"`
	Port     string `json:"port,omitempty"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	ExitCode int    `json:"exitCode,omitempty"`
}

type scriptedSSHBehavior struct {
	Rules    []scriptedSSHRule `json:"rules,omitempty"`
	Stdout   string            `json:"stdout,omitempty"`
	Stderr   string            `json:"stderr,omitempty"`
	ExitCode int               `json:"exitCode,omitempty"`

	// ConfigQueryPassthrough forwards `-G` configuration queries to the real ssh
	// client so route and capability probing observes genuine client behavior and
	// never enters the simulated remote-command path.
	ConfigQueryPassthrough bool `json:"configQueryPassthrough,omitempty"`
	// DecodePayload unwraps nested base64 workspace-owner launcher payloads before
	// rules are matched, and records the decoded scripts in the fake ssh log
	// alongside the raw command.
	DecodePayload bool `json:"decodePayload,omitempty"`
	// WorkspaceOwnerProtocol answers acquire/renew/inspect/release probes before
	// user rules run, so remote ownership handshakes complete.
	WorkspaceOwnerProtocol bool `json:"workspaceOwnerProtocol,omitempty"`
	// MatchStdin includes the remote command's stdin in the text rules match.
	MatchStdin bool `json:"matchStdin,omitempty"`
	// RealSSH is the ssh client resolved for ConfigQueryPassthrough.
	RealSSH  string `json:"realSSH,omitempty"`
	ProbeURL string `json:"probeURL,omitempty"`
}

// resolveRealSSHExecutable finds an ssh client outside the fixture directory so
// `-G` passthrough can never re-enter the fixture.
func resolveRealSSHExecutable(fixtureDir string) string {
	if runtime.GOOS == "windows" {
		if windowsDir := strings.TrimSpace(os.Getenv("WINDIR")); windowsDir != "" {
			candidate := filepath.Join(windowsDir, "System32", "OpenSSH", "ssh.exe")
			if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
				return candidate
			}
		}
	} else if info, err := os.Stat("/usr/bin/ssh"); err == nil && !info.IsDir() {
		return "/usr/bin/ssh"
	}
	resolved, err := exec.LookPath("ssh")
	if err != nil {
		return ""
	}
	absolute, err := filepath.Abs(resolved)
	if err != nil {
		return ""
	}
	if fixtureDir != "" {
		fixture, err := filepath.Abs(fixtureDir)
		if err == nil && strings.EqualFold(filepath.Dir(absolute), fixture) {
			return ""
		}
	}
	return absolute
}
