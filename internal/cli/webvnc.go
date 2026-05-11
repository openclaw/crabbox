package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"nhooyr.io/websocket"
)

func (a App) webvnc(ctx context.Context, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "status":
			return a.webVNCStatusCommand(ctx, args[1:])
		case "reset":
			return a.webVNCResetCommand(ctx, args[1:])
		case "daemon":
			return a.webVNCDaemonCommand(ctx, args[1:])
		}
	}
	defaults := defaultConfig()
	fs := newFlagSet("webvnc", a.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage:")
		fmt.Fprintln(fs.Output(), "  crabbox webvnc --id <lease-id-or-slug> [--open]")
		fmt.Fprintln(fs.Output(), "  crabbox webvnc status --id <lease-id-or-slug>")
		fmt.Fprintln(fs.Output(), "  crabbox webvnc reset --id <lease-id-or-slug> [--open]")
		fmt.Fprintln(fs.Output(), "  crabbox webvnc daemon start|status|stop --id <lease-id-or-slug>")
		fmt.Fprintln(fs.Output(), "")
		fmt.Fprintln(fs.Output(), "Bridge flags:")
		fmt.Fprintln(fs.Output(), "  --id <lease-id-or-slug>")
		fmt.Fprintln(fs.Output(), "  --provider hetzner|aws|azure")
		fmt.Fprintln(fs.Output(), "  --target linux|macos|windows")
		fmt.Fprintln(fs.Output(), "  --windows-mode normal|wsl2")
		fmt.Fprintln(fs.Output(), "  --static-host <host>")
		fmt.Fprintln(fs.Output(), "  --static-user <user>")
		fmt.Fprintln(fs.Output(), "  --static-port <port>")
		fmt.Fprintln(fs.Output(), "  --static-work-root <path>")
		fmt.Fprintln(fs.Output(), "  --network auto|tailscale|public")
		fmt.Fprintln(fs.Output(), "  --local-port <port>")
		fmt.Fprintln(fs.Output(), "  --open")
		fmt.Fprintln(fs.Output(), "  --reclaim")
	}
	provider := fs.String("provider", defaults.Provider, "provider: hetzner, aws, or azure")
	id := fs.String("id", "", "lease id or slug")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	localPort := fs.String("local-port", "", "local VNC tunnel port")
	openPortal := fs.Bool("open", false, "open the web portal VNC page")
	daemon := fs.Bool("daemon", false, "compatibility alias for daemon start")
	background := fs.Bool("background", false, "compatibility alias for daemon start")
	daemonStatus := fs.Bool("status", false, "compatibility alias for daemon status")
	stopDaemon := fs.Bool("stop", false, "compatibility alias for daemon stop")
	networkFlags := registerNetworkModeFlag(fs, defaults)
	targetFlags := registerTargetFlags(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	setIDFromFirstArg(fs, id)
	if *id == "" {
		return exit(2, "usage: crabbox webvnc --id <lease-id-or-slug>")
	}
	if *daemonStatus {
		return a.webVNCDaemonStatus(*id)
	}
	if *stopDaemon {
		return a.stopWebVNCDaemon(*id)
	}
	if *daemon || *background {
		return a.webVNCDaemonStart(ctx, stripLegacyWebVNCDaemonFlags(args))
	}
	cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{Desktop: true})
	if err != nil {
		return err
	}
	if isBlacksmithProvider(cfg.Provider) || isStaticProvider(cfg.Provider) {
		return exit(2, "webvnc currently supports coordinator-backed hetzner/aws/azure desktop leases")
	}
	coord, useCoordinator, err := newTargetCoordinatorClient(cfg)
	if err != nil {
		return err
	}
	if !useCoordinator || coord == nil || coord.Token == "" {
		return exit(2, "webvnc requires a configured coordinator login; run crabbox login first")
	}
	server, target, leaseID, err := a.resolveNetworkLeaseTarget(ctx, cfg, *id, false)
	if err != nil {
		return err
	}
	if err := enforceManagedLeaseCapabilities(cfg, server, leaseID); err != nil {
		return err
	}
	if err := a.claimAndTouchLeaseTarget(ctx, cfg, server, leaseID, *reclaim); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "lease: %s slug=%s provider=%s target=%s\n", leaseID, blank(serverSlug(server), "-"), blank(server.Provider, cfg.Provider), blank(target.TargetOS, cfg.TargetOS))
	fmt.Fprintln(a.Stdout, "bridge: probing VNC on target loopback 127.0.0.1:5900 over SSH")
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
		fmt.Fprintf(a.Stdout, "bridge: starting SSH tunnel localhost:%s -> %s:%s\n", *localPort, endpoint.Host, endpoint.Port)
		tunnel, err = startVNCForegroundTunnel(ctx, target, *localPort, endpoint.Host, endpoint.Port)
		if err != nil {
			return err
		}
		defer stopProcess(tunnel)
		connHost = "127.0.0.1"
		connPort = *localPort
	}

	portal := webVNCPortalURL(coord.BaseURL, leaseID, username, password)
	rescueCtx := rescueContext{Cfg: cfg, Target: target, LeaseID: leaseID}
	opened := false
	return serveWebVNCBridgePool(ctx, webVNCBridgePoolConfig{
		Coord:     coord,
		LeaseID:   leaseID,
		Host:      connHost,
		Port:      connPort,
		PoolSize:  defaultWebVNCBridgePoolSize,
		RescueCtx: rescueCtx,
		NativeVNC: nativeVNCOpenCommand(cfg, target, leaseID),
		Log:       a.Stdout,
		OnReady: func() error {
			fmt.Fprintln(a.Stdout, "bridge: connected; keep this process running while using WebVNC")
			fmt.Fprintf(a.Stdout, "webvnc: %s\n", portal)
			if strings.TrimSpace(password) != "" {
				fmt.Fprintf(a.Stdout, "password: %s\n", strings.TrimSpace(password))
				if strings.TrimSpace(username) != "" {
					fmt.Fprintf(a.Stdout, "username: %s\n", strings.TrimSpace(username))
				}
			}
			if *openPortal && !opened {
				if err := openLocalURL(portal); err != nil {
					return err
				}
				opened = true
				fmt.Fprintf(a.Stdout, "opened: %s\n", portal)
			}
			return nil
		},
	})
}

const defaultWebVNCBridgePoolSize = 4

type webVNCBridgePoolConfig struct {
	Coord     *CoordinatorClient
	LeaseID   string
	Host      string
	Port      string
	PoolSize  int
	RescueCtx rescueContext
	NativeVNC string
	Log       io.Writer
	OnReady   func() error
}

type webVNCBridgePoolEvent struct {
	Kind    string
	Slot    int
	Attempt int
	Err     error
}

func serveWebVNCBridgePool(ctx context.Context, cfg webVNCBridgePoolConfig) error {
	if cfg.PoolSize < 1 {
		cfg.PoolSize = 1
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	events := make(chan webVNCBridgePoolEvent, cfg.PoolSize)
	for slot := 0; slot < cfg.PoolSize; slot++ {
		go serveWebVNCBridgeSlot(ctx, cfg, slot, events)
	}
	ready := false
	initialFailures := make(map[int]bool)
	var firstErr error
	for {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case event := <-events:
			switch event.Kind {
			case "ready":
				if !ready {
					ready = true
					if cfg.OnReady != nil {
						if err := cfg.OnReady(); err != nil {
							return err
						}
					}
				}
			case "initial-error":
				if !ready {
					initialFailures[event.Slot] = true
					if firstErr == nil {
						firstErr = event.Err
					}
					if len(initialFailures) >= cfg.PoolSize {
						printRescueWithFallback(cfg.Log, rescueVNCBridgeDisconnected, firstErr.Error(), cfg.NativeVNC, webVNCStatusRescueCommand(cfg.RescueCtx), webVNCResetRescueCommand(cfg.RescueCtx))
						return firstErr
					}
				}
			case "retry":
				if ready && event.Err != nil {
					printRescueWithFallback(cfg.Log, classifyWebVNCBridgeProblem(event.Err), event.Err.Error(), cfg.NativeVNC, webVNCStatusRescueCommand(cfg.RescueCtx), webVNCResetRescueCommand(cfg.RescueCtx))
					fmt.Fprintf(cfg.Log, "bridge[%d]: reconnecting in %s\n", event.Slot+1, webVNCReconnectDelay(event.Attempt))
				}
			case "fatal":
				if event.Err != nil {
					return event.Err
				}
			}
		}
	}
}

func serveWebVNCBridgeSlot(ctx context.Context, cfg webVNCBridgePoolConfig, slot int, events chan<- webVNCBridgePoolEvent) {
	connectedOnce := false
	attempt := 0
	for {
		bridge, err := connectWebVNCBridge(ctx, cfg.Coord, cfg.LeaseID, cfg.Host, cfg.Port)
		if err != nil {
			attempt, kind := nextWebVNCBridgeFailure(connectedOnce, attempt)
			events <- webVNCBridgePoolEvent{Kind: kind, Slot: slot, Attempt: attempt, Err: err}
			if err := waitWebVNCReconnect(ctx, webVNCReconnectDelay(attempt)); err != nil {
				events <- webVNCBridgePoolEvent{Kind: "fatal", Slot: slot, Err: err}
				return
			}
			continue
		}
		connectedOnce = true
		attempt = 0
		events <- webVNCBridgePoolEvent{Kind: "ready", Slot: slot}
		err = bridge.Serve(ctx)
		if !retryableWebVNCBridgeError(err) {
			events <- webVNCBridgePoolEvent{Kind: "fatal", Slot: slot, Err: err}
			return
		}
		attempt++
		events <- webVNCBridgePoolEvent{Kind: "retry", Slot: slot, Attempt: attempt, Err: err}
		if err := waitWebVNCReconnect(ctx, webVNCReconnectDelay(attempt)); err != nil {
			events <- webVNCBridgePoolEvent{Kind: "fatal", Slot: slot, Err: err}
			return
		}
	}
}

func nextWebVNCBridgeFailure(connectedOnce bool, attempt int) (int, string) {
	attempt++
	if !connectedOnce && attempt == 1 {
		return attempt, "initial-error"
	}
	return attempt, "retry"
}

func (a App) webVNCDaemonCommand(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return exit(2, "usage: crabbox webvnc daemon start|status|stop --id <lease-id-or-slug>")
	}
	if isHelpArg(args[0]) {
		fmt.Fprintln(a.Stdout, "Usage: crabbox webvnc daemon start|status|stop --id <lease-id-or-slug>")
		return nil
	}
	switch args[0] {
	case "start":
		return a.webVNCDaemonStart(ctx, args[1:])
	case "status":
		return a.webVNCDaemonStatusCommand(args[1:])
	case "stop":
		return a.webVNCDaemonStopCommand(args[1:])
	default:
		return exit(2, "usage: crabbox webvnc daemon start|status|stop --id <lease-id-or-slug>")
	}
}

func (a App) webVNCDaemonStart(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("webvnc daemon start", a.Stderr)
	provider := fs.String("provider", defaults.Provider, "provider: hetzner, aws, or azure")
	id := fs.String("id", "", "lease id or slug")
	localPort := fs.String("local-port", "", "local VNC tunnel port")
	openPortal := fs.Bool("open", false, "open the web portal VNC page")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	setIDFromFirstArg(fs, id)
	if *id == "" {
		return exit(2, "usage: crabbox webvnc daemon start --id <lease-id-or-slug>")
	}
	cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{Desktop: true})
	if err != nil {
		return err
	}
	target := SSHTarget{TargetOS: cfg.TargetOS, WindowsMode: cfg.WindowsMode}
	bridgeID := *id
	if !isBlacksmithProvider(cfg.Provider) && !isStaticProvider(cfg.Provider) {
		coord, useCoordinator, err := newTargetCoordinatorClient(cfg)
		if err != nil {
			return err
		}
		if useCoordinator && coord != nil && coord.Token != "" {
			server, resolvedTarget, leaseID, err := a.resolveNetworkLeaseTarget(ctx, cfg, *id, false)
			if err != nil {
				return err
			}
			if err := enforceManagedLeaseCapabilities(cfg, server, leaseID); err != nil {
				return err
			}
			if server.Provider != "" {
				cfg.Provider = server.Provider
			}
			if resolvedTarget.TargetOS != "" {
				cfg.TargetOS = resolvedTarget.TargetOS
			}
			if resolvedTarget.WindowsMode != "" {
				cfg.WindowsMode = resolvedTarget.WindowsMode
			}
			target = resolvedTarget
			bridgeID = leaseID
		}
	}
	daemonArgs := webVNCBridgeArgs(cfg, target, bridgeID, *openPortal)
	if strings.TrimSpace(*localPort) != "" {
		daemonArgs = append(daemonArgs, "--local-port", strings.TrimSpace(*localPort))
	}
	if *reclaim {
		daemonArgs = append(daemonArgs, "--reclaim")
	}
	return a.startWebVNCDaemon(daemonArgs, *id)
}

func (a App) webVNCDaemonStatusCommand(args []string) error {
	fs := newFlagSet("webvnc daemon status", a.Stderr)
	id := fs.String("id", "", "lease id or slug")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	setIDFromFirstArg(fs, id)
	if *id == "" {
		return exit(2, "usage: crabbox webvnc daemon status --id <lease-id-or-slug>")
	}
	return a.webVNCDaemonStatus(*id)
}

func (a App) webVNCDaemonStopCommand(args []string) error {
	fs := newFlagSet("webvnc daemon stop", a.Stderr)
	id := fs.String("id", "", "lease id or slug")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	setIDFromFirstArg(fs, id)
	if *id == "" {
		return exit(2, "usage: crabbox webvnc daemon stop --id <lease-id-or-slug>")
	}
	return a.stopWebVNCDaemon(*id)
}

func (a App) webVNCStatusCommand(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("webvnc status", a.Stderr)
	provider := fs.String("provider", defaults.Provider, "provider: hetzner, aws, or azure")
	id := fs.String("id", "", "lease id or slug")
	localPort := fs.String("local-port", "", "local VNC tunnel port")
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	setIDFromFirstArg(fs, id)
	if *id == "" {
		return exit(2, "usage: crabbox webvnc status --id <lease-id-or-slug>")
	}
	cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{Desktop: true})
	if err != nil {
		return err
	}
	if isBlacksmithProvider(cfg.Provider) || isStaticProvider(cfg.Provider) {
		return exit(2, "webvnc status currently supports coordinator-backed hetzner/aws/azure desktop leases")
	}
	coord, useCoordinator, err := newTargetCoordinatorClient(cfg)
	if err != nil {
		return err
	}
	if !useCoordinator || coord == nil || coord.Token == "" {
		return exit(2, "webvnc status requires a configured coordinator login; run crabbox login first")
	}
	server, target, leaseID, err := a.resolveNetworkLeaseTarget(ctx, cfg, *id, false)
	if err != nil {
		return err
	}
	if err := enforceManagedLeaseCapabilities(cfg, server, leaseID); err != nil {
		return err
	}
	if *localPort == "" {
		*localPort = availableLocalVNCPort()
	}
	endpoint, endpointErr := resolveVNCEndpoint(ctx, cfg, &target)
	password := ""
	username := ""
	if endpointErr == nil && endpoint.Managed {
		password, _ = runSSHOutput(ctx, target, vncPasswordCommand(target))
		if target.TargetOS == targetMacOS {
			username = target.User
		}
	}
	status, statusErr := coord.WebVNCStatus(ctx, leaseID)
	daemon, daemonErr := localWebVNCDaemonStatus(leaseID)
	if daemonErr == nil && leaseID != *id {
		if aliasDaemon, err := localWebVNCDaemonStatus(*id); err == nil && !aliasDaemon.Missing {
			daemon = aliasDaemon
		}
	}
	fmt.Fprintf(a.Stdout, "lease: %s slug=%s provider=%s target=%s\n", leaseID, blank(serverSlug(server), "-"), blank(server.Provider, cfg.Provider), blank(target.TargetOS, cfg.TargetOS))
	rescueCtx := rescueContext{Cfg: cfg, Target: target, LeaseID: leaseID}
	if daemonErr != nil {
		fmt.Fprintf(a.Stdout, "webvnc daemon: error=%v\n", daemonErr)
	} else {
		printLocalWebVNCDaemonStatus(a.Stdout, daemon)
		if daemon.Missing || daemon.Stale {
			printRescue(a.Stdout, rescueVNCBridgeNotRunning, "", webVNCDaemonStartRescueCommand(rescueCtx))
		}
	}
	if endpointErr != nil {
		fmt.Fprintf(a.Stdout, "vnc target: unreachable 127.0.0.1:5900 (%v)\n", endpointErr)
		printRescue(a.Stdout, rescueVNCTargetUnreachable, endpointErr.Error(), desktopDoctorCommand(rescueCtx))
	} else {
		fmt.Fprintf(a.Stdout, "vnc target: reachable %s:%s managed=%t\n", endpoint.Host, endpoint.Port, endpoint.Managed)
		if endpoint.Direct {
			fmt.Fprintln(a.Stdout, "ssh tunnel: not required")
		} else {
			fmt.Fprintf(a.Stdout, "ssh tunnel: %s\n", vncTunnelCommand(target, *localPort))
		}
	}
	if statusErr != nil {
		fmt.Fprintf(a.Stdout, "portal bridge: unknown (%v)\n", statusErr)
		printRescue(a.Stdout, rescueVNCBridgeDisconnected, statusErr.Error(), webVNCStatusRescueCommand(rescueCtx), webVNCResetRescueCommand(rescueCtx))
	} else {
		fmt.Fprintf(a.Stdout, "portal bridge: connected=%t viewers=%d observers=%d slots=%d\n", status.BridgeConnected, status.ViewerCount, status.ObserverCount, status.AvailableViewerSlots)
		if strings.TrimSpace(status.ControllerLabel) != "" {
			fmt.Fprintf(a.Stdout, "portal controller: %s\n", strings.TrimSpace(status.ControllerLabel))
		}
		if strings.TrimSpace(status.Message) != "" {
			fmt.Fprintf(a.Stdout, "portal message: %s\n", status.Message)
		}
		for _, event := range status.Events {
			fmt.Fprintf(a.Stdout, "event: %s %s%s\n", event.At, event.Event, optionalReason(event.Reason))
		}
	}
	for _, line := range recentWebVNCLogEvents(daemon.LogPath, 6) {
		fmt.Fprintf(a.Stdout, "log event: %s\n", line)
	}
	portal := webVNCPortalURL(coord.BaseURL, leaseID, username, password)
	fmt.Fprintf(a.Stdout, "webvnc: %s\n", portal)
	if strings.TrimSpace(password) != "" {
		fmt.Fprintf(a.Stdout, "password: %s\n", strings.TrimSpace(password))
		if strings.TrimSpace(username) != "" {
			fmt.Fprintf(a.Stdout, "username: %s\n", strings.TrimSpace(username))
		}
	}
	fmt.Fprintf(a.Stdout, "fallback: %s\n", nativeVNCOpenCommand(cfg, target, leaseID))
	if statusErr == nil && !status.BridgeConnected {
		printRescue(a.Stdout, rescueVNCBridgeNotRunning, "portal has no active WebVNC bridge for this lease", webVNCDaemonStartRescueCommand(rescueCtx), webVNCResetRescueCommand(rescueCtx))
	} else if statusErr == nil && webVNCObserverSlotsExhausted(status) {
		printRescue(a.Stdout, rescueVNCObserverSlotsFull, "all WebVNC observer slots are in use or stale", webVNCDaemonStartRescueCommand(rescueCtx), webVNCResetRescueCommand(rescueCtx))
	}
	return nil
}

func (a App) webVNCResetCommand(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("webvnc reset", a.Stderr)
	provider := fs.String("provider", defaults.Provider, "provider: hetzner, aws, or azure")
	id := fs.String("id", "", "lease id or slug")
	openPortal := fs.Bool("open", false, "open the web portal VNC page")
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	setIDFromFirstArg(fs, id)
	if *id == "" {
		return exit(2, "usage: crabbox webvnc reset --id <lease-id-or-slug>")
	}
	cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{Desktop: true})
	if err != nil {
		return err
	}
	if isBlacksmithProvider(cfg.Provider) || isStaticProvider(cfg.Provider) {
		return exit(2, "webvnc reset currently supports coordinator-backed hetzner/aws/azure desktop leases")
	}
	coord, useCoordinator, err := newTargetCoordinatorClient(cfg)
	if err != nil {
		return err
	}
	if !useCoordinator || coord == nil || coord.Token == "" {
		return exit(2, "webvnc reset requires a configured coordinator login; run crabbox login first")
	}
	server, target, leaseID, err := a.resolveNetworkLeaseTarget(ctx, cfg, *id, false)
	if err != nil {
		return err
	}
	if err := enforceManagedLeaseCapabilities(cfg, server, leaseID); err != nil {
		return err
	}
	if _, err := coord.ResetWebVNC(ctx, leaseID); err != nil {
		fmt.Fprintf(a.Stdout, "portal reset: skipped (%v)\n", err)
	}
	if leaseID != *id {
		_, _ = a.stopWebVNCDaemonIfRunning(*id)
	}
	if _, err := a.stopWebVNCDaemonIfRunning(leaseID); err != nil {
		return err
	}
	rescueCtx := rescueContext{Cfg: cfg, Target: target, LeaseID: leaseID}
	if out, err := runSSHCombinedOutput(ctx, target, webVNCResetRemoteCommand(target)); err != nil {
		printRescue(a.Stdout, classifyDesktopFailure(out), trimFailureDetail(out), desktopDoctorCommand(rescueCtx))
		return exit(5, "reset target WebVNC/input stack: %v", err)
	}
	password := ""
	username := ""
	if target.TargetOS == targetMacOS {
		username = target.User
	}
	password, _ = runSSHOutput(ctx, target, vncPasswordCommand(target))
	portal := webVNCPortalURL(coord.BaseURL, leaseID, username, password)
	daemonArgs := webVNCBridgeArgs(cfg, target, leaseID, *openPortal)
	daemonName := *id
	if strings.TrimSpace(daemonName) == "" {
		daemonName = leaseID
	}
	if err := a.startWebVNCDaemon(daemonArgs, daemonName); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "webvnc reset: lease=%s slug=%s\n", leaseID, blank(serverSlug(server), "-"))
	fmt.Fprintf(a.Stdout, "webvnc: %s\n", portal)
	if strings.TrimSpace(password) != "" {
		fmt.Fprintf(a.Stdout, "password: %s\n", strings.TrimSpace(password))
	}
	fmt.Fprintf(a.Stdout, "fallback: %s\n", nativeVNCOpenCommand(cfg, target, leaseID))
	return nil
}

func (a App) startWebVNCDaemon(args []string, leaseID string) error {
	exe, err := os.Executable()
	if err != nil {
		return exit(2, "resolve crabbox executable: %v", err)
	}
	if stopped, err := a.stopWebVNCDaemonIfRunning(leaseID); err != nil {
		return err
	} else if stopped {
		fmt.Fprintln(a.Stdout, "webvnc daemon: replacing previous daemon")
	}
	logPath, pidPath, err := webVNCDaemonPaths(leaseID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return exit(2, "create WebVNC daemon directory: %v", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return exit(2, "open WebVNC daemon log: %v", err)
	}
	defer logFile.Close()
	childArgs := append([]string{"webvnc"}, args...)
	cmd := exec.Command("sh", "-c", webVNCDaemonSupervisorScript(exe, childArgs))
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	configureDaemonCommand(cmd)
	if err := cmd.Start(); err != nil {
		return exit(5, "start WebVNC daemon: %v", err)
	}
	pid := cmd.Process.Pid
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d\n", pid)), 0o600); err != nil {
		_ = cmd.Process.Kill()
		return exit(2, "write WebVNC daemon pid: %v", err)
	}
	if err := cmd.Process.Release(); err != nil {
		return exit(5, "release WebVNC daemon process: %v", err)
	}
	fmt.Fprintf(a.Stdout, "webvnc daemon: pid=%d log=%s\n", pid, logPath)
	if webVNCDaemonLogReady(logPath, 10*time.Second) {
		fmt.Fprintln(a.Stdout, "webvnc daemon: ready")
	} else {
		fmt.Fprintln(a.Stdout, "webvnc daemon: starting; run crabbox webvnc status --id <lease-id-or-slug> to check bridge readiness")
	}
	fmt.Fprintln(a.Stdout, "webvnc daemon: stop with crabbox webvnc daemon stop --id <lease-id-or-slug>")
	return nil
}

func webVNCDaemonLogReady(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if webVNCDaemonLogHasReady(path) {
			return true
		}
		if timeout <= 0 || time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func webVNCDaemonLogHasReady(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), "bridge: connected; keep this process running while using WebVNC")
}

func webVNCDaemonSupervisorScript(exe string, args []string) string {
	firstArgs := make([]string, 0, len(args)+1)
	firstArgs = append(firstArgs, shellQuote(exe))
	for _, arg := range args {
		firstArgs = append(firstArgs, shellQuote(arg))
	}
	restartArgs := make([]string, 0, len(args)+1)
	restartArgs = append(restartArgs, shellQuote(exe))
	for _, arg := range stripWebVNCOpenFlags(args) {
		restartArgs = append(restartArgs, shellQuote(arg))
	}
	return "set -u\n" +
		"echo 'webvnc daemon supervisor: starting'\n" +
		"first=1\n" +
		"while :; do\n" +
		"  if [ \"$first\" = 1 ]; then\n" +
		"    " + strings.Join(firstArgs, " ") + "\n" +
		"    first=0\n" +
		"  else\n" +
		"    " + strings.Join(restartArgs, " ") + "\n" +
		"  fi\n" +
		"  code=$?\n" +
		"  echo \"webvnc daemon supervisor: child exited code=$code; restarting in 1s\"\n" +
		"  sleep 1\n" +
		"done\n"
}

func (a App) webVNCDaemonStatus(leaseID string) error {
	status, err := localWebVNCDaemonStatus(leaseID)
	if err != nil {
		return err
	}
	printLocalWebVNCDaemonStatus(a.Stdout, status)
	return nil
}

type localWebVNCDaemon struct {
	LeaseID string
	LogPath string
	PIDPath string
	PID     int
	Command string
	Alive   bool
	Stale   bool
	Missing bool
}

func localWebVNCDaemonStatus(leaseID string) (localWebVNCDaemon, error) {
	logPath, pidPath, err := webVNCDaemonPaths(leaseID)
	if err != nil {
		return localWebVNCDaemon{}, err
	}
	status := localWebVNCDaemon{LeaseID: leaseID, LogPath: logPath, PIDPath: pidPath}
	pid, err := readWebVNCDaemonPID(pidPath)
	if err != nil {
		if os.IsNotExist(err) {
			status.Missing = true
			return status, nil
		}
		return localWebVNCDaemon{}, err
	}
	status.PID = pid
	command, alive := webVNCDaemonProcessCommand(pid)
	status.Command = strings.TrimSpace(command)
	status.Alive = alive
	if !alive {
		status.Stale = true
		return status, nil
	}
	return status, nil
}

func printLocalWebVNCDaemonStatus(w io.Writer, status localWebVNCDaemon) {
	if status.Missing {
		fmt.Fprintf(w, "webvnc daemon: no pid file for %s\n", status.LeaseID)
		fmt.Fprintf(w, "webvnc daemon: expected log=%s\n", status.LogPath)
		return
	}
	if status.Stale {
		fmt.Fprintf(w, "webvnc daemon: stale pid=%d log=%s\n", status.PID, status.LogPath)
		return
	}
	fmt.Fprintf(w, "webvnc daemon: pid=%d log=%s\n", status.PID, status.LogPath)
	if strings.TrimSpace(status.Command) != "" {
		fmt.Fprintf(w, "webvnc daemon: command=%s\n", strings.TrimSpace(status.Command))
	}
}

func (a App) stopWebVNCDaemon(leaseID string) error {
	stopped, err := a.stopWebVNCDaemonIfRunning(leaseID)
	if err != nil {
		return err
	}
	if !stopped {
		fmt.Fprintf(a.Stdout, "webvnc daemon: no pid file for %s\n", leaseID)
	}
	return nil
}

func (a App) stopWebVNCDaemonIfRunning(leaseID string) (bool, error) {
	_, pidPath, err := webVNCDaemonPaths(leaseID)
	if err != nil {
		return false, err
	}
	pid, err := readWebVNCDaemonPID(pidPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	command, alive := webVNCDaemonProcessCommand(pid)
	if !alive {
		_ = os.Remove(pidPath)
		fmt.Fprintf(a.Stdout, "webvnc daemon: removed stale pid=%d\n", pid)
		return true, nil
	}
	if !isWebVNCDaemonCommand(command) {
		return false, exit(5, "refusing to stop pid %d; command does not look like crabbox webvnc: %s", pid, strings.TrimSpace(command))
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false, exit(5, "find WebVNC daemon pid %d: %v", pid, err)
	}
	if err := stopDaemonProcess(process, pid); err != nil {
		return false, exit(5, "stop WebVNC daemon pid %d: %v", pid, err)
	}
	_ = os.Remove(pidPath)
	fmt.Fprintf(a.Stdout, "webvnc daemon: stopped pid=%d\n", pid)
	return true, nil
}

func webVNCDaemonProcessCommand(pid int) (string, bool) {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return "", false
	}
	command := strings.TrimSpace(string(out))
	return command, command != ""
}

func isWebVNCDaemonCommand(command string) bool {
	command = strings.ToLower(command)
	return strings.Contains(command, "crabbox") && strings.Contains(command, "webvnc")
}

func readWebVNCDaemonPID(pidPath string) (int, error) {
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, exit(2, "invalid WebVNC daemon pid file %s", pidPath)
	}
	return pid, nil
}

func stripWebVNCOpenFlags(args []string) []string {
	out := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "--open" || strings.HasPrefix(arg, "--open=") {
			continue
		}
		out = append(out, arg)
	}
	return out
}

func optionalReason(reason string) string {
	if strings.TrimSpace(reason) == "" {
		return ""
	}
	return " reason=" + strings.TrimSpace(reason)
}

func nativeVNCOpenCommand(cfg Config, target SSHTarget, leaseID string) string {
	targetOS := firstNonBlank(target.TargetOS, cfg.TargetOS)
	args := []string{"crabbox", "vnc", "--provider", cfg.Provider, "--target", targetOS}
	if cfg.Network != "" && cfg.Network != NetworkAuto {
		args = append(args, "--network", string(cfg.Network))
	}
	windowsMode := firstNonBlank(target.WindowsMode, cfg.WindowsMode)
	if targetOS == targetWindows && windowsMode != "" {
		args = append(args, "--windows-mode", windowsMode)
	}
	args = append(args, "--id", leaseID, "--open")
	return strings.Join(readableShellWords(args), " ")
}

func stripLegacyWebVNCDaemonFlags(args []string) []string {
	out := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "--daemon" || arg == "--background" ||
			strings.HasPrefix(arg, "--daemon=") || strings.HasPrefix(arg, "--background=") {
			continue
		}
		out = append(out, arg)
	}
	return out
}

func readableShellWords(words []string) []string {
	out := make([]string, 0, len(words))
	for _, word := range words {
		if shellBareWord(word) {
			out = append(out, word)
		} else {
			out = append(out, shellQuote(word))
		}
	}
	return out
}

func shellBareWord(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("_./:@=-", r) {
			continue
		}
		return false
	}
	return true
}

func webVNCBridgeArgs(cfg Config, target SSHTarget, leaseID string, openPortal bool) []string {
	targetOS := firstNonBlank(target.TargetOS, cfg.TargetOS)
	args := []string{"--provider", cfg.Provider, "--target", targetOS}
	if cfg.Network != "" && cfg.Network != NetworkAuto {
		args = append(args, "--network", string(cfg.Network))
	}
	windowsMode := firstNonBlank(target.WindowsMode, cfg.WindowsMode)
	if targetOS == targetWindows && windowsMode != "" {
		args = append(args, "--windows-mode", windowsMode)
	}
	args = append(args, "--id", leaseID)
	if openPortal {
		args = append(args, "--open")
	}
	return args
}

func recentWebVNCLogEvents(path string, limit int) []string {
	if strings.TrimSpace(path) == "" || limit <= 0 {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	out := make([]string, 0, limit)
	for i := len(lines) - 1; i >= 0 && len(out) < limit; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if strings.Contains(line, "viewer") || strings.Contains(line, "reconnect") || strings.Contains(line, "connected") || strings.Contains(line, "reset") {
			out = append(out, line)
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func webVNCResetRemoteCommand(target SSHTarget) string {
	if isWindowsNativeTarget(target) {
		return `$ErrorActionPreference = "SilentlyContinue"
Restart-Service -Name tvnserver -ErrorAction SilentlyContinue
Start-Sleep -Seconds 1`
	}
	if target.TargetOS == targetMacOS {
		return `set -eu
sudo launchctl kickstart -k system/com.apple.screensharing >/dev/null 2>&1 || true`
	}
	return `set -eu
sudo systemctl restart crabbox-desktop-session.service crabbox-x11vnc.service`
}

func webVNCDaemonPaths(leaseID string) (string, string, error) {
	dir, err := crabboxStateDir()
	if err != nil {
		return "", "", err
	}
	name := safeWebVNCDaemonName(leaseID)
	bridgeDir := filepath.Join(dir, "webvnc")
	return filepath.Join(bridgeDir, name+".log"), filepath.Join(bridgeDir, name+".pid"), nil
}

func safeWebVNCDaemonName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "bridge"
	}
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
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
	ws, resp, err := websocket.Dial(ctx, webVNCAgentURL(coord.BaseURL, leaseID), &websocket.DialOptions{
		HTTPHeader: bridgeTicketHeaders(coord, ticket.Ticket),
	})
	if retryBridgeTicketInQuery(resp, err) {
		ws, _, err = websocket.Dial(ctx, webVNCAgentURLWithTicket(coord.BaseURL, leaseID, ticket.Ticket), &websocket.DialOptions{
			HTTPHeader: coord.webVNCAccessHeaders(),
		})
	}
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

func retryableWebVNCBridgeError(err error) bool {
	if err == nil {
		return true
	}
	message := err.Error()
	return strings.Contains(message, "WebVNC viewer disconnected") ||
		strings.Contains(message, "replaced by a newer WebVNC viewer") ||
		strings.Contains(message, "WebVNC bridge reset") ||
		strings.Contains(message, "failed to read frame header: EOF") ||
		strings.Contains(message, "status = StatusNormalClosure")
}

func classifyWebVNCBridgeProblem(err error) string {
	if err == nil {
		return rescueVNCBridgeDisconnected
	}
	message := err.Error()
	if strings.Contains(message, "replaced by a newer WebVNC viewer") || strings.Contains(message, "another viewer") {
		return rescueVNCStaleViewer
	}
	return rescueVNCBridgeDisconnected
}

func webVNCObserverSlotsExhausted(status CoordinatorWebVNCStatus) bool {
	if !status.BridgeConnected || status.AvailableViewerSlots != 0 {
		return false
	}
	if status.ViewerCount > 0 {
		return true
	}
	return strings.Contains(status.Message, "available WebVNC observer slot")
}

func webVNCReconnectDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := time.Duration(attempt) * 500 * time.Millisecond
	if delay > 5*time.Second {
		return 5 * time.Second
	}
	return delay
}

func waitWebVNCReconnect(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return nil
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

func bridgeTicketHeaders(coord *CoordinatorClient, ticket string) http.Header {
	headers := coord.webVNCAccessHeaders()
	headers.Set("Authorization", "Bearer "+ticket)
	return headers
}

func retryBridgeTicketInQuery(resp *http.Response, err error) bool {
	if err == nil || resp == nil {
		return false
	}
	if resp.Body != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	return resp.StatusCode == http.StatusUnauthorized
}

func webVNCAgentURL(base, leaseID string) string {
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
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func webVNCAgentURLWithTicket(base, leaseID, ticket string) string {
	u, err := url.Parse(webVNCAgentURL(base, leaseID))
	if err != nil {
		return base
	}
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
	u.Fragment = ""
	u.RawFragment = ""
	if strings.TrimSpace(username) != "" || strings.TrimSpace(password) != "" {
		values := url.Values{}
		if strings.TrimSpace(username) != "" {
			values.Set("username", strings.TrimSpace(username))
		}
		if strings.TrimSpace(password) != "" {
			values.Set("password", strings.TrimSpace(password))
		}
		u.RawFragment = values.Encode()
		if fragment, err := url.PathUnescape(u.RawFragment); err == nil {
			u.Fragment = fragment
		}
	}
	return u.String()
}

func stopProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}
