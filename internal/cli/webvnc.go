package cli

import (
	"context"
	"crypto/des"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
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
	"sync"
	"time"

	"nhooyr.io/websocket"
)

const (
	webVNCDaemonPortReservationEnv   = "CRABBOX_WEBVNC_PORT_RESERVATION"
	webVNCDaemonPortReservationFDEnv = "CRABBOX_WEBVNC_PORT_RESERVATION_FD"
)

type webVNCExpectedProviderIdentity struct {
	Identity ProviderIdentityExpectation
	Scope    string
	set      bool
}

type webVNCExpectedProviderIdentityFlags struct {
	leaseID        *string
	attemptLeaseID *string
	slug           *string
	resourceID     *string
	scope          *string
}

func registerWebVNCExpectedProviderIdentityFlags(fs *flag.FlagSet) webVNCExpectedProviderIdentityFlags {
	return webVNCExpectedProviderIdentityFlags{
		leaseID:        fs.String("expected-provider-lease-id", "", "internal: persisted provider lease identity"),
		attemptLeaseID: fs.String("expected-provider-attempt-lease-id", "", "internal: persisted provider attempt identity"),
		slug:           fs.String("expected-provider-slug", "", "internal: persisted provider slug identity"),
		resourceID:     fs.String("expected-provider-resource-id", "", "internal: persisted provider resource identity"),
		scope:          fs.String("expected-provider-scope", "", "internal: persisted provider routing scope"),
	}
}

func (f webVNCExpectedProviderIdentityFlags) value(fs *flag.FlagSet) (webVNCExpectedProviderIdentity, error) {
	names := []string{
		"expected-provider-lease-id",
		"expected-provider-attempt-lease-id",
		"expected-provider-slug",
		"expected-provider-resource-id",
		"expected-provider-scope",
	}
	seen := 0
	for _, name := range names {
		if flagWasSet(fs, name) {
			seen++
		}
	}
	if seen == 0 {
		return webVNCExpectedProviderIdentity{}, nil
	}
	if seen != len(names) {
		return webVNCExpectedProviderIdentity{}, exit(2, "controller WebVNC requires the complete expected provider identity")
	}
	expected := webVNCExpectedProviderIdentity{
		Identity: ProviderIdentityExpectation{
			LeaseID:        strings.TrimSpace(*f.leaseID),
			AttemptLeaseID: strings.TrimSpace(*f.attemptLeaseID),
			Slug:           strings.TrimSpace(*f.slug),
			ResourceID:     strings.TrimSpace(*f.resourceID),
		},
		Scope: strings.TrimSpace(*f.scope),
		set:   true,
	}
	if expected.Identity.LeaseID == "" || expected.Identity.AttemptLeaseID == "" || expected.Identity.Slug == "" || expected.Identity.ResourceID == "" || expected.Scope == "" {
		return webVNCExpectedProviderIdentity{}, exit(2, "controller WebVNC expected provider identity fields must be non-empty")
	}
	if *f.leaseID != expected.Identity.LeaseID || *f.attemptLeaseID != expected.Identity.AttemptLeaseID || *f.slug != expected.Identity.Slug || *f.resourceID != expected.Identity.ResourceID {
		return webVNCExpectedProviderIdentity{}, exit(2, "invalid expected provider identity")
	}
	if err := ValidateProviderIdentityExpectation(expected.Identity); err != nil {
		return webVNCExpectedProviderIdentity{}, err
	}
	if *f.scope != expected.Scope || !validControllerInventoryIdentity(expected.Scope) {
		return webVNCExpectedProviderIdentity{}, exit(2, "invalid expected provider scope")
	}
	return expected, nil
}

func (e webVNCExpectedProviderIdentity) args() []string {
	if !e.set {
		return nil
	}
	return []string{
		"--expected-provider-lease-id", e.Identity.LeaseID,
		"--expected-provider-attempt-lease-id", e.Identity.AttemptLeaseID,
		"--expected-provider-slug", e.Identity.Slug,
		"--expected-provider-resource-id", e.Identity.ResourceID,
		"--expected-provider-scope", e.Scope,
	}
}

func validateWebVNCResolvedProviderIdentity(cfg Config, server Server, target SSHTarget, leaseID string, expected webVNCExpectedProviderIdentity) error {
	if !expected.set {
		return nil
	}
	provider, scope, _, err := controllerProviderIdentityForConfig(cfg)
	if err != nil {
		return fmt.Errorf("resolve WebVNC provider scope: %w", err)
	}
	if scope != expected.Scope {
		return exit(4, "provider=%s scope mismatch before WebVNC: expected %s, found %s", provider, expected.Scope, scope)
	}
	if err := ValidateLeaseTargetProviderIdentity(LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, expected.Identity); err != nil {
		return fmt.Errorf("validate WebVNC provider identity: %w", err)
	}
	return nil
}

func (a App) webvnc(ctx context.Context, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "local":
			return a.webVNCLocal(ctx, args[1:])
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
		fmt.Fprintln(fs.Output(), "  crabbox webvnc local --vnc-host 127.0.0.1 --vnc-port <port> --username <user> --password-stdin [--security-type auto|vnc] [--local-port <port>] [--open]")
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
		fmt.Fprintln(fs.Output(), "  --redact-credentials=false  reveal viewer credentials (unsafe)")
		fmt.Fprintln(fs.Output(), "  --reclaim")
	}
	provider := fs.String("provider", defaults.Provider, "desktop SSH provider")
	id := fs.String("id", "", "lease id or slug")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	noProviderSideEffects := fs.Bool("no-provider-side-effects", false, "internal: resolve without claiming, touching, or heartbeating the provider")
	controllerOwnerID := fs.String("controller-owner-id", "", "internal: controller ownership identity")
	localPort := fs.String("local-port", "", "local VNC tunnel port")
	openPortal := fs.Bool("open", false, "open the web portal VNC page")
	takeControl := fs.Bool("take-control", false, "ask the portal viewer to take keyboard and mouse control after connecting")
	redactCredentials := registerWebVNCCredentialOutputFlag(fs)
	daemon := fs.Bool("daemon", false, "compatibility alias for daemon start")
	background := fs.Bool("background", false, "compatibility alias for daemon start")
	daemonStatus := fs.Bool("status", false, "compatibility alias for daemon status")
	stopDaemon := fs.Bool("stop", false, "compatibility alias for daemon stop")
	providerFlags := registerProviderFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	targetFlags := registerTargetFlags(fs, defaults)
	expectedIdentityFlags := registerWebVNCExpectedProviderIdentityFlags(fs)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	expectedIdentity, err := expectedIdentityFlags.value(fs)
	if err != nil {
		return err
	}
	*controllerOwnerID = strings.TrimSpace(*controllerOwnerID)
	if *controllerOwnerID != "" && !validWebVNCControllerOwnerID(*controllerOwnerID) {
		return exit(2, "--controller-owner-id must be a valid controller ownership identity")
	}
	if *redactCredentials {
		a.Stdout = webVNCRedactingWriter{Writer: a.Stdout}
	}
	setIDFromFirstArg(fs, id)
	if *id == "" {
		return exit(2, "usage: crabbox webvnc --id <lease-id-or-slug>")
	}
	if *noProviderSideEffects && *reclaim {
		return exit(2, "--no-provider-side-effects cannot be combined with --reclaim")
	}
	if *controllerOwnerID != "" {
		if !*noProviderSideEffects || !expectedIdentity.set {
			return exit(2, "--controller-owner-id requires controller-owned resolution with the complete expected provider identity")
		}
	}
	if *daemonStatus {
		return a.webVNCDaemonStatus(*id, *controllerOwnerID)
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
	cfg, _, err = macOSPortalWebVNCConfigForLease(cfg, *id)
	if err != nil {
		return err
	}
	if useDirectSSHWebVNC(cfg) {
		// macOS leases (e.g. tart) have no guest-side noVNC/websockify; serve the
		// browser viewer from a host-side bridge over the guest's native Screen
		// Sharing instead of the Linux directSSHWebVNC path.
		if isMacOSDesktopProvider(cfg) {
			return a.macOSWebVNCBridge(ctx, cfg, *id, *localPort, *openPortal, *reclaim, *noProviderSideEffects, expectedIdentity)
		}
		return a.directSSHWebVNC(ctx, cfg, *id, *localPort, *openPortal, *takeControl, *reclaim, *noProviderSideEffects, expectedIdentity, *controllerOwnerID)
	}
	var inheritedListener net.Listener
	if inheritedWebVNCDaemonPortReservation(*localPort) {
		inheritedListener, err = inheritedWebVNCDaemonListener(*localPort)
		if err != nil {
			return exit(5, "adopt local WebVNC daemon listener: %v", err)
		}
		defer inheritedListener.Close()
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
	server, target, leaseID, err := a.resolveWebVNCLeaseTarget(ctx, cfg, *id, *reclaim, *noProviderSideEffects, expectedIdentity)
	if err != nil {
		return err
	}
	if err := enforceManagedLeaseCapabilities(cfg, server, leaseID); err != nil {
		return err
	}
	if !*noProviderSideEffects {
		if err := a.claimAndTouchLeaseTarget(ctx, cfg, server, target, leaseID, *reclaim); err != nil {
			return err
		}
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
	credentials, err := resolveWebVNCPortalCredentials(ctx, cfg, target, endpoint, runSSHOutput)
	if err != nil {
		return err
	}
	username := credentials.Username
	password := credentials.Password

	connHost := endpoint.Host
	connPort := endpoint.Port
	var tunnel *vncForegroundTunnel
	var proxyDone <-chan error
	if !endpoint.Direct {
		requestedTunnelPort := *localPort
		excludedTunnelPorts := []string(nil)
		if inheritedListener != nil {
			requestedTunnelPort = ""
			excludedTunnelPorts = append(excludedTunnelPorts, *localPort)
		}
		var tunnelPort string
		tunnel, tunnelPort, err = startVNCForegroundTunnelOnReservedPort(ctx, target, requestedTunnelPort, endpoint.Host, endpoint.Port, excludedTunnelPorts...)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "bridge: starting SSH tunnel localhost:%s -> %s:%s\n", tunnelPort, endpoint.Host, endpoint.Port)
		defer stopProcess(tunnel)
		connHost = "127.0.0.1"
		connPort = tunnelPort
		if inheritedListener != nil {
			proxyDone = serveWebVNCLoopbackProxy(ctx, inheritedListener, tunnelPort, tunnel.PID())
			connPort = *localPort
		} else {
			*localPort = tunnelPort
		}
	}

	rescueCtx := rescueContext{Cfg: cfg, Target: target, LeaseID: leaseID}
	opened := false
	bridgeCtx := ctx
	if tunnel != nil || proxyDone != nil {
		var cancelBridge context.CancelCauseFunc
		bridgeCtx, cancelBridge = context.WithCancelCause(ctx)
		defer cancelBridge(context.Canceled)
		go func() {
			select {
			case <-tunnel.Done():
				cancelBridge(tunnel.ExitError())
			case proxyErr := <-proxyDone:
				cancelBridge(proxyErr)
			case <-bridgeCtx.Done():
			}
		}()
	}
	return serveWebVNCBridgePool(bridgeCtx, webVNCBridgePoolConfig{
		Coord:            coord,
		LeaseID:          leaseID,
		Host:             connHost,
		Port:             connPort,
		PoolSize:         webVNCBridgePoolSizeForTarget(target),
		IdleTimeout:      cfg.IdleTimeout,
		Telemetry:        leaseTelemetryCollectorForTarget(target),
		DisableHeartbeat: *noProviderSideEffects,
		RescueCtx:        rescueCtx,
		NativeVNC:        nativeVNCOpenCommand(cfg, target, leaseID),
		Log:              a.Stdout,
		OnReady: func() error {
			portalUsername, portalPassword := "", ""
			if *openPortal || !*redactCredentials {
				portalUsername, portalPassword = username, password
			}
			portal, err := createWebVNCPortalURL(ctx, coord, leaseID, portalUsername, portalPassword, webVNCPortalOptions{TakeControl: *takeControl})
			if err != nil {
				return err
			}
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
	Coord            *CoordinatorClient
	LeaseID          string
	Host             string
	Port             string
	PoolSize         int
	IdleTimeout      time.Duration
	Telemetry        leaseTelemetryCollector
	DisableHeartbeat bool
	RescueCtx        rescueContext
	NativeVNC        string
	Log              io.Writer
	OnReady          func() error
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
	if !cfg.DisableHeartbeat && cfg.Coord != nil && strings.TrimSpace(cfg.LeaseID) != "" {
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
			var kind string
			// Keep attempt outside this block so consecutive failures increase the retry delay.
			attempt, kind = nextWebVNCBridgeFailure(connectedOnce, attempt)
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
	controllerOwned := fs.Bool("controller-owned", false, "internal: controller-owned persistent bridge")
	controllerOwnerID := fs.String("controller-owner-id", "", "internal: controller ownership identity")
	expectedIdentityFlags := registerWebVNCExpectedProviderIdentityFlags(fs)
	providerFlags := registerProviderFlags(fs, defaults)
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	expectedIdentity, err := expectedIdentityFlags.value(fs)
	if err != nil {
		return err
	}
	*controllerOwnerID = strings.TrimSpace(*controllerOwnerID)
	if *controllerOwnerID != "" && !validWebVNCControllerOwnerID(*controllerOwnerID) {
		return exit(2, "--controller-owner-id must be a valid controller ownership identity")
	}
	setIDFromFirstArg(fs, id)
	if *id == "" {
		return exit(2, "usage: crabbox webvnc daemon start --id <lease-id-or-slug>")
	}
	if *controllerOwned && *reclaim {
		return exit(2, "--controller-owned cannot be combined with --reclaim")
	}
	if *controllerOwned {
		if *controllerOwnerID == "" {
			return exit(2, "--controller-owned requires --controller-owner-id")
		}
		if !expectedIdentity.set {
			return exit(2, "--controller-owned requires the complete expected provider identity")
		}
	} else if *controllerOwnerID != "" {
		return exit(2, "--controller-owner-id requires --controller-owned")
	}
	cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{LeaseID: *id, Desktop: true})
	if err != nil {
		return err
	}
	if err := applyProviderFlags(&cfg, fs, providerFlags); err != nil {
		return err
	}
	cfg, _, err = macOSPortalWebVNCConfigForLease(cfg, *id)
	if err != nil {
		return err
	}
	target := SSHTarget{TargetOS: cfg.TargetOS, WindowsMode: cfg.WindowsMode}
	bridgeID := *id
	identityValidated := false
	if useDirectSSHWebVNC(cfg) {
		if err := guardMacOSDirectWebVNC(cfg); err != nil {
			return err
		}
		var server Server
		var resolvedTarget SSHTarget
		var leaseID string
		if *controllerOwned {
			server, resolvedTarget, leaseID, err = a.resolveNetworkLeaseTargetReadOnly(ctx, cfg, *id, expectedIdentity.Identity)
		} else {
			server, resolvedTarget, leaseID, err = a.resolveNetworkLeaseTargetForRepo(ctx, cfg, *id, false, *reclaim)
		}
		if err != nil {
			return err
		}
		if err := validateWebVNCResolvedProviderIdentity(cfg, server, resolvedTarget, leaseID, expectedIdentity); err != nil {
			return err
		}
		identityValidated = expectedIdentity.set
		if err := enforceManagedLeaseCapabilities(cfg, server, leaseID); err != nil {
			return err
		}
		if !*controllerOwned {
			if err := a.claimAndTouchLeaseTarget(ctx, cfg, server, resolvedTarget, leaseID, *reclaim); err != nil {
				return err
			}
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
		if useCoordinator && coord != nil && coord.hasConfiguredAuth() {
			var server Server
			var resolvedTarget SSHTarget
			var leaseID string
			if *controllerOwned {
				server, resolvedTarget, leaseID, err = a.resolveNetworkLeaseTargetReadOnly(ctx, cfg, *id, expectedIdentity.Identity)
			} else {
				server, resolvedTarget, leaseID, err = a.resolveNetworkLeaseTargetForRepo(ctx, cfg, *id, false, *reclaim)
			}
			if err != nil {
				return err
			}
			if err := validateWebVNCResolvedProviderIdentity(cfg, server, resolvedTarget, leaseID, expectedIdentity); err != nil {
				return err
			}
			identityValidated = expectedIdentity.set
			if err := enforceManagedLeaseCapabilities(cfg, server, leaseID); err != nil {
				return err
			}
			if !*controllerOwned {
				if err := a.claimAndTouchLeaseTarget(ctx, cfg, server, resolvedTarget, leaseID, *reclaim); err != nil {
					return err
				}
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
	if expectedIdentity.set && !identityValidated {
		return exit(4, "controller WebVNC provider identity could not be resolved and validated")
	}
	daemonArgs := webVNCBridgeArgs(cfg, target, bridgeID, *openPortal, *takeControl)
	if strings.TrimSpace(*localPort) != "" {
		daemonArgs = append(daemonArgs, "--local-port", strings.TrimSpace(*localPort))
	}
	if *reclaim {
		daemonArgs = append(daemonArgs, "--reclaim")
	}
	daemonArgs = append(daemonArgs, expectedIdentity.args()...)
	return a.startWebVNCDaemon(daemonArgs, *id, *controllerOwned, *controllerOwnerID)
}

type webVNCRedactingWriter struct {
	io.Writer
}

func registerWebVNCCredentialOutputFlag(fs *flag.FlagSet) *bool {
	return fs.Bool("redact-credentials", true, "omit credential-bearing URLs and passwords from output; set false to reveal (unsafe)")
}

func (w webVNCRedactingWriter) Write(data []byte) (int, error) {
	text := string(data)
	lines := strings.SplitAfter(text, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		for _, prefix := range []string{"password:", "username:", "webvnc:", "opened:"} {
			if strings.HasPrefix(trimmed, prefix) {
				if (prefix == "webvnc:" || prefix == "opened:") &&
					!webVNCOutputURLHasCredentials(strings.TrimSpace(strings.TrimPrefix(trimmed, prefix))) {
					break
				}
				ending := ""
				if strings.HasSuffix(line, "\n") {
					ending = "\n"
				}
				lines[i] = prefix + " [redacted]" + ending
				break
			}
		}
	}
	_, err := io.WriteString(w.Writer, strings.Join(lines, ""))
	if err != nil {
		return 0, err
	}
	return len(data), nil
}

func webVNCOutputURLHasCredentials(value string) bool {
	if strings.Contains(value, "password=") || strings.Contains(value, "username=") || strings.Contains(value, "handoff=") {
		return true
	}
	parsed, err := url.Parse(value)
	return err == nil && parsed.User != nil
}

func (a App) webVNCDaemonStatusCommand(args []string) error {
	fs := newFlagSet("webvnc daemon status", a.Stderr)
	id := fs.String("id", "", "lease id or slug")
	controllerOwnerID := fs.String("controller-owner-id", "", "internal: controller ownership identity")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	*controllerOwnerID = strings.TrimSpace(*controllerOwnerID)
	if *controllerOwnerID != "" && !validWebVNCControllerOwnerID(*controllerOwnerID) {
		return exit(2, "--controller-owner-id must be a valid controller ownership identity")
	}
	setIDFromFirstArg(fs, id)
	if *id == "" {
		return exit(2, "usage: crabbox webvnc daemon status --id <lease-id-or-slug>")
	}
	return a.webVNCDaemonStatus(*id, *controllerOwnerID)
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
			printLocalWebVNCDaemonStatus(a.Stdout, status, "")
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
	redactCredentials := registerWebVNCCredentialOutputFlag(fs)
	provider := fs.String("provider", defaults.Provider, "desktop SSH provider")
	id := fs.String("id", "", "lease id or slug")
	localPort := fs.String("local-port", "", "local VNC tunnel port")
	expectedListenerOwnerPID := fs.Int("expected-listener-owner-pid", 0, "internal: expected WebVNC listener owner pid")
	controllerOwnerID := fs.String("controller-owner-id", "", "internal: controller ownership identity")
	expectedIdentityFlags := registerWebVNCExpectedProviderIdentityFlags(fs)
	providerFlags := registerProviderFlags(fs, defaults)
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *redactCredentials {
		a.Stdout = webVNCRedactingWriter{Writer: a.Stdout}
	}
	expectedIdentity, err := expectedIdentityFlags.value(fs)
	if err != nil {
		return err
	}
	*controllerOwnerID = strings.TrimSpace(*controllerOwnerID)
	if *controllerOwnerID != "" && !validWebVNCControllerOwnerID(*controllerOwnerID) {
		return exit(2, "--controller-owner-id must be a valid controller ownership identity")
	}
	if *expectedListenerOwnerPID < 0 {
		return exit(2, "--expected-listener-owner-pid must be positive")
	}
	if *controllerOwnerID != "" {
		if !expectedIdentity.set {
			return exit(2, "--controller-owner-id requires the complete expected provider identity")
		}
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
	cfg, _, err = macOSPortalWebVNCConfigForLease(cfg, *id)
	if err != nil {
		return err
	}
	if useDirectSSHWebVNC(cfg) {
		if err := guardMacOSDirectWebVNC(cfg); err != nil {
			return err
		}
		if expectedIdentity.set && *expectedListenerOwnerPID == 0 {
			return exit(2, "controller WebVNC status requires --expected-listener-owner-pid")
		}
		if expectedIdentity.set && *controllerOwnerID == "" {
			return exit(2, "controller WebVNC status requires --controller-owner-id")
		}
		return a.directSSHWebVNCStatus(ctx, cfg, *id, *localPort, *expectedListenerOwnerPID, expectedIdentity, *controllerOwnerID)
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
	var server Server
	var target SSHTarget
	var leaseID string
	if expectedIdentity.set {
		server, target, leaseID, err = a.resolveNetworkLeaseTargetReadOnly(ctx, cfg, *id, expectedIdentity.Identity)
	} else {
		server, target, leaseID, err = a.resolveNetworkLeaseTarget(ctx, cfg, *id, false)
	}
	if err != nil {
		return err
	}
	if err := validateWebVNCResolvedProviderIdentity(cfg, server, target, leaseID, expectedIdentity); err != nil {
		return err
	}
	commandCfg := resolvedWebVNCCommandConfig(cfg, server, target)
	if err := enforceManagedLeaseCapabilities(cfg, server, leaseID); err != nil {
		return err
	}
	if *localPort == "" {
		*localPort = availableLocalVNCPort()
	}
	endpoint, endpointErr := resolveVNCEndpoint(ctx, cfg, &target)
	credentials := rfbCredentials{}
	if endpointErr == nil && !*redactCredentials {
		credentials, err = resolveWebVNCPortalCredentials(ctx, cfg, target, endpoint, runSSHOutput)
		if err != nil {
			return err
		}
	}
	username := credentials.Username
	password := credentials.Password
	status, statusErr := coord.WebVNCStatus(ctx, leaseID)
	daemon, daemonErr := localWebVNCDaemonStatus(leaseID)
	if daemonErr == nil && leaseID != *id {
		if aliasDaemon, err := localWebVNCDaemonStatus(*id); err == nil && !aliasDaemon.Missing {
			daemon = aliasDaemon
		}
	}
	fmt.Fprintf(a.Stdout, "lease: %s slug=%s provider=%s target=%s\n", leaseID, blank(serverSlug(server), "-"), blank(server.Provider, cfg.Provider), blank(target.TargetOS, cfg.TargetOS))
	rescueCtx := rescueContext{Cfg: commandCfg, Target: target, LeaseID: leaseID}
	if daemonErr != nil {
		fmt.Fprintf(a.Stdout, "webvnc daemon: error=%v\n", daemonErr)
	} else {
		printLocalWebVNCDaemonStatus(a.Stdout, daemon, "")
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
	portalUsername, portalPassword := "", ""
	if !*redactCredentials {
		portalUsername, portalPassword = username, password
	}
	portal, err := createWebVNCPortalURL(ctx, coord, leaseID, portalUsername, portalPassword)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "webvnc: %s\n", portal)
	if strings.TrimSpace(password) != "" {
		fmt.Fprintf(a.Stdout, "password: %s\n", strings.TrimSpace(password))
		if strings.TrimSpace(username) != "" {
			fmt.Fprintf(a.Stdout, "username: %s\n", strings.TrimSpace(username))
		}
	}
	fmt.Fprintf(a.Stdout, "fallback: %s\n", nativeVNCOpenCommand(commandCfg, target, leaseID))
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
	redactCredentials := registerWebVNCCredentialOutputFlag(fs)
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
	if *redactCredentials {
		a.Stdout = webVNCRedactingWriter{Writer: a.Stdout}
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
	cfg, automaticMacOSPortal, err := macOSPortalWebVNCConfigForLease(cfg, *id)
	if err != nil {
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
	commandCfg := resolvedWebVNCCommandConfig(cfg, server, target)
	if automaticMacOSPortal {
		if err := a.claimAndTouchLeaseTarget(ctx, cfg, server, target, leaseID, false); err != nil {
			return err
		}
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
	rescueCtx := rescueContext{Cfg: commandCfg, Target: target, LeaseID: leaseID}
	if out, err := runSSHCombinedOutput(ctx, target, webVNCResetRemoteCommand(target)); err != nil {
		printRescue(a.Stdout, classifyDesktopFailure(out), trimFailureDetail(out), desktopDoctorCommand(rescueCtx))
		return exit(5, "reset target WebVNC/input stack: %v", err)
	}
	credentials, err := resolveWebVNCPortalCredentials(ctx, cfg, target, vncEndpoint{Managed: true}, runSSHOutput)
	if err != nil {
		return err
	}
	username := credentials.Username
	password := credentials.Password
	portalUsername, portalPassword := "", ""
	if *openPortal || !*redactCredentials {
		portalUsername, portalPassword = username, password
	}
	portal, err := createWebVNCPortalURL(ctx, coord, leaseID, portalUsername, portalPassword, webVNCPortalOptions{TakeControl: *takeControl})
	if err != nil {
		return err
	}
	daemonArgs := webVNCBridgeArgs(commandCfg, target, leaseID, *openPortal, *takeControl)
	daemonName := *id
	if strings.TrimSpace(daemonName) == "" {
		daemonName = leaseID
	}
	if err := a.startWebVNCDaemon(daemonArgs, daemonName, false, ""); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "webvnc reset: lease=%s slug=%s\n", leaseID, blank(serverSlug(server), "-"))
	fmt.Fprintf(a.Stdout, "webvnc: %s\n", portal)
	if strings.TrimSpace(password) != "" {
		fmt.Fprintf(a.Stdout, "password: %s\n", strings.TrimSpace(password))
	}
	fmt.Fprintf(a.Stdout, "fallback: %s\n", nativeVNCOpenCommand(commandCfg, target, leaseID))
	return nil
}

func (a App) startWebVNCDaemon(args []string, leaseID string, controllerOwned bool, controllerOwnerID string) error {
	args = prepareWebVNCDaemonArgs(args, controllerOwned)
	localPort := webVNCDaemonLocalPortArg(args)
	if localPort != "" && !validWebVNCDaemonPort(localPort) {
		return exit(2, "invalid local WebVNC port %q", localPort)
	}
	if controllerOwned && !validWebVNCControllerOwnerID(controllerOwnerID) {
		return exit(2, "controller-owned WebVNC daemon requires a valid owner identity")
	}
	if !controllerOwned && strings.TrimSpace(controllerOwnerID) != "" {
		return exit(2, "ordinary WebVNC daemon cannot carry a controller owner identity")
	}
	if controllerOwned {
		args = append(args, "--controller-owner-id", controllerOwnerID)
	}
	unlock, err := acquireWebVNCDaemonLock(leaseID)
	if err != nil {
		return exit(2, "lock WebVNC daemon state: %v", err)
	}
	defer unlock()
	exe, err := os.Executable()
	if err != nil {
		return exit(2, "resolve crabbox executable: %v", err)
	}
	if stopped, err := a.stopWebVNCDaemonIfRunningLocked(leaseID); err != nil {
		return err
	} else if stopped {
		fmt.Fprintln(a.Stdout, "webvnc daemon: replacing previous daemon")
	}
	portReservation, err := reserveWebVNCDaemonPort(localPort)
	if err != nil {
		return exit(5, "%v", err)
	}
	defer portReservation.release()
	if localPort == "" {
		localPort = portReservation.port
		args = append(args, "--local-port", localPort)
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
	defer func() {
		if err := logFile.Close(); err != nil {
			fmt.Fprintf(a.Stderr, "warning: close WebVNC daemon log: %v\n", err)
		}
	}()
	childArgs := append([]string{"webvnc"}, args...)
	nonce, err := newWebVNCDaemonNonce()
	if err != nil {
		return exit(2, "create WebVNC daemon identity: %v", err)
	}
	gateReader, gateWriter, err := os.Pipe()
	if err != nil {
		return exit(2, "create WebVNC daemon launch gate: %v", err)
	}
	defer gateWriter.Close()
	supervisorScript := webVNCDaemonGatedSupervisorScript(exe, childArgs)
	cmd := exec.Command("sh", "-c", supervisorScript, "crabbox-webvnc-supervisor", nonce)
	cmd.Stdin = gateReader
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = webVNCDaemonPortReservationEnvironment(os.Environ(), "", "")
	configureDaemonCommand(cmd)
	descriptor, err := portReservation.inherit(cmd)
	if err != nil {
		_ = gateReader.Close()
		return exit(2, "inherit WebVNC daemon port reservation: %v", err)
	}
	cmd.Env = webVNCDaemonPortReservationEnvironment(cmd.Env, localPort, descriptor)
	if err := cmd.Start(); err != nil {
		_ = gateReader.Close()
		return exit(5, "start WebVNC daemon: %v", err)
	}
	_ = gateReader.Close()
	pid := cmd.Process.Pid
	gateMayHaveReleased := false
	stopStartedDaemon := func() error {
		_ = gateWriter.Close()
		if !gateMayHaveReleased {
			// The supervisor is still blocked in its launch gate and has no
			// descendants. EOF makes it exit without starting the bridge.
			_ = cmd.Wait()
			return nil
		}
		terminateErr := terminateWebVNCDaemonProcessTree(pid)
		_ = cmd.Wait()
		return terminateErr
	}
	failStartedDaemon := func(failure error, identityInstalled bool) error {
		cleanupErr := stopStartedDaemon()
		if identityInstalled && cleanupErr == nil {
			cleanupErr = removeWebVNCDaemonIdentity(pidPath)
		}
		if cleanupErr != nil {
			return fmt.Errorf("%w; cleanup WebVNC daemon pid %d: %v", failure, pid, cleanupErr)
		}
		return failure
	}
	if err := portReservation.handoff(); err != nil {
		return failStartedDaemon(exit(5, "handoff WebVNC daemon port reservation: %v", err), false)
	}
	started, err := webVNCDaemonProcessStartIdentity(pid)
	if err != nil {
		return failStartedDaemon(exit(5, "identify WebVNC daemon pid %d: %v", pid, err), false)
	}
	bootID, err := processBootIdentity()
	if err != nil {
		return failStartedDaemon(exit(5, "identify WebVNC daemon boot: %v", err), false)
	}
	identity := webVNCDaemonIdentity{
		Version:               webVNCDaemonIdentityVersion,
		WorkspaceID:           leaseID,
		PID:                   pid,
		LocalPort:             localPort,
		ProcessStarted:        started,
		BootID:                bootID,
		Nonce:                 nonce,
		ControllerOwned:       controllerOwned,
		NoProviderSideEffects: controllerOwned,
		ControllerOwnerID:     controllerOwnerID,
	}
	command, alive := webVNCDaemonProcessCommand(pid)
	if !alive || !webVNCDaemonIdentityMatchesProcess(identity, command, started) {
		return failStartedDaemon(exit(5, "new WebVNC daemon pid %d did not retain its process identity", pid), false)
	}
	if err := writeWebVNCDaemonIdentity(pidPath, identity); err != nil {
		return failStartedDaemon(exit(2, "write WebVNC daemon identity: %v", err), false)
	}
	gateMayHaveReleased = true
	if _, err := io.WriteString(gateWriter, "run\n"); err != nil {
		return failStartedDaemon(exit(5, "release WebVNC daemon launch gate: %v", err), true)
	}
	_ = gateWriter.Close()
	if err := cmd.Process.Release(); err != nil {
		return failStartedDaemon(exit(5, "release WebVNC daemon process: %v", err), true)
	}
	fmt.Fprintf(a.Stdout, "webvnc daemon: pid=%d log=%s\n", pid, logPath)
	fmt.Fprintf(a.Stdout, "webvnc daemon: local-port=%s\n", localPort)
	printWebVNCDaemonOwnership(a.Stdout, identity, "")
	if webVNCDaemonLogReady(logPath, 10*time.Second) {
		fmt.Fprintln(a.Stdout, "webvnc daemon: ready")
	} else {
		fmt.Fprintln(a.Stdout, "webvnc daemon: starting; run crabbox webvnc status --id <lease-id-or-slug> to check bridge readiness")
	}
	fmt.Fprintln(a.Stdout, "webvnc daemon: stop with crabbox webvnc daemon stop --id <lease-id-or-slug>")
	return nil
}

func prepareWebVNCDaemonArgs(args []string, controllerOwned bool) []string {
	out := make([]string, 0, len(args)+4)
	for _, arg := range args {
		if webVNCDaemonFlagArg(arg, "redact-credentials") ||
			webVNCDaemonFlagArg(arg, "reclaim") ||
			webVNCDaemonFlagArg(arg, "no-provider-side-effects") {
			continue
		}
		out = append(out, arg)
	}
	prefix := []string{"--redact-credentials=true"}
	if controllerOwned {
		prefix = append(prefix, "--no-provider-side-effects=true")
	}
	return append(prefix, out...)
}

func webVNCDaemonLocalPortArg(args []string) string {
	for index, arg := range args {
		for _, prefix := range []string{"--local-port=", "-local-port="} {
			if value, ok := strings.CutPrefix(arg, prefix); ok {
				return strings.TrimSpace(value)
			}
		}
		if (arg == "--local-port" || arg == "-local-port") && index+1 < len(args) {
			return strings.TrimSpace(args[index+1])
		}
	}
	return ""
}

type webVNCDaemonPortReservation struct {
	port          string
	tcpListener   *net.TCPListener
	inheritedFile *os.File
	inherited     bool
}

type webVNCTunnelPortReservation struct {
	port   string
	socket *net.UDPConn
}

func reserveWebVNCTunnelPort(requested string, excluded ...string) (*webVNCTunnelPortReservation, error) {
	requested = strings.TrimSpace(requested)
	excludedPorts := make(map[string]struct{}, len(excluded))
	for _, port := range excluded {
		port = strings.TrimSpace(port)
		if port != "" {
			excludedPorts[port] = struct{}{}
		}
	}
	ports := make([]string, 0, 99)
	if requested != "" {
		if !validWebVNCDaemonPort(requested) {
			return nil, fmt.Errorf("invalid local VNC tunnel port %q", requested)
		}
		ports = append(ports, requested)
	} else {
		for port := 5901; port <= 5999; port++ {
			ports = append(ports, strconv.Itoa(port))
		}
	}
	for _, port := range ports {
		if _, skip := excludedPorts[port]; skip {
			continue
		}
		portNumber, _ := strconv.Atoi(port)
		reservation, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: portNumber})
		if err != nil {
			if webVNCDaemonPortReservationUnavailable(err) {
				continue
			}
			return nil, fmt.Errorf("coordinate local VNC tunnel port %s: %w", port, err)
		}
		probe, probeErr := net.Listen("tcp4", net.JoinHostPort(vncLoopbackHost, port))
		if probeErr != nil {
			_ = reservation.Close()
			continue
		}
		_ = probe.Close()
		return &webVNCTunnelPortReservation{port: port, socket: reservation}, nil
	}
	if requested != "" {
		return nil, fmt.Errorf("local VNC tunnel port %s is reserved or already in use", requested)
	}
	return nil, fmt.Errorf("no unreserved local VNC tunnel port is available")
}

func (r *webVNCTunnelPortReservation) release() {
	if r == nil || r.socket == nil {
		return
	}
	socket := r.socket
	r.socket = nil
	_ = socket.Close()
}

func reserveWebVNCDaemonPort(requested string, excluded ...string) (*webVNCDaemonPortReservation, error) {
	requested = strings.TrimSpace(requested)
	excludedPorts := make(map[string]struct{}, len(excluded))
	for _, port := range excluded {
		port = strings.TrimSpace(port)
		if port != "" {
			excludedPorts[port] = struct{}{}
		}
	}
	ports := make([]string, 0, 99)
	if requested != "" {
		if !validWebVNCDaemonPort(requested) {
			return nil, fmt.Errorf("invalid local WebVNC port %q", requested)
		}
		ports = append(ports, requested)
	} else {
		for port := 5901; port <= 5999; port++ {
			ports = append(ports, strconv.Itoa(port))
		}
	}
	for _, port := range ports {
		if _, skip := excludedPorts[port]; skip {
			continue
		}
		listener, acquired, err := tryAcquireWebVNCDaemonPortReservation(port)
		if err != nil {
			return nil, fmt.Errorf("reserve local WebVNC port %s: %w", port, err)
		}
		if !acquired {
			continue
		}
		return &webVNCDaemonPortReservation{port: port, tcpListener: listener}, nil
	}
	if requested != "" {
		return nil, fmt.Errorf("local WebVNC port %s is reserved or already in use", requested)
	}
	return nil, fmt.Errorf("no unreserved local WebVNC port is available")
}

func tryAcquireWebVNCDaemonPortReservation(port string) (*net.TCPListener, bool, error) {
	portNumber, err := strconv.Atoi(port)
	if err != nil {
		return nil, false, err
	}
	reservation, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: portNumber})
	if err != nil {
		if webVNCDaemonPortReservationUnavailable(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return reservation, true, nil
}

func validWebVNCDaemonPort(value string) bool {
	port, err := strconv.Atoi(value)
	return err == nil && port >= 1 && port <= 65535 && strconv.Itoa(port) == value
}

func webVNCDaemonPortReservationEnvironment(environ []string, port, descriptor string) []string {
	portPrefix := webVNCDaemonPortReservationEnv + "="
	descriptorPrefix := webVNCDaemonPortReservationFDEnv + "="
	out := make([]string, 0, len(environ)+2)
	for _, entry := range environ {
		if !strings.HasPrefix(entry, portPrefix) && !strings.HasPrefix(entry, descriptorPrefix) {
			out = append(out, entry)
		}
	}
	if port != "" && descriptor != "" {
		out = append(out, portPrefix+port, descriptorPrefix+descriptor)
	}
	return out
}

func inheritedWebVNCDaemonPortReservation(port string) bool {
	port = strings.TrimSpace(port)
	return port != "" && strings.TrimSpace(os.Getenv(webVNCDaemonPortReservationEnv)) == port &&
		strings.TrimSpace(os.Getenv(webVNCDaemonPortReservationFDEnv)) != ""
}

func (r *webVNCDaemonPortReservation) inherit(cmd *exec.Cmd) (string, error) {
	if r == nil || r.tcpListener == nil || r.inherited {
		return "", fmt.Errorf("WebVNC daemon port reservation is unavailable")
	}
	descriptor, inheritedFile, err := inheritWebVNCDaemonPortReservation(cmd, r.tcpListener)
	if err != nil {
		return "", err
	}
	r.inheritedFile = inheritedFile
	r.inherited = true
	return descriptor, nil
}

func (r *webVNCDaemonPortReservation) listener() (net.Listener, error) {
	if r == nil || r.tcpListener == nil || r.inherited {
		return nil, fmt.Errorf("WebVNC daemon port reservation is unavailable")
	}
	listener := r.tcpListener
	r.tcpListener = nil
	return listener, nil
}

func acquireWebVNCLoopbackListener(requested string) (net.Listener, string, error) {
	requested = strings.TrimSpace(requested)
	if inheritedWebVNCDaemonPortReservation(requested) {
		listener, err := inheritedWebVNCDaemonListener(requested)
		return listener, requested, err
	}
	reservation, err := reserveWebVNCDaemonPort(requested)
	if err != nil {
		return nil, "", err
	}
	port := reservation.port
	listener, err := reservation.listener()
	if err != nil {
		reservation.release()
		return nil, "", err
	}
	return listener, port, nil
}

func inheritedWebVNCDaemonListener(port string) (net.Listener, error) {
	if !inheritedWebVNCDaemonPortReservation(port) {
		return nil, fmt.Errorf("inherited WebVNC daemon TCP listener is unavailable")
	}
	descriptor, err := strconv.ParseUint(strings.TrimSpace(os.Getenv(webVNCDaemonPortReservationFDEnv)), 10, strconv.IntSize)
	if err != nil || descriptor <= 2 {
		return nil, fmt.Errorf("inherited WebVNC daemon TCP listener descriptor is invalid")
	}
	file := os.NewFile(uintptr(descriptor), "crabbox-webvnc-listener")
	if file == nil {
		return nil, fmt.Errorf("open inherited WebVNC daemon TCP listener")
	}
	listener, err := net.FileListener(file)
	_ = file.Close()
	if err != nil {
		return nil, fmt.Errorf("adopt inherited WebVNC daemon TCP listener: %w", err)
	}
	tcpAddress, ok := listener.Addr().(*net.TCPAddr)
	if !ok || !tcpAddress.IP.Equal(net.IPv4(127, 0, 0, 1)) || strconv.Itoa(tcpAddress.Port) != port {
		_ = listener.Close()
		return nil, fmt.Errorf("inherited WebVNC daemon TCP listener address does not match 127.0.0.1:%s", port)
	}
	return listener, nil
}

func serveWebVNCLoopbackProxy(ctx context.Context, listener net.Listener, targetPort string, expectedOwnerPID ...int) <-chan error {
	done := make(chan error, 1)
	ownerPID := 0
	if len(expectedOwnerPID) > 0 {
		ownerPID = expectedOwnerPID[0]
	}
	proxyCtx, cancelProxy := context.WithCancelCause(ctx)
	var finishOnce sync.Once
	finish := func(err error) {
		finishOnce.Do(func() {
			if err == nil {
				err = context.Canceled
			}
			cancelProxy(err)
			_ = listener.Close()
			done <- err
		})
	}
	go func() {
		go func() {
			<-proxyCtx.Done()
			finish(context.Cause(proxyCtx))
		}()
		for {
			incoming, err := listener.Accept()
			if err != nil {
				if proxyCtx.Err() != nil {
					finish(context.Cause(proxyCtx))
				} else {
					finish(fmt.Errorf("accept local WebVNC proxy connection: %w", err))
				}
				return
			}
			go func() {
				if err := relayWebVNCLoopbackProxyConnection(proxyCtx, incoming, targetPort, ownerPID); err != nil && ownerPID > 0 {
					finish(err)
				}
			}()
		}
	}()
	return done
}

func relayWebVNCLoopbackProxyConnection(ctx context.Context, incoming net.Conn, targetPort string, expectedOwnerPID int) error {
	defer incoming.Close()
	if expectedOwnerPID > 0 {
		if err := controllerVerifyDaemonOwnedListener(targetPort, expectedOwnerPID); err != nil {
			return fmt.Errorf("verify local VNC tunnel listener before proxy connect: %w", err)
		}
	}
	outgoing, err := (&net.Dialer{}).DialContext(ctx, "tcp4", net.JoinHostPort(vncLoopbackHost, targetPort))
	if err != nil {
		return err
	}
	defer outgoing.Close()
	if expectedOwnerPID > 0 {
		if err := controllerVerifyDaemonOwnedListener(targetPort, expectedOwnerPID); err != nil {
			return fmt.Errorf("verify local VNC tunnel listener after proxy connect: %w", err)
		}
	}
	done := make(chan struct{}, 2)
	copyOneWay := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		done <- struct{}{}
	}
	go copyOneWay(outgoing, incoming)
	go copyOneWay(incoming, outgoing)
	select {
	case <-ctx.Done():
	case <-done:
	}
	return nil
}

func (r *webVNCDaemonPortReservation) handoff() error {
	if r == nil || r.tcpListener == nil {
		return fmt.Errorf("WebVNC daemon port reservation is unavailable")
	}
	// The supervisor already inherited this bound socket. Closing the parent's
	// copy leaves the kernel reservation live until the supervisor exits.
	listener := r.tcpListener
	r.tcpListener = nil
	listenerErr := listener.Close()
	var fileErr error
	if r.inheritedFile != nil {
		file := r.inheritedFile
		r.inheritedFile = nil
		fileErr = file.Close()
	}
	return errors.Join(listenerErr, fileErr)
}

func (r *webVNCDaemonPortReservation) release() {
	if r == nil {
		return
	}
	if r.tcpListener != nil {
		listener := r.tcpListener
		r.tcpListener = nil
		_ = listener.Close()
	}
	if r.inheritedFile != nil {
		file := r.inheritedFile
		r.inheritedFile = nil
		_ = file.Close()
	}
}

func webVNCDaemonFlagArg(arg, name string) bool {
	for _, prefix := range []string{"--" + name, "-" + name} {
		if arg == prefix || strings.HasPrefix(arg, prefix+"=") {
			return true
		}
	}
	return false
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

func webVNCDaemonGatedSupervisorScript(exe string, args []string) string {
	return "IFS= read -r gate || exit 125\n" +
		"[ \"$gate\" = run ] || exit 125\n" +
		webVNCDaemonSupervisorScript(exe, args)
}

func (a App) webVNCDaemonStatus(leaseID, expectedOwnerID string) error {
	status, err := localWebVNCDaemonStatus(leaseID)
	if err != nil {
		return err
	}
	printLocalWebVNCDaemonStatus(a.Stdout, status, expectedOwnerID)
	return nil
}

type localWebVNCDaemon struct {
	LeaseID               string
	LogPath               string
	PIDPath               string
	PID                   int
	LocalPort             string
	Command               string
	ControllerOwned       bool
	NoProviderSideEffects bool
	ControllerOwnerID     string
	Alive                 bool
	Stale                 bool
	Missing               bool
}

const webVNCDaemonIdentityVersion = 1

type webVNCDaemonIdentity struct {
	Version               int    `json:"version"`
	WorkspaceID           string `json:"workspaceId"`
	PID                   int    `json:"pid"`
	LocalPort             string `json:"localPort,omitempty"`
	ProcessStarted        string `json:"processStarted"`
	BootID                string `json:"bootId,omitempty"`
	Nonce                 string `json:"nonce"`
	ControllerOwned       bool   `json:"controllerOwned,omitempty"`
	NoProviderSideEffects bool   `json:"noProviderSideEffects,omitempty"`
	ControllerOwnerID     string `json:"controllerOwnerId,omitempty"`
	LegacyOwnerToken      string `json:"controllerOwnerToken,omitempty"`
}

func localWebVNCDaemonStatus(leaseID string) (localWebVNCDaemon, error) {
	unlock, err := acquireWebVNCDaemonLock(leaseID)
	if err != nil {
		return localWebVNCDaemon{}, err
	}
	defer unlock()
	return localWebVNCDaemonStatusLocked(leaseID)
}

func localWebVNCDaemonStatusLocked(leaseID string) (localWebVNCDaemon, error) {
	logPath, pidPath, err := webVNCDaemonPaths(leaseID)
	if err != nil {
		return localWebVNCDaemon{}, err
	}
	status := localWebVNCDaemon{LeaseID: leaseID, LogPath: logPath, PIDPath: pidPath}
	identity, err := readWebVNCDaemonIdentity(pidPath)
	if err != nil {
		if os.IsNotExist(err) {
			status.Missing = true
			return status, nil
		}
		return localWebVNCDaemon{}, err
	}
	status.PID = identity.PID
	status.LocalPort = strings.TrimSpace(identity.LocalPort)
	status.ControllerOwned = identity.ControllerOwned
	status.NoProviderSideEffects = identity.NoProviderSideEffects
	status.ControllerOwnerID = identity.ControllerOwnerID
	command, alive := webVNCDaemonProcessCommand(identity.PID)
	status.Command = strings.TrimSpace(command)
	status.Alive = alive
	if !alive {
		status.Stale = true
		return status, nil
	}
	if status.LocalPort == "" {
		status.LocalPort = webVNCDaemonLocalPortArg(strings.Fields(status.Command))
	}
	if status.LocalPort != "" && !validWebVNCDaemonPort(status.LocalPort) {
		status.Stale = true
		return status, nil
	}
	started, startErr := webVNCDaemonProcessStartIdentity(identity.PID)
	if startErr != nil || identity.WorkspaceID != leaseID || !webVNCDaemonIdentityMatchesProcess(identity, command, started) ||
		(identity.ControllerOwned && (!validWebVNCControllerOwnerID(identity.ControllerOwnerID) || identity.LegacyOwnerToken != "")) {
		status.Stale = true
		return status, nil
	}
	return status, nil
}

func printLocalWebVNCDaemonStatus(w io.Writer, status localWebVNCDaemon, expectedOwnerID string) {
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
	if status.LocalPort != "" {
		fmt.Fprintf(w, "webvnc daemon: local-port=%s\n", status.LocalPort)
	}
	printWebVNCDaemonOwnership(w, webVNCDaemonIdentity{
		ControllerOwned: status.ControllerOwned, NoProviderSideEffects: status.NoProviderSideEffects,
		ControllerOwnerID: status.ControllerOwnerID,
	}, expectedOwnerID)
	if strings.TrimSpace(status.Command) != "" {
		command := strings.TrimSpace(status.Command)
		if ownerID := strings.TrimSpace(status.ControllerOwnerID); ownerID != "" {
			command = strings.ReplaceAll(command, ownerID, "[redacted]")
		}
		fmt.Fprintf(w, "webvnc daemon: command=%s\n", command)
	}
}

func printWebVNCDaemonOwnership(w io.Writer, identity webVNCDaemonIdentity, expectedOwnerID string) {
	if !identity.ControllerOwned {
		return
	}
	match := "unchecked"
	if strings.TrimSpace(expectedOwnerID) != "" {
		match = strconv.FormatBool(identity.ControllerOwnerID == expectedOwnerID)
	}
	fmt.Fprintf(w, "webvnc daemon: controller-owned=true no-provider-side-effects=%t owner-match=%s\n", identity.NoProviderSideEffects, match)
}

func validWebVNCControllerOwnerID(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
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
	unlock, err := acquireWebVNCDaemonLock(leaseID)
	if err != nil {
		return false, exit(2, "lock WebVNC daemon state: %v", err)
	}
	defer unlock()
	return a.stopWebVNCDaemonIfRunningLocked(leaseID)
}

func (a App) stopWebVNCDaemonIfRunningLocked(leaseID string) (bool, error) {
	_, pidPath, err := webVNCDaemonPaths(leaseID)
	if err != nil {
		return false, err
	}
	identity, err := readWebVNCDaemonIdentity(pidPath)
	if err != nil {
		if os.IsNotExist(err) {
			// On Windows, a prior removal can leave the deterministic tombstone
			// after the source identity has already been renamed away. Retrying
			// the same locked removal finishes that interrupted cleanup.
			if removeErr := removeWebVNCDaemonIdentity(pidPath); removeErr != nil {
				return false, removeErr
			}
			return false, nil
		}
		return false, err
	}
	pid := identity.PID
	if identity.Version != webVNCDaemonIdentityVersion || identity.WorkspaceID == "" || identity.ProcessStarted == "" ||
		!validPersistedProcessBootIdentity(identity.BootID) || !validWebVNCDaemonNonce(identity.Nonce) {
		return false, exit(5, "refusing to stop unverified WebVNC daemon pid %d; remove stale identity file %s after verifying the process", pid, pidPath)
	}
	if identity.WorkspaceID != leaseID {
		return false, exit(5, "refusing to stop WebVNC daemon pid %d for workspace %q as workspace %q", pid, identity.WorkspaceID, leaseID)
	}
	sameBoot, bootErr := processBootIdentityMatches(identity.BootID)
	if bootErr != nil {
		return false, exit(5, "refusing to stop WebVNC daemon pid %d without current boot identity: %v", pid, bootErr)
	}
	if !sameBoot {
		if err := removeWebVNCDaemonIdentity(pidPath); err != nil {
			return false, exit(5, "remove prior-boot WebVNC daemon identity: %v", err)
		}
		fmt.Fprintf(a.Stdout, "webvnc daemon: removed prior-boot identity pid=%d\n", pid)
		return true, nil
	}
	command, alive := webVNCDaemonProcessCommand(pid)
	defunct := alive && strings.Contains(strings.ToLower(command), "<defunct>")
	if defunct {
		alive = false
	}
	if !alive {
		if started, startErr := webVNCDaemonProcessStartIdentity(pid); startErr == nil {
			if !defunct || !webVNCDaemonIdentityMatchesProcess(identity, command, started) {
				return false, exit(5, "refusing to drop unverified WebVNC daemon identity pid %d after pid reuse or command inspection failure", pid)
			}
		} else if webVNCDaemonProcessGroupAlive(pid) {
			return false, exit(5, "refusing to signal WebVNC daemon process group %d without its recorded supervisor identity", pid)
		} else {
			if err := removeWebVNCDaemonIdentity(pidPath); err != nil {
				return false, exit(5, "remove exited WebVNC daemon identity: %v", err)
			}
			fmt.Fprintf(a.Stdout, "webvnc daemon: removed exited identity pid=%d\n", pid)
			return true, nil
		}
		if err := terminateWebVNCDaemonProcessTree(pid); err != nil {
			return false, exit(5, "stop WebVNC daemon process group %d without supervisor: %v", pid, err)
		}
		if err := removeWebVNCDaemonIdentity(pidPath); err != nil {
			return false, exit(5, "remove stopped WebVNC daemon identity: %v", err)
		}
		fmt.Fprintf(a.Stdout, "webvnc daemon: stopped orphaned process group pid=%d\n", pid)
		return true, nil
	}
	started, startErr := webVNCDaemonProcessStartIdentity(pid)
	if startErr != nil || !webVNCDaemonIdentityMatchesProcess(identity, command, started) {
		return false, exit(5, "refusing to drop unverified WebVNC daemon identity pid %d while its recorded process group may still contain the credential bridge", pid)
	}
	if err := terminateWebVNCDaemonProcessTree(pid); err != nil {
		return false, exit(5, "stop WebVNC daemon process tree pid %d: %v", pid, err)
	}
	if err := removeWebVNCDaemonIdentity(pidPath); err != nil {
		return false, exit(5, "remove stopped WebVNC daemon identity: %v", err)
	}
	fmt.Fprintf(a.Stdout, "webvnc daemon: stopped pid=%d\n", pid)
	return true, nil
}

func acquireWebVNCDaemonLock(leaseID string) (func(), error) {
	_, pidPath, err := webVNCDaemonPaths(leaseID)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(pidPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	return acquireWebVNCDaemonFileLock(pidPath + ".lock")
}

func acquireWebVNCDaemonFileLock(lockPath string) (func(), error) {
	if info, err := os.Lstat(lockPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("WebVNC daemon lock must be a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	closeWithError := func(lockErr error) (func(), error) {
		_ = file.Close()
		return nil, lockErr
	}
	info, err := file.Stat()
	if err != nil {
		return closeWithError(err)
	}
	if !info.Mode().IsRegular() {
		return closeWithError(fmt.Errorf("WebVNC daemon lock must be a regular file"))
	}
	if err := file.Chmod(0o600); err != nil {
		return closeWithError(err)
	}
	if err := lockWebVNCDaemonFile(file); err != nil {
		return closeWithError(err)
	}
	return func() {
		_ = unlockWebVNCDaemonFile(file)
		_ = file.Close()
	}, nil
}

func webVNCDaemonIdentityMatchesProcess(identity webVNCDaemonIdentity, command, started string) bool {
	bootMatches, err := processBootIdentityMatches(identity.BootID)
	return identity.Version == webVNCDaemonIdentityVersion &&
		identity.PID > 0 &&
		identity.WorkspaceID != "" &&
		err == nil && bootMatches &&
		identity.ProcessStarted == strings.TrimSpace(started) &&
		validWebVNCDaemonNonce(identity.Nonce) && strings.Contains(command, identity.Nonce) &&
		isWebVNCDaemonCommand(command)
}

func processBootIdentityMatches(recorded string) (bool, error) {
	if !processBootIdentityRequired() {
		return true, nil
	}
	recorded = strings.ToLower(strings.TrimSpace(recorded))
	if !validPersistedProcessBootIdentity(recorded) {
		return false, nil
	}
	current, err := processBootIdentity()
	if err != nil {
		return false, err
	}
	return recorded == strings.ToLower(strings.TrimSpace(current)), nil
}

func validWebVNCDaemonNonce(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func isWebVNCDaemonCommand(command string) bool {
	command = strings.ToLower(command)
	return strings.Contains(command, "crabbox") && strings.Contains(command, "webvnc")
}

func readWebVNCDaemonPID(pidPath string) (int, error) {
	identity, err := readWebVNCDaemonIdentity(pidPath)
	if err != nil {
		return 0, err
	}
	return identity.PID, nil
}

func readWebVNCDaemonIdentity(pidPath string) (webVNCDaemonIdentity, error) {
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return webVNCDaemonIdentity{}, err
	}
	var identity webVNCDaemonIdentity
	if err := json.Unmarshal(data, &identity); err == nil {
		if identity.PID <= 0 {
			return webVNCDaemonIdentity{}, exit(2, "invalid WebVNC daemon identity file %s", pidPath)
		}
		return identity, nil
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return webVNCDaemonIdentity{}, exit(2, "invalid WebVNC daemon identity file %s", pidPath)
	}
	// Legacy PID-only files are readable for diagnostics but deliberately lack
	// the identity version needed to reuse or signal their process.
	return webVNCDaemonIdentity{PID: pid}, nil
}

func writeWebVNCDaemonIdentity(pidPath string, identity webVNCDaemonIdentity) error {
	data, err := json.Marshal(identity)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(pidPath), ".webvnc-identity-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := replaceControllerFile(tmpPath, pidPath); err != nil {
		return err
	}
	if err := syncControllerDirectory(filepath.Dir(pidPath)); err != nil {
		return fmt.Errorf("sync WebVNC daemon identity directory: %w", err)
	}
	return nil
}

func removeWebVNCDaemonIdentity(pidPath string) error {
	if err := removeControllerFile(pidPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncControllerDirectory(filepath.Dir(pidPath))
}

func newWebVNCDaemonNonce() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", value[:]), nil
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

func resolvedWebVNCCommandConfig(cfg Config, server Server, target SSHTarget) Config {
	if provider := strings.TrimSpace(server.Provider); provider != "" {
		cfg.Provider = provider
	}
	if targetOS := strings.TrimSpace(target.TargetOS); targetOS != "" {
		cfg.TargetOS = targetOS
	}
	if windowsMode := strings.TrimSpace(target.WindowsMode); windowsMode != "" {
		cfg.WindowsMode = windowsMode
	}
	return cfg
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
if [ -x /usr/local/bin/crabbox-start-desktop ]; then
  sudo /bin/bash /usr/local/bin/crabbox-start-desktop
elif [ "${CRABBOX_DESKTOP_ENV:-xfce}" != "xfce" ]; then
  sudo systemctl restart crabbox-desktop.service crabbox-wayvnc.service
elif systemctl cat crabbox-xvfb.service 2>/dev/null | grep -q Xtigervnc; then
  sudo systemctl restart crabbox-xvfb.service crabbox-desktop.service crabbox-desktop-session.service
elif systemctl cat crabbox-desktop.service >/dev/null 2>&1; then
  sudo systemctl restart crabbox-desktop.service crabbox-x11vnc.service
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

type vncForegroundTunnel struct {
	cmd    *exec.Cmd
	done   chan struct{}
	output *strings.Builder
	mu     sync.Mutex
	err    error
}

func (t *vncForegroundTunnel) PID() int {
	if t == nil || t.cmd == nil || t.cmd.Process == nil {
		return 0
	}
	return t.cmd.Process.Pid
}

func (t *vncForegroundTunnel) Done() <-chan struct{} {
	if t == nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	return t.done
}

func (t *vncForegroundTunnel) ExitError() error {
	if t == nil {
		return errors.New("VNC SSH tunnel is unavailable")
	}
	<-t.done
	t.mu.Lock()
	err := t.err
	t.mu.Unlock()
	if err == nil {
		err = errors.New("VNC SSH tunnel exited")
	}
	if text := strings.TrimSpace(t.output.String()); text != "" {
		return fmt.Errorf("%w: %s", err, text)
	}
	return err
}

func startVNCForegroundTunnel(ctx context.Context, target SSHTarget, localPort, remoteHost, remotePort string) (*vncForegroundTunnel, error) {
	cmd := exec.CommandContext(ctx, "ssh", vncTunnelArgs(target, localPort, remoteHost, remotePort)...)
	var output strings.Builder
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	tunnel := &vncForegroundTunnel{cmd: cmd, done: make(chan struct{}), output: &output}
	go func() {
		err := cmd.Wait()
		tunnel.mu.Lock()
		tunnel.err = err
		tunnel.mu.Unlock()
		close(tunnel.done)
	}()
	deadline := time.Now().Add(vncTunnelReadinessTimeout())
	var listenerErr error
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			stopProcess(tunnel)
			return nil, context.Cause(ctx)
		}
		if ready, err := startedTunnelListenerReady(ctx, localPort, cmd.Process.Pid); ready {
			return tunnel, nil
		} else {
			listenerErr = err
		}
		select {
		case <-tunnel.Done():
			return nil, tunnel.ExitError()
		default:
		}
		time.Sleep(100 * time.Millisecond)
	}
	stopProcess(tunnel)
	if listenerErr != nil {
		return nil, exit(5, "timed out verifying VNC SSH tunnel listener on 127.0.0.1:%s: %v", localPort, listenerErr)
	}
	return nil, exit(5, "timed out starting VNC SSH tunnel on localhost:%s", localPort)
}

func startVNCForegroundTunnelOnReservedPort(
	ctx context.Context,
	target SSHTarget,
	requestedPort, remoteHost, remotePort string,
	excluded ...string,
) (*vncForegroundTunnel, string, error) {
	attempted := append([]string(nil), excluded...)
	var lastBindErr error
	for {
		reservation, err := reserveWebVNCTunnelPort(requestedPort, attempted...)
		if err != nil {
			return nil, "", errors.Join(lastBindErr, err)
		}
		port := reservation.port
		tunnel, startErr := startVNCForegroundTunnel(ctx, target, port, remoteHost, remotePort)
		reservation.release()
		if startErr == nil {
			return tunnel, port, nil
		}
		if requestedPort != "" || !vncTunnelLocalBindConflict(startErr) {
			return nil, "", startErr
		}
		lastBindErr = startErr
		attempted = append(attempted, port)
	}
}

func vncTunnelLocalBindConflict(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "address already in use") ||
		strings.Contains(message, "cannot listen to port") ||
		strings.Contains(message, "local forwarding failed")
}

func startedTunnelListenerReady(ctx context.Context, localPort string, processID int) (bool, error) {
	_ = ctx
	if !controllerListenerOwnershipSupported() {
		return false, controllerVerifyDaemonOwnedListener(localPort, processID)
	}
	if err := controllerVerifyDaemonOwnedListener(localPort, processID); err != nil {
		return false, err
	}
	return true, nil
}

type webVNCBridge struct {
	tcp                   net.Conn
	ws                    *websocket.Conn
	target                SSHTarget
	log                   io.Writer
	desktopThemeUpdates   chan string
	applyDesktopThemeFunc func(context.Context, string) error
}

const webVNCDesktopThemeSSHAttemptTimeout = 35 * time.Second

func connectWebVNCBridge(ctx context.Context, coord *CoordinatorClient, leaseID, host, port string, target SSHTarget, log io.Writer) (*webVNCBridge, error) {
	agentBaseURL, err := webVNCAgentBaseURL(coord.BaseURL)
	if err != nil {
		return nil, err
	}
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
	splitAgentOrigin := !sameWebVNCOrigin(agentBaseURL, coord.BaseURL)
	var headers http.Header
	if splitAgentOrigin {
		headers = splitWebVNCAgentUpgradeHeaders(ticket.Ticket)
	} else {
		headers, err = bridgeUpgradeTicketHeaders(ctx, coord, ticket.Ticket)
		if err != nil {
			_ = tcp.Close()
			return nil, err
		}
	}
	agentURL := webVNCAgentURLWithCapabilities(agentBaseURL, leaseID, capabilities)
	ws, resp, err := websocket.Dial(ctx, agentURL, webVNCWebSocketDialOptions(headers))
	if retryBridgeTicketInAuthorization(resp, err) {
		var retryHeaders http.Header
		if splitAgentOrigin {
			retryHeaders = splitWebVNCAgentAuthorizationHeaders(ticket.Ticket)
		} else {
			retryHeaders = bridgeTicketHeaders(coord, ticket.Ticket)
		}
		ws, _, err = websocket.Dial(ctx, agentURL, webVNCWebSocketDialOptions(retryHeaders))
	}
	if err != nil {
		_ = tcp.Close()
		return nil, err
	}
	return &webVNCBridge{
		tcp:                 tcp,
		ws:                  ws,
		target:              target,
		log:                 log,
		desktopThemeUpdates: make(chan string, 1),
	}, nil
}

func splitWebVNCAgentUpgradeHeaders(ticket string) http.Header {
	headers := http.Header{}
	headers.Set("X-Crabbox-Bridge-Ticket", ticket)
	return headers
}

func splitWebVNCAgentAuthorizationHeaders(ticket string) http.Header {
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+ticket)
	return headers
}

func sameWebVNCOrigin(left, right string) bool {
	leftScheme, leftHost, leftPort, leftOK := normalizedWebVNCOrigin(left)
	rightScheme, rightHost, rightPort, rightOK := normalizedWebVNCOrigin(right)
	return leftOK && rightOK && leftScheme == rightScheme && leftHost == rightHost && leftPort == rightPort
}

func normalizedWebVNCOrigin(value string) (scheme, host, port string, ok bool) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" {
		return "", "", "", false
	}
	scheme = strings.ToLower(parsed.Scheme)
	host = strings.ToLower(parsed.Hostname())
	port = parsed.Port()
	if port == "" {
		switch scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		default:
			return "", "", "", false
		}
	} else {
		numericPort, err := strconv.Atoi(port)
		if err != nil || numericPort < 1 || numericPort > 65535 {
			return "", "", "", false
		}
		port = strconv.Itoa(numericPort)
	}
	return scheme, host, port, true
}

func webVNCWebSocketDialOptions(headers http.Header) *websocket.DialOptions {
	return &websocket.DialOptions{
		HTTPClient: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		HTTPHeader: headers,
	}
}

func webVNCAgentBaseURL(base string) (string, error) {
	const environment = "CRABBOX_WEBVNC_AGENT_BASE_URL"
	value, configured := os.LookupEnv(environment)
	if !configured || value == "" {
		return base, nil
	}
	if value != strings.TrimSpace(value) {
		return "", fmt.Errorf("%s must not contain surrounding whitespace", environment)
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", fmt.Errorf("%s must be one exact HTTPS origin", environment)
	}
	if !validWebVNCOriginPort(parsed.Host) {
		return "", fmt.Errorf("%s must use a canonical valid nonzero port", environment)
	}
	if parsed.Scheme != "https" {
		host := parsed.Hostname()
		ip := net.ParseIP(host)
		if parsed.Scheme != "http" || parsed.Port() == "" || (host != "localhost" && (ip == nil || !ip.IsLoopback())) {
			return "", fmt.Errorf("%s must be one exact HTTPS origin (HTTP is allowed only for loopback with an explicit port)", environment)
		}
	}
	return strings.TrimSuffix(value, "/"), nil
}

func validWebVNCOriginPort(host string) bool {
	rawPort := ""
	hasPort := false
	if strings.HasPrefix(host, "[") {
		closing := strings.LastIndex(host, "]")
		if closing < 0 {
			return false
		}
		suffix := host[closing+1:]
		if suffix == "" {
			return true
		}
		if !strings.HasPrefix(suffix, ":") {
			return false
		}
		rawPort, hasPort = suffix[1:], true
	} else if separator := strings.LastIndex(host, ":"); separator >= 0 {
		rawPort, hasPort = host[separator+1:], true
	}
	if !hasPort {
		return true
	}
	if len(rawPort) < 1 || len(rawPort) > 5 || rawPort[0] == '0' {
		return false
	}
	for _, character := range rawPort {
		if character < '0' || character > '9' {
			return false
		}
	}
	numericPort, err := strconv.Atoi(rawPort)
	return err == nil && numericPort <= 65535
}

func (b *webVNCBridge) Serve(ctx context.Context) error {
	defer b.Close()
	if b.desktopThemeUpdates == nil {
		b.desktopThemeUpdates = make(chan string, 1)
	}
	themeCtx, cancelThemes := context.WithCancel(ctx)
	themeDone := make(chan struct{})
	go func() {
		defer close(themeDone)
		b.serveDesktopThemeUpdates(themeCtx)
	}()
	defer func() {
		cancelThemes()
		<-themeDone
	}()
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
	return exit(2, "this webvnc subcommand is not available for macOS leases; run `crabbox webvnc --id <id>` for the host-side browser viewer, or use a native VNC client over an SSH tunnel:\n  ssh -o GatewayPorts=no -L 127.0.0.1:5900:127.0.0.1:5900 %s@<lease-ip>\n  open vnc://127.0.0.1:5900", blank(cfg.SSHUser, "<user>"))
}

// isMacOSDesktopProvider reports whether the resolved lease uses macOS native
// Screen Sharing. An explicit target wins for multi-target providers such as
// Parallels; provider-spec inference covers entrypoints that have not resolved
// the target yet, such as Tart's macOS-only profile.
func isMacOSDesktopProvider(cfg Config) bool {
	switch strings.ToLower(strings.TrimSpace(cfg.TargetOS)) {
	case targetMacOS:
		return true
	case "":
		// Fall through to provider-spec inference.
	default:
		return false
	}
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

// macOSPortalWebVNCConfig registers direct macOS leases when the operator
// already has coordinator auth. The registration follows the provider lease,
// so overlapping bridges can share it and stop owns the matching cleanup.
func macOSPortalWebVNCConfig(cfg Config) (Config, bool, error) {
	if !useDirectSSHWebVNC(cfg) || !isMacOSDesktopProvider(cfg) || strings.TrimSpace(cfg.Coordinator) == "" {
		return cfg, false, nil
	}
	if strings.TrimSpace(cfg.CoordToken) == "" && len(cfg.CoordTokenCommand) == 0 {
		return cfg, false, nil
	}
	coord, configured, err := newCoordinatorClient(cfg)
	if err != nil {
		return cfg, false, err
	}
	if !configured || coord == nil || !coord.hasConfiguredAuth() {
		return cfg, false, nil
	}
	cfg.BrokerMode = BrokerModeRegistered
	cfg.macOSPortalAuto = true
	cfg.macOSPortalCoordinator = coord.BaseURL
	return cfg, true, nil
}

func macOSPortalWebVNCConfigForLease(cfg Config, id string) (Config, bool, error) {
	var claim leaseClaim
	var claimExists bool
	if supportsDirectSSHWebVNC(cfg.Provider) && strings.TrimSpace(id) != "" {
		var err error
		claim, claimExists, err = resolveLeaseClaimForProvider(id, canonicalClaimProvider(cfg.Provider))
		if err != nil {
			return cfg, false, err
		}
		claimedTarget := firstNonBlank(claim.TargetOS, claim.Labels["target"])
		if claimExists && !cfg.targetFlagExplicit && claimedTarget == targetMacOS {
			cfg.TargetOS = targetMacOS
			cfg.WindowsMode = claim.WindowsMode
		}
	}
	routed, automatic, err := macOSPortalWebVNCConfig(cfg)
	if err != nil {
		return routed, false, err
	}
	boundCoordinator := strings.TrimSpace(claim.CoordinatorRegistrationURL)
	if !claimExists || boundCoordinator == "" {
		return routed, automatic, nil
	}
	if !shouldRegisterCoordinatorLease(routed) {
		return routed, automatic, nil
	}
	currentCoordinator, bindingErr := coordinatorRegistrationURLForConfig(routed)
	if bindingErr != nil {
		return routed, false, bindingErr
	}
	routed.macOSPortalAuto = true
	routed.macOSPortalCoordinator = boundCoordinator
	if currentCoordinator != boundCoordinator {
		return routed, false, exit(4, "macOS portal coordinator changed from persisted registration binding")
	}
	return routed, true, nil
}

func resolveWebVNCPortalCredentials(
	ctx context.Context,
	cfg Config,
	target SSHTarget,
	endpoint vncEndpoint,
	readPassword macOSVNCPasswordReader,
) (rfbCredentials, error) {
	if !endpoint.Managed {
		return rfbCredentials{}, nil
	}
	if target.TargetOS == targetMacOS {
		return resolveMacOSWebVNCCredentials(ctx, cfg, target, readPassword)
	}
	password, _ := readPassword(ctx, target, vncPasswordCommand(target))
	return rfbCredentials{Password: strings.TrimSpace(password)}, nil
}

func directSSHWebVNCAllowsNone(server Server, endpoint vncEndpoint) bool {
	// Generated WayVNC binds only to the remote loopback interface. Access is
	// still gated by the authenticated SSH hop, loopback-only websockify, and
	// local listener ownership verification; None is never accepted for a
	// direct, static, or non-managed VNC endpoint.
	return isWaylandDesktopEnv(server.Labels["desktop_env"]) && endpoint.Managed && !endpoint.Direct &&
		endpoint.Host == "127.0.0.1" && endpoint.Port == managedVNCPort
}

func (a App) resolveWebVNCLeaseTarget(ctx context.Context, cfg Config, id string, reclaim, noProviderSideEffects bool, expected webVNCExpectedProviderIdentity) (Server, SSHTarget, string, error) {
	var server Server
	var target SSHTarget
	var leaseID string
	var err error
	if noProviderSideEffects {
		server, target, leaseID, err = a.resolveNetworkLeaseTargetReadOnly(ctx, cfg, id, expected.Identity)
	} else {
		server, target, leaseID, err = a.resolveNetworkLeaseTargetForRepo(ctx, cfg, id, false, reclaim)
	}
	if err != nil {
		return Server{}, SSHTarget{}, "", err
	}
	if err := validateWebVNCResolvedProviderIdentity(cfg, server, target, leaseID, expected); err != nil {
		return Server{}, SSHTarget{}, "", err
	}
	return server, target, leaseID, nil
}

type directSSHWebVNCRemoteOwner struct {
	ID            string
	PreferredPort string
}

const (
	directSSHWebVNCRemotePortBase = 20000
	directSSHWebVNCRemotePortSpan = 40000
)

func directSSHWebVNCRemoteOwnerFromID(ownerID string) (directSSHWebVNCRemoteOwner, error) {
	ownerID = strings.TrimSpace(ownerID)
	if !validWebVNCControllerOwnerID(ownerID) {
		return directSSHWebVNCRemoteOwner{}, fmt.Errorf("invalid direct SSH WebVNC owner identity")
	}
	prefix, err := strconv.ParseUint(ownerID[:8], 16, 32)
	if err != nil {
		return directSSHWebVNCRemoteOwner{}, fmt.Errorf("parse direct SSH WebVNC owner identity: %w", err)
	}
	port := directSSHWebVNCRemotePortBase + int(prefix%directSSHWebVNCRemotePortSpan)
	return directSSHWebVNCRemoteOwner{ID: ownerID, PreferredPort: strconv.Itoa(port)}, nil
}

const directSSHWebVNCRemotePortPrefix = "direct-webvnc-port="

func directSSHWebVNCRemotePortFromOutput(output string) (string, error) {
	remotePort := ""
	for _, line := range strings.Split(output, "\n") {
		value, ok := strings.CutPrefix(strings.TrimSpace(line), directSSHWebVNCRemotePortPrefix)
		if !ok {
			continue
		}
		port, err := strconv.Atoi(value)
		if err != nil || port < directSSHWebVNCRemotePortBase || port >= directSSHWebVNCRemotePortBase+directSSHWebVNCRemotePortSpan {
			return "", fmt.Errorf("invalid direct SSH WebVNC remote port %q", value)
		}
		if remotePort != "" {
			return "", fmt.Errorf("direct SSH WebVNC remote port was reported more than once")
		}
		remotePort = value
	}
	if remotePort == "" {
		return "", fmt.Errorf("direct SSH WebVNC remote port was not reported")
	}
	return remotePort, nil
}

func directSSHWebVNCRemoteRunning(output string) bool {
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == "running" {
			return true
		}
	}
	return false
}

func directSSHWebVNCRemoteOwnerForLease(cfg Config, server Server, leaseID string, expected webVNCExpectedProviderIdentity, controllerOwnerID string) (directSSHWebVNCRemoteOwner, error) {
	ownerID := strings.TrimSpace(controllerOwnerID)
	if ownerID == "" {
		values := []string{
			"manual", cfg.Provider, server.Provider, leaseID, server.CloudID,
			strconv.FormatInt(server.ID, 10), server.Name, serverSlug(server),
			expected.Identity.LeaseID, expected.Identity.AttemptLeaseID,
			expected.Identity.Slug, expected.Identity.ResourceID, expected.Scope,
		}
		hash := sha256.New()
		for _, value := range values {
			_, _ = hash.Write([]byte(strconv.Itoa(len(value))))
			_, _ = hash.Write([]byte{':'})
			_, _ = hash.Write([]byte(value))
		}
		ownerID = fmt.Sprintf("%x", hash.Sum(nil))
	}
	return directSSHWebVNCRemoteOwnerFromID(ownerID)
}

func (a App) directSSHWebVNC(ctx context.Context, cfg Config, id, localPort string, openViewer, _ bool, reclaim, noProviderSideEffects bool, expected webVNCExpectedProviderIdentity, controllerOwnerID string) error {
	listener, localPort, err := acquireWebVNCLoopbackListener(localPort)
	if err != nil {
		return exit(5, "reserve local direct SSH WebVNC listener: %v", err)
	}
	defer listener.Close()
	server, target, leaseID, err := a.resolveWebVNCLeaseTarget(ctx, cfg, id, reclaim, noProviderSideEffects, expected)
	if err != nil {
		return err
	}
	if err := enforceManagedLeaseCapabilities(cfg, server, leaseID); err != nil {
		return err
	}
	if !noProviderSideEffects {
		if err := a.claimAndTouchLeaseTarget(ctx, cfg, server, target, leaseID, reclaim); err != nil {
			return err
		}
	}
	endpoint, err := resolveVNCEndpoint(ctx, cfg, &target)
	if err != nil {
		return err
	}
	if directSSHWebVNCUsesLocalBridge(target) {
		return a.directSSHWindowsWebVNC(ctx, cfg, server, target, leaseID, endpoint, listener, localPort, openViewer)
	}
	allowNone := directSSHWebVNCAllowsNone(server, endpoint)
	remoteOwner, err := directSSHWebVNCRemoteOwnerForLease(cfg, server, leaseID, expected, controllerOwnerID)
	if err != nil {
		return exit(5, "resolve direct SSH WebVNC owner: %v", err)
	}
	remoteOutput, err := runDirectSSHWebVNCRemoteCombinedOutput(ctx, target, directSSHNoVNCRemoteCommand(remoteOwner))
	if err != nil {
		rescueCtx := rescueContext{Cfg: cfg, Target: target, LeaseID: leaseID}
		printRescue(a.Stdout, classifyDesktopFailure(remoteOutput), trimFailureDetail(remoteOutput), desktopDoctorCommand(rescueCtx))
		return exit(5, "start direct SSH WebVNC bridge: %v", err)
	}
	remotePort, err := directSSHWebVNCRemotePortFromOutput(remoteOutput)
	if err != nil {
		return exit(5, "start direct SSH WebVNC bridge: %v", err)
	}
	fmt.Fprintf(a.Stdout, "lease: %s slug=%s provider=%s target=%s\n", leaseID, blank(serverSlug(server), "-"), blank(server.Provider, cfg.Provider), blank(target.TargetOS, cfg.TargetOS))
	tunnel, tunnelPort, err := startVNCForegroundTunnelOnReservedPort(ctx, target, "", "127.0.0.1", remotePort, localPort)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "bridge: starting SSH tunnel localhost:%s -> 127.0.0.1:%s\n", tunnelPort, remotePort)
	defer stopProcess(tunnel)
	proxyCtx, cancelProxy := context.WithCancelCause(ctx)
	defer cancelProxy(context.Canceled)
	go func() {
		select {
		case <-tunnel.Done():
			cancelProxy(tunnel.ExitError())
		case <-proxyCtx.Done():
		}
	}()
	proxyDone := serveWebVNCLoopbackProxy(proxyCtx, listener, tunnelPort, tunnel.PID())
	if err := verifyVNCForegroundTunnelListener(tunnel, tunnelPort); err != nil {
		return exit(5, "verify direct SSH WebVNC tunnel before credential retrieval: %v", err)
	}
	passwordOutput, passwordErr := runSSHOutput(ctx, target, vncPasswordCommand(target))
	password := strings.TrimSpace(passwordOutput)
	if passwordErr != nil && !allowNone {
		return exit(5, "read direct SSH WebVNC credential: %v", passwordErr)
	}
	if password == "" && !allowNone {
		return exit(5, "read direct SSH WebVNC credential: empty VNC password")
	}
	if err := verifyVNCForegroundTunnelListener(tunnel, tunnelPort); err != nil {
		return exit(5, "verify direct SSH WebVNC tunnel before authentication: %v", err)
	}
	if err := probeDirectSSHWebVNCWithSecurity(ctx, localPort, password, allowNone); err != nil {
		return exit(5, "authenticate direct SSH WebVNC websocket: %v", err)
	}
	if err := verifyVNCForegroundTunnelListener(tunnel, tunnelPort); err != nil {
		return exit(5, "verify direct SSH WebVNC tunnel after authentication: %v", err)
	}
	viewerURL := directSSHWebVNCURL(localPort, password)
	fmt.Fprintln(a.Stdout, "bridge: connected; keep this process running while using WebVNC")
	fmt.Fprintf(a.Stdout, "webvnc: %s\n", viewerURL)
	fmt.Fprintf(a.Stdout, "password: %s\n", password)
	if openViewer {
		if err := openLocalURL(viewerURL); err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "opened: %s\n", viewerURL)
	}
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-tunnel.Done():
		return tunnel.ExitError()
	case proxyErr := <-proxyDone:
		return proxyErr
	}
}

func directSSHWebVNCUsesLocalBridge(target SSHTarget) bool {
	return isWindowsNativeTarget(target)
}

func (a App) directSSHWindowsWebVNC(
	ctx context.Context,
	cfg Config,
	server Server,
	target SSHTarget,
	leaseID string,
	endpoint vncEndpoint,
	webListener net.Listener,
	webPort string,
	openViewer bool,
) error {
	tunnel, vncPort, err := startVNCForegroundTunnelOnReservedPort(ctx, target, "", endpoint.Host, endpoint.Port, webPort)
	if err != nil {
		return err
	}
	defer stopProcess(tunnel)
	bridgeCtx, cancelBridge := context.WithCancelCause(ctx)
	defer cancelBridge(context.Canceled)
	go func() {
		select {
		case <-tunnel.Done():
			cancelBridge(tunnel.ExitError())
		case <-bridgeCtx.Done():
		}
	}()
	password, err := runSSHOutput(ctx, target, vncPasswordCommand(target))
	if err != nil {
		return exit(5, "read native Windows VNC credential: %v", err)
	}
	password = strings.TrimSpace(password)
	if password == "" {
		return exit(5, "read native Windows VNC credential: empty VNC password")
	}
	credentials := rfbCredentials{Username: target.User, Password: password}
	fmt.Fprintf(a.Stdout, "lease: %s slug=%s provider=%s target=windows\n", leaseID, blank(serverSlug(server), "-"), blank(server.Provider, cfg.Provider))
	fmt.Fprintf(a.Stdout, "bridge: serving noVNC locally; SSH tunnel -> guest %s:%s; keep this running while viewing\n", endpoint.Host, endpoint.Port)
	return a.serveLocalWebVNCBridge(
		bridgeCtx,
		webListener,
		webPort,
		credentials,
		openViewer,
		true,
		func(ctx context.Context) (net.Conn, error) {
			return dialVNCForegroundTunnel(ctx, tunnel, vncPort)
		},
		nil,
	)
}

func verifyVNCForegroundTunnelListener(tunnel *vncForegroundTunnel, port string) error {
	if tunnel == nil || tunnel.PID() <= 0 {
		return errors.New("VNC SSH tunnel is unavailable")
	}
	select {
	case <-tunnel.Done():
		return tunnel.ExitError()
	default:
	}
	if err := controllerVerifyDaemonOwnedListener(port, tunnel.PID()); err != nil {
		return err
	}
	select {
	case <-tunnel.Done():
		return tunnel.ExitError()
	default:
		return nil
	}
}

func (a App) directSSHWebVNCStatus(ctx context.Context, cfg Config, id, localPort string, expectedListenerOwnerPID int, expected webVNCExpectedProviderIdentity, controllerOwnerID string) error {
	var server Server
	var target SSHTarget
	var leaseID string
	var err error
	if expected.set {
		server, target, leaseID, err = a.resolveNetworkLeaseTargetReadOnly(ctx, cfg, id, expected.Identity)
	} else {
		server, target, leaseID, err = a.resolveNetworkLeaseTarget(ctx, cfg, id, false)
	}
	if err != nil {
		return err
	}
	if err := validateWebVNCResolvedProviderIdentity(cfg, server, target, leaseID, expected); err != nil {
		return err
	}
	if err := enforceManagedLeaseCapabilities(cfg, server, leaseID); err != nil {
		return err
	}
	if !expected.set && (localPort == "" || expectedListenerOwnerPID == 0) {
		daemon, daemonErr := localWebVNCDaemonStatus(leaseID)
		if daemonErr == nil && leaseID != id {
			if aliasDaemon, aliasErr := localWebVNCDaemonStatus(id); aliasErr == nil && !aliasDaemon.Missing {
				daemon = aliasDaemon
			}
		}
		if daemonErr == nil {
			localPort, expectedListenerOwnerPID = directSSHWebVNCStatusListenerIdentity(localPort, expectedListenerOwnerPID, daemon)
		}
	}
	if localPort == "" {
		localPort = availableLocalVNCPort()
	}
	remoteOwner, err := directSSHWebVNCRemoteOwnerForLease(cfg, server, leaseID, expected, controllerOwnerID)
	if err != nil {
		return exit(5, "resolve direct SSH WebVNC owner: %v", err)
	}
	endpoint, endpointErr := resolveVNCEndpoint(ctx, cfg, &target)
	allowNone := endpointErr == nil && directSSHWebVNCAllowsNone(server, endpoint)
	websockify := "unknown"
	remotePort := ""
	if out, err := runDirectSSHWebVNCRemoteCombinedOutput(ctx, target, directSSHWebVNCRemoteStatusCommand(remoteOwner)); err == nil {
		if port, portErr := directSSHWebVNCRemotePortFromOutput(out); portErr == nil && directSSHWebVNCRemoteRunning(out) {
			websockify = "running"
			remotePort = port
		} else {
			websockify = strings.TrimSpace(out)
		}
	}
	password := ""
	authenticated := false
	var authenticationErr error
	if endpointErr == nil && endpoint.Managed && websockify == "running" {
		authenticationErr = verifyDirectSSHWebVNCListenerOwner(localPort, expectedListenerOwnerPID)
		if authenticationErr == nil {
			var passwordErr error
			password, passwordErr = runSSHOutput(ctx, target, vncPasswordCommand(target))
			password = strings.TrimSpace(password)
			if passwordErr != nil && !allowNone {
				authenticationErr = passwordErr
			} else if passwordErr != nil {
				password = ""
			}
		}
		if authenticationErr == nil && password == "" && !allowNone {
			authenticationErr = errors.New("empty VNC password")
		}
		if authenticationErr == nil {
			authenticationErr = verifyDirectSSHWebVNCListenerOwner(localPort, expectedListenerOwnerPID)
		}
		if authenticationErr == nil {
			authenticationErr = probeDirectSSHWebVNCWithSecurity(ctx, localPort, password, allowNone)
		}
		if authenticationErr == nil {
			authenticationErr = verifyDirectSSHWebVNCListenerOwner(localPort, expectedListenerOwnerPID)
		}
		authenticated = authenticationErr == nil
	}
	fmt.Fprintf(a.Stdout, "lease: %s slug=%s provider=%s target=%s\n", leaseID, blank(serverSlug(server), "-"), blank(server.Provider, cfg.Provider), blank(target.TargetOS, cfg.TargetOS))
	if endpointErr != nil {
		fmt.Fprintf(a.Stdout, "vnc target: unreachable 127.0.0.1:5900 (%v)\n", endpointErr)
		printRescue(a.Stdout, rescueVNCTargetUnreachable, endpointErr.Error(), desktopDoctorCommand(rescueContext{Cfg: cfg, Target: target, LeaseID: leaseID}))
	} else {
		fmt.Fprintf(a.Stdout, "vnc target: reachable %s:%s managed=%t\n", endpoint.Host, endpoint.Port, endpoint.Managed)
	}
	switch {
	case authenticated:
		fmt.Fprintln(a.Stdout, "direct ssh webvnc: running")
	case websockify == "running" && authenticationErr != nil:
		fmt.Fprintf(a.Stdout, "direct ssh webvnc: unauthenticated (%v)\n", authenticationErr)
	default:
		fmt.Fprintf(a.Stdout, "direct ssh webvnc: %s\n", blank(websockify, "unknown"))
	}
	if remotePort != "" {
		fmt.Fprintf(a.Stdout, "ssh tunnel: %s\n", vncTunnelCommandTo(target, localPort, "127.0.0.1", remotePort))
	} else {
		fmt.Fprintln(a.Stdout, "ssh tunnel: unavailable (remote WebVNC stopped)")
	}
	if authenticated {
		fmt.Fprintf(a.Stdout, "webvnc: %s\n", directSSHWebVNCURL(localPort, password))
		fmt.Fprintf(a.Stdout, "password: %s\n", password)
	}
	fmt.Fprintf(a.Stdout, "fallback: %s\n", nativeVNCOpenCommand(cfg, target, leaseID))
	return nil
}

func directSSHWebVNCStatusListenerIdentity(localPort string, expectedPID int, daemon localWebVNCDaemon) (string, int) {
	if !daemon.Alive || daemon.Missing || daemon.Stale {
		return localPort, expectedPID
	}
	if localPort == "" {
		localPort = daemon.LocalPort
	}
	if expectedPID == 0 {
		expectedPID = daemon.PID
	}
	return localPort, expectedPID
}

func verifyDirectSSHWebVNCListenerOwner(localPort string, expectedPID int) error {
	if expectedPID <= 0 {
		return errors.New("expected local WebVNC listener owner PID is required")
	}
	return controllerVerifyDaemonOwnedListener(localPort, expectedPID)
}

func (a App) directSSHWebVNCReset(ctx context.Context, cfg Config, id string, openViewer, takeControl bool) error {
	server, target, leaseID, err := a.resolveNetworkLeaseTarget(ctx, cfg, id, false)
	if err != nil {
		return err
	}
	if err := enforceManagedLeaseCapabilities(cfg, server, leaseID); err != nil {
		return err
	}
	remoteOwner, err := directSSHWebVNCRemoteOwnerForLease(cfg, server, leaseID, webVNCExpectedProviderIdentity{}, "")
	if err != nil {
		return exit(5, "resolve direct SSH WebVNC owner: %v", err)
	}
	if out, err := runDirectSSHWebVNCRemoteCombinedOutput(ctx, target, directSSHWebVNCResetRemoteCommand(remoteOwner)); err != nil {
		printRescue(a.Stdout, classifyDesktopFailure(out), trimFailureDetail(out), desktopDoctorCommand(rescueContext{Cfg: cfg, Target: target, LeaseID: leaseID}))
		return exit(5, "reset direct SSH WebVNC/input stack: %v", err)
	}
	fmt.Fprintf(a.Stdout, "webvnc reset: lease=%s slug=%s\n", leaseID, blank(serverSlug(server), "-"))
	if openViewer {
		return a.directSSHWebVNC(ctx, cfg, leaseID, "", true, takeControl, false, false, webVNCExpectedProviderIdentity{}, "")
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

const directSSHWebVNCProbeTimeout = 5 * time.Second

func probeDirectSSHWebVNC(ctx context.Context, localPort, password string) error {
	return probeDirectSSHWebVNCWithSecurity(ctx, localPort, password, false)
}

func probeDirectSSHWebVNCWithSecurity(ctx context.Context, localPort, password string, allowNone bool) error {
	port, err := strconv.Atoi(strings.TrimSpace(localPort))
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid local WebVNC port %q", localPort)
	}
	password = strings.TrimSpace(password)
	if password == "" && !allowNone {
		return fmt.Errorf("VNC password is empty")
	}
	probeCtx, cancel := context.WithTimeout(ctx, directSSHWebVNCProbeTimeout)
	defer cancel()
	ws, response, err := websocket.Dial(probeCtx, fmt.Sprintf("ws://127.0.0.1:%d/websockify", port), &websocket.DialOptions{
		Subprotocols:    []string{"binary"},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return fmt.Errorf("open noVNC websocket: %w", err)
	}
	conn := websocket.NetConn(probeCtx, ws, websocket.MessageBinary)
	defer conn.Close()
	if deadline, ok := probeCtx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err := authenticateDirectSSHWebVNCRFBWithSecurity(conn, password, allowNone); err != nil {
		return fmt.Errorf("authenticate noVNC websocket: %w", err)
	}
	return nil
}

func authenticateDirectSSHWebVNCRFBWithSecurity(conn net.Conn, password string, allowNone bool) error {
	version := make([]byte, 12)
	if _, err := io.ReadFull(conn, version); err != nil {
		return fmt.Errorf("read RFB version: %w", err)
	}
	if !strings.HasPrefix(string(version), "RFB 003.") {
		return fmt.Errorf("unexpected RFB version %q", string(version))
	}
	if _, err := conn.Write([]byte("RFB 003.008\n")); err != nil {
		return fmt.Errorf("write RFB version: %w", err)
	}
	count := []byte{0}
	if _, err := io.ReadFull(conn, count); err != nil {
		return fmt.Errorf("read RFB security type count: %w", err)
	}
	if count[0] == 0 {
		reason, err := readRFBReason(conn)
		if err != nil {
			return err
		}
		return fmt.Errorf("RFB server rejected security negotiation: %s", reason)
	}
	types := make([]byte, int(count[0]))
	if _, err := io.ReadFull(conn, types); err != nil {
		return fmt.Errorf("read RFB security types: %w", err)
	}
	const vncAuthentication byte = 2
	const noAuthentication byte = 1
	hasPasswordAuth := false
	hasNoAuth := false
	for _, securityType := range types {
		if securityType == vncAuthentication {
			hasPasswordAuth = true
		}
		if securityType == noAuthentication {
			hasNoAuth = true
		}
	}
	if !hasPasswordAuth && !(allowNone && hasNoAuth) {
		return fmt.Errorf("RFB server did not offer password authentication")
	}
	if !hasPasswordAuth {
		if _, err := conn.Write([]byte{noAuthentication}); err != nil {
			return fmt.Errorf("select RFB no authentication: %w", err)
		}
		result := make([]byte, 4)
		if _, err := io.ReadFull(conn, result); err != nil {
			return fmt.Errorf("read RFB no-auth security result: %w", err)
		}
		if status := binary.BigEndian.Uint32(result); status != 0 {
			return fmt.Errorf("RFB no-auth security failed with status %d", status)
		}
		return nil
	}
	if _, err := conn.Write([]byte{vncAuthentication}); err != nil {
		return fmt.Errorf("select RFB password authentication: %w", err)
	}
	challenge := make([]byte, 16)
	if _, err := io.ReadFull(conn, challenge); err != nil {
		return fmt.Errorf("read RFB password challenge: %w", err)
	}
	response, err := directSSHWebVNCChallengeResponse(password, challenge)
	if err != nil {
		return err
	}
	if _, err := conn.Write(response); err != nil {
		return fmt.Errorf("write RFB password response: %w", err)
	}
	result := make([]byte, 4)
	if _, err := io.ReadFull(conn, result); err != nil {
		return fmt.Errorf("read RFB security result: %w", err)
	}
	if status := binary.BigEndian.Uint32(result); status != 0 {
		reason, reasonErr := readRFBReason(conn)
		if reasonErr != nil {
			return fmt.Errorf("RFB password authentication failed with status %d", status)
		}
		return fmt.Errorf("RFB password authentication failed: %s", reason)
	}
	return nil
}

func directSSHWebVNCChallengeResponse(password string, challenge []byte) ([]byte, error) {
	if len(challenge) != 16 {
		return nil, fmt.Errorf("invalid RFB password challenge length %d", len(challenge))
	}
	var key [8]byte
	copy(key[:], []byte(password))
	for i := range key {
		key[i] = reverseVNCKeyByte(key[i])
	}
	// RFB 3.8 mandates this legacy DES challenge response. The VNC service and
	// noVNC proxy are loopback-only and the hop between hosts is protected by SSH.
	cipher, err := des.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("create RFB password cipher: %w", err)
	}
	response := make([]byte, len(challenge))
	for offset := 0; offset < len(challenge); offset += des.BlockSize {
		cipher.Encrypt(response[offset:offset+des.BlockSize], challenge[offset:offset+des.BlockSize])
	}
	return response, nil
}

func reverseVNCKeyByte(value byte) byte {
	value = value>>4 | value<<4
	value = (value&0xcc)>>2 | (value&0x33)<<2
	return (value&0xaa)>>1 | (value&0x55)<<1
}

func vncTunnelCommandTo(target SSHTarget, localPort, remoteHost, remotePort string) string {
	return strings.Join(shellWords(append([]string{"ssh"}, vncTunnelArgs(target, localPort, remoteHost, remotePort)...)), " ")
}

func runDirectSSHWebVNCRemoteCombinedOutput(ctx context.Context, target SSHTarget, remote string) (string, error) {
	if isWindowsWSL2Target(target) {
		// Large lifecycle scripts exceed Windows' command-line limit when nested
		// in PowerShell EncodedCommand. Stage them over SSH stdin instead.
		return runWSL2ControlScriptCombinedOutput(ctx, target, remote, 0, "10", "3")
	}
	return runSSHCombinedOutput(ctx, target, remote)
}

func directSSHNoVNCRemoteCommand(owner directSSHWebVNCRemoteOwner) string {
	return `set -eu
if ! command -v websockify >/dev/null 2>&1; then
  echo "missing websockify; warm a new --desktop lease or install novnc websockify" >&2
  exit 127
fi
if ! command -v ss >/dev/null 2>&1; then
  echo "missing ss; warm a new --desktop lease or install iproute2" >&2
  exit 127
fi
if ! command -v flock >/dev/null 2>&1; then
  echo "missing flock; warm a new --desktop lease or install util-linux" >&2
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
` + directSSHWebVNCRemoteIdentityFunctions(owner) + `
direct_webvnc_prepare_state
direct_webvnc_acquire_lock
candidate_pid=""
identity_tmp=""
direct_webvnc_start_cleanup() {
  if [ -n "$candidate_pid" ]; then
    kill "$candidate_pid" >/dev/null 2>&1 || true
  fi
  if [ -n "$identity_tmp" ]; then
    rm -f -- "$identity_tmp"
  fi
  direct_webvnc_release_lock
}
trap direct_webvnc_start_cleanup EXIT HUP INT TERM
if direct_webvnc_identity_valid; then
	direct_webvnc_print_port
  exit 0
fi
if direct_webvnc_process_identity_valid; then
  echo "refusing live direct WebVNC process without its exact owner socket" >&2
  exit 1
fi
# A stale identity may point at a port that has since been claimed by an
# unrelated process. Preserve that process, discard only the stale owner
# record, and let the allocator select another free port.
rm -f -- "$identity"
if [ -L "$log" ]; then
  echo "refusing symlink direct WebVNC log" >&2
  exit 1
fi
port_cursor="$preferred_remote_port"
launch_attempt=0
while [ "$launch_attempt" -lt 32 ]; do
  launch_attempt=$((launch_attempt + 1))
  if ! direct_webvnc_allocate_port; then
    echo "no free direct WebVNC loopback port in allocation range" >&2
    exit 1
  fi
  process_nonce="$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')"
  if ! printf '%s\n' "$process_nonce" | grep -Eq '^[0-9a-f]{32}$'; then
    echo "failed to create direct WebVNC process identity" >&2
    exit 1
  fi
  nohup env CRABBOX_DIRECT_WEBVNC_PROCESS_NONCE="$process_nonce" websockify --web="$web_dir" "127.0.0.1:$remote_port" 127.0.0.1:5900 9>&- >"$log" 2>&1 &
  candidate_pid=$!
  pid="$candidate_pid"
  started=""
  for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22 23 24 25 26 27 28 29 30 31 32 33 34 35 36 37 38 39 40 41 42 43 44 45 46 47 48 49 50; do
    if [ -r "/proc/$pid/stat" ]; then
      started="$(awk '{print $22}' "/proc/$pid/stat")"
      break
    fi
    if ! kill -0 "$pid" >/dev/null 2>&1; then
      break
    fi
    sleep 0.1
  done
  if [ -n "$started" ]; then
    boot_id="$(direct_webvnc_current_boot_id)" || {
      echo "failed to read Linux boot identity" >&2
      exit 1
    }
    identity_tmp="$state_dir/.$expected_owner_id.identity.$$.$launch_attempt"
    printf '%s %s %s %s %s %s\n' "$pid" "$started" "$boot_id" "$expected_owner_id" "$remote_port" "$process_nonce" >"$identity_tmp"
    chmod 0600 "$identity_tmp"
    mv -f -- "$identity_tmp" "$identity"
    identity_tmp=""
    for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
      if direct_webvnc_identity_valid; then
        candidate_pid=""
        direct_webvnc_print_port
        direct_webvnc_release_lock
        trap - EXIT HUP INT TERM
        exit 0
      fi
      if ! direct_webvnc_process_alive_same "$pid" "$started"; then
        break
      fi
      sleep 0.1
    done
  fi
  kill "$candidate_pid" >/dev/null 2>&1 || true
  candidate_pid=""
  rm -f -- "$identity"
done
cat "$log" >&2 || true
echo "failed to start direct WebVNC after collision retries" >&2
exit 1`
}

func directSSHWebVNCRemoteIdentityFunctions(owner directSSHWebVNCRemoteOwner) string {
	return `expected_owner_id=` + shellQuote(owner.ID) + `
preferred_remote_port=` + shellQuote(owner.PreferredPort) + `
remote_port=""
port_base=` + strconv.Itoa(directSSHWebVNCRemotePortBase) + `
port_span=` + strconv.Itoa(directSSHWebVNCRemotePortSpan) + `
state_root="${XDG_STATE_HOME:-$HOME/.local/state}"
state_dir="$state_root/crabbox/direct-webvnc"
identity="$state_dir/$expected_owner_id.identity"
log="$state_dir/$expected_owner_id.log"
allocation_lock="$state_dir/allocation.lock"
allocation_lock_held=""
direct_webvnc_valid_port() {
  candidate_port="$1"
  case "$candidate_port" in *[!0-9]*|'') return 1 ;; esac
  [ "$candidate_port" -ge "$port_base" ] && [ "$candidate_port" -lt $((port_base + port_span)) ]
}
direct_webvnc_prepare_state() {
  printf '%s\n' "$expected_owner_id" | grep -Eq '^[0-9a-f]{64}$' || return 1
  direct_webvnc_valid_port "$preferred_remote_port" || return 1
  case "$state_dir" in
    /*) ;;
    *) echo "direct WebVNC state directory must be absolute" >&2; return 1 ;;
  esac
  if [ -L "$state_dir" ]; then
    echo "refusing symlink direct WebVNC state directory" >&2
    return 1
  fi
  mkdir -p -- "$state_dir"
  if [ ! -d "$state_dir" ] || [ "$(stat -c %u "$state_dir")" != "$(id -u)" ]; then
    echo "direct WebVNC state directory must be owned by the SSH user" >&2
    return 1
  fi
  chmod 0700 "$state_dir"
  if [ -L "$allocation_lock" ]; then
    echo "refusing symlink direct WebVNC allocation lock" >&2
    return 1
  fi
  if [ -L "$identity" ]; then
    echo "refusing symlink direct WebVNC identity" >&2
    return 1
  fi
}
direct_webvnc_acquire_lock() {
  if [ -L "$allocation_lock" ]; then
    echo "refusing symlink direct WebVNC allocation lock" >&2
    return 1
  fi
  exec 9>>"$allocation_lock"
  chmod 0600 "$allocation_lock"
  if ! flock -x 9; then
    exec 9>&-
    return 1
  fi
  allocation_lock_held=1
  if [ -L "$identity" ]; then
    echo "refusing symlink direct WebVNC identity" >&2
    direct_webvnc_release_lock
    return 1
  fi
}
direct_webvnc_release_lock() {
  if [ -n "$allocation_lock_held" ]; then
    flock -u 9 >/dev/null 2>&1 || true
    exec 9>&-
    allocation_lock_held=""
  fi
}
direct_webvnc_current_boot_id() {
  [ -r /proc/sys/kernel/random/boot_id ] || return 1
  current_boot_id="$(cat /proc/sys/kernel/random/boot_id)" || return 1
  printf '%s\n' "$current_boot_id" | grep -Eq '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' || return 1
  printf '%s\n' "$current_boot_id"
}
direct_webvnc_load_identity() {
  remote_port=""
  [ ! -L "$identity" ] && [ -f "$identity" ] || return 1
  IFS=' ' read -r pid started boot_id owner_id recorded_port process_nonce extra <"$identity" || return 1
  [ -z "${extra:-}" ] || return 1
  case "$pid:$started" in
    *[!0-9:]*|:*|*:|*:*:*) return 1 ;;
  esac
  printf '%s\n' "$boot_id" | grep -Eq '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' || return 1
  [ "$owner_id" = "$expected_owner_id" ] || return 1
  direct_webvnc_valid_port "$recorded_port" || return 1
  printf '%s\n' "$process_nonce" | grep -Eq '^[0-9a-f]{32}$' || return 1
  remote_port="$recorded_port"
}
direct_webvnc_socket() {
  direct_webvnc_valid_port "$remote_port" || return 1
  ss -H -4 -ltnp "sport = :$remote_port" 2>/dev/null | awk -v expected="127.0.0.1:$remote_port" '$1 == "LISTEN" && $4 == expected { print }'
}
direct_webvnc_port_in_use() {
  candidate_port="$1"
  direct_webvnc_valid_port "$candidate_port" || return 0
  [ -n "$(ss -H -4 -ltn "sport = :$candidate_port" 2>/dev/null)" ]
}
direct_webvnc_allocate_port() {
  candidate_port="$port_cursor"
  checked=0
  while [ "$checked" -lt "$port_span" ]; do
    if ! direct_webvnc_port_in_use "$candidate_port"; then
      remote_port="$candidate_port"
      port_cursor=$((candidate_port + 1))
      if [ "$port_cursor" -ge $((port_base + port_span)) ]; then
        port_cursor="$port_base"
      fi
      return 0
    fi
    candidate_port=$((candidate_port + 1))
    if [ "$candidate_port" -ge $((port_base + port_span)) ]; then
      candidate_port="$port_base"
    fi
    checked=$((checked + 1))
  done
  return 1
}
direct_webvnc_print_port() {
  direct_webvnc_valid_port "$remote_port" || return 1
  printf '` + directSSHWebVNCRemotePortPrefix + `%s\n' "$remote_port"
}
direct_webvnc_process_identity_valid() {
  direct_webvnc_load_identity || return 1
  [ "$boot_id" = "$(direct_webvnc_current_boot_id)" ] || return 1
  [ -r "/proc/$pid/stat" ] && [ -r "/proc/$pid/cmdline" ] && [ -r "/proc/$pid/environ" ] || return 1
  [ "$(awk '{print $22}' "/proc/$pid/stat")" = "$started" ] || return 1
  tr '\000' '\n' <"/proc/$pid/environ" | grep -Fx -- "CRABBOX_DIRECT_WEBVNC_PROCESS_NONCE=$process_nonce" >/dev/null || return 1
  command_lines="$(tr '\000' '\n' <"/proc/$pid/cmdline")"
  printf '%s\n' "$command_lines" | grep -Eq '(^|/)websockify$' || return 1
  printf '%s\n' "$command_lines" | grep -Fx -- "127.0.0.1:$remote_port" >/dev/null || return 1
  printf '%s\n' "$command_lines" | grep -Fx -- '127.0.0.1:5900' >/dev/null || return 1
}
direct_webvnc_process_alive_same() {
  expected_pid="$1"
  expected_started="$2"
  [ -r "/proc/$expected_pid/stat" ] || return 1
  [ "$(awk '{print $22}' "/proc/$expected_pid/stat")" = "$expected_started" ] || return 1
  [ "$(awk '{print $3}' "/proc/$expected_pid/stat")" != "Z" ] || return 1
}
direct_webvnc_identity_valid() {
  direct_webvnc_process_identity_valid || return 1
  socket="$(direct_webvnc_socket)"
  [ "$(printf '%s\n' "$socket" | sed '/^$/d' | wc -l)" -eq 1 ] || return 1
  socket_pids="$(printf '%s\n' "$socket" | grep -o 'pid=[0-9]*' | cut -d= -f2 | sort -u)"
  [ "$socket_pids" = "$pid" ] || return 1
}`
}

func directSSHWebVNCRemoteStatusCommand(owner directSSHWebVNCRemoteOwner) string {
	return `set -u
` + directSSHWebVNCRemoteIdentityFunctions(owner) + `
if ! command -v flock >/dev/null 2>&1; then
  echo stopped
  exit 0
fi
if ! direct_webvnc_prepare_state || ! direct_webvnc_acquire_lock; then
  echo stopped
  exit 0
fi
trap direct_webvnc_release_lock EXIT HUP INT TERM
if direct_webvnc_identity_valid; then
  echo running
  direct_webvnc_print_port
else
  echo stopped
fi
direct_webvnc_release_lock
trap - EXIT HUP INT TERM`
}

func directSSHWebVNCResetRemoteCommand(owner directSSHWebVNCRemoteOwner) string {
	return `set -eu
if ! command -v flock >/dev/null 2>&1; then
  echo "missing flock; warm a new --desktop lease or install util-linux" >&2
  exit 127
fi
` + directSSHWebVNCRemoteIdentityFunctions(owner) + `
direct_webvnc_prepare_state
direct_webvnc_acquire_lock
trap direct_webvnc_release_lock EXIT HUP INT TERM
if direct_webvnc_process_identity_valid; then
  socket="$(direct_webvnc_socket)"
  owned_pid="$pid"
  owned_started="$started"
  owned_boot_id="$boot_id"
  owned_owner_id="$owner_id"
  owned_port="$recorded_port"
  owned_process_nonce="$process_nonce"
  if [ -n "$socket" ] && ! direct_webvnc_identity_valid; then
    echo "refusing unrelated listener on owner socket 127.0.0.1:$remote_port" >&2
    exit 1
  fi
  if ! direct_webvnc_process_identity_valid \
    || [ "$pid" != "$owned_pid" ] \
    || [ "$started" != "$owned_started" ] \
    || [ "$boot_id" != "$owned_boot_id" ] \
    || [ "$owner_id" != "$owned_owner_id" ] \
    || [ "$recorded_port" != "$owned_port" ] \
    || [ "$process_nonce" != "$owned_process_nonce" ]; then
    echo "owned direct WebVNC identity changed before stop" >&2
    exit 1
  fi
  if ! kill "$owned_pid" >/dev/null 2>&1 && direct_webvnc_process_alive_same "$owned_pid" "$owned_started"; then
    echo "failed to stop owned direct WebVNC process" >&2
    exit 1
  fi
  for i in 1 2 3 4 5; do
    if ! direct_webvnc_process_alive_same "$owned_pid" "$owned_started"; then
      break
    fi
    sleep 1
  done
  if direct_webvnc_process_alive_same "$owned_pid" "$owned_started"; then
    if ! direct_webvnc_process_identity_valid \
      || [ "$pid" != "$owned_pid" ] \
      || [ "$started" != "$owned_started" ] \
      || [ "$boot_id" != "$owned_boot_id" ] \
      || [ "$owner_id" != "$owned_owner_id" ] \
      || [ "$recorded_port" != "$owned_port" ] \
      || [ "$process_nonce" != "$owned_process_nonce" ]; then
      echo "owned direct WebVNC identity changed before forced stop" >&2
      exit 1
    fi
    kill -KILL "$owned_pid" >/dev/null 2>&1 || true
    for i in 1 2 3 4 5; do
      if ! direct_webvnc_process_alive_same "$owned_pid" "$owned_started"; then
        break
      fi
      sleep 1
    done
  fi
  if direct_webvnc_process_alive_same "$owned_pid" "$owned_started"; then
    echo "owned direct WebVNC process did not stop" >&2
    exit 1
  fi
  if [ -n "$(direct_webvnc_socket)" ]; then
    echo "refusing unrelated listener on owner socket 127.0.0.1:$remote_port" >&2
    exit 1
  fi
  expected_identity="$owned_pid $owned_started $owned_boot_id $owned_owner_id $owned_port $owned_process_nonce"
  if [ -L "$identity" ] || [ ! -f "$identity" ] || [ "$(cat "$identity")" != "$expected_identity" ]; then
    echo "owned direct WebVNC identity changed during stop" >&2
    exit 1
  fi
  rm -f -- "$identity"
else
  # Never signal a process that failed exact identity validation. A stale
  # port collision is handled by the locked allocator used below.
  rm -f -- "$identity"
fi
direct_webvnc_release_lock
trap - EXIT HUP INT TERM
if [ -x /usr/local/bin/crabbox-start-desktop ]; then
  if grep -q 'desktop-theme' /usr/local/bin/crabbox-start-desktop; then
    sudo CRABBOX_SSH_USER="$(id -un)" /usr/local/bin/crabbox-start-desktop
  else
    sudo /bin/bash /usr/local/bin/crabbox-start-desktop
  fi
fi
` + directSSHNoVNCRemoteCommand(owner)
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
			handled, err := b.handleControlFrame(data)
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

func (b *webVNCBridge) handleControlFrame(data []byte) (bool, error) {
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
	var theme string
	switch strings.TrimSpace(msg.Theme) {
	case "light":
		theme = "light"
	case "dark":
		theme = "dark"
	default:
		return true, fmt.Errorf("ignore invalid desktop theme %q", msg.Theme)
	}
	if b.target.TargetOS != "" && b.target.TargetOS != targetLinux {
		return true, nil
	}
	b.queueDesktopThemeUpdate(theme)
	return true, nil
}

func (b *webVNCBridge) queueDesktopThemeUpdate(theme string) {
	select {
	case b.desktopThemeUpdates <- theme:
		return
	default:
	}
	select {
	case <-b.desktopThemeUpdates:
	default:
	}
	select {
	case b.desktopThemeUpdates <- theme:
	default:
	}
}

func (b *webVNCBridge) serveDesktopThemeUpdates(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case theme := <-b.desktopThemeUpdates:
			applyCtx, cancel := context.WithTimeout(ctx, webVNCDesktopThemeApplyTimeout(b.target))
			err := b.applyDesktopTheme(applyCtx, theme)
			cancel()
			if err != nil && ctx.Err() == nil && b.log != nil {
				fmt.Fprintf(b.log, "bridge: %v\n", err)
			}
		}
	}
}

func webVNCDesktopThemeApplyTimeout(target SSHTarget) time.Duration {
	candidates := len(sshPortCandidates(target.Port, target.FallbackPorts))
	if candidates == 0 {
		candidates = 1
	}
	return time.Duration(candidates) * webVNCDesktopThemeSSHAttemptTimeout
}

func (b *webVNCBridge) applyDesktopTheme(ctx context.Context, theme string) error {
	if b.applyDesktopThemeFunc != nil {
		return b.applyDesktopThemeFunc(ctx, theme)
	}
	out, err := runSSHCombinedOutput(ctx, b.target, webVNCDesktopThemeCommand(theme, b.target.User))
	if err != nil {
		detail := strings.TrimSpace(out)
		if detail == "" {
			return fmt.Errorf("apply desktop theme %s: %w", theme, err)
		}
		return fmt.Errorf("apply desktop theme %s: %w: %s", theme, err, detail)
	}
	return nil
}

func webVNCDesktopThemeCommand(theme, user string) string {
	switch strings.TrimSpace(theme) {
	case "light":
		theme = "light"
	case "dark":
		theme = "dark"
	default:
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
    (
      if XDG_RUNTIME_DIR="$runtime" WAYLAND_DISPLAY="${WAYLAND_DISPLAY:-wayland-0}" swaybg -i "$wallpaper_file" -m fill; then
        exit 0
      else
        status=$?
      fi
      [ "$status" -lt 128 ] || exit "$status"
      exec env XDG_RUNTIME_DIR="$runtime" WAYLAND_DISPLAY="${WAYLAND_DISPLAY:-wayland-0}" swaybg -c "$wallpaper_bg"
    ) </dev/null >/tmp/crabbox-swaybg.log 2>&1 &
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

func bridgeUpgradeTicketHeaders(ctx context.Context, coord *CoordinatorClient, ticket string) (http.Header, error) {
	headers, err := coord.webVNCAccessHeaders(ctx)
	if err != nil {
		return nil, err
	}
	headers.Set("X-Crabbox-Bridge-Ticket", ticket)
	return headers, nil
}

func retryBridgeTicketInAuthorization(resp *http.Response, err error) bool {
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

type webVNCPortalOptions struct {
	TakeControl bool
}

func createWebVNCPortalURL(ctx context.Context, coord *CoordinatorClient, leaseID, username, password string, opts ...webVNCPortalOptions) (string, error) {
	handoff := ""
	if strings.TrimSpace(username) != "" || strings.TrimSpace(password) != "" {
		issued, err := coord.CreateWebVNCCredentialHandoff(ctx, leaseID, username, password)
		if err != nil {
			return "", fmt.Errorf("create WebVNC credential handoff: %w", err)
		}
		if !validWebVNCCredentialHandoffTicket(issued.Ticket) {
			return "", fmt.Errorf("create WebVNC credential handoff: coordinator returned an invalid ticket")
		}
		handoff = issued.Ticket
	}
	return webVNCPortalURL(coord.BaseURL, leaseID, handoff, opts...), nil
}

func validWebVNCCredentialHandoffTicket(value string) bool {
	const prefix = "vnc_handoff_"
	if len(value) != len(prefix)+32 || !strings.HasPrefix(value, prefix) {
		return false
	}
	for _, character := range value[len(prefix):] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func webVNCPortalURL(base, leaseID, handoff string, opts ...webVNCPortalOptions) string {
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
	if strings.TrimSpace(handoff) != "" || takeControl {
		values := url.Values{}
		if strings.TrimSpace(handoff) != "" {
			values.Set("handoff", strings.TrimSpace(handoff))
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

func stopProcess(tunnel *vncForegroundTunnel) {
	if tunnel == nil || tunnel.cmd == nil || tunnel.cmd.Process == nil {
		return
	}
	_ = tunnel.cmd.Process.Kill()
	select {
	case <-tunnel.Done():
	case <-time.After(2 * time.Second):
	}
}
