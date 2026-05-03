package cli

import (
	"strings"
	"testing"
)

func TestVNCTunnelCommandQuotesKeyPath(t *testing.T) {
	got := vncTunnelCommand(SSHTarget{
		Key:  "/tmp/Application Support/crabbox/id_ed25519",
		Port: "2222",
		User: "crabbox",
		Host: "203.0.113.10",
	}, "5907")
	if !strings.Contains(got, "'-i' '/tmp/Application Support/crabbox/id_ed25519'") {
		t.Fatalf("tunnel key path should be shell-quoted: %q", got)
	}
	if !strings.Contains(got, "'-L' '5907:127.0.0.1:5900'") {
		t.Fatalf("tunnel should forward VNC loopback: %q", got)
	}
}

func TestVNCLoopbackCheckCommandSupportsWindows(t *testing.T) {
	got := vncLoopbackCheckCommand(SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal})
	if !strings.Contains(got, "powershell.exe") {
		t.Fatalf("windows VNC check should use PowerShell: %q", got)
	}
	if !strings.Contains(got, "EncodedCommand") {
		t.Fatalf("windows VNC check should be encoded for OpenSSH: %q", got)
	}
}

func TestOpenURLCommandIncludesURL(t *testing.T) {
	name, args := openURLCommand("vnc://localhost:5901")
	if name == "" {
		t.Skip("current OS has no URL opener")
	}
	if len(args) == 0 || args[len(args)-1] != "vnc://localhost:5901" {
		t.Fatalf("openURLCommand args=%#v should include URL", args)
	}
}
