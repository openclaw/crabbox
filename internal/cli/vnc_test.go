package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestVNCNativeHandoffJSONContract(t *testing.T) {
	var output bytes.Buffer
	handoff := vncNativeHandoff{
		Schema:   vncNativeHandoffSchema,
		Host:     vncLoopbackHost,
		Port:     5907,
		Username: "operator",
		Password: "private value",
	}
	if err := json.NewEncoder(&output).Encode(handoff); err != nil {
		t.Fatal(err)
	}
	if strings.Count(output.String(), "\n") != 1 {
		t.Fatalf("handoff must be exactly one JSON line: %q", output.String())
	}
	var decoded vncNativeHandoff
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded != handoff {
		t.Fatalf("decoded handoff=%#v want=%#v", decoded, handoff)
	}
}

func TestVNCNativeHandoffRejectsHostManagedEndpoints(t *testing.T) {
	for _, endpoint := range []vncEndpoint{
		{Host: "127.0.0.1", Port: managedVNCPort},
		{Direct: true, Host: "192.0.2.10", Port: managedVNCPort},
	} {
		if err := validateNativeVNCHandoffEndpoint(endpoint); err == nil || !strings.Contains(err.Error(), "Crabbox-managed") {
			t.Fatalf("endpoint=%#v error=%v, want Crabbox-managed rejection", endpoint, err)
		}
	}
	if err := validateNativeVNCHandoffEndpoint(vncEndpoint{
		Host: "127.0.0.1", Port: managedVNCPort, Managed: true,
	}); err != nil {
		t.Fatalf("managed loopback endpoint rejected: %v", err)
	}
}

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

func TestVNCTunnelDisablesSSHMultiplexing(t *testing.T) {
	args := strings.Join(vncTunnelArgs(SSHTarget{
		Port: "22", User: "crabbox", Host: "192.0.2.10",
	}, "5907", "127.0.0.1", "5900"), "\n")
	for _, want := range []string{"ExitOnForwardFailure=yes", "ControlMaster=no", "ControlPath=none", "ControlPersist=no", "ForkAfterAuthentication=no"} {
		if !strings.Contains(args, want) {
			t.Fatalf("dedicated tunnel missing %q: %s", want, args)
		}
	}
	for _, unwanted := range []string{"ControlMaster=auto", "ControlPersist=10m"} {
		if strings.Contains(args, unwanted) {
			t.Fatalf("dedicated tunnel inherited %q: %s", unwanted, args)
		}
	}
	if !strings.Contains(args, "127.0.0.1:5907:127.0.0.1:5900") {
		t.Fatalf("dedicated tunnel did not bind explicit IPv4 loopback: %s", args)
	}
}

func TestVNCTunnelReadinessCoversSSHConnectAndListenerVerification(t *testing.T) {
	want := vncTunnelSSHConnectTimeout + vncTunnelListenerVerificationWindow
	if got := vncTunnelReadinessTimeout(); got != want || got <= vncTunnelSSHConnectTimeout {
		t.Fatalf("readiness timeout=%s want=%s connect=%s", got, want, vncTunnelSSHConnectTimeout)
	}
	args := strings.Join(vncTunnelArgs(SSHTarget{Port: "22", User: "crabbox", Host: "192.0.2.10"}, "5907", "127.0.0.1", "5900"), " ")
	if !strings.Contains(args, "ConnectTimeout=10") {
		t.Fatalf("tunnel args do not share readiness connect timeout: %s", args)
	}
}

func TestStartVNCTunnelVerifiesOwnedLoopbackListener(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("listener ownership verification requires Linux or macOS")
	}
	dir := t.TempDir()
	sshPath := filepath.Join(dir, "ssh")
	script := `#!/bin/sh
forward=
while [ "$#" -gt 0 ]; do
  if [ "$1" = -L ]; then shift; forward=$1; break; fi
  shift
done
port=$(printf '%s' "$forward" | cut -d: -f2)
export CRABBOX_TEST_CONTROLLER_LISTENER_PORT="$port"
exec "$CRABBOX_TEST_BINARY" -test.run='^TestControllerOwnedListenerHelper$'
`
	if err := os.WriteFile(sshPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_TEST_BINARY", os.Args[0])
	port := availableControllerListenerTestPort(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pid, err := startVNCTunnel(ctx, SSHTarget{Port: "22", User: "crabbox", Host: "192.0.2.10"}, port, "127.0.0.1", "5900")
	if err != nil {
		t.Fatal(err)
	}
	if pid <= 0 {
		t.Fatal("verified tunnel did not return its owning pid")
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	defer stopDaemonProcess(process, pid)
	if err := controllerVerifyDaemonOwnedListener(port, pid); err != nil {
		t.Fatalf("returned tunnel does not own exact loopback listener: %v", err)
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
