package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"nhooyr.io/websocket"
)

const (
	vncLoopbackHost                     = "127.0.0.1"
	vncTunnelSSHConnectTimeout          = 10 * time.Second
	vncTunnelListenerVerificationWindow = 5 * time.Second
)

func vncTunnelReadinessTimeout() time.Duration {
	return vncTunnelSSHConnectTimeout + vncTunnelListenerVerificationWindow
}

func (a App) vnc(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("vnc", a.Stderr)
	provider := fs.String("provider", defaults.Provider, providerHelpSSH())
	id := fs.String("id", "", "lease id or slug")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	localPort := fs.String("local-port", "", "local VNC tunnel port")
	openClient := fs.Bool("open", false, "open the VNC client locally")
	nativeHandoff := fs.Bool("native-handoff", false, "emit a native-client JSON handoff and keep its tunnel in the foreground")
	nativeGrantURL := fs.String("native-grant-url", "", "coordinator URL for a one-time native VNC grant")
	nativeGrantStdin := fs.Bool("native-grant-stdin", false, "read a one-time native VNC grant from stdin")
	hostManaged := fs.Bool("host-managed", false, "allow opening host-managed static VNC")
	providerFlags := registerProviderFlags(fs, defaults)
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *nativeHandoff && *openClient {
		return exit(2, "--native-handoff and --open cannot be used together")
	}
	if (*nativeGrantURL != "" || *nativeGrantStdin) && (!*nativeHandoff || *nativeGrantURL == "" || !*nativeGrantStdin) {
		return exit(2, "--native-grant-url and --native-grant-stdin must be used together with --native-handoff")
	}
	setIDFromFirstArg(fs, id)
	if *nativeGrantURL != "" {
		return a.vncFromNativeGrant(ctx, *id, *nativeGrantURL, *localPort)
	}
	cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{LeaseID: *id, Desktop: true})
	if err != nil {
		return err
	}
	if err := applyProviderFlags(&cfg, fs, providerFlags); err != nil {
		return err
	}
	if isBlacksmithProvider(cfg.Provider) {
		return exit(2, "desktop/VNC is not supported for provider=%s; Blacksmith owns machine connectivity", cfg.Provider)
	}
	if err := requireLeaseID(*id, "crabbox vnc --id <lease-id-or-slug>", cfg); err != nil {
		return err
	}
	if *openClient && isStaticProvider(cfg.Provider) && !*hostManaged {
		return exit(2, "static %s VNC is an existing host, not a Crabbox-created box; rerun with --host-managed only if you want to open that host's OS login prompt", cfg.TargetOS)
	}
	server, target, leaseID, err := a.resolveNetworkLeaseTargetForRepo(ctx, cfg, *id, true, *reclaim)
	if err != nil {
		return err
	}
	if err := enforceManagedLeaseCapabilities(cfg, server, leaseID); err != nil {
		return err
	}
	if err := a.claimAndTouchLeaseTarget(ctx, cfg, server, target, leaseID, *reclaim); err != nil {
		return err
	}
	endpoint, err := resolveVNCEndpoint(ctx, cfg, &target)
	if err != nil {
		return err
	}
	password := ""
	if endpoint.Managed {
		password, _ = runSSHOutput(ctx, target, vncPasswordCommand(target))
	}
	if !isStaticProvider(cfg.Provider) && password == "" {
		password, _ = runSSHOutput(ctx, target, vncPasswordCommand(target))
	}
	if *nativeHandoff {
		if err := validateNativeVNCHandoffEndpoint(endpoint); err != nil {
			return err
		}
		username := ""
		if endpoint.Managed && (target.TargetOS == targetWindows || target.TargetOS == targetMacOS) {
			username = target.User
		}
		return runVNCNativeHandoff(ctx, a.Stdout, target, *localPort, endpoint, username, strings.TrimSpace(password))
	}
	if *localPort == "" {
		*localPort = availableLocalVNCPort()
	}
	tunnel := vncTunnelCommand(target, *localPort)
	staticHostVNC := isStaticProvider(cfg.Provider) && !endpoint.Managed
	if staticHostVNC {
		fmt.Fprintf(a.Stdout, "target: static-host slug=%s provider=%s os=%s host=%s\n", blank(serverSlug(server), "-"), blank(server.Provider, cfg.Provider), blank(target.TargetOS, cfg.TargetOS), target.Host)
	} else {
		fmt.Fprintf(a.Stdout, "lease: %s slug=%s provider=%s target=%s\n", leaseID, blank(serverSlug(server), "-"), blank(server.Provider, cfg.Provider), blank(target.TargetOS, cfg.TargetOS))
	}
	if staticHostVNC {
		fmt.Fprintln(a.Stdout, "managed: false")
		fmt.Fprintln(a.Stdout, "note: this is an existing host VNC service, not a Crabbox-created box")
	} else {
		fmt.Fprintln(a.Stdout, "managed: true")
	}
	if target.TargetOS == targetLinux {
		fmt.Fprintf(a.Stdout, "display: %s\n", desktopDisplay)
	}
	if endpoint.Direct {
		fmt.Fprintln(a.Stdout, "direct vnc:")
		fmt.Fprintf(a.Stdout, "  %s:%s\n", endpoint.Host, endpoint.Port)
		fmt.Fprintf(a.Stdout, "  vnc://%s:%s\n", endpoint.Host, endpoint.Port)
	} else {
		fmt.Fprintln(a.Stdout, "ssh tunnel:")
		fmt.Fprintf(a.Stdout, "  %s\n", tunnel)
	}
	fmt.Fprintln(a.Stdout, "vnc:")
	if endpoint.Direct {
		fmt.Fprintf(a.Stdout, "  %s:%s\n", endpoint.Host, endpoint.Port)
	} else {
		fmt.Fprintf(a.Stdout, "  %s:%s\n", vncLoopbackHost, *localPort)
	}
	if strings.TrimSpace(password) != "" {
		fmt.Fprintf(a.Stdout, "password: %s\n", strings.TrimSpace(password))
		if endpoint.Managed && target.TargetOS == targetWindows {
			fmt.Fprintf(a.Stdout, "windows username: %s\n", target.User)
			fmt.Fprintf(a.Stdout, "windows password: %s\n", strings.TrimSpace(password))
		}
		if endpoint.Managed && target.TargetOS == targetMacOS {
			fmt.Fprintf(a.Stdout, "macos username: %s\n", target.User)
			fmt.Fprintf(a.Stdout, "macos password: %s\n", strings.TrimSpace(password))
		}
	} else if staticHostVNC {
		fmt.Fprintln(a.Stdout, "credentials: host-managed")
		if target.TargetOS == targetMacOS {
			fmt.Fprintln(a.Stdout, "credential hint: use the macOS account or Screen Sharing password configured on that host")
		}
		if target.TargetOS == targetWindows {
			fmt.Fprintln(a.Stdout, "credential hint: use the Windows/VNC password configured on that host")
		}
	}
	if *openClient {
		if staticHostVNC {
			fmt.Fprintln(a.Stdout, "opening existing host VNC; expect that host's OS credential prompt")
		}
		url := fmt.Sprintf("vnc://%s:%s", endpoint.Host, endpoint.Port)
		if !endpoint.Direct {
			pid, err := startVNCTunnel(ctx, target, *localPort, endpoint.Host, endpoint.Port)
			if err != nil {
				return err
			}
			if pid > 0 {
				fmt.Fprintf(a.Stdout, "tunnel pid: %d\n", pid)
			} else {
				fmt.Fprintln(a.Stdout, "tunnel: started in background")
			}
			url = fmt.Sprintf("vnc://%s:%s", vncLoopbackHost, *localPort)
		}
		if err := openLocalURL(url); err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "opened: %s\n", url)
	}
	if endpoint.Direct {
		fmt.Fprintln(a.Stdout, "Connect directly to the printed VNC endpoint.")
	} else {
		fmt.Fprintln(a.Stdout, "Keep the tunnel process running while connected.")
	}
	return nil
}

func (a App) vncFromNativeGrant(ctx context.Context, expectedLeaseID, brokerURL, localPort string) error {
	parsed, err := url.Parse(strings.TrimSpace(brokerURL))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Scheme != "https" && !(parsed.Scheme == "http" && isNativeVNCLoopbackHost(parsed.Hostname()))) {
		return exit(2, "--native-grant-url must be an HTTPS coordinator URL or loopback HTTP URL")
	}
	ticketBytes, err := io.ReadAll(io.LimitReader(a.input(), 4097))
	if err != nil {
		return exit(2, "read native VNC grant: %v", err)
	}
	ticket := strings.TrimSuffix(string(ticketBytes), "\n")
	ticket = strings.TrimSuffix(ticket, "\r")
	if len(ticketBytes) > 4096 || !validNativeVNCTicket(ticket) {
		return exit(2, "native VNC grant is invalid")
	}
	parsed.Path = "/v1/native-vnc/handoff"
	parsed.RawPath = ""
	if parsed.Scheme == "https" {
		parsed.Scheme = "wss"
	} else {
		parsed.Scheme = "ws"
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+ticket)
	ws, response, err := websocket.Dial(ctx, parsed.String(), &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		if response != nil {
			return exit(5, "native VNC coordinator websocket: http %d", response.StatusCode)
		}
		return exit(5, "native VNC coordinator websocket: %v", err)
	}
	defer ws.Close(websocket.StatusNormalClosure, "native VNC closed")
	ws.SetReadLimit(1 << 20)
	messageType, payload, err := ws.Read(ctx)
	if err != nil || messageType != websocket.MessageText {
		return exit(5, "native VNC coordinator returned an invalid ready message")
	}
	var ready struct {
		Schema   string `json:"schema"`
		LeaseID  string `json:"leaseId"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal(payload, &ready); err != nil || ready.Schema != "crabbox/native-vnc-ready/v1" ||
		ready.LeaseID == "" || len(ready.LeaseID) > 256 || len(ready.Username) > 256 ||
		ready.Password == "" || len(ready.Password) > 256 ||
		strings.ContainsAny(ready.LeaseID+ready.Username+ready.Password, "\x00\r\n") {
		return exit(5, "native VNC coordinator returned an invalid ready message")
	}
	if expectedLeaseID != "" && ready.LeaseID != expectedLeaseID {
		return exit(5, "native VNC grant returned a different lease")
	}
	return runNativeVNCWebSocketHandoff(ctx, a.Stdout, ws, localPort, ready.Username, ready.Password)
}

func runNativeVNCWebSocketHandoff(
	ctx context.Context,
	stdout interface{ Write([]byte) (int, error) },
	ws *websocket.Conn,
	requestedPort, username, password string,
) error {
	address := net.JoinHostPort(vncLoopbackHost, requestedPort)
	if requestedPort == "" {
		address = net.JoinHostPort(vncLoopbackHost, "0")
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return exit(5, "reserve native VNC loopback port: %v", err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	handoff := vncNativeHandoff{
		Schema:   vncNativeHandoffSchema,
		Host:     vncLoopbackHost,
		Port:     port,
		Username: username,
		Password: password,
	}
	if err := json.NewEncoder(stdout).Encode(handoff); err != nil {
		return err
	}
	acceptDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
		case <-acceptDone:
		}
	}()
	tcp, err := listener.Accept()
	close(acceptDone)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return exit(5, "accept native VNC client: %v", err)
	}
	defer tcp.Close()
	if err := ws.Write(ctx, websocket.MessageText, []byte("start")); err != nil {
		return exit(5, "start native VNC coordinator tunnel: %v", err)
	}
	relayCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errors := make(chan error, 2)
	go func() { errors <- copyNativeVNCWebSocketToTCP(relayCtx, ws, tcp) }()
	go func() { errors <- copyTCPToWebSocket(relayCtx, ws, tcp) }()
	err = <-errors
	cancel()
	if err != nil && ctx.Err() == nil && !isExpectedNativeVNCRelayClose(err) {
		return exit(5, "native VNC tunnel: %v", err)
	}
	return nil
}

func validNativeVNCTicket(ticket string) bool {
	const prefix = "native_vnc_"
	if len(ticket) != len(prefix)+32 || !strings.HasPrefix(ticket, prefix) {
		return false
	}
	for _, character := range ticket[len(prefix):] {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}

func isNativeVNCLoopbackHost(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}

func copyNativeVNCWebSocketToTCP(ctx context.Context, ws *websocket.Conn, tcp net.Conn) error {
	for {
		messageType, payload, err := ws.Read(ctx)
		if err != nil {
			return err
		}
		if messageType != websocket.MessageBinary {
			return fmt.Errorf("coordinator sent a non-binary VNC frame")
		}
		if _, err := tcp.Write(payload); err != nil {
			return err
		}
	}
}

func isExpectedNativeVNCRelayClose(err error) bool {
	status := websocket.CloseStatus(err)
	return status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway
}

const vncNativeHandoffSchema = "crabbox/vnc-handoff/v1"

type vncNativeHandoff struct {
	Schema   string `json:"schema"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
}

func validateNativeVNCHandoffEndpoint(endpoint vncEndpoint) error {
	if !endpoint.Managed {
		return exit(2, "--native-handoff requires a Crabbox-managed desktop over a loopback SSH tunnel")
	}
	if endpoint.Direct {
		return exit(2, "--native-handoff requires a loopback SSH tunnel")
	}
	return nil
}

func runVNCNativeHandoff(
	ctx context.Context,
	stdout interface{ Write([]byte) (int, error) },
	target SSHTarget,
	requestedPort string,
	endpoint vncEndpoint,
	username, password string,
) error {
	tunnel, localPort, err := startVNCForegroundTunnelOnReservedPort(
		ctx,
		target,
		requestedPort,
		endpoint.Host,
		endpoint.Port,
	)
	if err != nil {
		return err
	}
	defer stopProcess(tunnel)
	port, err := strconv.Atoi(localPort)
	if err != nil || port < 1 || port > 65535 {
		return exit(5, "invalid reserved VNC tunnel port")
	}
	if len(username) > 256 || len(password) > 4096 {
		return exit(5, "native VNC credentials exceed the handoff limit")
	}
	handoff := vncNativeHandoff{
		Schema: vncNativeHandoffSchema, Host: vncLoopbackHost, Port: port,
		Username: username, Password: password,
	}
	if err := json.NewEncoder(stdout).Encode(handoff); err != nil {
		return fmt.Errorf("write native VNC handoff: %w", err)
	}
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-tunnel.Done():
		return tunnel.ExitError()
	}
}

func vncTunnelCommand(target SSHTarget, localPort string) string {
	return strings.Join(shellWords(append([]string{"ssh"}, vncTunnelArgs(target, localPort, "127.0.0.1", managedVNCPort)...)), " ")
}

func startVNCTunnel(ctx context.Context, target SSHTarget, localPort, remoteHost, remotePort string) (int, error) {
	cmd := exec.Command("ssh", vncTunnelArgs(target, localPort, remoteHost, remotePort)...)
	configureDaemonCommand(cmd)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	deadline := time.Now().Add(vncTunnelReadinessTimeout())
	var listenerErr error
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			_ = stopDaemonProcess(cmd.Process, cmd.Process.Pid)
			_ = cmd.Wait()
			return 0, context.Cause(ctx)
		}
		if ready, err := startedTunnelListenerReady(ctx, localPort, cmd.Process.Pid); ready {
			pid := cmd.Process.Pid
			if err := cmd.Process.Release(); err != nil {
				_ = stopDaemonProcess(cmd.Process, pid)
				_ = cmd.Wait()
				return 0, err
			}
			return pid, nil
		} else {
			listenerErr = err
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = stopDaemonProcess(cmd.Process, cmd.Process.Pid)
	_ = cmd.Wait()
	if text := strings.TrimSpace(output.String()); text != "" {
		return 0, exit(5, "start VNC SSH tunnel on 127.0.0.1:%s: %s", localPort, text)
	}
	if listenerErr != nil {
		return 0, exit(5, "verify VNC SSH tunnel listener on %s:%s: %v", vncLoopbackHost, localPort, listenerErr)
	}
	return 0, exit(5, "timed out starting VNC SSH tunnel on %s:%s", vncLoopbackHost, localPort)
}

func vncTunnelArgs(target SSHTarget, localPort, remoteHost, remotePort string) []string {
	args := append(sshForwardingDenyArgs(),
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile="+sshConfigFileValue(knownHostsFile(target)),
		"-o", "ConnectTimeout="+strconv.Itoa(int(vncTunnelSSHConnectTimeout/time.Second)),
		"-o", "ConnectionAttempts=1",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "GatewayPorts=no",
		"-o", "ControlMaster=no",
		"-o", "ControlPath=none",
		"-o", "ControlPersist=no",
		"-o", "ForkAfterAuthentication=no",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=2",
		"-p", target.Port,
	)
	if target.Key != "" {
		args = append([]string{"-i", target.Key, "-o", "IdentitiesOnly=yes"}, args...)
	}
	if target.ProxyCommand != "" {
		args = append(args, "-o", "ProxyCommand="+target.ProxyCommand)
	}
	args = append(args,
		"-N",
		"-L", fmt.Sprintf("%s:%s:%s:%s", vncLoopbackHost, localPort, remoteHost, remotePort),
		target.User+"@"+target.Host,
	)
	return args
}

func openLocalURL(url string) error {
	name, args := openURLCommand(url)
	if name == "" {
		return exit(2, "opening VNC URLs is not supported on this local OS")
	}
	return exec.Command(name, args...).Start()
}

func openURLCommand(url string) (string, []string) {
	switch runtime.GOOS {
	case "darwin":
		return "open", []string{url}
	case "windows":
		return "rundll32", []string{"url.dll,FileProtocolHandler", url}
	case "linux":
		return "xdg-open", []string{url}
	default:
		return "", nil
	}
}
