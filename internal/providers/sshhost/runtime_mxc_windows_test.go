//go:build windows

package sshhost

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestMXCEndpointCommandThroughCMD(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "space & 'quote’")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "endpoint.ps1")
	script := `[Console]::OutputEncoding = [Text.UTF8Encoding]::new()
$args | ConvertTo-Json -Compress
exit 7
`
	if err := os.WriteFile(target, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state & 'literal’.json")
	// Match the pinned MXC executor's cmd.exe /c wrapper, not just a decoder.
	out, err := exec.Command("cmd.exe", "/c", mxcEndpointCommand(target, state)).CombinedOutput()
	if err == nil || err.(*exec.ExitError).ExitCode() != 7 {
		t.Fatalf("exit: %v output: %s", err, out)
	}
	var args []string
	if err := json.Unmarshal(out, &args); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if len(args) != 4 || args[0] != "host" || args[1] != "endpoint" || args[2] != "--endpoint-state" || args[3] != state {
		t.Fatalf("arguments: %q", args)
	}
}
