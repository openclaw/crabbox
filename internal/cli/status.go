package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
)

func (a App) status(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("status", a.Stderr)
	provider := fs.String("provider", defaults.Provider, providerHelpAll())
	id := fs.String("id", "", "lease id or slug")
	wait := fs.Bool("wait", false, "wait until ready")
	waitTimeout := fs.Duration("wait-timeout", 5*time.Minute, "maximum wait duration")
	jsonOut := fs.Bool("json", false, "print JSON")
	providerFlags := registerProviderFlags(fs, defaults)
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	setIDFromFirstArg(fs, id)
	cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{LeaseID: *id})
	if err != nil {
		return err
	}
	if err := applyProviderFlags(&cfg, fs, providerFlags); err != nil {
		return err
	}
	if err := requireLeaseID(*id, "crabbox status --id <lease-id-or-slug>", cfg); err != nil {
		return err
	}
	backend, err := loadBackend(cfg, runtimeForApp(a))
	if err != nil {
		return err
	}
	statusBackend, isStatus := backend.(interface {
		Status(context.Context, StatusRequest) (statusView, error)
	})
	delegated, isDelegated := backend.(DelegatedRunBackend)
	sshBackend, isSSH := backend.(SSHLeaseBackend)
	statusCtx := ctx
	cancel := func() {}
	if *wait {
		statusCtx, cancel = context.WithTimeout(ctx, *waitTimeout)
	}
	defer cancel()
	for {
		var state statusView
		var err error
		if isStatus {
			state, err = statusBackend.Status(statusCtx, StatusRequest{Options: leaseOptionsFromConfig(cfg), ID: *id, Wait: *wait, WaitTimeout: *waitTimeout})
		} else if isDelegated {
			state, err = delegated.Status(statusCtx, StatusRequest{Options: leaseOptionsFromConfig(cfg), ID: *id, Wait: *wait, WaitTimeout: *waitTimeout})
		} else if isSSH {
			var lease LeaseTarget
			lease, err = sshBackend.Resolve(statusCtx, ResolveRequest{Options: leaseOptionsFromConfig(cfg), ID: *id, StatusOnly: true, ReadyProbe: *wait})
			if err == nil {
				state, err = statusViewFromLeaseTarget(statusCtx, cfg, lease)
				if err == nil && *wait {
					_, touchErr := sshBackend.Touch(statusCtx, TouchRequest{Lease: lease, State: state.State, IdleTimeout: cfg.IdleTimeout})
					if touchErr != nil {
						fmt.Fprintf(a.Stderr, "warning: touch failed for %s: %v\n", lease.LeaseID, touchErr)
					}
				}
			}
		} else {
			state, err = a.leaseStatus(statusCtx, cfg, *id)
		}
		if err != nil {
			if *wait && errors.Is(statusCtx.Err(), context.DeadlineExceeded) {
				timeoutErr := exit(5, "timed out waiting for %s to become ready", *id)
				if err != statusCtx.Err() {
					return errors.Join(timeoutErr, err)
				}
				return timeoutErr
			}
			return err
		}
		if *jsonOut {
			if !*wait || statusWaitDone(state) {
				if err := json.NewEncoder(a.Stdout).Encode(state); err != nil {
					return err
				}
				if *wait {
					return statusWaitTerminalError(*id, state)
				}
				return nil
			}
		} else {
			tailscale := ""
			if state.Tailscale != nil && state.Tailscale.Enabled {
				tailscale = fmt.Sprintf(" tailscale=%s", blank(tailscaleTargetHost(*state.Tailscale), blank(state.Tailscale.State, "requested")))
			}
			telemetry := leaseTelemetryStatusSummary(state.Telemetry)
			if telemetry != "" {
				telemetry = " " + telemetry
			}
			fmt.Fprintf(a.Stdout, "%s slug=%s provider=%s target=%s windows_mode=%s state=%s type=%s host=%s pond=%s network=%s%s ready=%t has_host=%t idle_for=%s idle_timeout=%s expires=%s%s\n", state.ID, blank(state.Slug, "-"), state.Provider, state.TargetOS, blank(state.WindowsMode, "-"), state.State, state.ServerType, state.Host, blank(state.Pond, "-"), state.Network, tailscale, state.Ready, state.HasHost, blank(state.IdleFor, "-"), blank(state.IdleTimeout, "-"), blank(state.ExpiresAt, "-"), telemetry)
		}
		if *wait {
			if err := statusWaitTerminalError(*id, state); err != nil {
				return err
			}
		}
		if !*wait || statusWaitDone(state) {
			return nil
		}
		select {
		case <-statusCtx.Done():
			if errors.Is(statusCtx.Err(), context.DeadlineExceeded) {
				return exit(5, "timed out waiting for %s to become ready", *id)
			}
			return statusCtx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func statusWaitDone(state statusView) bool {
	return state.Ready || statusTerminalState(state.State)
}

func statusWaitTerminalError(id string, state statusView) error {
	if state.Ready || !statusTerminalState(state.State) {
		return nil
	}
	return exit(5, "lease %s reached terminal state %s before ready", id, state.State)
}

func statusTerminalState(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "deleting", "expired", "failed", "missing", "released", "stopped", "stopped_with_code", "terminated":
		return true
	default:
		return false
	}
}

func statusViewFromLeaseTarget(ctx context.Context, cfg Config, lease LeaseTarget) (statusView, error) {
	server := lease.Server
	target := lease.SSH
	hasHost := server.PublicNet.IPv4.IP != ""
	if target.NetworkKind == NetworkPublic && target.Host != "" {
		hasHost = true
	}
	if (cfg.Provider == "daytona" || server.Provider == "daytona") && target.Host != "" {
		hasHost = true
	}
	resolved, err := resolveNetworkTarget(ctx, cfg, server, target)
	if err != nil {
		return statusView{}, err
	}
	target = resolved.Target
	state := blank(server.Labels["state"], server.Status)
	ready := hasHost && leaseStatusStateCanBeReady(lease, state) && probeSSHReady(ctx, &target, 4*time.Second)
	meta := serverTailscaleMetadata(server)
	var tailscale *TailscaleMetadata
	if meta.Enabled {
		tailscale = &meta
	}
	return statusView{
		ID:               lease.LeaseID,
		Slug:             serverSlug(server),
		Provider:         blank(server.Provider, cfg.Provider),
		TargetOS:         blank(server.Labels["target"], cfg.TargetOS),
		WindowsMode:      blank(server.Labels["windows_mode"], cfg.WindowsMode),
		State:            state,
		ServerID:         server.DisplayID(),
		ServerType:       server.ServerType.Name,
		Host:             server.PublicNet.IPv4.IP,
		Pond:             blank(server.Labels[pondLabelKey], cfg.Pond),
		Network:          resolved.Network,
		Tailscale:        tailscale,
		SSHHost:          target.Host,
		SSHUser:          redactedSSHUser(cfg, server, target),
		SSHPort:          target.Port,
		SSHFallbackPorts: target.FallbackPorts,
		SSHKey:           target.Key,
		LastTouchedAt:    blank(leaseLabelTimeDisplay(server.Labels["last_touched_at"]), server.Labels["last_touched_at"]),
		IdleFor:          idleForString(server.Labels["last_touched_at"], time.Now()),
		IdleTimeout:      leaseLabelDurationDisplay(server.Labels["idle_timeout_secs"], server.Labels["idle_timeout"]),
		ExpiresAt:        blank(leaseLabelTimeDisplay(server.Labels["expires_at"]), server.Labels["expires_at"]),
		Labels:           server.Labels,
		HasHost:          hasHost,
		Ready:            ready,
	}, nil
}

func leaseStatusStateCanBeReady(lease LeaseTarget, state string) bool {
	if lease.Coordinator != nil {
		return state == "active"
	}
	return state != "provisioning" && !statusTerminalState(state)
}

type StatusView struct {
	ID               string             `json:"id"`
	Slug             string             `json:"slug,omitempty"`
	Provider         string             `json:"provider"`
	TargetOS         string             `json:"target"`
	WindowsMode      string             `json:"windowsMode,omitempty"`
	State            string             `json:"state"`
	ServerID         string             `json:"serverId"`
	ServerType       string             `json:"serverType"`
	Host             string             `json:"host"`
	Pond             string             `json:"pond,omitempty"`
	Network          NetworkMode        `json:"network"`
	Tailscale        *TailscaleMetadata `json:"tailscale,omitempty"`
	SSHHost          string             `json:"sshHost"`
	SSHUser          string             `json:"sshUser"`
	SSHPort          string             `json:"sshPort"`
	SSHFallbackPorts []string           `json:"sshFallbackPorts,omitempty"`
	SSHKey           string             `json:"sshKey"`
	LastTouchedAt    string             `json:"lastTouchedAt,omitempty"`
	IdleFor          string             `json:"idleFor,omitempty"`
	IdleTimeout      string             `json:"idleTimeout,omitempty"`
	ExpiresAt        string             `json:"expiresAt,omitempty"`
	Labels           map[string]string  `json:"labels,omitempty"`
	HasHost          bool               `json:"hasHost"`
	Ready            bool               `json:"ready"`
	Telemetry        *LeaseTelemetry    `json:"telemetry,omitempty"`
	TelemetryHistory []*LeaseTelemetry  `json:"telemetryHistory,omitempty"`
}

type statusView = StatusView

func (a App) leaseStatus(ctx context.Context, cfg Config, id string) (statusView, error) {
	backend, err := loadBackend(cfg, runtimeForApp(a))
	if err != nil {
		return statusView{}, err
	}
	if statusBackend, ok := backend.(interface {
		Status(context.Context, StatusRequest) (statusView, error)
	}); ok {
		return statusBackend.Status(ctx, StatusRequest{Options: leaseOptionsFromConfig(cfg), ID: id})
	}
	if delegated, ok := backend.(DelegatedRunBackend); ok {
		return delegated.Status(ctx, StatusRequest{Options: leaseOptionsFromConfig(cfg), ID: id})
	}
	sshBackend, ok := backend.(SSHLeaseBackend)
	if !ok {
		return statusView{}, exit(2, "provider=%s does not support status", backend.Spec().Name)
	}
	lease, err := sshBackend.Resolve(ctx, ResolveRequest{Options: leaseOptionsFromConfig(cfg), ID: id, StatusOnly: true})
	if err != nil {
		return statusView{}, err
	}
	return statusViewFromLeaseTarget(ctx, cfg, lease)
}

func (a App) resolveLeaseTargetForRepo(ctx context.Context, cfg Config, id string, repo Repo, reclaim bool) (Server, SSHTarget, string, error) {
	return a.resolveLeaseTargetForRepoWithConfig(ctx, &cfg, id, repo, reclaim)
}

func (a App) resolveLeaseTargetForRepoWithConfig(ctx context.Context, cfg *Config, id string, repo Repo, reclaim bool) (Server, SSHTarget, string, error) {
	return a.resolveLeaseTargetWithRequestConfig(ctx, cfg, ResolveRequest{Repo: repo, ID: id, Reclaim: reclaim})
}

func (a App) resolveLeaseTargetWithRequestConfig(ctx context.Context, cfg *Config, req ResolveRequest) (Server, SSHTarget, string, error) {
	return a.resolveSSHTargetWithRequestConfig(ctx, cfg, req, false)
}

func (a App) resolveLoginTargetWithRequestConfig(ctx context.Context, cfg *Config, req ResolveRequest) (Server, SSHTarget, string, error) {
	return a.resolveSSHTargetWithRequestConfig(ctx, cfg, req, true)
}

func (a App) resolveSSHTargetWithRequestConfig(ctx context.Context, cfg *Config, req ResolveRequest, allowLoginOnly bool) (Server, SSHTarget, string, error) {
	if cfg == nil {
		return Server{}, SSHTarget{}, "", exit(2, "lease target config is required")
	}
	if err := autoRouteExternalLeaseForConfig(cfg, req.ID); err != nil {
		return Server{}, SSHTarget{}, "", err
	}
	backend, err := loadBackend(*cfg, runtimeForApp(a))
	if err != nil {
		return Server{}, SSHTarget{}, "", err
	}
	sshBackend, ok := backend.(SSHLoginBackend)
	if !ok {
		return Server{}, SSHTarget{}, "", exit(2, "provider=%s does not expose an SSH target", backend.Spec().Name)
	}
	if !allowLoginOnly {
		if _, ok := backend.(SSHLeaseBackend); !ok {
			return Server{}, SSHTarget{}, "", exit(2, "provider=%s exposes SSH login only, not a Crabbox-managed SSH lease", backend.Spec().Name)
		}
	}
	req.Options = leaseOptionsFromConfig(*cfg)
	req.Options.ProviderScope = providerClaimScope(backend.Spec().Name, *cfg)
	lease, err := resolveSSHLeaseTarget(ctx, sshBackend, req)
	if err != nil {
		return Server{}, SSHTarget{}, "", err
	}
	applyResolvedLeaseConfig(cfg, lease.Server, &lease.SSH)
	return lease.Server, lease.SSH, lease.LeaseID, nil
}

func resolveSSHLeaseTarget(ctx context.Context, backend SSHLoginBackend, req ResolveRequest) (LeaseTarget, error) {
	claimsBefore, err := snapshotLeaseClaims()
	if err != nil {
		return LeaseTarget{}, err
	}
	lease, err := backend.Resolve(ctx, req)
	if err != nil {
		return LeaseTarget{}, err
	}
	resolvedLeaseID := lease.LeaseID
	resolvedClaimExistedBefore := false
	for _, claim := range claimsBefore.claims {
		if claim.LeaseID == resolvedLeaseID {
			resolvedClaimExistedBefore = true
			break
		}
	}
	claimBefore, claimExistedBefore, err := resolvedLeaseClaimBefore(claimsBefore, backend.Spec().Name, req.Options.ProviderScope, req.ID, lease)
	if err != nil {
		return LeaseTarget{}, err
	}
	expectedRepoRoot := strings.TrimSpace(req.Repo.Root)
	if expectedRepoRoot == "" && claimExistedBefore {
		expectedRepoRoot = strings.TrimSpace(claimBefore.RepoRoot)
	}
	if claimExistedBefore {
		leaseIDChanged := lease.LeaseID != claimBefore.LeaseID
		var discardedClaim leaseClaim
		discardedClaimExists := false
		if leaseIDChanged && !resolvedClaimExistedBefore && validLeaseClaimID(resolvedLeaseID) {
			resolvedClaim, resolvedClaimExists, err := readLeaseClaimWithPresence(resolvedLeaseID)
			if err != nil {
				return LeaseTarget{}, err
			}
			if resolvedClaimExists {
				if !resolvedLeaseClaimAttestsResult(resolvedClaim, lease.Server, expectedRepoRoot, req.Options.ProviderScope) {
					return LeaseTarget{}, exit(2, "lease %s claim changed during resolve; retry", resolvedLeaseID)
				}
				discardedClaim = resolvedClaim
				discardedClaimExists = true
			}
		}
		lease.LeaseID = claimBefore.LeaseID
		lease.Server.Labels = cloneStringMap(lease.Server.Labels)
		if lease.Server.Labels == nil {
			lease.Server.Labels = map[string]string{}
		}
		lease.Server.Labels["lease"] = claimBefore.LeaseID
		if claimBefore.Slug != "" {
			lease.Server.Labels["slug"] = claimBefore.Slug
		}
		if rebinder, ok := backend.(ResolvedLeaseTargetRebinder); ok && leaseIDChanged {
			if err := rebinder.RebindResolvedLeaseTarget(&lease, claimBefore.LeaseID); err != nil {
				return LeaseTarget{}, err
			}
		}
		if leaseIDChanged && !resolvedClaimExistedBefore && validLeaseClaimID(resolvedLeaseID) {
			if aliasKeyPath, err := testboxKeyPath(resolvedLeaseID); err == nil && lease.SSH.Key == aliasKeyPath {
				return LeaseTarget{}, exit(2, "lease %s resolved to %s but the canonical stored SSH key is unavailable", resolvedLeaseID, claimBefore.LeaseID)
			}
			if discardedClaimExists {
				if err := removeLeaseClaimIfUnchanged(resolvedLeaseID, discardedClaim); err != nil {
					return LeaseTarget{}, err
				}
			}
			removeStoredTestboxKey(resolvedLeaseID)
		}
	}
	var claimAfter leaseClaim
	claimExistsAfter := false
	if lease.LeaseID != "" {
		if claimAfter, claimExistsAfter, err = readLeaseClaimWithPresence(lease.LeaseID); err != nil {
			return LeaseTarget{}, err
		}
	}
	lease.Server.claimSnapshotSet = true
	if claimExistsAfter && ((claimExistedBefore && reflect.DeepEqual(claimBefore, claimAfter)) ||
		(claimExistedBefore && resolvedLeaseClaimMutationAttests(claimBefore, claimAfter, lease.Server, expectedRepoRoot)) ||
		(!claimExistedBefore && resolvedLeaseClaimAttestsResult(claimAfter, lease.Server, expectedRepoRoot, req.Options.ProviderScope))) {
		lease.Server.claimSnapshot = cloneLeaseClaim(claimAfter)
		lease.Server.claimSnapshotExists = true
	} else if claimExistedBefore {
		lease.Server.claimSnapshot = cloneLeaseClaim(claimBefore)
		lease.Server.claimSnapshotExists = true
	}
	return lease, nil
}

func resolvedLeaseClaimBefore(snapshot leaseClaimsSnapshot, provider, providerScope, identifier string, lease LeaseTarget) (leaseClaim, bool, error) {
	provider = canonicalClaimProvider(provider)
	providerScope = strings.TrimSpace(providerScope)
	for _, id := range []string{lease.LeaseID, identifier} {
		if err := snapshot.invalid[id]; err != nil {
			return leaseClaim{}, false, err
		}
	}
	providerMatches := func(claim leaseClaim) bool {
		claimProvider := canonicalClaimProvider(claim.Provider)
		return claimProvider == "" || claimProvider == provider
	}
	scopeMatches := func(claim leaseClaim) bool {
		return strings.TrimSpace(claim.ProviderScope) == providerScope
	}
	for _, claim := range snapshot.claims {
		if providerMatches(claim) && claim.LeaseID == lease.LeaseID &&
			(strings.TrimSpace(claim.ProviderScope) == "" || providerScope == "" || scopeMatches(claim)) {
			return cloneLeaseClaim(claim), true, nil
		}
	}
	findUnique := func(match func(leaseClaim) bool) (leaseClaim, bool, error) {
		var found leaseClaim
		for _, claim := range snapshot.claims {
			if !providerMatches(claim) || !match(claim) {
				continue
			}
			if found.LeaseID != "" && found.LeaseID != claim.LeaseID {
				return leaseClaim{}, false, exit(2, "multiple provider=%s claims match resolved lease %s", provider, firstNonBlank(lease.LeaseID, identifier))
			}
			found = cloneLeaseClaim(claim)
		}
		return found, found.LeaseID != "", nil
	}
	if lease.Server.CloudID != "" {
		if claim, ok, err := findUnique(func(candidate leaseClaim) bool {
			return canonicalClaimProvider(candidate.Provider) == provider && scopeMatches(candidate) && candidate.CloudID == lease.Server.CloudID
		}); err != nil || ok {
			return claim, ok, err
		}
	}
	return findUnique(func(candidate leaseClaim) bool {
		if candidate.LeaseID == identifier {
			return strings.TrimSpace(candidate.ProviderScope) == "" || providerScope == "" || scopeMatches(candidate)
		}
		slug := normalizeLeaseSlug(identifier)
		if slug != "" && normalizeLeaseSlug(candidate.Slug) == slug {
			return scopeMatches(candidate) &&
				(candidate.CloudID == "" || lease.Server.CloudID == "" || candidate.CloudID == lease.Server.CloudID)
		}
		return canonicalClaimProvider(candidate.Provider) == provider && scopeMatches(candidate) && candidate.CloudID == identifier
	})
}

func resolvedLeaseClaimAttestsResult(claim leaseClaim, server Server, expectedRepoRoot, expectedProviderScope string) bool {
	claimProvider := canonicalClaimProvider(claim.Provider)
	serverProvider := canonicalClaimProvider(firstNonBlank(server.Labels["provider"], server.Provider))
	claimState := strings.ToLower(strings.TrimSpace(claim.Labels["state"]))
	serverState := strings.ToLower(strings.TrimSpace(firstNonBlank(server.Labels["state"], server.Status)))
	return (claimProvider == "" || serverProvider == "" || claimProvider == serverProvider) &&
		(strings.TrimSpace(expectedProviderScope) == "" || strings.TrimSpace(claim.ProviderScope) == strings.TrimSpace(expectedProviderScope)) &&
		(claim.CloudID == "" || server.CloudID == "" || claim.CloudID == server.CloudID) &&
		resolvedLeaseClaimStateAttests(claimState, serverState) &&
		strings.TrimSpace(claim.RepoRoot) == expectedRepoRoot
}

func resolvedLeaseClaimMutationAttests(before, after leaseClaim, server Server, expectedRepoRoot string) bool {
	if !resolvedLeaseClaimAttestsResult(after, server, expectedRepoRoot, "") {
		return false
	}
	beforeState := strings.TrimSpace(before.Labels["state"])
	afterState := strings.TrimSpace(after.Labels["state"])
	return before.LeaseID == after.LeaseID &&
		(before.Provider == "" || canonicalClaimProvider(before.Provider) == canonicalClaimProvider(after.Provider)) &&
		(before.ProviderScope == "" || before.ProviderScope == after.ProviderScope) &&
		(before.CloudID == "" || before.CloudID == after.CloudID) &&
		(before.Slug == "" || before.Slug == after.Slug) &&
		(beforeState == "" || afterState != "")
}

func resolvedLeaseClaimStateAttests(claimState, serverState string) bool {
	if claimState == "" || claimState == serverState {
		return true
	}
	return (claimState == "ready" && serverState == "leased") ||
		(claimState == "leased" && serverState == "ready")
}

func idleForString(value string, now time.Time) string {
	if value == "" {
		return ""
	}
	touched, ok := parseLeaseLabelTime(value)
	if !ok || touched.After(now) {
		return ""
	}
	return now.Sub(touched).Round(time.Second).String()
}

func IdleForString(value string, now time.Time) string {
	return idleForString(value, now)
}

func redactedSSHUser(cfg Config, server Server, target SSHTarget) string {
	if target.AuthSecret {
		return "<token>"
	}
	if cfg.Provider == "daytona" || server.Provider == "daytona" {
		return "<token>"
	}
	return target.User
}

func formatSecondsDuration(seconds int) string {
	if seconds <= 0 {
		return ""
	}
	return (time.Duration(seconds) * time.Second).String()
}
