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
	"slices"
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

func TestResolveNativeVNCCredentialsPrefersProviderCredentials(t *testing.T) {
	credentials, err := resolveNativeVNCCredentials(
		context.Background(),
		Config{Provider: "direct-webvnc-test"},
		SSHTarget{TargetOS: targetMacOS, User: "ssh-user"},
		vncEndpoint{Managed: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if credentials.Username != "provider-user" || credentials.Password != " provider-secret " {
		t.Fatalf("credentials=%#v", credentials)
	}
}

func TestWriteVNCCredentialsDoesNotPrintExternalOperatorPassword(t *testing.T) {
	const secret = "operator-screen-sharing-secret"
	for _, provider := range []string{"external", "exec-provider"} {
		t.Run(provider, func(t *testing.T) {
			var output bytes.Buffer
			writeVNCCredentials(
				&output,
				Config{Provider: provider, External: ExternalConfig{Connection: ExternalConnectionConfig{Desktop: ExternalDesktopConfig{
					Username: "screen-user", PasswordEnv: "SCREEN_SHARING_PASSWORD",
				}}}},
				SSHTarget{TargetOS: targetMacOS, User: "screen-user"},
				vncEndpoint{Managed: true},
				false,
				rfbCredentials{Username: "screen-user", Password: secret},
			)
			got := output.String()
			if strings.Contains(got, secret) {
				t.Fatalf("ordinary VNC output exposed the operator password: %q", got)
			}
			for _, want := range []string{
				"credentials: operator-managed",
				"macos username: screen-user",
				"password comes from environment variable SCREEN_SHARING_PASSWORD and is not printed",
			} {
				if !strings.Contains(got, want) {
					t.Fatalf("output missing %q: %q", want, got)
				}
			}
		})
	}
}

func TestOpenLocalURLScrubsExternalDesktopPassword(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell opener fixture")
	}
	name, _ := openURLCommand("https://example.test")
	if name == "" {
		t.Skip("local URL opening unsupported")
	}
	dir := t.TempDir()
	result := filepath.Join(dir, "result")
	home := filepath.Join(dir, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	opener := filepath.Join(dir, name)
	script := "#!/bin/sh\n" +
		"if [ \"${TEST_ARD_PASSWORD+x}\" = x ] || [ \"${CRABBOX_COORDINATOR_TOKEN+x}\" = x ] || [ \"${CRABBOX_COORDINATOR_ADMIN_TOKEN+x}\" = x ] || [ \"${CF_ACCESS_CLIENT_SECRET+x}\" = x ] || [ \"${GH_TOKEN+x}\" = x ] || [ \"${GITHUB_TOKEN+x}\" = x ] || [ \"${AWS_SECRET_ACCESS_KEY+x}\" = x ] || [ \"${CUSTOM_AMBIENT_SECRET+x}\" = x ]; then printf leaked > " + shellQuote(result) + "; exit 0; fi\n" +
		"if [ \"$HOME\" != " + shellQuote(home) + " ]; then printf stripped > " + shellQuote(result) + "; exit 0; fi\n" +
		"printf scrubbed > " + shellQuote(result) + "\n"
	if err := os.WriteFile(opener, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", home)
	t.Setenv("DISPLAY", ":99")
	t.Setenv("TEST_ARD_PASSWORD", "must-not-reach-browser")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "must-not-reach-browser")
	t.Setenv("CRABBOX_COORDINATOR_ADMIN_TOKEN", "must-not-reach-browser")
	t.Setenv("CF_ACCESS_CLIENT_SECRET", "must-not-reach-browser")
	t.Setenv("GH_TOKEN", "must-not-reach-browser")
	t.Setenv("GITHUB_TOKEN", "must-not-reach-browser")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "must-not-reach-browser")
	t.Setenv("CUSTOM_AMBIENT_SECRET", "must-not-reach-browser")
	if err := openLocalURLWithEnvironment("https://example.test", "TEST_ARD_PASSWORD"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(result); err == nil {
			if string(data) != "scrubbed" {
				t.Fatalf("opener environment=%q", data)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("fake opener did not report its environment")
}

func TestBrowserOpenerEnvironmentUsesMinimalAllowlist(t *testing.T) {
	environment := []string{
		"PATH=/usr/bin",
		"HOME=/tmp/home",
		"LANG=en_US.UTF-8",
		"LC_CTYPE=en_US.UTF-8",
		"DISPLAY=:99",
		"XAUTHORITY=/run/user/1000/xauthority",
		"WAYLAND_DISPLAY=wayland-0",
		"XDG_RUNTIME_DIR=/run/user/1000",
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus",
		"XDG_CONFIG_HOME=/tmp/config",
		"XDG_CONFIG_DIRS=/etc/xdg",
		"XDG_DATA_HOME=/tmp/data",
		"XDG_DATA_DIRS=/usr/local/share:/usr/share",
		"BROWSER=example-browser",
		"SSH_AUTH_SOCK=/tmp/agent.sock",
		"CRABBOX_COORDINATOR_TOKEN=coordinator-secret",
		"GH_TOKEN=github-secret",
		"LC_SECRET=locale-shaped-secret",
		"CUSTOM_AMBIENT_SECRET=custom-secret",
	}
	filtered := browserOpenerEnvironment(environment)
	got := make(map[string]string, len(filtered))
	for _, entry := range filtered {
		name, value, _ := strings.Cut(entry, "=")
		got[name] = value
	}
	for name, value := range map[string]string{
		"PATH":     "/usr/bin",
		"HOME":     "/tmp/home",
		"LANG":     "en_US.UTF-8",
		"LC_CTYPE": "en_US.UTF-8",
	} {
		if got[name] != value {
			t.Fatalf("%s=%q want %q", name, got[name], value)
		}
	}
	if runtime.GOOS == "linux" {
		for name, value := range map[string]string{
			"DISPLAY":                  ":99",
			"XAUTHORITY":               "/run/user/1000/xauthority",
			"WAYLAND_DISPLAY":          "wayland-0",
			"XDG_RUNTIME_DIR":          "/run/user/1000",
			"DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/1000/bus",
			"XDG_CONFIG_HOME":          "/tmp/config",
			"XDG_CONFIG_DIRS":          "/etc/xdg",
			"XDG_DATA_HOME":            "/tmp/data",
			"XDG_DATA_DIRS":            "/usr/local/share:/usr/share",
			"BROWSER":                  "example-browser",
		} {
			if got[name] != value {
				t.Fatalf("%s=%q want %q", name, got[name], value)
			}
		}
	} else {
		for _, name := range []string{"DISPLAY", "XDG_CONFIG_HOME", "XDG_CONFIG_DIRS", "XDG_DATA_HOME", "XDG_DATA_DIRS", "BROWSER"} {
			if _, ok := got[name]; ok {
				t.Fatalf("%s unexpectedly preserved on %s", name, runtime.GOOS)
			}
		}
	}
	for _, name := range []string{
		"SSH_AUTH_SOCK",
		"CRABBOX_COORDINATOR_TOKEN",
		"GH_TOKEN",
		"LC_SECRET",
		"CUSTOM_AMBIENT_SECRET",
	} {
		if _, ok := got[name]; ok {
			t.Fatalf("%s unexpectedly preserved", name)
		}
	}
	if denied := browserOpenerEnvironment(environment, "HOME"); slices.Contains(denied, "HOME=/tmp/home") {
		t.Fatal("explicit denylist did not override the browser allowlist")
	}
}

func TestWriteVNCCredentialsDoesNotApplyExternalMacHintToOtherTargets(t *testing.T) {
	for _, target := range []struct {
		name     string
		targetOS string
		want     []string
	}{
		{name: "Linux", targetOS: targetLinux, want: []string{"password: generated-secret"}},
		{name: "Windows", targetOS: targetWindows, want: []string{
			"password: generated-secret",
			"windows username: crabbox",
			"windows password: generated-secret",
		}},
	} {
		t.Run(target.name, func(t *testing.T) {
			var output bytes.Buffer
			writeVNCCredentials(
				&output,
				Config{Provider: "external", External: ExternalConfig{Connection: ExternalConnectionConfig{Desktop: ExternalDesktopConfig{
					PasswordEnv: "SCREEN_SHARING_PASSWORD",
				}}}},
				SSHTarget{TargetOS: target.targetOS, User: "crabbox"},
				vncEndpoint{Managed: true},
				false,
				rfbCredentials{Username: "crabbox", Password: "generated-secret"},
			)
			got := output.String()
			for _, want := range target.want {
				if !strings.Contains(got, want) {
					t.Fatalf("output missing %q: %q", want, got)
				}
			}
			for _, unwanted := range []string{"credentials: operator-managed", "SCREEN_SHARING_PASSWORD", "is not printed"} {
				if strings.Contains(got, unwanted) {
					t.Fatalf("output contains macOS-only credential hint %q: %q", unwanted, got)
				}
			}
		})
	}
}

func TestWriteVNCCredentialsPrintsManagedMacPasswordAfterProviderSwitch(t *testing.T) {
	var output bytes.Buffer
	cfg := Config{Provider: "aws"}
	cfg.External.Connection.Desktop.PasswordEnv = "SCREEN_SHARING_PASSWORD"
	cfg.credentialProvenance.externalDesktopEnv = credentialSourceTrustedFile
	writeVNCCredentials(
		&output,
		cfg,
		SSHTarget{TargetOS: targetMacOS, User: "ec2-user"},
		vncEndpoint{Managed: true},
		false,
		rfbCredentials{Username: "ec2-user", Password: "generated-secret"},
	)
	got := output.String()
	for _, want := range []string{"password: generated-secret", "macos username: ec2-user", "macos password: generated-secret"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q: %q", want, got)
		}
	}
	if strings.Contains(got, "credentials: operator-managed") || strings.Contains(got, "SCREEN_SHARING_PASSWORD") {
		t.Fatalf("provider-switched output used External credential hint: %q", got)
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
