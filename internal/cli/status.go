package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

func (a App) status(ctx context.Context, args []string) error {
	fs := newFlagSet("status", a.Stderr)
	provider := fs.String("provider", defaultConfig().Provider, "provider: hetzner, aws, or blacksmith-testbox")
	id := fs.String("id", "", "lease id or slug")
	wait := fs.Bool("wait", false, "wait until ready")
	waitTimeout := fs.Duration("wait-timeout", 5*time.Minute, "maximum wait duration")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *id == "" && fs.NArg() > 0 {
		*id = fs.Arg(0)
	}
	if *id == "" {
		return exit(2, "usage: crabbox status --id <lease-id-or-slug>")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	cfg.Provider = *provider
	if isBlacksmithProvider(cfg.Provider) {
		return a.blacksmithStatus(ctx, cfg, *id, *wait, *waitTimeout, *jsonOut)
	}
	deadline := time.Now().Add(*waitTimeout)
	for {
		state, err := a.leaseStatus(ctx, cfg, *id)
		if err != nil {
			return err
		}
		if *wait {
			a.touchLeaseBestEffort(ctx, cfg, *id, state.ID)
		}
		if *jsonOut {
			if !*wait || state.Ready {
				return json.NewEncoder(a.Stdout).Encode(state)
			}
		} else {
			fmt.Fprintf(a.Stdout, "%s slug=%s provider=%s state=%s type=%s host=%s ready=%t has_host=%t idle_for=%s idle_timeout=%s expires=%s\n", state.ID, blank(state.Slug, "-"), state.Provider, state.State, state.ServerType, state.Host, state.Ready, state.HasHost, blank(state.IdleFor, "-"), blank(state.IdleTimeout, "-"), blank(state.ExpiresAt, "-"))
		}
		if !*wait || state.Ready {
			return nil
		}
		if time.Now().After(deadline) {
			return exit(5, "timed out waiting for %s to become ready", *id)
		}
		time.Sleep(5 * time.Second)
	}
}

type statusView struct {
	ID            string            `json:"id"`
	Slug          string            `json:"slug,omitempty"`
	Provider      string            `json:"provider"`
	State         string            `json:"state"`
	ServerID      string            `json:"serverId"`
	ServerType    string            `json:"serverType"`
	Host          string            `json:"host"`
	SSHUser       string            `json:"sshUser"`
	SSHPort       string            `json:"sshPort"`
	SSHKey        string            `json:"sshKey"`
	LastTouchedAt string            `json:"lastTouchedAt,omitempty"`
	IdleFor       string            `json:"idleFor,omitempty"`
	IdleTimeout   string            `json:"idleTimeout,omitempty"`
	ExpiresAt     string            `json:"expiresAt,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
	HasHost       bool              `json:"hasHost"`
	Ready         bool              `json:"ready"`
}

func (a App) leaseStatus(ctx context.Context, cfg Config, id string) (statusView, error) {
	if coord, ok, err := newCoordinatorClient(cfg); err != nil {
		return statusView{}, err
	} else if ok {
		lease, err := coord.GetLease(ctx, id)
		if err != nil {
			return statusView{}, err
		}
		_, target, _ := leaseToServerTarget(lease, cfg)
		hasHost := lease.Host != ""
		ready := lease.State == "active" && hasHost && probeSSHReady(ctx, &target, 4*time.Second)
		return statusView{
			ID:            lease.ID,
			Slug:          lease.Slug,
			Provider:      blank(lease.Provider, cfg.Provider),
			State:         lease.State,
			ServerID:      leaseDisplayID(lease),
			ServerType:    lease.ServerType,
			Host:          lease.Host,
			SSHUser:       target.User,
			SSHPort:       target.Port,
			SSHKey:        target.Key,
			LastTouchedAt: lease.LastTouchedAt,
			IdleFor:       idleForString(lease.LastTouchedAt, time.Now()),
			IdleTimeout:   formatSecondsDuration(lease.IdleTimeoutSeconds),
			ExpiresAt:     lease.ExpiresAt,
			Labels:        map[string]string{"keep": fmt.Sprint(lease.Keep)},
			HasHost:       hasHost,
			Ready:         ready,
		}, nil
	}
	server, target, leaseID, err := a.findLease(ctx, cfg, id)
	if err != nil {
		return statusView{}, err
	}
	hasHost := server.PublicNet.IPv4.IP != ""
	ready := hasHost && server.Labels["state"] != "provisioning" && probeSSHReady(ctx, &target, 4*time.Second)
	return statusView{
		ID:            leaseID,
		Slug:          serverSlug(server),
		Provider:      blank(server.Provider, cfg.Provider),
		State:         blank(server.Labels["state"], server.Status),
		ServerID:      server.DisplayID(),
		ServerType:    server.ServerType.Name,
		Host:          server.PublicNet.IPv4.IP,
		SSHUser:       target.User,
		SSHPort:       target.Port,
		SSHKey:        target.Key,
		LastTouchedAt: blank(leaseLabelTimeDisplay(server.Labels["last_touched_at"]), server.Labels["last_touched_at"]),
		IdleFor:       idleForString(server.Labels["last_touched_at"], time.Now()),
		IdleTimeout:   leaseLabelDurationDisplay(server.Labels["idle_timeout_secs"], server.Labels["idle_timeout"]),
		ExpiresAt:     blank(leaseLabelTimeDisplay(server.Labels["expires_at"]), server.Labels["expires_at"]),
		Labels:        server.Labels,
		HasHost:       hasHost,
		Ready:         ready,
	}, nil
}

func (a App) resolveLeaseTarget(ctx context.Context, cfg Config, id string) (Server, SSHTarget, string, error) {
	if coord, ok, err := newCoordinatorClient(cfg); err != nil {
		return Server{}, SSHTarget{}, "", err
	} else if ok {
		lease, err := coord.GetLease(ctx, id)
		if err != nil {
			return Server{}, SSHTarget{}, "", err
		}
		server, target, leaseID := leaseToServerTarget(lease, cfg)
		return server, target, leaseID, nil
	}
	return a.findLease(ctx, cfg, id)
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

func formatSecondsDuration(seconds int) string {
	if seconds <= 0 {
		return ""
	}
	return (time.Duration(seconds) * time.Second).String()
}

func formatSecondsDurationString(value string) string {
	duration, ok := parseDurationSecondsLabel(value)
	if !ok {
		return ""
	}
	return duration.String()
}
