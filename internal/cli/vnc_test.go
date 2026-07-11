package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"
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

func TestVNCNativeGrantRelaysCoordinatorWebSocketToLoopback(t *testing.T) {
	const ticket = "native_vnc_0123456789abcdef0123456789abcdef"
	serverErr := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/native-vnc/handoff" || r.Header.Get("Authorization") != "Bearer "+ticket {
			serverErr <- fmt.Errorf("request path=%q authorization=%q", r.URL.Path, r.Header.Get("Authorization"))
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			serverErr <- err
			return
		}
		defer ws.Close(websocket.StatusNormalClosure, "done")
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		ready := `{"schema":"crabbox/native-vnc-ready/v1","leaseId":"cbx_native123","username":"dev","password":"private"}`
		if err := ws.Write(ctx, websocket.MessageText, []byte(ready)); err != nil {
			serverErr <- err
			return
		}
		typ, start, err := ws.Read(ctx)
		if err != nil || typ != websocket.MessageText || string(start) != "start" {
			serverErr <- fmt.Errorf("start type=%v value=%q error=%v", typ, start, err)
			return
		}
		if err := ws.Write(ctx, websocket.MessageBinary, []byte("RFB 003.008\n")); err != nil {
			serverErr <- err
			return
		}
		typ, client, err := ws.Read(ctx)
		if err != nil || typ != websocket.MessageBinary || string(client) != "client-vnc" {
			serverErr <- fmt.Errorf("client type=%v value=%q error=%v", typ, client, err)
			return
		}
		serverErr <- nil
	}))
	defer server.Close()

	stdoutReader, stdoutWriter := io.Pipe()
	app := App{Stdout: stdoutWriter, Stderr: io.Discard, Stdin: strings.NewReader(ticket + "\n")}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- app.vncFromNativeGrant(ctx, "cbx_native123", server.URL, "")
		_ = stdoutWriter.Close()
	}()
	line, err := bufio.NewReader(stdoutReader).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var handoff vncNativeHandoff
	if err := json.Unmarshal(line, &handoff); err != nil {
		t.Fatal(err)
	}
	if handoff.Host != vncLoopbackHost || handoff.Username != "dev" || handoff.Password != "private" {
		t.Fatalf("handoff=%#v", handoff)
	}
	local, err := net.DialTimeout("tcp", net.JoinHostPort(handoff.Host, strconv.Itoa(handoff.Port)), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	buffer := make([]byte, len("RFB 003.008\n"))
	if _, err := io.ReadFull(local, buffer); err != nil {
		t.Fatal(err)
	}
	if string(buffer) != "RFB 003.008\n" {
		t.Fatalf("server VNC bytes=%q", buffer)
	}
	if _, err := local.Write([]byte("client-vnc")); err != nil {
		t.Fatal(err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
	_ = local.Close()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestValidNativeVNCTicket(t *testing.T) {
	for _, ticket := range []string{
		"native_vnc_0123456789abcdef0123456789abcdef",
	} {
		if !validNativeVNCTicket(ticket) {
			t.Fatalf("valid ticket rejected: %q", ticket)
		}
	}
	for _, ticket := range []string{
		"native_vnc_0123456789abcdef",
		"native_vnc_0123456789ABCDEF0123456789ABCDEF",
		"native_vnc_0123456789abcdef0123456789abcdeg",
		" native_vnc_0123456789abcdef0123456789abcdef",
	} {
		if validNativeVNCTicket(ticket) {
			t.Fatalf("invalid ticket accepted: %q", ticket)
		}
	}
}

func TestNativeVNCLoopbackHost(t *testing.T) {
	for _, host := range []string{"localhost", "LOCALHOST", "127.0.0.1", "::1"} {
		if !isNativeVNCLoopbackHost(host) {
			t.Fatalf("loopback host rejected: %q", host)
		}
	}
	for _, host := range []string{"", "0.0.0.0", "127.0.0.2", "localhost.example.com"} {
		if isNativeVNCLoopbackHost(host) {
			t.Fatalf("non-loopback host accepted: %q", host)
		}
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
	for _, want := range []string{"ForwardAgent=no", "ForwardX11=no", "ForwardX11Trusted=no", "ExitOnForwardFailure=yes", "ControlMaster=no", "ControlPath=none", "ControlPersist=no", "ForkAfterAuthentication=no"} {
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
