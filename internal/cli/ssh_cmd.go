package cli

import (
	"context"
	"flag"
	"fmt"
	"strings"
)

func (a App) ssh(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("ssh", a.Stderr)
	provider := fs.String("provider", defaults.Provider, providerHelpSSH())
	id := fs.String("id", "", "lease id or slug")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	showSecret := fs.Bool("show-secret", false, "print secret auth material for token-based SSH providers")
	providerFlags := registerProviderFlags(fs, defaults)
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	setIDFromFirstArg(fs, id)
	cfg, err := loadSSHCommandConfig(fs, *provider, providerFlags, targetFlags, networkFlags, leaseTargetConfigOptions{LeaseID: *id})
	if err != nil {
		return err
	}
	if err := applyProviderFlags(&cfg, fs, providerFlags); err != nil {
		return err
	}
	if err := requireLeaseID(*id, "crabbox ssh --id <lease-id-or-slug>", cfg); err != nil {
		return err
	}
	server, target, leaseID, err := a.resolveNetworkLeaseTargetForRepo(ctx, cfg, *id, false, *reclaim)
	if err != nil {
		return err
	}
	if err := a.claimAndTouchLeaseTarget(ctx, cfg, server, target, leaseID, *reclaim); err != nil {
		return err
	}
	if target.AuthSecret && !*showSecret {
		fmt.Fprintf(a.Stderr, "warning: ssh auth user is secret; rerun with --show-secret to print a pasteable command\n")
	}
	fmt.Fprintln(a.Stdout, sshCommandLine(target, target.AuthSecret && !*showSecret))
	return nil
}

func loadSSHCommandConfig(fs *flag.FlagSet, provider string, providerFlags providerFlagValues, targetFlags targetFlagValues, networkFlags networkModeFlagValues, opts leaseTargetConfigOptions) (Config, error) {
	cfg, err := loadLeaseTargetConfig(fs, provider, targetFlags, networkFlags, opts)
	if err != nil {
		return Config{}, err
	}
	if err := applyProviderFlags(&cfg, fs, providerFlags); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func sshCommandLine(target SSHTarget, redactSecret bool) string {
	renderTarget := target
	if redactSecret {
		renderTarget.User = "<token>"
	}
	args := append([]string{"ssh"}, sshBaseArgs(renderTarget)...)
	args = append(args, renderTarget.User+"@"+renderTarget.Host)
	return strings.Join(shellWords(args), " ")
}
