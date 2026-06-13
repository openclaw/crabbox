package cli

import (
	"context"
	"encoding/json"
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
		fmt.Fprintln(fs.Output(), "  --provider <desktop-ssh-provider>")
		fmt.Fprintln(fs.Output(), "  --target linux|macos|windows")
		fmt.Fprintln(fs.Output(), "  --windows-mode normal|wsl2")
		fmt.Fprintln(fs.Output(), "  --static-host <host>")
		fmt.Fprintln(fs.Output(), "  --static-user <user>")
		fmt.Fprintln(fs.Output(), "  --static-port <port>")
		fmt.Fprintln(fs.Output(), "  --static-work-root <path>")
		fmt.Fprintln(fs.Output(), "  --network auto|tailscale|public")
		fmt.Fprintln(fs.Output(), "  --local-port <port>")
		fmt.Fprintln(fs.Output(), "  --open")
		fmt.Fprintln(fs.Output(), "  --take-control")
		fmt.Fprintln(fs.Output(), "  --reclaim")
	}
	provider := fs.String("provider", defaults.Provider, "desktop SSH provider")
	id := fs.String("id", "", "lease id or slug")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	localPort := fs.String("local-port", "", "local VNC tunnel port")
	openPortal := fs.Bool("open", false, "open the web portal VNC page")
	takeControl := fs.Bool("take-control", false, "ask the portal viewer to take keyboard and mouse control after connecting")
	daemon := fs.Bool("daemon", false, "compatibility alias for daemon start")
	background := fs.Bool("background", false, "compatibility alias for daemon start")
	daemonStatus := fs.Bool("status", false, "compatibility alias for daemon status")
	stopDaemon := fs.Bool("stop", false, "compatibility alias for daemon stop")
	providerFlags := registerProviderFlags(fs, defaults)
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
	cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{LeaseID: *id, Desktop: true})
	if err != nil {
		return err
	}
	if err := applyProviderFlags(&cfg, fs, providerFlags); err != nil {
		return err
	}
	if useDirectSSHWebVNC(cfg) {
		// macOS leases (e.g. tart) have no guest-side noVNC/websockify; serve the
		// browser viewer from a host-side bridge over the guest's native Screen
		// Sharing instead of the Linux directSSHWebVNC path.
		if isMacOSDesktopProvider(cfg) {
			return a.macOSWebVNCBridge(ctx, cfg, *id, *localPort, *openPortal, *reclaim)
		}
		return a.directSSHWebVNC(ctx, cfg, *id, *localPort, *openPortal, *takeControl, *reclaim)
	}
	if isBlacksmithProvider(cfg.Provider) || (isStaticProvider(cfg.Provider) && !shouldRegisterCoordinatorLease(cfg)) {
		return exit(2, "webvnc requires a coordinator-managed or registered desktop lease")
	}
	coord, useCoordinator, err := newTargetCoordinatorClient(cfg)
	if err != nil {
		return err
	}
	if !useCoordinator || !coord.hasConfiguredAuth() {
		return exit(2, "webvnc requires a configured coordinator login; run crabbox login --url <broker-url> first")
	}
	server, target, leaseID, err := a.resolveNetworkLeaseTargetForRepo(ctx, cfg, *id, false, *reclaim)
	if err != nil {
		return err
	}
	if err := enforceManagedLeaseCapabilities(cfg, server, leaseID); err != nil {
		return err
	}
	if err := a.claimAndTouchLeaseTarget(ctx, cfg, server, target, leaseID, *reclaim); err != nil {
		return err
	}
	if err := ensureOpenWebVNCPortalAccess(ctx, coord, leaseID, *openPortal, a.Stdout); err != nil {
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

	portal := webVNCPortalURL(coord.BaseURL, leaseID, username, password, webVNCPortalOptions{TakeControl: *takeControl})
	rescueCtx := rescueContext{Cfg: cfg, Target: target, LeaseID: leaseID}
	opened := false
	return serveWebVNCBridgePool(ctx, webVNCBridgePoolConfig{
		Coord:       coord,
		LeaseID:     leaseID,
		Host:        connHost,
		Port:        connPort,
		PoolSize:    webVNCBridgePoolSizeForTarget(target),
		IdleTimeout: cfg.IdleTimeout,
		Telemetry:   leaseTelemetryCollectorForTarget(target),
		RescueCtx:   rescueCtx,
		NativeVNC:   nativeVNCOpenCommand(cfg, target, leaseID),
		Log:         a.Stdout,
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
const macOSWebVNCBridgePoolSize = 2

func webVNCBridgePoolSizeForTarget(target SSHTarget) int {
	if target.TargetOS == targetMacOS {
		return macOSWebVNCBridgePoolSize
	}
	return defaultWebVNCBridgePoolSize
}

type webVNCBridgePoolConfig struct {
	Coord       *CoordinatorClient
	LeaseID     string
	Host        string
	Port        string
	PoolSize    int
	IdleTimeout time.Duration
	Telemetry   leaseTelemetryCollector
	RescueCtx   rescueContext
	NativeVNC   string
	Log         io.Writer
	OnReady     func() error
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
	if cfg.Coord != nil && strings.TrimSpace(cfg.LeaseID) != "" {
		stopHeartbeat := startCoordinatorHeartbeat(ctx, cfg.Coord, cfg.LeaseID, cfg.IdleTimeout, nil, cfg.Telemetry, cfg.Log)
		defer stopHeartbeat()
	}
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
		bridge, err := connectWebVNCBridge(ctx, cfg.Coord, cfg.LeaseID, cfg.Host, cfg.Port, cfg.RescueCtx.Target, cfg.Log)
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
		return exit(2, "usage: crabbox webvnc daemon start|status|stop|list --id <lease-id-or-slug>")
	}
	if isHelpArg(args[0]) {
		fmt.Fprintln(a.Stdout, "Usage: crabbox webvnc daemon start|status|stop|list --id <lease-id-or-slug>")
		return nil
	}
	switch args[0] {
	case "start":
		return a.webVNCDaemonStart(ctx, args[1:])
	case "status":
		return a.webVNCDaemonStatusCommand(args[1:])
	case "stop":
		return a.webVNCDaemonStopCommand(args[1:])
	case "list", "ls":
		return a.webVNCDaemonListCommand(args[1:])
	default:
		return exit(2, "usage: crabbox webvnc daemon start|status|stop|list --id <lease-id-or-slug>")
	}
}

func (a App) webVNCDaemonStart(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("webvnc daemon start", a.Stderr)
	provider := fs.String("provider", defaults.Provider, "desktop SSH provider")
	id := fs.String("id", "", "lease id or slug")
	localPort := fs.String("local-port", "", "local VNC tunnel port")
	openPortal := fs.Bool("open", false, "open the web portal VNC page")
	takeControl := fs.Bool("take-control", false, "ask the portal viewer to take keyboard and mouse control after connecting")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	providerFlags := registerProviderFlags(fs, defaults)
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	setIDFromFirstArg(fs, id)
	if *id == "" {
		return exit(2, "usage: crabbox webvnc daemon start --id <lease-id-or-slug>")
	}
	cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{LeaseID: *id, Desktop: true})
	if err != nil {
		return err
	}
	if err := applyProviderFlags(&cfg, fs, providerFlags); err != nil {
		return err
	}
	target := SSHTarget{TargetOS: cfg.TargetOS, WindowsMode: cfg.WindowsMode}
	bridgeID := *id
	if useDirectSSHWebVNC(cfg) {
		if err := guardMacOSDirectWebVNC(cfg); err != nil {
			return err
		}
		server, resolvedTarget, leaseID, err := a.resolveNetworkLeaseTargetForRepo(ctx, cfg, *id, false, *reclaim)
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
	} else if !isBlacksmithProvider(cfg.Provider) && (!isStaticProvider(cfg.Provider) || shouldRegisterCoordinatorLease(cfg)) {
		coord, useCoordinator, err := newTargetCoordinatorClient(cfg)
		if err != nil {
			return err
		}
		if useCoordinator && coord.hasConfiguredAuth() {
			server, resolvedTarget, leaseID, err := a.resolveNetworkLeaseTargetForRepo(ctx, cfg, *id, false, *reclaim)
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
	daemonArgs := webVNCBridgeArgs(cfg, target, bridgeID, *openPortal, *takeControl)
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

func (a App) webVNCDaemonListCommand(args []string) error {
	fs := newFlagSet("webvnc daemon list", a.Stderr)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return exit(2, "usage: crabbox webvnc daemon list")
	}
	dir, err := crabboxStateDir()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(filepath.Join(dir, "webvnc"))
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(a.Stdout, "webvnc daemon: none")
			return nil
		}
		return err
	}
	count := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".pid") {
			continue
		}
		leaseID := strings.TrimSuffix(name, ".pid")
		status, err := localWebVNCDaemonStatus(leaseID)
		if err != nil {
			fmt.Fprintf(a.Stdout, "webvnc daemon: %s error=%v\n", leaseID, err)
		} else {
			printLocalWebVNCDaemonStatus(a.Stdout, status)
		}
		count++
	}
	if count == 0 {
		fmt.Fprintln(a.Stdout, "webvnc daemon: none")
	}
	return nil
}

func (a App) webVNCStatusCommand(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("webvnc status", a.Stderr)
	provider := fs.String("provider", defaults.Provider, "desktop SSH provider")
	id := fs.String("id", "", "lease id or slug")
	localPort := fs.String("local-port", "", "local VNC tunnel port")
	providerFlags := registerProviderFlags(fs, defaults)
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	setIDFromFirstArg(fs, id)
	if *id == "" {
		return exit(2, "usage: crabbox webvnc status --id <lease-id-or-slug>")
	}
	cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{LeaseID: *id, Desktop: true})
	if err != nil {
		return err
	}
	if err := applyProviderFlags(&cfg, fs, providerFlags); err != nil {
		return err
	}
	if useDirectSSHWebVNC(cfg) {
		if err := guardMacOSDirectWebVNC(cfg); err != nil {
			return err
		}
		return a.directSSHWebVNCStatus(ctx, cfg, *id, *localPort)
	}
	if isBlacksmithProvider(cfg.Provider) || (isStaticProvider(cfg.Provider) && !shouldRegisterCoordinatorLease(cfg)) {
		return exit(2, "webvnc status requires a coordinator-managed or registered desktop lease")
	}
	coord, useCoordinator, err := newTargetCoordinatorClient(cfg)
	if err != nil {
		return err
	}
	if !useCoordinator || !coord.hasConfiguredAuth() {
		return exit(2, "webvnc status requires a configured coordinator login; run crabbox login --url <broker-url> first")
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
	provider := fs.String("provider", defaults.Provider, "desktop SSH provider")
	id := fs.String("id", "", "lease id or slug")
	openPortal := fs.Bool("open", false, "open the web portal VNC page")
	takeControl := fs.Bool("take-control", false, "ask the portal viewer to take keyboard and mouse control after connecting")
	providerFlags := registerProviderFlags(fs, defaults)
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	setIDFromFirstArg(fs, id)
	if *id == "" {
		return exit(2, "usage: crabbox webvnc reset --id <lease-id-or-slug>")
	}
	cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{LeaseID: *id, Desktop: true})
	if err != nil {
		return err
	}
	if err := applyProviderFlags(&cfg, fs, providerFlags); err != nil {
		return err
	}
	if useDirectSSHWebVNC(cfg) {
		if err := guardMacOSDirectWebVNC(cfg); err != nil {
			return err
		}
		return a.directSSHWebVNCReset(ctx, cfg, *id, *openPortal, *takeControl)
	}
	if isBlacksmithProvider(cfg.Provider) || (isStaticProvider(cfg.Provider) && !shouldRegisterCoordinatorLease(cfg)) {
		return exit(2, "webvnc reset requires a coordinator-managed or registered desktop lease")
	}
	coord, useCoordinator, err := newTargetCoordinatorClient(cfg)
	if err != nil {
		return err
	}
	if !useCoordinator || !coord.hasConfiguredAuth() {
		return exit(2, "webvnc reset requires a configured coordinator login; run crabbox login --url <broker-url> first")
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
	if err := ensureOpenWebVNCPortalAccess(ctx, coord, leaseID, *openPortal, a.Stdout); err != nil {
		return err
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
	portal := webVNCPortalURL(coord.BaseURL, leaseID, username, password, webVNCPortalOptions{TakeControl: *takeControl})
	daemonArgs := webVNCBridgeArgs(cfg, target, leaseID, *openPortal, *takeControl)
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
	args = ensureWebVNCDaemonReclaimArg(args)
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

func ensureWebVNCDaemonReclaimArg(args []string) []string {
	for _, arg := range args {
		if arg == "--reclaim" || strings.HasPrefix(arg, "--reclaim=") {
			return args
		}
	}
	out := append([]string(nil), args...)
	return append(out, "--reclaim")
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
	args = append(args, providerCommandRoutingArgs(cfg, leaseID)...)
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

func readableShellCommand(words []string) string {
	out := make([]string, 0, len(words))
	seenCommand := false
	for _, word := range words {
		if !seenCommand && isShellEnvAssignment(word) {
			key, value, _ := strings.Cut(word, "=")
			out = append(out, key+"="+shellQuote(value))
			continue
		}
		seenCommand = true
		if shellBareWord(word) {
			out = append(out, word)
		} else {
			out = append(out, shellQuote(word))
		}
	}
	return strings.Join(out, " ")
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

func webVNCBridgeArgs(cfg Config, target SSHTarget, leaseID string, openPortal, takeControl bool) []string {
	targetOS := firstNonBlank(target.TargetOS, cfg.TargetOS)
	args := []string{"--provider", cfg.Provider, "--target", targetOS}
	if cfg.Network != "" && cfg.Network != NetworkAuto {
		args = append(args, "--network", string(cfg.Network))
	}
	windowsMode := firstNonBlank(target.WindowsMode, cfg.WindowsMode)
	if targetOS == targetWindows && windowsMode != "" {
		args = append(args, "--windows-mode", windowsMode)
	}
	args = append(args, providerCommandRoutingArgs(cfg, leaseID)...)
	args = append(args, "--id", leaseID)
	if openPortal {
		args = append(args, "--open")
	}
	if takeControl {
		args = append(args, "--take-control")
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
if [ -f /var/lib/crabbox/desktop.env ]; then . /var/lib/crabbox/desktop.env; fi
if [ "${CRABBOX_DESKTOP_ENV:-xfce}" != "xfce" ]; then
  sudo systemctl restart crabbox-desktop.service crabbox-wayvnc.service
else
  sudo systemctl restart crabbox-desktop-session.service crabbox-x11vnc.service
fi`
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
	var output strings.Builder
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			stopProcess(cmd)
			return nil, context.Cause(ctx)
		}
		if tcpReachable(ctx, "127.0.0.1", localPort, 200*time.Millisecond) {
			return cmd, nil
		}
		select {
		case err := <-done:
			if text := strings.TrimSpace(output.String()); text != "" {
				return nil, fmt.Errorf("%w: %s", err, text)
			}
			return nil, err
		default:
		}
		time.Sleep(100 * time.Millisecond)
	}
	stopProcess(cmd)
	return nil, exit(5, "timed out starting VNC SSH tunnel on localhost:%s", localPort)
}

type webVNCBridge struct {
	tcp    net.Conn
	ws     *websocket.Conn
	target SSHTarget
	log    io.Writer
}

func connectWebVNCBridge(ctx context.Context, coord *CoordinatorClient, leaseID, host, port string, target SSHTarget, log io.Writer) (*webVNCBridge, error) {
	tcp, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return nil, err
	}
	ticket, err := coord.CreateWebVNCTicket(ctx, leaseID)
	if err != nil {
		_ = tcp.Close()
		return nil, err
	}
	capabilities := webVNCBridgeCapabilities(ctx, target)
	ws, resp, err := websocket.Dial(ctx, webVNCAgentURLWithCapabilities(coord.BaseURL, leaseID, capabilities), &websocket.DialOptions{
		HTTPHeader: bridgeTicketHeaders(coord, ticket.Ticket),
	})
	if retryBridgeTicketInQuery(resp, err) {
		headers, headerErr := coord.webVNCAccessHeaders(ctx)
		if headerErr != nil {
			err = headerErr
		} else {
			ws, _, err = websocket.Dial(ctx, webVNCAgentURLWithTicketAndCapabilities(coord.BaseURL, leaseID, ticket.Ticket, capabilities), &websocket.DialOptions{
				HTTPHeader: headers,
			})
		}
	}
	if err != nil {
		_ = tcp.Close()
		return nil, err
	}
	return &webVNCBridge{tcp: tcp, ws: ws, target: target, log: log}, nil
}

func (b *webVNCBridge) Serve(ctx context.Context) error {
	defer b.Close()
	errc := make(chan error, 2)
	go func() { errc <- b.copyWebSocketToTCP(ctx) }()
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

func isLocalContainerProvider(provider string) bool {
	p, err := ProviderFor(provider)
	return err == nil && p.Name() == "local-container"
}

// guardMacOSDirectWebVNC rejects the direct WebVNC browser path for macOS
// desktop leases (e.g. tart). That path shells noVNC/websockify on the guest,
// which is Linux-only; macOS leases expose native Screen Sharing instead, so we
// point the user at a native VNC client over an SSH tunnel.
func guardMacOSDirectWebVNC(cfg Config) error {
	if !isMacOSDesktopProvider(cfg) {
		return nil
	}
	return exit(2, "this webvnc subcommand is not available for macOS leases; run `crabbox webvnc --id <id>` for the host-side browser viewer, or use a native VNC client over an SSH tunnel:\n  ssh -L 5900:127.0.0.1:5900 %s@<lease-ip>\n  open vnc://127.0.0.1:5900", blank(cfg.SSHUser, "<user>"))
}

// isMacOSDesktopProvider reports whether the lease belongs to a provider whose
// ONLY target is macOS (e.g. tart) — those use the host-side Screen Sharing
// bridge. It is keyed off the provider spec (not the resolved cfg.TargetOS,
// which the webvnc subcommands don't always populate) so every entrypoint
// classifies uniformly. Multi-target providers (e.g. parallels, which also
// serves Linux/Windows) keep the existing WebVNC path even for their macOS
// leases, so a single macOS target must not divert them into the tart bridge.
func isMacOSDesktopProvider(cfg Config) bool {
	p, err := ProviderFor(cfg.Provider)
	if err != nil {
		return false
	}
	targets := p.Spec().Targets
	if len(targets) == 0 {
		return false
	}
	for _, t := range targets {
		if t.OS != targetMacOS {
			return false
		}
	}
	return true
}

func supportsDirectSSHWebVNC(provider string) bool {
	p, err := ProviderFor(provider)
	if err != nil || isBlacksmithProvider(provider) || isStaticProvider(provider) {
		return false
	}
	spec := p.Spec()
	return spec.Kind == ProviderKindSSHLease &&
		spec.Coordinator == CoordinatorNever &&
		spec.Features.Has(FeatureDesktop)
}

func useDirectSSHWebVNC(cfg Config) bool {
	return supportsDirectSSHWebVNC(cfg.Provider) && !shouldRegisterCoordinatorLease(cfg)
}

func (a App) directSSHWebVNC(ctx context.Context, cfg Config, id, localPort string, openViewer, _ bool, reclaim bool) error {
	server, target, leaseID, err := a.resolveNetworkLeaseTargetForRepo(ctx, cfg, id, false, reclaim)
	if err != nil {
		return err
	}
	if err := enforceManagedLeaseCapabilities(cfg, server, leaseID); err != nil {
		return err
	}
	if err := a.claimAndTouchLeaseTarget(ctx, cfg, server, target, leaseID, reclaim); err != nil {
		return err
	}
	if _, err := resolveVNCEndpoint(ctx, cfg, &target); err != nil {
		return err
	}
	if out, err := runSSHCombinedOutput(ctx, target, directSSHNoVNCRemoteCommand()); err != nil {
		rescueCtx := rescueContext{Cfg: cfg, Target: target, LeaseID: leaseID}
		printRescue(a.Stdout, classifyDesktopFailure(out), trimFailureDetail(out), desktopDoctorCommand(rescueCtx))
		return exit(5, "start direct SSH WebVNC bridge: %v", err)
	}
	if localPort == "" {
		localPort = availableLocalVNCPort()
	}
	fmt.Fprintf(a.Stdout, "lease: %s slug=%s provider=%s target=%s\n", leaseID, blank(serverSlug(server), "-"), blank(server.Provider, cfg.Provider), blank(target.TargetOS, cfg.TargetOS))
	fmt.Fprintf(a.Stdout, "bridge: starting SSH tunnel localhost:%s -> 127.0.0.1:6080\n", localPort)
	tunnel, err := startVNCForegroundTunnel(ctx, target, localPort, "127.0.0.1", "6080")
	if err != nil {
		return err
	}
	defer stopProcess(tunnel)
	password, _ := runSSHOutput(ctx, target, vncPasswordCommand(target))
	viewerURL := directSSHWebVNCURL(localPort, strings.TrimSpace(password))
	fmt.Fprintln(a.Stdout, "bridge: connected; keep this process running while using WebVNC")
	fmt.Fprintf(a.Stdout, "webvnc: %s\n", viewerURL)
	if strings.TrimSpace(password) != "" {
		fmt.Fprintf(a.Stdout, "password: %s\n", strings.TrimSpace(password))
	}
	if openViewer {
		if err := openLocalURL(viewerURL); err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "opened: %s\n", viewerURL)
	}
	<-ctx.Done()
	return context.Cause(ctx)
}

func (a App) directSSHWebVNCStatus(ctx context.Context, cfg Config, id, localPort string) error {
	server, target, leaseID, err := a.resolveNetworkLeaseTarget(ctx, cfg, id, false)
	if err != nil {
		return err
	}
	if err := enforceManagedLeaseCapabilities(cfg, server, leaseID); err != nil {
		return err
	}
	if localPort == "" {
		localPort = availableLocalVNCPort()
	}
	endpoint, endpointErr := resolveVNCEndpoint(ctx, cfg, &target)
	websockify := "unknown"
	if out, err := runSSHOutput(ctx, target, `if ss -ltn | grep -q '127.0.0.1:6080'; then echo running; else echo stopped; fi`); err == nil {
		websockify = strings.TrimSpace(out)
	}
	password := ""
	if endpointErr == nil && endpoint.Managed {
		password, _ = runSSHOutput(ctx, target, vncPasswordCommand(target))
	}
	fmt.Fprintf(a.Stdout, "lease: %s slug=%s provider=%s target=%s\n", leaseID, blank(serverSlug(server), "-"), blank(server.Provider, cfg.Provider), blank(target.TargetOS, cfg.TargetOS))
	if endpointErr != nil {
		fmt.Fprintf(a.Stdout, "vnc target: unreachable 127.0.0.1:5900 (%v)\n", endpointErr)
		printRescue(a.Stdout, rescueVNCTargetUnreachable, endpointErr.Error(), desktopDoctorCommand(rescueContext{Cfg: cfg, Target: target, LeaseID: leaseID}))
	} else {
		fmt.Fprintf(a.Stdout, "vnc target: reachable %s:%s managed=%t\n", endpoint.Host, endpoint.Port, endpoint.Managed)
	}
	fmt.Fprintf(a.Stdout, "direct ssh webvnc: %s\n", blank(websockify, "unknown"))
	fmt.Fprintf(a.Stdout, "ssh tunnel: %s\n", vncTunnelCommandTo(target, localPort, "127.0.0.1", "6080"))
	fmt.Fprintf(a.Stdout, "webvnc: %s\n", directSSHWebVNCURL(localPort, strings.TrimSpace(password)))
	if strings.TrimSpace(password) != "" {
		fmt.Fprintf(a.Stdout, "password: %s\n", strings.TrimSpace(password))
	}
	fmt.Fprintf(a.Stdout, "fallback: %s\n", nativeVNCOpenCommand(cfg, target, leaseID))
	return nil
}

func (a App) directSSHWebVNCReset(ctx context.Context, cfg Config, id string, openViewer, takeControl bool) error {
	server, target, leaseID, err := a.resolveNetworkLeaseTarget(ctx, cfg, id, false)
	if err != nil {
		return err
	}
	if err := enforceManagedLeaseCapabilities(cfg, server, leaseID); err != nil {
		return err
	}
	if out, err := runSSHCombinedOutput(ctx, target, directSSHWebVNCResetRemoteCommand()); err != nil {
		printRescue(a.Stdout, classifyDesktopFailure(out), trimFailureDetail(out), desktopDoctorCommand(rescueContext{Cfg: cfg, Target: target, LeaseID: leaseID}))
		return exit(5, "reset direct SSH WebVNC/input stack: %v", err)
	}
	fmt.Fprintf(a.Stdout, "webvnc reset: lease=%s slug=%s\n", leaseID, blank(serverSlug(server), "-"))
	if openViewer {
		return a.directSSHWebVNC(ctx, cfg, leaseID, "", true, takeControl, false)
	}
	command := append([]string{"crabbox", "webvnc"}, webVNCBridgeArgs(cfg, target, leaseID, false, false)...)
	fmt.Fprintf(a.Stdout, "webvnc: run %s\n", readableShellCommand(command))
	return nil
}

func directSSHWebVNCURL(localPort, password string) string {
	values := url.Values{}
	values.Set("host", "127.0.0.1")
	values.Set("port", localPort)
	values.Set("path", "websockify")
	values.Set("autoconnect", "1")
	values.Set("resize", "scale")
	values.Set("compression", "0")
	values.Set("quality", "6")
	if strings.TrimSpace(password) != "" {
		values.Set("password", strings.TrimSpace(password))
	}
	return "http://127.0.0.1:" + localPort + "/vnc.html?" + values.Encode()
}

func vncTunnelCommandTo(target SSHTarget, localPort, remoteHost, remotePort string) string {
	return strings.Join(shellWords(append([]string{"ssh"}, vncTunnelArgs(target, localPort, remoteHost, remotePort)...)), " ")
}

func directSSHNoVNCRemoteCommand() string {
	return `set -eu
if ! command -v websockify >/dev/null 2>&1; then
  echo "missing websockify; warm a new --desktop lease or install novnc websockify" >&2
  exit 127
fi
web_dir=""
for candidate in /usr/share/novnc /usr/share/novnc/core /usr/share/novnc/html; do
  if [ -f "$candidate/vnc.html" ]; then
    web_dir="$candidate"
    break
  fi
done
if [ -z "$web_dir" ]; then
  echo "missing noVNC web assets; warm a new --desktop lease or install novnc" >&2
  exit 127
fi
if ! ss -ltn | grep -q '127.0.0.1:6080'; then
  nohup websockify --web="$web_dir" 127.0.0.1:6080 127.0.0.1:5900 >/tmp/crabbox-websockify.log 2>&1 &
fi
for i in 1 2 3 4 5; do
  if ss -ltn | grep -q '127.0.0.1:6080'; then
    exit 0
  fi
  sleep 1
done
cat /tmp/crabbox-websockify.log >&2 || true
exit 1`
}

func directSSHWebVNCResetRemoteCommand() string {
	return `set -eu
pkill -f 'websockify.*127.0.0.1:6080' >/dev/null 2>&1 || true
if [ -x /usr/local/bin/crabbox-start-desktop ]; then
  sudo CRABBOX_SSH_USER="$(id -un)" /usr/local/bin/crabbox-start-desktop
fi
` + directSSHNoVNCRemoteCommand()
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

func webVNCBridgeCapabilities(ctx context.Context, target SSHTarget) string {
	if target.TargetOS != "" && target.TargetOS != targetLinux {
		return ""
	}
	out, err := runSSHCombinedOutput(ctx, target, webVNCDesktopThemeCapabilityCommand())
	if err != nil || strings.TrimSpace(out) != "" {
		return ""
	}
	return "desktop_theme"
}

func webVNCDesktopThemeCapabilityCommand() string {
	return `set -eu
if [ -x /usr/local/bin/crabbox-configure-desktop-theme ] && grep -q 'desktop-theme' /usr/local/bin/crabbox-configure-desktop-theme; then
  exit 0
fi
if [ -x /usr/local/bin/crabbox-start-desktop ] && grep -q 'desktop-theme' /usr/local/bin/crabbox-start-desktop; then
  exit 0
fi
if [ -f /var/lib/crabbox/desktop.env ] && grep -q '^CRABBOX_DESKTOP_ENV=gnome$' /var/lib/crabbox/desktop.env; then
  exit 0
fi
echo "desktop theme helper does not support dynamic themes" >&2
exit 1
`
}

type webVNCBridgeControlMessage struct {
	Type  string `json:"type"`
	Theme string `json:"theme,omitempty"`
}

func (b *webVNCBridge) copyWebSocketToTCP(ctx context.Context) error {
	for {
		typ, data, err := b.ws.Read(ctx)
		if err != nil {
			return err
		}
		if typ == websocket.MessageText {
			handled, err := b.handleControlFrame(ctx, data)
			if err != nil && b.log != nil {
				fmt.Fprintf(b.log, "bridge: %v\n", err)
			}
			if handled {
				continue
			}
		}
		if _, err := b.tcp.Write(data); err != nil {
			return err
		}
	}
}

func (b *webVNCBridge) handleControlFrame(ctx context.Context, data []byte) (bool, error) {
	if len(data) == 0 || data[0] != '{' {
		return false, nil
	}
	var msg webVNCBridgeControlMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return false, nil
	}
	if msg.Type != "desktop_theme" {
		return false, nil
	}
	theme := strings.TrimSpace(msg.Theme)
	if theme != "light" && theme != "dark" {
		return true, fmt.Errorf("ignore invalid desktop theme %q", msg.Theme)
	}
	if b.target.TargetOS != "" && b.target.TargetOS != targetLinux {
		return true, nil
	}
	out, err := runSSHCombinedOutput(ctx, b.target, webVNCDesktopThemeCommand(theme, b.target.User))
	if err != nil {
		detail := strings.TrimSpace(out)
		if detail == "" {
			return true, fmt.Errorf("apply desktop theme %s: %w", theme, err)
		}
		return true, fmt.Errorf("apply desktop theme %s: %w: %s", theme, err, detail)
	}
	return true, nil
}

func webVNCDesktopThemeCommand(theme, user string) string {
	theme = strings.TrimSpace(theme)
	if theme != "light" && theme != "dark" {
		theme = "dark"
	}
	user = strings.TrimSpace(user)
	if user == "" {
		user = "crabbox"
	}
	return "set -eu\n" +
		"if [ -f /var/lib/crabbox/desktop.env ] && grep -q '^CRABBOX_DESKTOP_ENV=gnome$' /var/lib/crabbox/desktop.env; then\n" +
		webVNCGNOMEDesktopThemeFallbackCommand(theme, user) +
		"elif command -v /usr/local/bin/crabbox-configure-desktop-theme >/dev/null 2>&1 && grep -q 'desktop-theme' /usr/local/bin/crabbox-configure-desktop-theme; then\n" +
		"  env DISPLAY=:99 CRABBOX_DESKTOP_USER=" + shellQuote(user) + " /usr/local/bin/crabbox-configure-desktop-theme " + shellQuote(theme) + "\n" +
		"elif command -v /usr/local/bin/crabbox-start-desktop >/dev/null 2>&1 && grep -q 'desktop-theme' /usr/local/bin/crabbox-start-desktop; then\n" +
		"  sudo env DISPLAY=:99 CRABBOX_SSH_USER=" + shellQuote(user) + " /usr/local/bin/crabbox-start-desktop " + shellQuote(theme) + "\n" +
		"else\n" +
		"  echo 'crabbox desktop theme helper not installed' >&2\n" +
		"  exit 127\n" +
		"fi\n"
}

func webVNCGNOMEDesktopThemeFallbackCommand(theme, user string) string {
	return "  theme=" + shellQuote(theme) + "\n" +
		"  user=" + shellQuote(user) + "\n" +
		`  home_dir="$(getent passwd "$user" | cut -d: -f6)"
  if [ -z "$home_dir" ]; then
    home_dir="/home/$user"
  fi
  config_dir="$home_dir/.config"
  case "$theme" in
    light)
      gtk_theme=Adwaita
      gtk_prefer_dark_ini=0
      gsettings_scheme=prefer-light
      terminal_fg="#1f2937"
      terminal_bg="#f8fafc"
      labwc_title_bg="#f3f4f6"
      labwc_title_fg="#111827"
      labwc_inactive_title_bg="#e5e7eb"
      labwc_inactive_title_fg="#374151"
      labwc_border="#cbd5e1"
      terminal_menu_bg="#f3f4f6"
      terminal_menu_fg="#111827"
      terminal_menu_hover_bg="#e5e7eb"
      wallpaper_bg="#e7eef7"
      wallpaper_panel="#d6e7f2"
      wallpaper_accent="#0891b2"
      wallpaper_grid="#b9c7d7"
      ;;
    *)
      theme=dark
      gtk_theme=Adwaita-dark
      gtk_prefer_dark_ini=1
      gsettings_scheme=prefer-dark
      terminal_fg="#e5e7eb"
      terminal_bg="#000000"
      labwc_title_bg="#1f2329"
      labwc_title_fg="#e5e7eb"
      labwc_inactive_title_bg="#111827"
      labwc_inactive_title_fg="#9ca3af"
      labwc_border="#30363d"
      terminal_menu_bg="#2b2f36"
      terminal_menu_fg="#d1d5db"
      terminal_menu_hover_bg="#374151"
      wallpaper_bg="#0d1117"
      wallpaper_panel="#111827"
      wallpaper_accent="#22d3ee"
      wallpaper_grid="#1f2937"
      ;;
  esac
  mkdir -p "$config_dir/crabbox" "$config_dir/gtk-3.0" "$config_dir/gtk-4.0" "$config_dir/labwc"
  chmod 0700 "$config_dir" "$config_dir/crabbox" "$config_dir/gtk-3.0" "$config_dir/gtk-4.0" "$config_dir/labwc" 2>/dev/null || true
  printf '%s\n' "$theme" > "$config_dir/crabbox/desktop-theme"
  for gtk_dir in "$config_dir/gtk-3.0" "$config_dir/gtk-4.0"; do
    cat > "$gtk_dir/settings.ini" <<EOF
[Settings]
gtk-theme-name=$gtk_theme
gtk-icon-theme-name=Adwaita
gtk-application-prefer-dark-theme=$gtk_prefer_dark_ini
EOF
  done
  cat > "$home_dir/.gtkrc-2.0" <<EOF
gtk-theme-name="$gtk_theme"
gtk-icon-theme-name="Adwaita"
gtk-application-prefer-dark-theme=$gtk_prefer_dark_ini
EOF
  cat > "$config_dir/gtk-3.0/gtk.css" <<EOF
menubar, .menubar {
  background-color: $terminal_menu_bg;
  color: $terminal_menu_fg;
}
menubar menuitem, menubar menuitem label {
  color: $terminal_menu_fg;
}
menubar menuitem:hover {
  background-color: $terminal_menu_hover_bg;
  color: $terminal_menu_fg;
}
EOF
  . /var/lib/crabbox/desktop.env
  display="${DISPLAY:-:0}"
  runtime="${XDG_RUNTIME_DIR:-/tmp/crabbox-runtime-$(id -u "$user")}"
  dbus_address="${DBUS_SESSION_BUS_ADDRESS:-}"
  if [ -z "$dbus_address" ]; then
    labwc_pid="$(pgrep -u "$user" -n -x labwc 2>/dev/null || true)"
    if [ -n "$labwc_pid" ] && [ -r "/proc/$labwc_pid/environ" ]; then
      dbus_address="$(tr '\0' '\n' < "/proc/$labwc_pid/environ" | sed -n 's/^DBUS_SESSION_BUS_ADDRESS=//p' | head -n1)"
    fi
  fi
  if command -v gsettings >/dev/null 2>&1; then
    DISPLAY="$display" XDG_RUNTIME_DIR="$runtime" DBUS_SESSION_BUS_ADDRESS="$dbus_address" GDK_BACKEND=x11 gsettings set org.gnome.desktop.interface color-scheme "$gsettings_scheme" >/dev/null 2>&1 || true
    DISPLAY="$display" XDG_RUNTIME_DIR="$runtime" DBUS_SESSION_BUS_ADDRESS="$dbus_address" GDK_BACKEND=x11 gsettings set org.gnome.desktop.interface gtk-theme "$gtk_theme" >/dev/null 2>&1 || true
    profiles="$(DISPLAY="$display" XDG_RUNTIME_DIR="$runtime" DBUS_SESSION_BUS_ADDRESS="$dbus_address" GDK_BACKEND=x11 gsettings get org.gnome.Terminal.ProfilesList list 2>/dev/null | tr -d "[],'" || true)"
    default_profile="$(DISPLAY="$display" XDG_RUNTIME_DIR="$runtime" DBUS_SESSION_BUS_ADDRESS="$dbus_address" GDK_BACKEND=x11 gsettings get org.gnome.Terminal.ProfilesList default 2>/dev/null | tr -d "'" || true)"
    if [ -n "$default_profile" ] && ! printf ' %s ' "$profiles" | grep -q " $default_profile "; then
      profiles="$profiles $default_profile"
    fi
    for profile in $profiles; do
      [ -n "$profile" ] || continue
      profile_path="/org/gnome/terminal/legacy/profiles:/:$profile/"
      DISPLAY="$display" XDG_RUNTIME_DIR="$runtime" DBUS_SESSION_BUS_ADDRESS="$dbus_address" GDK_BACKEND=x11 gsettings set "org.gnome.Terminal.Legacy.Profile:$profile_path" use-theme-colors false >/dev/null 2>&1 || true
      DISPLAY="$display" XDG_RUNTIME_DIR="$runtime" DBUS_SESSION_BUS_ADDRESS="$dbus_address" GDK_BACKEND=x11 gsettings set "org.gnome.Terminal.Legacy.Profile:$profile_path" foreground-color "$terminal_fg" >/dev/null 2>&1 || true
      DISPLAY="$display" XDG_RUNTIME_DIR="$runtime" DBUS_SESSION_BUS_ADDRESS="$dbus_address" GDK_BACKEND=x11 gsettings set "org.gnome.Terminal.Legacy.Profile:$profile_path" background-color "$terminal_bg" >/dev/null 2>&1 || true
      DISPLAY="$display" XDG_RUNTIME_DIR="$runtime" DBUS_SESSION_BUS_ADDRESS="$dbus_address" GDK_BACKEND=x11 gsettings set "org.gnome.Terminal.Legacy.Profile:$profile_path" use-transparent-background false >/dev/null 2>&1 || true
    done
  fi
  cat > "$config_dir/labwc/themerc-override" <<EOF
window.active.title.bg.color: $labwc_title_bg
window.active.label.text.color: $labwc_title_fg
window.inactive.title.bg.color: $labwc_inactive_title_bg
window.inactive.label.text.color: $labwc_inactive_title_fg
window.active.border.color: $labwc_border
window.inactive.border.color: $labwc_border
window.active.button.unpressed.image.color: $labwc_title_fg
window.inactive.button.unpressed.image.color: $labwc_inactive_title_fg
window.active.button.hover.image.color: $labwc_title_fg
window.inactive.button.hover.image.color: $labwc_inactive_title_fg
window.active.button.pressed.image.color: $labwc_title_fg
window.inactive.button.pressed.image.color: $labwc_inactive_title_fg
EOF
  if command -v labwc >/dev/null 2>&1; then
    labwc_pid="$(pgrep -u "$user" -n -x labwc 2>/dev/null || true)"
    if [ -n "$labwc_pid" ]; then
      LABWC_PID="$labwc_pid" XDG_RUNTIME_DIR="$runtime" WAYLAND_DISPLAY="${WAYLAND_DISPLAY:-wayland-0}" labwc --reconfigure >/dev/null 2>&1 || kill -HUP "$labwc_pid" >/dev/null 2>&1 || true
    fi
  fi
  wallpaper_file="$config_dir/crabbox/desktop-background-$theme.svg"
  cat > "$wallpaper_file" <<EOF
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1920 1080">
  <rect width="1920" height="1080" fill="$wallpaper_bg"/>
  <path d="M0 720 C360 620 520 760 860 650 C1210 540 1430 660 1920 520 L1920 1080 L0 1080 Z" fill="$wallpaper_panel"/>
  <g stroke="$wallpaper_grid" stroke-width="1" opacity="0.45">
    <path d="M0 180 H1920M0 360 H1920M0 540 H1920M0 720 H1920M0 900 H1920"/>
    <path d="M240 0 V1080M480 0 V1080M720 0 V1080M960 0 V1080M1200 0 V1080M1440 0 V1080M1680 0 V1080"/>
  </g>
  <path d="M220 740 C520 520 790 910 1090 670 S1510 520 1710 700" fill="none" stroke="$wallpaper_accent" stroke-width="18" stroke-linecap="round" opacity="0.8"/>
  <rect x="1320" y="180" width="360" height="170" rx="18" fill="$wallpaper_accent" opacity="0.12"/>
</svg>
EOF
  if command -v swaybg >/dev/null 2>&1; then
    pkill -u "$user" -x swaybg >/dev/null 2>&1 || true
    (XDG_RUNTIME_DIR="$runtime" WAYLAND_DISPLAY="${WAYLAND_DISPLAY:-wayland-0}" swaybg -i "$wallpaper_file" -m fill >/tmp/crabbox-swaybg.log 2>&1 || XDG_RUNTIME_DIR="$runtime" WAYLAND_DISPLAY="${WAYLAND_DISPLAY:-wayland-0}" swaybg -c "$wallpaper_bg" >/tmp/crabbox-swaybg.log 2>&1) &
  fi
  if pgrep -u "$user" -x gnome-panel >/dev/null 2>&1; then
    pkill -TERM -u "$user" -x gnome-panel >/dev/null 2>&1 || true
    DISPLAY="$display" XDG_RUNTIME_DIR="$runtime" DBUS_SESSION_BUS_ADDRESS="$dbus_address" GDK_BACKEND=x11 GTK_THEME="$gtk_theme" nohup gnome-panel >/tmp/crabbox-gnome-panel.log 2>&1 &
  fi
  previous_terminal_theme="$(cat "$config_dir/crabbox/gnome-terminal-theme" 2>/dev/null || true)"
  printf '%s\n' "$theme" > "$config_dir/crabbox/gnome-terminal-theme"
  if [ "$theme" = dark ] && command -v gnome-terminal >/dev/null 2>&1 && { [ "$previous_terminal_theme" != "$theme" ] || ! pgrep -u "$user" -f '/gnome-terminal-server' >/dev/null 2>&1; }; then
    (sleep 0.4; DISPLAY="$display" XDG_RUNTIME_DIR="$runtime" DBUS_SESSION_BUS_ADDRESS="$dbus_address" GDK_BACKEND=x11 GTK_THEME="$gtk_theme" NO_AT_BRIDGE=1 gnome-terminal -- bash -l >/tmp/crabbox-gnome-terminal.log 2>&1 &) >/dev/null 2>&1 &
  fi
`
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

func (c *CoordinatorClient) webVNCEdgeHeaders() http.Header {
	header := http.Header{}
	c.addAccessHeaders(header)
	return header
}

func (c *CoordinatorClient) webVNCAccessHeaders(ctx context.Context) (http.Header, error) {
	header := c.webVNCEdgeHeaders()
	token, err := c.authorizationToken(ctx)
	if err != nil {
		return nil, err
	}
	if token != "" {
		header.Set("Authorization", "Bearer "+token)
	}
	return header, nil
}

func bridgeTicketHeaders(coord *CoordinatorClient, ticket string) http.Header {
	headers := coord.webVNCEdgeHeaders()
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
	return resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden
}

func webVNCAgentURL(base, leaseID string) string {
	return webVNCAgentURLWithCapabilities(base, leaseID, "")
}

func webVNCAgentURLWithCapabilities(base, leaseID, capabilities string) string {
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
	if strings.TrimSpace(capabilities) != "" {
		values.Set("capabilities", capabilities)
	}
	u.RawQuery = values.Encode()
	u.Fragment = ""
	return u.String()
}

func webVNCAgentURLWithTicket(base, leaseID, ticket string) string {
	return webVNCAgentURLWithTicketAndCapabilities(base, leaseID, ticket, "")
}

func webVNCAgentURLWithTicketAndCapabilities(base, leaseID, ticket, capabilities string) string {
	u, err := url.Parse(webVNCAgentURLWithCapabilities(base, leaseID, capabilities))
	if err != nil {
		return base
	}
	values := u.Query()
	values.Set("ticket", ticket)
	u.RawQuery = values.Encode()
	u.Fragment = ""
	return u.String()
}

type webVNCPortalOptions struct {
	TakeControl bool
}

func webVNCPortalURL(base, leaseID, username, password string, opts ...webVNCPortalOptions) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/portal/leases/" + url.PathEscape(leaseID) + "/vnc"
	u.RawQuery = ""
	u.Fragment = ""
	u.RawFragment = ""
	takeControl := false
	for _, opt := range opts {
		takeControl = takeControl || opt.TakeControl
	}
	if strings.TrimSpace(username) != "" || strings.TrimSpace(password) != "" || takeControl {
		values := url.Values{}
		if strings.TrimSpace(username) != "" {
			values.Set("username", strings.TrimSpace(username))
		}
		if strings.TrimSpace(password) != "" {
			values.Set("password", strings.TrimSpace(password))
		}
		if takeControl {
			values.Set("control", "take")
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
