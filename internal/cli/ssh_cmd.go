package cli

import (
	"context"
	"fmt"
)

func (a App) ssh(ctx context.Context, args []string) error {
	fs := newFlagSet("ssh", a.Stderr)
	provider := fs.String("provider", defaultConfig().Provider, "provider: hetzner, aws, or ssh")
	id := fs.String("id", "", "lease id or slug")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
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
		return exit(2, "usage: crabbox ssh --id <lease-id-or-slug>")
	}
	server, target, leaseID, err := a.resolveLeaseTarget(ctx, cfg, *id)
	if err != nil {
		return err
	}
	if resolved, err := resolveNetworkTarget(ctx, cfg, server, target); err != nil {
		return err
	} else {
		target = resolved.Target
	}
	repo, err := findRepo()
	if err != nil {
		return err
	}
	if err := claimLeaseForRepoConfig(leaseID, serverSlug(server), cfg, repo.Root, cfg.IdleTimeout, *reclaim); err != nil {
		return err
	}
	a.touchActiveLeaseBestEffort(ctx, cfg, server, leaseID)
	fmt.Fprintf(a.Stdout, "ssh -i %s -p %s %s@%s\n", target.Key, target.Port, target.User, target.Host)
	return nil
}
