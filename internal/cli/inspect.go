package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

func (a App) inspect(ctx context.Context, args []string) error {
	fs := newFlagSet("inspect", a.Stderr)
	provider := fs.String("provider", defaultConfig().Provider, "provider: hetzner, aws, or ssh")
	id := fs.String("id", "", "lease id or slug")
	jsonOut := fs.Bool("json", false, "print JSON")
	targetFlags := registerTargetFlags(fs, defaultConfig())
	networkFlags := registerNetworkModeFlag(fs, defaultConfig())
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *id == "" && fs.NArg() > 0 {
		*id = fs.Arg(0)
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	cfg.Provider = *provider
	if err := applyTargetFlagOverrides(&cfg, fs, targetFlags); err != nil {
		return err
	}
	if err := applyNetworkModeFlagOverride(&cfg, fs, networkFlags); err != nil {
		return err
	}
	if *id == "" && !isStaticProvider(cfg.Provider) {
		return exit(2, "usage: crabbox inspect --id <lease-id-or-slug>")
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
	}
	for key, value := range state.Labels {
		fmt.Fprintf(a.Stdout, "label.%s=%s\n", key, value)
	}
	return nil
}
