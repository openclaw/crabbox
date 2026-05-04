package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"nhooyr.io/websocket"
)

func (a App) webvnc(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("webvnc", a.Stderr)
	provider := fs.String("provider", defaults.Provider, "provider: hetzner or aws")
	id := fs.String("id", "", "lease id or slug")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	localPort := fs.String("local-port", "", "local VNC tunnel port")
	openPortal := fs.Bool("open", false, "open the web portal VNC page")
	targetFlags := registerTargetFlags(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *id == "" && fs.NArg() > 0 {
		*id = fs.Arg(0)
	}
	if *id == "" {
		return exit(2, "usage: crabbox webvnc --id <lease-id-or-slug>")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	cfg.Provider = *provider
	cfg.Desktop = true
	if err := applyTargetFlagOverrides(&cfg, fs, targetFlags); err != nil {
		return err
	}
	if isBlacksmithProvider(cfg.Provider) || isStaticProvider(cfg.Provider) {
		return exit(2, "webvnc currently supports coordinator-backed hetzner/aws desktop leases")
	}
	coord, useCoordinator, err := newTargetCoordinatorClient(cfg)
	if err != nil {
		return err
	}
	if !useCoordinator || coord == nil || coord.Token == "" {
		return exit(2, "webvnc requires a configured coordinator login; run crabbox login first")
	}
	server, target, leaseID, err := a.resolveLeaseTarget(ctx, cfg, *id)
	if err != nil {
		return err
	}
	if err := enforceManagedLeaseCapabilities(cfg, server, leaseID); err != nil {
		return err
	}
	repo, err := findRepo()
	if err != nil {
		return err
	}
	if err := claimLeaseForRepoConfig(leaseID, serverSlug(server), cfg, repo.Root, cfg.IdleTimeout, *reclaim); err != nil {
		return err
	}
	a.touchActiveLeaseBestEffort(ctx, cfg, server, leaseID)
	endpoint, err := resolveVNCEndpoint(ctx, cfg, &target)
	if err != nil {
		return err
	}
	if *localPort == "" {
		*localPort = availableLocalVNCPort()
	}
	password := ""
	if endpoint.Managed {
		password, _ = runSSHOutput(ctx, target, vncPasswordCommand(target))
	}
	username := ""
	if endpoint.Managed && target.TargetOS == targetMacOS {
		username = target.User
	}

	connHost := endpoint.Host
	connPort := endpoint.Port
	var tunnel *exec.Cmd
	if !endpoint.Direct {
		tunnel, err = startVNCForegroundTunnel(ctx, target, *localPort, endpoint.Host, endpoint.Port)
		if err != nil {
			return err
		}
		defer stopProcess(tunnel)
		connHost = "127.0.0.1"
		connPort = *localPort
	}

	bridge, err := connectWebVNCBridge(ctx, coord, leaseID, connHost, connPort)
	if err != nil {
		return err
	}
	fmt.Fprintln(a.Stdout, "bridge: connected; keep this process running while using WebVNC")

	portal := webVNCPortalURL(coord.BaseURL, leaseID, username, password)
	fmt.Fprintf(a.Stdout, "webvnc: %s\n", portal)
	if strings.TrimSpace(password) != "" {
		fmt.Fprintf(a.Stdout, "password: %s\n", strings.TrimSpace(password))
		if strings.TrimSpace(username) != "" {
			fmt.Fprintf(a.Stdout, "username: %s\n", strings.TrimSpace(username))
		}
	}
	if *openPortal {
		if err := openLocalURL(portal); err != nil {
			bridge.Close()
			return err
		}
		fmt.Fprintf(a.Stdout, "opened: %s\n", portal)
	}
	return bridge.Serve(ctx)
}

func startVNCForegroundTunnel(ctx context.Context, target SSHTarget, localPort, remoteHost, remotePort string) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, "ssh", vncTunnelArgs(target, localPort, remoteHost, remotePort)...)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		_ = cmd.Wait()
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			stopProcess(cmd)
			return nil, context.Cause(ctx)
		}
		if tcpReachable(ctx, "127.0.0.1", localPort, 200*time.Millisecond) {
			return cmd, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	stopProcess(cmd)
	return nil, exit(5, "timed out starting VNC SSH tunnel on localhost:%s", localPort)
}

type webVNCBridge struct {
	tcp net.Conn
	ws  *websocket.Conn
}

func connectWebVNCBridge(ctx context.Context, coord *CoordinatorClient, leaseID, host, port string) (*webVNCBridge, error) {
	tcp, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return nil, err
	}
	ticket, err := coord.CreateWebVNCTicket(ctx, leaseID)
	if err != nil {
		_ = tcp.Close()
		return nil, err
	}
	ws, _, err := websocket.Dial(ctx, webVNCAgentURL(coord.BaseURL, leaseID, ticket.Ticket), &websocket.DialOptions{
		HTTPHeader: coord.webVNCAccessHeaders(),
	})
	if err != nil {
		_ = tcp.Close()
		return nil, err
	}
	return &webVNCBridge{tcp: tcp, ws: ws}, nil
}

func (b *webVNCBridge) Serve(ctx context.Context) error {
	defer b.Close()
	errc := make(chan error, 2)
	go func() { errc <- copyWebSocketToTCP(ctx, b.ws, b.tcp) }()
	go func() { errc <- copyTCPToWebSocket(ctx, b.ws, b.tcp) }()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case err := <-errc:
		if err == nil || strings.Contains(err.Error(), "status = StatusNormalClosure") {
			return nil
		}
		return err
	}
}

func (b *webVNCBridge) Close() {
	if b == nil {
		return
	}
	if b.ws != nil {
		_ = b.ws.Close(websocket.StatusNormalClosure, "bridge stopped")
	}
	if b.tcp != nil {
		_ = b.tcp.Close()
	}
}

func copyWebSocketToTCP(ctx context.Context, ws *websocket.Conn, tcp net.Conn) error {
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			return err
		}
		if _, err := tcp.Write(data); err != nil {
			return err
		}
	}
}

func copyTCPToWebSocket(ctx context.Context, ws *websocket.Conn, tcp net.Conn) error {
	buf := make([]byte, 32*1024)
	for {
		n, err := tcp.Read(buf)
		if n > 0 {
			if writeErr := ws.Write(ctx, websocket.MessageBinary, buf[:n]); writeErr != nil {
				return writeErr
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func (c *CoordinatorClient) webVNCAccessHeaders() http.Header {
	header := http.Header{}
	c.addAccessHeaders(header)
	return header
}

func webVNCAgentURL(base, leaseID, ticket string) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/v1/leases/" + url.PathEscape(leaseID) + "/webvnc/agent"
	values := url.Values{}
	values.Set("ticket", ticket)
	u.RawQuery = values.Encode()
	u.Fragment = ""
	return u.String()
}

func webVNCPortalURL(base, leaseID, username, password string) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/portal/leases/" + url.PathEscape(leaseID) + "/vnc"
	u.RawQuery = ""
	if strings.TrimSpace(username) != "" || strings.TrimSpace(password) != "" {
		values := url.Values{}
		if strings.TrimSpace(username) != "" {
			values.Set("username", strings.TrimSpace(username))
		}
		if strings.TrimSpace(password) != "" {
			values.Set("password", strings.TrimSpace(password))
		}
		u.Fragment = values.Encode()
	}
	return u.String()
}

func stopProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}
