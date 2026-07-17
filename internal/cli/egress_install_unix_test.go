//go:build !windows

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallRemoteEgressClientUsesAtomicPromotion(t *testing.T) {
	dir := t.TempDir()
	goScript := `#!/bin/sh
out=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-o" ]; then shift; out="$1"; break; fi
  shift
done
[ -n "$out" ] || exit 1
printf '#!/bin/sh\nexit 0\n' > "$out"
/bin/chmod 700 "$out"
`
	scpScript := `#!/bin/sh
printf '%s\n' "$@" > "$CRABBOX_FAKE_SCP_LOG"
`
	sshScript := `#!/bin/sh
cmd=""
for arg do cmd="$arg"; done
printf '%s\n' "$cmd" >> "$CRABBOX_FAKE_SSH_LOG"
/bin/cat >/dev/null || true
`
	for name, script := range map[string]string{"go": goScript, "scp": scpScript, "ssh": sshScript} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	scpLog := filepath.Join(dir, "scp.log")
	sshLog := filepath.Join(dir, "ssh.log")
	t.Setenv("PATH", dir)
	t.Setenv("CRABBOX_FAKE_SCP_LOG", scpLog)
	t.Setenv("CRABBOX_FAKE_SSH_LOG", sshLog)
	target := SSHTarget{User: "crabbox", Host: "runner.example", Port: "22", TargetOS: targetLinux}
	if err := installRemoteEgressClient(context.Background(), target); err != nil {
		t.Fatal(err)
	}

	scpData, err := os.ReadFile(scpLog)
	if err != nil {
		t.Fatal(err)
	}
	var destination string
	for _, line := range strings.Split(strings.TrimSpace(string(scpData)), "\n") {
		if strings.HasPrefix(line, "crabbox@runner.example:") {
			destination = strings.TrimPrefix(line, "crabbox@runner.example:")
		}
	}
	if !strings.HasPrefix(destination, egressRemoteBinary+".tmp-") || len(strings.TrimPrefix(destination, egressRemoteBinary+".tmp-")) != 16 {
		t.Fatalf("scp destination is not a unique temporary path: %q", destination)
	}
	sshData, err := os.ReadFile(sshLog)
	if err != nil {
		t.Fatal(err)
	}
	promotion := string(sshData)
	for _, want := range []string{"chmod 700 " + shellQuote(destination), "mv -f " + shellQuote(destination) + " " + shellQuote(egressRemoteBinary)} {
		if !strings.Contains(promotion, want) {
			t.Fatalf("promotion command missing %q: %s", want, promotion)
		}
	}
	if strings.Contains(promotion, "rm -f") {
		t.Fatalf("successful promotion unexpectedly ran cleanup: %s", promotion)
	}
}
