package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
				return exit(5, "timed out waiting for %s to become ready", *id)
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
	case "expired", "failed", "released", "stopped", "stopped_with_code", "terminated":
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

func (a App) resolveLeaseTarget(ctx context.Context, cfg Config, id string) (Server, SSHTarget, string, error) {
	backend, err := loadBackend(cfg, runtimeForApp(a))
	if err != nil {
		return Server{}, SSHTarget{}, "", err
	}
	sshBackend, ok := backend.(SSHLeaseBackend)
	if !ok {
		return Server{}, SSHTarget{}, "", exit(2, "provider=%s does not expose an SSH target", backend.Spec().Name)
	}
	lease, err := sshBackend.Resolve(ctx, ResolveRequest{Options: leaseOptionsFromConfig(cfg), ID: id})
	if err != nil {
		return Server{}, SSHTarget{}, "", err
	}
	return lease.Server, lease.SSH, lease.LeaseID, nil
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
