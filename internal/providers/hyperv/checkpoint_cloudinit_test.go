package hyperv

import (
	"context"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestEnableCloudInitForCheckpointLiftsAndRestoresDisabledMarker(t *testing.T) {
	b := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{}})
	var scripts []string
	b.runSSHOutput = func(_ context.Context, _ core.SSHTarget, script string) (string, error) {
		scripts = append(scripts, script)
		if strings.Contains(script, "rm -f") {
			return "disabled\n", nil
		}
		return "", nil
	}
	wasDisabled, restore, err := b.enableCloudInitForCheckpoint(context.Background(), core.SSHTarget{})
	if err != nil || !wasDisabled || restore == nil {
		t.Fatalf("wasDisabled=%v restore=%v err=%v", wasDisabled, restore != nil, err)
	}
	if err := restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(scripts) != 2 || !strings.Contains(scripts[1], "touch /etc/cloud/cloud-init.disabled") {
		t.Fatalf("scripts=%q", scripts)
	}
}

func TestEnableCloudInitForCheckpointLeavesEnabledGuestAlone(t *testing.T) {
	b := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{}})
	b.runSSHOutput = func(context.Context, core.SSHTarget, string) (string, error) { return "", nil }
	wasDisabled, restore, err := b.enableCloudInitForCheckpoint(context.Background(), core.SSHTarget{})
	if err != nil || wasDisabled || restore != nil {
		t.Fatalf("wasDisabled=%v restore=%v err=%v", wasDisabled, restore != nil, err)
	}
}

func TestLinuxForkSpecializationReDisablesCloudInitOnlyWhenSourceHadItOff(t *testing.T) {
	on := linuxForkSpecializationUserData("crabbox", "ssh-ed25519 AAAA", "fork", "crabbox-fork-1", true)
	off := linuxForkSpecializationUserData("crabbox", "ssh-ed25519 AAAA", "fork", "crabbox-fork-1", false)
	idx := strings.Index(on, "touch /etc/cloud/cloud-init.disabled")
	if idx < 0 || idx > strings.Index(on, "systemctl poweroff") {
		t.Fatal("specialization must re-disable cloud-init before powering off")
	}
	if strings.Contains(off, "cloud-init.disabled") {
		t.Fatal("specialization re-disabled cloud-init for a source that had it enabled")
	}
}
