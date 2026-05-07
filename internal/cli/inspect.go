package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

func (a App) inspect(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("inspect", a.Stderr)
	provider := fs.String("provider", defaults.Provider, providerHelpSSH())
	id := fs.String("id", "", "lease id or slug")
	jsonOut := fs.Bool("json", false, "print JSON")
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	setIDFromFirstArg(fs, id)
	cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{})
	if err != nil {
		return err
	}
	if err := requireLeaseID(*id, "crabbox inspect --id <lease-id-or-slug>", cfg); err != nil {
		return err
	}
	state, err := a.leaseStatus(ctx, cfg, *id)
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(state)
	}
	fmt.Fprintf(a.Stdout, "id=%s\nslug=%s\nprovider=%s\ntarget=%s\nwindows_mode=%s\nstate=%s\nserver=%s\nhost=%s\nnetwork=%s\nssh=%s -p %s %s@%s\nssh_fallback_ports=%s\nidle_for=%s\nidle_timeout=%s\nlast_touched=%s\nexpires=%s\n", state.ID, blank(state.Slug, "-"), state.Provider, state.TargetOS, blank(state.WindowsMode, "-"), state.State, state.ServerID, state.Host, state.Network, state.SSHKey, state.SSHPort, state.SSHUser, state.SSHHost, blank(strings.Join(state.SSHFallbackPorts, ","), "-"), blank(state.IdleFor, "-"), blank(state.IdleTimeout, "-"), blank(state.LastTouchedAt, "-"), blank(state.ExpiresAt, "-"))
	if state.Tailscale != nil && state.Tailscale.Enabled {
		fmt.Fprintf(a.Stdout, "tailscale.state=%s\ntailscale.hostname=%s\ntailscale.fqdn=%s\ntailscale.ipv4=%s\ntailscale.tags=%s\n", blank(state.Tailscale.State, "-"), blank(state.Tailscale.Hostname, "-"), blank(state.Tailscale.FQDN, "-"), blank(state.Tailscale.IPv4, "-"), blank(strings.Join(state.Tailscale.Tags, ","), "-"))
		if state.Tailscale.ExitNode != "" {
			fmt.Fprintf(a.Stdout, "tailscale.exit_node=%s\ntailscale.exit_node_allow_lan_access=%t\n", state.Tailscale.ExitNode, state.Tailscale.ExitNodeAllowLANAccess)
		}
	}
	for key, value := range state.Labels {
		fmt.Fprintf(a.Stdout, "label.%s=%s\n", key, value)
	}
	return nil
}
