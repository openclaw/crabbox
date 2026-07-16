package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
)

func (a App) zed(ctx context.Context, args []string) error {
	resolved, err := a.resolveSSHCommandTarget(ctx, "zed", args, false)
	if err != nil {
		return err
	}
	if err := validateZedTarget(resolved.Config, resolved.Lease.SSH); err != nil {
		return err
	}
	repo, err := findRepo()
	if err != nil {
		return err
	}
	_, folder, hydratedByActions := codeWorkspace(ctx, resolved.Lease.SSH, resolved.Config, resolved.Lease.LeaseID, repo)
	if err := runSSHQuiet(ctx, resolved.Lease.SSH, "test -d "+shellQuote(folder)); err != nil {
		return exit(5, "remote folder %q is not ready; sync it first with: crabbox run --id %s --sync-only", folder, resolved.Lease.LeaseID)
	}
	if hydratedByActions {
		fmt.Fprintf(a.Stderr, "using GitHub Actions workspace for %s\n", resolved.Lease.LeaseID)
	}

	stopLeaseActivity := a.startInteractiveSSHLeaseActivity(ctx, resolved.Config, resolved.Lease)
	defer stopLeaseActivity()
	writeZedInstructions(a.Stdout, resolved.Lease.SSH, folder)

	<-ctx.Done()
	return nil
}

func validateZedTarget(cfg Config, target SSHTarget) error {
	if target.AuthSecret {
		return exit(2, "zed does not support token-as-username SSH credentials; use a key-based SSH provider")
	}
	if firstNonBlank(target.TargetOS, cfg.TargetOS) == targetWindows {
		return exit(2, "zed remote projects require a Linux or macOS target")
	}
	return nil
}

func zedSSHCommandLine(target SSHTarget) string {
	args := append(sshBaseArgs(target), target.User+"@"+target.Host)
	return "ssh " + strings.Join(shellWords(args), " ")
}

func writeZedInstructions(out io.Writer, target SSHTarget, folder string) {
	fmt.Fprintln(out, "Zed Remote Projects")
	fmt.Fprintln(out, "1. Open Remote Projects and select Connect New Server.")
	fmt.Fprintf(out, "2. Paste: %s\n", zedSSHCommandLine(target))
	fmt.Fprintf(out, "3. Open: %s\n", folder)
	fmt.Fprintln(out, "Keep this process running to maintain lease activity; press Ctrl-C when the Zed session is finished.")
}
