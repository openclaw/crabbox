package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/term"
)

// execCommand leaves workspace policy to its caller while Crabbox retains
// current-claim authority and the private transport for the entire operation.
func (a App) execCommand(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("exec", a.Stderr)
	provider := registerProviderSelectionFlag(fs, defaults, providerHelpSSH())
	id := fs.String("id", "", "canonical lease id")
	pty := fs.Bool("pty", false, "allocate a remote pseudo-terminal")
	check := fs.Bool("check", false, "print configured execution and fixed-ID repository cleanup capabilities offline as JSON")
	providerFlags := registerProviderFlags(fs, defaults)
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	command := fs.Args()
	if *check && (*id != "" || *pty || len(command) != 0) {
		return exit(2, "exec --check cannot combine a lease ID, --pty, or a command")
	}
	if !*check && (!isCanonicalLeaseID(*id) || len(command) == 0) {
		return exit(2, "usage: crabbox exec --id <canonical-lease-id> [--pty] -- <command> [args...]")
	}
	for _, arg := range command {
		if strings.ContainsRune(arg, '\x00') {
			return exit(2, "command arguments cannot contain NUL bytes")
		}
	}
	if input, ok := a.input().(*os.File); !*check && ok && term.IsTerminal(int(input.Fd())) {
		return exit(2, "exec requires piped or redirected stdin; redirect from /dev/null when no input is needed")
	}
	cfg, err := loadSSHCommandConfig(fs, *provider, providerFlags, targetFlags, networkFlags, leaseTargetConfigOptions{LeaseID: *id})
	if err != nil {
		return err
	}
	capabilities, err := execCapabilitiesForConfig(cfg)
	if err != nil {
		return err
	}
	if *check {
		return json.NewEncoder(a.Stdout).Encode(capabilities)
	}
	if !capabilities.Execution {
		return exit(2, "provider=%s target=%s does not support claim-fenced SSH execution", capabilities.Provider, capabilities.Target)
	}
	boundary, err := findRepositoryBoundary()
	if err != nil {
		return err
	}
	providerRuntime := runtimeForApp(a)
	providerRuntime.Stdout = a.Stderr
	backend, err := loadBackend(cfg, providerRuntime)
	if err != nil {
		return err
	}
	resolver, ok := backend.(RunLeaseClaimResolver)
	if !ok || !backend.Spec().Features.Has(FeatureSSH) {
		return exit(2, "provider=%s does not support claim-fenced SSH execution", backend.Spec().Name)
	}
	claim, exists, err := readLeaseClaimWithPresence(*id)
	if err != nil {
		return err
	}
	if !exists || claim.CloudID == "" || claim.Provider == "" || claim.RepoRoot == "" {
		return exit(2, "exec requires an existing, resource-bound repository claim for %s", *id)
	}
	ctx, cancel := pondMeshTerminationContext(ctx)
	defer cancel()
	options := leaseOptionsFromConfig(cfg)
	return withLeaseClaimUnchangedContext(ctx, *id, claim, true, func() error {
		if canonicalClaimProvider(claim.Provider) != canonicalClaimProvider(backend.Spec().Name) ||
			(options.ProviderScope != "" && claim.ProviderScope != options.ProviderScope) {
			return exit(2, "lease %s does not match the requested provider scope", *id)
		}
		if err := checkLeaseClaimRepositoryOwner(*id, claim, boundary.root, false); err != nil {
			return err
		}
		if err := AuthorizeCheckpointRelease(claim, ""); err != nil {
			return err
		}
		// This resolver contract cannot publish or reenter claim operations. The
		// shared fence lets independent commands run but excludes claim writers.
		lease, err := resolver.ResolveRunLeaseUnderClaim(ctx, ResolveRequest{
			ID: *id, Repo: Repo{Root: boundary.root}, Options: options, Prepare: true,
		}, claim)
		if err != nil {
			return err
		}
		if lease.LeaseID != claim.LeaseID || lease.Server.CloudID != claim.CloudID ||
			canonicalClaimProvider(lease.Server.Provider) != canonicalClaimProvider(claim.Provider) ||
			!resolvedLeaseClaimIdentityCompatible(claim, lease.Server) {
			return exit(2, "lease %s resolved outside its original claim", *id)
		}
		applyResolvedLeaseConfig(&cfg, lease.Server, &lease.SSH)
		if lease.SSH.TargetOS != targetLinux && lease.SSH.TargetOS != targetMacOS {
			return exit(2, "exec currently requires a Linux or macOS SSH target")
		}
		resolved, err := resolveSSHTargetNetwork(ctx, cfg, lease.Server, lease.SSH, false)
		if err != nil {
			return err
		}
		lease.SSH = resolved.Target
		if activity, ok := backend.(SSHRunActivityBackend); ok {
			stop, err := activity.BeginSSHRunActivity(ctx, lease)
			if err != nil {
				return err
			}
			defer stop()
		}
		return a.execSSHCommand(ctx, lease.SSH, command, *pty)
	})
}

type execCapabilities struct {
	Provider        string `json:"provider"`
	Target          string `json:"target"`
	Execution       bool   `json:"execution"`
	CurrentRepoStop bool   `json:"currentRepoStop"`
}

func execCapabilitiesForConfig(cfg Config) (execCapabilities, error) {
	if !providerSelectionIsActionable(cfg) {
		return execCapabilities{}, exit(2, "%s", providerSelectionRequiredDiagnostic)
	}
	provider, err := ProviderFor(cfg.Provider)
	if err != nil {
		return execCapabilities{}, err
	}
	applySingleProviderTargetDefault(&cfg)
	spec := provider.Spec()
	direct := !ShouldUseCoordinator(cfg, spec)
	posix := (cfg.TargetOS == targetLinux || cfg.TargetOS == targetMacOS) && providerSpecSupportsTarget(spec, cfg.TargetOS, cfg.WindowsMode)
	return execCapabilities{
		Provider: provider.Name(), Target: cfg.TargetOS,
		Execution:       direct && posix && spec.Features.Has(FeatureSSH) && spec.Features.Has(FeatureClaimExec),
		CurrentRepoStop: direct && spec.Features.Has(FeatureFixedCurrentRepoStop),
	}, nil
}

func (a App) execSSHCommand(ctx context.Context, target SSHTarget, command []string, pty bool) (err error) {
	target.NoControlMaster = true
	session, err := newSSHTransportSession(ctx, target, false)
	if err != nil {
		return err
	}
	defer func() {
		if cleanupErr := session.Close(); cleanupErr != nil {
			// A typed command exit must not hide a failed lifecycle cleanup in main.
			err = fmt.Errorf("private SSH transport cleanup failed: %v (command outcome: %v)", cleanupErr, err)
		}
	}()
	args := session.commandPrefixWithOptions("10", "3")
	if pty {
		args = append(args, "-tt")
	} else {
		args = append(args, "-T")
	}
	if target.AuthSecret {
		args = append(args, "-o", "LogLevel=QUIET")
	}
	args = append(args, session.host(), "exec "+strings.Join(shellWords(command), " "))
	handle := newOwnedSSHTransportCommand(ctx, target, args)
	handle.cmd.Stdin = a.input()
	handle.cmd.Stdout = a.Stdout
	handle.cmd.Stderr = a.Stderr
	if err := handle.Start(); err != nil {
		return fmt.Errorf("start SSH command: %w", err)
	}
	err = waitOwnedSSHTransportCommand(ctx, handle)
	if cleanupErr := handle.joinCancellationError(handle.finishPondMeshPlatform()); cleanupErr != nil {
		return fmt.Errorf("SSH process cleanup failed: %v (command outcome: %v)", cleanupErr, err)
	}
	var commandExit *exec.ExitError
	if errors.As(err, &commandExit) && commandExit.ExitCode() >= 0 {
		return ExitError{Code: commandExit.ExitCode()}
	}
	return err
}
