//go:build !windows

package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func installScriptedSSHExecutable(t *testing.T, dir string, behavior scriptedSSHBehavior) string {
	t.Helper()
	sshPath := filepath.Join(dir, "ssh")
	if behavior.ConfigQueryPassthrough && behavior.RealSSH == "" {
		behavior.RealSSH = resolveRealSSHExecutable(dir)
	}
	if err := os.WriteFile(sshPath, []byte(scriptedSSHShellScript(behavior)), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func scriptedSSHShellScript(behavior scriptedSSHBehavior) string {
	var script strings.Builder
	script.WriteString("#!/bin/sh\n")
	if behavior.ConfigQueryPassthrough {
		// Configuration queries must never enter the simulated remote-command path.
		script.WriteString("for arg do\n  if [ \"$arg\" = -G ]; then\n")
		if behavior.RealSSH != "" {
			script.WriteString("    exec " + shellQuote(behavior.RealSSH) + " \"$@\"\n")
		}
		script.WriteString("    exit " + strconv.Itoa(scriptedSSHConfigQueryUnavailable) + "\n")
		script.WriteString("  fi\ndone\n")
	}
	script.WriteString(`cmd=""
port=""
previous=""
for arg do
  if [ "$previous" = "-p" ]; then port="$arg"; fi
  previous="$arg"
  cmd="$arg"
done
if [ -n "${CRABBOX_FAKE_SSH_ARGS_LOG:-}" ]; then
  printf '%s' "$*" > "$CRABBOX_FAKE_SSH_ARGS_LOG"
fi
if [ -n "${CRABBOX_FAKE_SSH_PORTS:-}" ]; then
  printf '%s\n' "$port" >> "$CRABBOX_FAKE_SSH_PORTS"
fi
if [ -n "${CRABBOX_FAKE_SSH_CALLS:-}" ]; then
  printf '%s:%s\n' "$port" "$cmd" >> "$CRABBOX_FAKE_SSH_CALLS"
fi
`)
	if behavior.MatchStdin {
		script.WriteString(`input=$(/bin/cat) || input=""
if [ -n "${CRABBOX_FAKE_SSH_STDIN_LOG:-}" ]; then
  printf '%s' "$input" >> "$CRABBOX_FAKE_SSH_STDIN_LOG"
fi
`)
	} else {
		script.WriteString(`if [ -n "${CRABBOX_FAKE_SSH_STDIN_LOG:-}" ]; then
  /bin/cat >> "$CRABBOX_FAKE_SSH_STDIN_LOG" || true
else
  /bin/cat >/dev/null || true
fi
`)
	}
	script.WriteString("match=$cmd\n")
	if behavior.DecodePayload {
		// Remote scripts arrive base64-wrapped, sometimes nested; unwrap them so
		// rules can match the plaintext the caller actually asked about.
		script.WriteString(`decoded=""
current=$cmd
decode_depth=0
while [ "$decode_depth" -lt ` + strconv.Itoa(remoteWorkspaceOwnerPayloadDepth) + ` ]; do
  case "$current" in
    *'` + remoteWorkspaceOwnerPayloadPrefix + `'*'` + remoteWorkspaceOwnerPayloadSuffix + `'*)
      payload_b64=${current#*'` + remoteWorkspaceOwnerPayloadPrefix + `'}
      payload_b64=${payload_b64%%'` + remoteWorkspaceOwnerPayloadSuffix + `'*}
      current=$(printf %s "$payload_b64" | /usr/bin/base64 --decode 2>/dev/null) ||
        current=$(printf %s "$payload_b64" | /usr/bin/base64 -d 2>/dev/null) ||
        current=$(printf %s "$payload_b64" | /usr/bin/base64 -D 2>/dev/null) || break
      if [ -z "$decoded" ]; then
        decoded=$current
      else
        decoded="$decoded
$current"
      fi
      decode_depth=$((decode_depth + 1))
      ;;
    *) break ;;
  esac
done
match="$cmd
$decoded"
if [ -n "${CRABBOX_FAKE_SSH_LOG:-}" ]; then
  printf '%s\n%s\n---\n' "$cmd" "$decoded" >> "$CRABBOX_FAKE_SSH_LOG"
fi
`)
	} else {
		script.WriteString(`if [ -n "${CRABBOX_FAKE_SSH_LOG:-}" ]; then
  printf '%s\n---\n' "$cmd" >> "$CRABBOX_FAKE_SSH_LOG"
fi
`)
	}
	if behavior.MatchStdin {
		script.WriteString(`match="$match
$input"
`)
	}
	if behavior.WorkspaceOwnerProtocol {
		script.WriteString(`if [ -n "${CRABBOX_FAKE_SSH_DETAILED_LOG:-}" ]; then
  printf '%s\n' "$@" "$match" '---' >> "$CRABBOX_FAKE_SSH_DETAILED_LOG"
fi
`)
		script.WriteString(`case "$match" in
  *"protocol_action='acquire'"*) printf ACQUIRED; exit 0 ;;
  *"protocol_action='renew'"*) printf RENEWED; exit 0 ;;
  *"protocol_action='inspect'"*) printf OWNED; exit 0 ;;
  *"protocol_action='release'"*) printf RELEASED; exit 0 ;;
esac
`)
	}
	if behavior.ProbeURL != "" {
		script.WriteString("if [ \"$current\" = 'exit 0' ]; then /usr/bin/curl --fail --silent --show-error --max-time 10 " + shellQuote(behavior.ProbeURL) + " >/dev/null || exit $?; fi\n")
	}
	for _, rule := range behavior.Rules {
		if rule.Port != "" {
			script.WriteString("if [ \"$port\" = " + shellQuote(rule.Port) + " ]; then\n")
		} else {
			script.WriteString("if :; then\n")
		}
		script.WriteString("case \"$match\" in\n")
		script.WriteString("  *" + shellQuote(rule.Contains) + "*)\n")
		if rule.Stdout != "" {
			script.WriteString("    printf '%s' " + shellQuote(rule.Stdout) + "\n")
		}
		if rule.Stderr != "" {
			script.WriteString("    printf '%s' " + shellQuote(rule.Stderr) + " >&2\n")
		}
		script.WriteString("    exit " + scriptedSSHExitCode(rule.ExitCode) + "\n")
		script.WriteString("    ;;\n")
		script.WriteString("esac\n")
		script.WriteString("fi\n")
	}
	if behavior.Stdout != "" {
		script.WriteString("printf '%s' " + shellQuote(behavior.Stdout) + "\n")
	}
	if behavior.Stderr != "" {
		script.WriteString("printf '%s' " + shellQuote(behavior.Stderr) + " >&2\n")
	}
	script.WriteString("exit " + scriptedSSHExitCode(behavior.ExitCode) + "\n")
	return script.String()
}

func scriptedSSHExitCode(value int) string {
	if value < 0 || value > 255 {
		return "2"
	}
	return strconv.Itoa(value)
}
