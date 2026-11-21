package ssh

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestStaticSSHSharedHostRecoveryCommand(t *testing.T) {
	f := newStaticCommandFixture(t, core.TargetMacOS)
	f.cfg.Static.Host = "buildbox.local"
	f.cfg.Static.Name = ""
	f.cfg.Static.ID = "slot-1"
	t.Setenv("CRABBOX_STATIC_NAME", "")
	t.Setenv("CRABBOX_STATIC_ID", f.cfg.Static.ID)
	f.acquire(t)
	other := f
	other.cfg.Static.ID = "slot-2"
	other.acquire(t)

	out := f.run(t, "run", "--id", "slot-1", "--no-sync", "--no-hydrate",
		"--timing-json", "--timing-record", "off", "--", "printf", "shared-host-fixture")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var report core.TimingReport
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &report); err != nil {
		t.Fatalf("decode timing report: %v\n%s", err, out)
	}
	if !strings.HasSuffix(report.StopCommand, " --id slot-1") {
		t.Fatalf("recovery must identify the slot, not its shared host: %s", report.StopCommand)
	}
	var recovery string
	for _, line := range lines {
		if strings.HasPrefix(line, "lease recovery ") {
			recovery = line
		}
	}
	if !strings.Contains(recovery, "stop_command="+strconv.Quote(report.StopCommand)) {
		t.Fatalf("recovery line and timing report disagree: %s", out)
	}
	// All fixture arguments are single words. Exercise the emitted CLI command,
	// not just its formatting, with both claims still present.
	var stdout, stderr bytes.Buffer
	args := strings.Fields(report.StopCommand)[1:]
	if err := (core.App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), args); err != nil {
		t.Fatalf("printed recovery command failed: %v\n%s", err, &stderr)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence("slot-1"); err != nil || exists {
		t.Fatalf("recovered slot still present: exists=%t err=%v", exists, err)
	}
	readStaticCommandClaim(t, "slot-2")
}
