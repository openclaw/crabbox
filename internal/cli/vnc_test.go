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
	if !strings.Contains(got, "IdentitiesOnly=yes") {
		t.Fatalf("key-backed tunnel should restrict SSH identities: %q", got)
	}
	if !strings.Contains(got, "GatewayPorts=no") {
		t.Fatalf("tunnel should disable wildcard gateway binding: %q", got)
	}
	if !strings.Contains(got, "'-L' '127.0.0.1:5907:127.0.0.1:5900'") {
		t.Fatalf("tunnel should forward VNC loopback: %q", got)
	}
}

func TestVNCTunnelCommandForwardsProxyCommand(t *testing.T) {
	got := vncTunnelCommand(SSHTarget{
		Port:         "22",
		User:         "crabbox",
		Host:         "10.211.55.3",
		ProxyCommand: "ssh -W 10.211.55.3:%p mac-host",
	}, "5907")
	if strings.Contains(got, "'-i' ''") {
		t.Fatalf("empty key must not emit -i: %q", got)
	}
	if strings.Contains(got, "IdentitiesOnly=yes") {
		t.Fatalf("SSH-config-backed tunnel must allow agent identities: %q", got)
	}
	if !strings.Contains(got, "ProxyCommand=ssh -W 10.211.55.3:%p mac-host") {
		t.Fatalf("tunnel should preserve proxy command: %q", got)
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

func TestVNCPasswordCommandSupportsManagedTargets(t *testing.T) {
	windows := vncPasswordCommand(SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal})
	if !strings.Contains(windows, "EncodedCommand") {
		t.Fatalf("windows password command should be encoded PowerShell: %q", windows)
	}
	if got := vncPasswordCommand(SSHTarget{TargetOS: targetMacOS}); got != "sudo cat '/var/db/crabbox/vnc.password'" {
		t.Fatalf("mac password command=%q", got)
	}
}

func TestWindowsBrowserProbeScriptIsRawPowerShell(t *testing.T) {
	got := windowsBrowserProbeScript()
	for _, want := range []string{
		"Get-Command msedge.exe",
		`${Env:ProgramFiles(x86)}\Microsoft\Edge\Application\msedge.exe`,
		`Write-Output ("BROWSER=" + $path)`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("windows browser probe missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "EncodedCommand") {
		t.Fatalf("browser probe should be raw PowerShell before SSH wrapping:\n%s", got)
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
