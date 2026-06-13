//go:build linux

package cli

import (
	"os/exec"
	"syscall"
	"testing"
)

func TestControllerCommandUsesParentDeathSignal(t *testing.T) {
	cmd := exec.Command("true")
	configureControllerCommand(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid || cmd.SysProcAttr.Pdeathsig != syscall.SIGKILL {
		t.Fatalf("controller child missing process-group/parent-death ownership: %#v", cmd.SysProcAttr)
	}
}
