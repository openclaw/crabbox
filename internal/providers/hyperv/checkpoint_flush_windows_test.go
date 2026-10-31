//go:build windows

package hyperv

import (
	"os/exec"
	"testing"
)

// The guest registry flush runs as Windows PowerShell inside the guest; a broken
// P/Invoke declaration or handle value would make every checkpoint create fail.
func TestGuestRegistryFlushScriptFlushesHivesInWindowsPowerShell(t *testing.T) {
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("powershell.exe unavailable")
	}
	output, err := exec.Command(powershell, "-NoProfile", "-NonInteractive", "-Command", guestRegistryFlushScript+"; 'flushed'").CombinedOutput()
	if err != nil || string(output) != "flushed\r\n" {
		t.Fatalf("registry flush script failed (err=%v): %q", err, output)
	}
}
