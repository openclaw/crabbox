package tensorlake

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func NewTensorlakeBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = providerName
	return &tensorlakeBackend{spec: spec, cfg: cfg, rt: rt}
}

type tensorlakeBackend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

func (b *tensorlakeBackend) Spec() ProviderSpec { return b.spec }

func (b *tensorlakeBackend) Warmup(ctx context.Context, req WarmupRequest) error {
	started := core.ClockNow(b.rt.Clock)
	cli, err := newTensorlakeCLI(b.cfg, b.rt)
	if err != nil {
		return err
	}
	claim, name, err := b.createSandbox(ctx, cli, req.Repo, req.Reclaim, req.RequestedSlug)
	if err != nil {
		return err
	}
	leaseID, sandboxID, slug := claim.LeaseID, claim.CloudID, claim.Slug
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s sandbox=%s name=%s\n", leaseID, slug, providerName, sandboxID, name)
	if !req.Keep {
		fmt.Fprintf(b.rt.Stderr, "warning: tensorlake warmup keeps the sandbox until explicit stop\n")
	}
	total := core.ClockNow(b.rt.Clock).Sub(started)
	return shared.CompleteWarmup(b.rt, req.TimingJSON, shared.WarmupCompletion{
		Provider: providerName,
		LeaseID:  leaseID,
		Slug:     slug,
		Total:    total,
	})
}

func (b *tensorlakeBackend) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	workdir, workdirErr := tensorlakeWorkdir(b.cfg)
	var cli *tensorlakeCLI
	var claim core.LeaseClaim
	session := func() shared.DelegatedSandbox {
		return shared.DelegatedSandbox{LeaseID: claim.LeaseID, Slug: claim.Slug, CleanupCommand: tensorlakeCleanupCommand(claim.LeaseID)}
	}
	admit := func(ctx context.Context) error {
		_, binding, err := bindingForClaim(claim)
		if err != nil {
			return err
		}
		return core.WithLeaseClaimUnchangedContext(ctx, claim.LeaseID, claim, func() error {
			item, err := cli.verifyBinding(ctx, binding)
			if err == nil && item.State == "terminated" {
				return exit(2, "Tensorlake sandbox has terminated; create a new lease")
			}
			return err
		})
	}
	return shared.RunDelegatedSandbox(ctx, req, shared.DelegatedSandboxLifecycle{
		Provider: providerName, Runtime: b.rt, Workdir: workdir,
		IdleTimeout: b.cfg.IdleTimeout, TTL: b.cfg.TTL, CleanupTimeout: envProfileCleanupTimeout,
		Preflight: func(context.Context) error {
			if err := rejectIncompatibleSyncOptions(req); err != nil {
				return err
			}
			if workdirErr != nil {
				return workdirErr
			}
			var err error
			cli, err = newTensorlakeCLI(b.cfg, b.rt)
			return err
		},
		PrepareArchive: func(ctx context.Context) (*core.PreparedArchive, error) { return b.prepareArchive(ctx, req) },
		Acquire: func(ctx context.Context) (shared.DelegatedSandbox, error) {
			var name string
			var err error
			claim, name, err = b.createSandbox(ctx, cli, req.Repo, req.Reclaim, req.RequestedSlug)
			if err != nil {
				return shared.DelegatedSandbox{}, err
			}
			fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=%s sandbox=%s name=%s\n", claim.LeaseID, claim.Slug, providerName, claim.CloudID, name)
			return session(), nil
		},
		Resolve: func(ctx context.Context) (shared.DelegatedSandbox, error) {
			var err error
			claim, err = b.resolveLease(ctx, cli, req.ID, req.Repo.Root, req.Reclaim)
			if err != nil {
				return shared.DelegatedSandbox{}, err
			}
			return session(), nil
		},
		AdmitReuse: admit,
		Setup: func(ctx context.Context) error {
			if req.ID == "" {
				if err := admit(ctx); err != nil {
					return err
				}
			}
			fmt.Fprintf(b.rt.Stderr, "provider=%s lease=%s sandbox=%s workdir=%s\n", providerName, claim.LeaseID, claim.CloudID, workdir)
			return nil
		},
		Sync: func(ctx context.Context, archive *core.PreparedArchive) ([]core.TimingPhase, time.Duration, error) {
			return b.syncWorkspace(ctx, cli, claim.CloudID, req, workdir, archive)
		},
		NoSync: func(ctx context.Context) error { return b.prepareWorkspace(ctx, cli, claim.CloudID, workdir) },
		Command: func(ctx context.Context) (shared.DelegatedSandboxCommand, error) {
			intent, err := core.ParseCommandIntent(req.Command, req.ShellMode, req.CommandLiteralArgs)
			if err != nil {
				return shared.DelegatedSandboxCommand{}, err
			}
			args := intent.Argv("bash", "-lc")
			if req.EnvSummary || strings.TrimSpace(os.Getenv("CRABBOX_ENV_ALLOW")) != "" {
				printEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, req.Env)
			}
			command := shared.DelegatedSandboxCommand{OutputScope: core.RunOutputProvider}
			if len(req.Env) > 0 {
				envPath, cleanup, err := b.uploadEnvProfile(ctx, cli, claim, req.Env)
				if cleanup != nil {
					command.Close = func(ctx context.Context) error {
						cleanup(ctx)
						return nil
					}
				}
				if err != nil {
					return command, err
				}
				args = shared.WrapCommandWithShellEnvProfile(args, envPath)
			}
			command.Run = func(ctx context.Context, stdout, stderr io.Writer) (int, error) {
				code, err := cli.execStream(ctx, claim.CloudID, workdir, args, stdout, stderr)
				if err != nil {
					return code, shared.ExitErrorWithCause(1, shared.RedactErrorSecrets(err.Error(), b.cfg.Tensorlake.APIKey), err)
				}
				return code, nil
			}
			return command, nil
		},
		Cleanup: func(ctx context.Context) error { return cli.removeBoundClaim(ctx, claim) },
	})
}

func (b *tensorlakeBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	_ = req
	cli, err := newTensorlakeCLI(b.cfg, b.rt)
	if err != nil {
		return nil, err
	}
	ids, err := cli.listIDs(ctx)
	if err != nil {
		return nil, err
	}
	servers := make([]Server, 0, len(ids))
	for _, id := range ids {
		leaseID := leasePrefix + id
		claim, ok, err := core.ReadLeaseClaimWithPresence(leaseID)
		if err != nil {
			return nil, err
		}
		if !ok || claim.Provider != providerName {
			continue
		}
		_, binding, err := bindingForClaim(claim)
		if err != nil {
			return nil, err
		}
		var item sandboxIdentity
		if err := core.WithLeaseClaimUnchanged(leaseID, claim, func() error {
			var err error
			item, err = cli.verifyBinding(ctx, binding)
			return err
		}); err != nil {
			return nil, err
		}

		servers = append(servers, Server{
			Provider: providerName,
			CloudID:  id,
			Name:     id,
			Status:   item.State,
			Labels: map[string]string{
				"provider": providerName,
				"lease":    claim.LeaseID,
				"slug":     claim.Slug,
				"target":   targetLinux,
				"state":    item.State,
			},
		})
	}
	return servers, nil
}

func (b *tensorlakeBackend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	servers, err := b.List(ctx, ListRequest{})
	if err != nil {
		return DoctorResult{}, err
	}
	return cliDoctorResult(providerName, len(servers), "unchecked"), nil
}

func (b *tensorlakeBackend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	cli, err := newTensorlakeCLI(b.cfg, b.rt)
	if err != nil {
		return StatusView{}, err
	}
	claim, err := b.resolveLease(ctx, cli, req.ID, "", false)
	if err != nil {
		return StatusView{}, err
	}
	leaseID, sandboxID, slug := claim.LeaseID, claim.CloudID, claim.Slug
	_, binding, err := bindingForClaim(claim)
	if err != nil {
		return StatusView{}, err
	}
	deadline := core.ClockNow(b.rt.Clock).Add(req.WaitTimeout)
	if req.WaitTimeout <= 0 {
		deadline = core.ClockNow(b.rt.Clock).Add(5 * time.Minute)
	}
	var lastDescribeErr error
	for {
		var item sandboxIdentity
		describeErr := core.WithLeaseClaimUnchanged(leaseID, claim, func() error {
			var err error
			item, err = cli.verifyBinding(ctx, binding)
			return err
		})
		state := item.State
		if describeErr != nil {
			if !req.Wait {
				return StatusView{}, describeErr
			}
			lastDescribeErr = describeErr
		} else {
			lastDescribeErr = nil
		}
		ready := describeErr == nil && isReadyState(state)
		view := StatusView{
			ID:       leaseID,
			Slug:     slug,
			Provider: providerName,
			TargetOS: targetLinux,
			State:    state,
			ServerID: sandboxID,
			Network:  NetworkPublic,
			Ready:    ready,
			Labels: map[string]string{
				"provider": providerName,
				"lease":    leaseID,
				"state":    state,
			},
		}
		if !req.Wait || view.Ready {
			return view, nil
		}
		if core.ClockNow(b.rt.Clock).After(deadline) {
			err := exit(5, "timed out waiting for tensorlake sandbox %s to become ready", sandboxID)
			if lastDescribeErr != nil {
				return StatusView{}, errors.Join(err, fmt.Errorf("last tensorlake describe failed: %w", lastDescribeErr))
			}
			return StatusView{}, err
		}
		select {
		case <-ctx.Done():
			err := ctx.Err()
			if lastDescribeErr != nil {
				return StatusView{}, errors.Join(err, fmt.Errorf("last tensorlake describe failed: %w", lastDescribeErr))
			}
			return StatusView{}, err
		case <-time.After(2 * time.Second):
		}
	}
}

func (b *tensorlakeBackend) Stop(ctx context.Context, req StopRequest) error {
	cli, err := newTensorlakeCLI(b.cfg, b.rt)
	if err != nil {
		return err
	}
	claim, err := b.resolveLease(ctx, cli, req.ID, "", false)
	if err != nil {
		return err
	}
	if err := cli.removeBoundClaim(ctx, claim); err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stderr, "released lease=%s sandbox=%s\n", claim.LeaseID, claim.CloudID)
	return nil
}

func (b *tensorlakeBackend) createSandbox(ctx context.Context, cli *tensorlakeCLI, repo Repo, reclaim bool, requestedSlug string) (core.LeaseClaim, string, error) {
	if strings.TrimSpace(repo.Root) == "" {
		return core.LeaseClaim{}, "", exit(2, "Tensorlake acquisition requires a repository root for durable ownership")
	}
	scope, err := cli.observeScope(ctx)
	if err != nil {
		return core.LeaseClaim{}, "", err
	}
	name := newSandboxName(repo)
	id, err := cli.createSandbox(ctx, name)
	if err != nil {
		return core.LeaseClaim{}, "", fmt.Errorf("%w; inspect Tensorlake sandbox name=%s before manual cleanup of an uncertain creation", err, name)
	}
	leaseID := leasePrefix + id
	retained := func(err error) (core.LeaseClaim, string, error) {
		return core.LeaseClaim{}, "", fmt.Errorf("%w; retained Tensorlake sandbox=%s lease=%s for manual inspection", err, id, leaseID)
	}
	item, err := cli.inspectIdentity(ctx, id)
	if err != nil {
		return retained(err)
	}
	if item.Name != name || item.State == "terminated" {
		return retained(exit(2, "Tensorlake creation returned an unexpected sandbox identity or state"))
	}
	binding := sandboxBinding{id, item.Namespace, scope}
	rollback := func(cause error) (core.LeaseClaim, string, error) {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), terminationTimeout)
		defer cancel()
		cleanupErr := core.CleanupLeaseClaimIfUnchangedAfterContext(cleanupCtx, leaseID, core.LeaseClaim{}, false, func() error {
			return cli.terminateBound(cleanupCtx, binding)
		})
		if cleanupErr != nil {
			return retained(errors.Join(cause, cleanupErr))
		}
		return core.LeaseClaim{}, "", cause
	}
	slug, err := allocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return rollback(err)
	}
	server := Server{Provider: providerName, CloudID: id, Name: name, Status: item.State, Labels: map[string]string{"provider": providerName, "lease": leaseID, "slug": slug, "target": targetLinux, "tensorlake_namespace": item.Namespace}}
	claim, err := core.ClaimLeaseTargetForRepoConfigScopeIfUnchangedDurableAfterContext(ctx, leaseID, slug, b.cfg, scope, server, core.SSHTarget{}, repo.Root, b.cfg.IdleTimeout, reclaim, core.LeaseClaim{}, false, func() error {
		item, err := cli.verifyBinding(ctx, binding)
		if err == nil && item.State == "terminated" {
			err = exit(2, "Tensorlake sandbox terminated before ownership publication")
		}
		return err
	})
	if err != nil {
		return rollback(err)
	}
	return claim, name, nil
}

func (b *tensorlakeBackend) resolveLease(ctx context.Context, cli *tensorlakeCLI, id, repoRoot string, reclaim bool) (core.LeaseClaim, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return core.LeaseClaim{}, exit(2, "provider=tensorlake requires a Crabbox-created sandbox slug or lease id")
	}
	strict := strings.HasPrefix(id, leasePrefix) || isLikelySandboxID(id)
	if isLikelySandboxID(id) {
		id = leasePrefix + id
	}
	claim, ok, exact, err := core.ResolveLeaseClaimForProviderWithExact(id, providerName)
	if err != nil {
		return core.LeaseClaim{}, err
	}
	if !ok || (strict || exact) && (!exact || claim.LeaseID != id) {
		return core.LeaseClaim{}, exit(4, "tensorlake sandbox %q is not exactly claimed by Crabbox", id)
	}
	_, binding, err := bindingForClaim(claim)
	if err != nil {
		return core.LeaseClaim{}, err
	}
	if repoRoot != "" {
		server := Server{Provider: providerName, CloudID: claim.CloudID, Labels: shared.CloneLabels(claim.Labels)}
		return core.ClaimLeaseTargetForRepoConfigScopeIfUnchangedDurableAfterContext(ctx, claim.LeaseID, claim.Slug, b.cfg, claim.ProviderScope, server, core.SSHTarget{}, repoRoot, timeoutOrDefault(b.cfg.IdleTimeout, time.Duration(claim.IdleTimeoutSeconds)*time.Second), reclaim, claim, true, func() error {
			item, err := cli.verifyBinding(ctx, binding)
			if err == nil && item.State == "terminated" {
				err = exit(2, "Tensorlake sandbox has terminated; create a new lease")
			}
			return err
		})
	}
	return claim, nil
}

func timeoutOrDefault(primary, fallback time.Duration) time.Duration {
	if primary > 0 {
		return primary
	}
	return fallback
}

func newSandboxName(repo Repo) string {
	base := normalizeLeaseSlug(repo.Name)
	if base == "" {
		base = "crabbox"
	}
	base = strings.TrimPrefix(base, strings.TrimSuffix(namePrefix, "-")+"-")
	maxBase := maxSandboxNameLen - len(namePrefix) - 1 - sandboxNameSuffixLen
	if maxBase < 1 {
		maxBase = 1
	}
	if len(base) > maxBase {
		base = strings.Trim(base[:maxBase], "-")
	}
	if base == "" {
		base = "crabbox"
	}
	return namePrefix + base + "-" + randomSuffix()
}

func isReadyState(state string) bool {
	switch strings.TrimSpace(strings.ToLower(state)) {
	case "running", "ready", "started", "active":
		return true
	default:
		return false
	}
}

func randomSuffix() string {
	return shared.RandomSuffix()
}

// tensorlakeWorkdir returns the configured absolute workspace path inside the
// sandbox, validating that it isn't relative or empty.
func tensorlakeWorkdir(cfg Config) (string, error) {
	workdir := strings.TrimSpace(cfg.Tensorlake.Workdir)
	if workdir == "" {
		workdir = core.TensorlakeConfigDefaultWorkdir
	}
	clean := path.Clean(workdir)
	if !strings.HasPrefix(clean, "/") {
		return "", exit(2, "tensorlake workdir %q must be an absolute path", workdir)
	}
	switch clean {
	case "/", "/bin", "/dev", "/etc", "/home", "/lib", "/lib64", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var", "/workspace":
		return "", exit(2, "tensorlake workdir %q is too broad; choose a dedicated subdirectory", clean)
	}
	return clean, nil
}
