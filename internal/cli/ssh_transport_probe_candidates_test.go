package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// recordingSSHScript is a fake ssh that records the Port of each private transport
// config it is handed, then exits with the code for that port (0 when unlisted).
func recordingSSHScript(t *testing.T, exitCodes map[string]int) (attempts string) {
	t.Helper()
	dir := t.TempDir()
	attempts = filepath.Join(dir, "attempts")
	var cases strings.Builder
	for port, code := range exitCodes {
		cases.WriteString("  " + port + ") exit " + strconv.Itoa(code) + " ;;\n")
	}
	script := `#!/bin/sh
config=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-F" ]; then config="$2"; shift 2; else shift; fi
done
port=$(/usr/bin/awk '$1 == "Port" {gsub(/"/, "", $2); print $2; exit}' "$config")
printf '%s\n' "$port" >> "$CRABBOX_TEST_SSH_ATTEMPTS"
case "$port" in
` + cases.String() + `esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CRABBOX_TEST_SSH_ATTEMPTS", attempts)
	return attempts
}

// A candidate whose private transport session cannot be created must not end the
// probe: every candidate is tried, and each one's failure is reported. Session
// creation is made to fail by pointing TMPDIR at a missing directory (the session
// lives in a private temp directory), before any connection is attempted.
func TestProbeSSHTransportLeaseAfterClaimTriesEveryCandidatePastSessionErrors(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX fake SSH helper")
	}
	attempts := recordingSSHScript(t, nil)
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	stderr := &bytes.Buffer{}
	app := App{Stderr: stderr}
	lease := LeaseTarget{LeaseID: "cbx_probe_sessions", SSH: SSHTarget{User: "alice", Host: "example.test", Port: "2201", FallbackPorts: []string{"2202"}}}
	if err := app.probeSSHTransportLeaseAfterClaim(t.Context(), baseConfig(), &lease, false); err != nil {
		t.Fatal(err)
	}
	got := stderr.String()
	if !strings.Contains(got, `port "2201"`) || !strings.Contains(got, `port "2202"`) || !strings.Contains(got, "create private SSH transport directory") {
		t.Fatalf("not every candidate was tried past a session error, or it was not reported: stderr=%q", got)
	}
	if data, err := os.ReadFile(attempts); err == nil && len(data) != 0 {
		t.Fatalf("ssh ran although no session could be created: attempts=%q", data)
	}
}

// When no candidate answers, the post-claim probe reports it instead of silently
// keeping the configured port.
func TestProbeSSHTransportLeaseAfterClaimReportsUnreachableEndpoint(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX fake SSH helper")
	}
	recordingSSHScript(t, map[string]int{"2201": 255, "2202": 255})
	stderr := &bytes.Buffer{}
	app := App{Stderr: stderr}
	lease := LeaseTarget{LeaseID: "cbx_probe_report", SSH: SSHTarget{User: "alice", Host: "example.test", Port: "2201", FallbackPorts: []string{"2202"}}}
	if err := app.probeSSHTransportLeaseAfterClaim(t.Context(), baseConfig(), &lease, false); err != nil {
		t.Fatal(err)
	}
	if lease.SSH.Port != "2201" {
		t.Fatalf("port=%q; an unreachable endpoint keeps the configured port", lease.SSH.Port)
	}
	got := stderr.String()
	if !strings.Contains(got, "2201") || !strings.Contains(got, "2202") || !strings.Contains(got, "no candidate answered") {
		t.Fatalf("unreachable endpoint was not reported: stderr=%q", got)
	}
}
