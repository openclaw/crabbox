package azuresandbox

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func (b *backend) Warmup(ctx context.Context, req core.WarmupRequest) error {
	return b.WarmupFixed(ctx, core.FixedWarmupRequest{WarmupRequest: req, RequestedLeaseID: core.NewLeaseID()})
}

func (b *backend) WarmupFixed(ctx context.Context, req core.FixedWarmupRequest) (resultErr error) {
	now := time.Now
	if b.rt.Clock != nil {
		now = b.rt.Clock.Now
	}
	started := now()
	timing := &sandboxProvisionTiming{now: now}
	ctx = context.WithValue(ctx, sandboxProvisionTimingKey{}, timing)
	completionStarted := false
	defer func() {
		if resultErr != nil && req.TimingJSON && !completionStarted {
			cause := resultErr
			if ctx.Err() != nil {
				// Credential errors intentionally omit their private SDK cause.
				cause = ctx.Err()
			}
			report := core.TimingReportWithRunResult(core.TimingReport{Provider: providerName, TotalMs: now().Sub(started).Milliseconds(), ExitCode: 1, RunnerPhases: timing.phases(), CreationEvents: timing.events()}, core.RunResult{}, cause)
			_ = core.WriteTimingJSON(b.rt.Stderr, report)
		}
	}()
	lease, err := b.acquire(ctx, req)
	if err != nil {
		return err
	}
	claim, err := b.claim(lease.LeaseID)
	if err != nil {
		return err
	}
	if req.OnAcquired != nil {
		if err := req.OnAcquired(core.FixedAcquisitionReceipt{LeaseID: lease.LeaseID, Slug: claim.Slug, Provider: providerName, ResourceID: lease.Server.CloudID}); err != nil {
			return err
		}
	}
	if req.BeforeComplete != nil {
		req.BeforeComplete()
	}
	completionStarted = true
	return shared.CompleteWarmup(b.rt, req.TimingJSON, shared.WarmupCompletion{Provider: providerName, LeaseID: lease.LeaseID, Slug: claim.Slug, Total: now().Sub(started), RunnerPhases: timing.phases(), CreationEvents: timing.events()})
}

func (b *backend) claim(id string) (core.LeaseClaim, error) {
	claim, exists, err := shared.ResolveProviderClaimStrict(id, providerName, (Provider{}).ClaimScope(b.cfg))
	if err != nil {
		return claim, err
	}
	if !exists || !leaseKind.IsFixedClaim(claim) || claim.ProviderScope != (Provider{}).ClaimScope(b.cfg) {
		return claim, fmt.Errorf("ACA Sandbox requires an exact locally owned lease in the configured group")
	}
	return claim, nil
}

// A heartbeat or configuration change cannot extend a legacy allocation.
// Teardown deliberately does not depend on this execution-admission check.
func executionDeadline(claim core.LeaseClaim) (time.Time, error) {
	if claim.FixedCreateIntent != nil && claim.FixedCreateIntent.Retention != nil {
		return core.SessionDiskExecutionDeadline(claim, time.Now())
	}
	i := claim.FixedCreateIntent
	if i == nil || i.State != "acquired" || claim.CloudID == "" {
		return time.Time{}, fmt.Errorf("ACA Sandbox has no completed allocation")
	}
	if i.Journal != nil && i.Journal.Phase == "deleting" {
		return time.Time{}, fmt.Errorf("ACA Sandbox cleanup is in progress; retry stop before using another lease")
	}
	deadline, err := time.Parse(time.RFC3339Nano, i.Attempt["expires_at"])
	if err != nil || !deadline.After(time.Now()) {
		return time.Time{}, fmt.Errorf("ACA Sandbox allocation expired; release the lease")
	}
	return deadline, nil
}

func (b *backend) List(ctx context.Context, _ core.ListRequest) ([]core.LeaseView, error) {
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return nil, err
	}
	var result []core.LeaseView
	for _, claim := range claims {
		if !leaseKind.IsFixedClaim(claim) || claim.ProviderScope != (Provider{}).ClaimScope(b.cfg) || claim.FixedCreateIntent.State == "released" {
			continue
		}
		s, err := b.inspect(ctx, claim)
		if err != nil {
			return nil, err
		}
		labels := maps.Clone(s.Labels)
		labels["lease"], labels["slug"], labels["target"] = claim.LeaseID, claim.Slug, core.TargetLinux
		labels["keep"] = "true" // Acquisition requires a kept lease.
		if claim.FixedCreateIntent.Retention != nil {
			labels["retention"] = "session-disk"
			labels["retained_disk"] = "true"
		}
		result = append(result, core.Server{Provider: providerName, CloudID: s.ID, ImmutableID: s.ID, Name: claim.Slug, Status: s.State, Labels: labels})
	}
	return result, nil
}

func (b *backend) inspect(ctx context.Context, claim core.LeaseClaim) (sandbox, error) {
	observed, err := core.InspectFixedResource(ctx, leaseKind, claim, core.FixedLeaseOperations[sandbox]{ObserveExact: b.observe})
	if err != nil {
		return sandbox{}, err
	}
	if len(observed.Candidates) != 1 {
		return sandbox{}, fmt.Errorf("ACA Sandbox resource is missing or uncertain; reconcile this lease")
	}
	return observed.Candidates[0], nil
}

// Callers hold the claim fence and bound ctx by the authorized work deadline.
// Recheck exact ownership on every observation, including after activation.
func (b *backend) ensureRunning(ctx context.Context, claim core.LeaseClaim) (sandbox, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	resumed := false
	for {
		s, err := b.inspect(ctx, claim)
		if err != nil {
			return sandbox{}, err
		}
		if claim.FixedCreateIntent.Retention != nil {
			if _, err := core.SessionDiskWindowDeadline(claim, b.now()); err != nil {
				return sandbox{}, err
			}
			if err := b.verifyRetentionPolicy(claim, s); err != nil {
				return sandbox{}, err
			}
		}
		if s.StateDetails.StoppedReason == "Disabled" {
			return sandbox{}, fmt.Errorf("ACA Sandbox is administratively disabled")
		}
		switch s.State {
		case "Running":
			return s, nil
		case "Stopped", "Suspended", "Idle":
			if claim.FixedCreateIntent.Retention != nil {
				return sandbox{}, fmt.Errorf("retained sandbox requires explicit pause/resume lifecycle admission")
			}
			if !resumed {
				if err := b.api.Resume(ctx, s.ID); err != nil {
					return sandbox{}, err
				}
				resumed = true
			}
		case "Creating", "Starting", "Resuming", "Stopping", "Suspending":
			// Wait for an in-progress transition without submitting it again.
		default:
			return sandbox{}, fmt.Errorf("ACA Sandbox cannot run in state %s", s.State)
		}
		finishWait := measureSandboxProvisionStep(ctx, "poll_wait."+sandboxPollState(s.State))
		select {
		case <-ctx.Done():
			finishWait()
			return sandbox{}, ctx.Err()
		case <-time.After(time.Second):
			finishWait()
		}
	}
}

func (b *backend) Status(ctx context.Context, req core.StatusRequest) (core.StatusView, error) {
	claim, err := b.claim(req.ID)
	if err != nil {
		return core.StatusView{}, err
	}
	view := core.StatusView{ID: claim.LeaseID, Slug: claim.Slug, Provider: providerName, TargetOS: core.TargetLinux, Network: core.NetworkPublic, ServerID: claim.CloudID}
	if claim.FixedCreateIntent.Retention != nil {
		return b.retainedStatus(ctx, claim, view)
	}
	expiresAt := claim.FixedCreateIntent.Attempt["expires_at"]
	deadline, expiryErr := time.Parse(time.RFC3339Nano, expiresAt)
	if expiryErr == nil {
		view.ExpiresAt = expiresAt
	}
	if claim.FixedCreateIntent.State == "released" {
		view.State = "deleted"
		return view, nil
	}
	s, err := b.inspect(ctx, claim)
	if err != nil {
		return view, err
	}
	view.State, view.ServerID, view.Labels = s.State, s.ID, s.Labels
	if expiryErr != nil {
		return view, fmt.Errorf("ACA Sandbox allocation deadline is unavailable; reconcile this lease")
	}
	// Expiry ends execution admission, not resource ownership or cleanup duty.
	// Check after observation so a deadline crossed during the read is terminal.
	if !deadline.After(time.Now()) {
		view.State = "expired"
	} else {
		view.Ready = s.State == "Running"
	}
	return view, nil
}

func (b *backend) Stop(ctx context.Context, req core.StopRequest) error {
	return b.stop(ctx, req, "")
}

func (b *backend) StopForRepository(ctx context.Context, req core.StopRequest, root string) error {
	return b.stop(ctx, req, root)
}

func (b *backend) stop(ctx context.Context, req core.StopRequest, root string) error {
	return b.stopExpected(ctx, req, root, core.ProviderIdentityExpectation{})
}

func (b *backend) StopFixed(ctx context.Context, req core.FixedStopRequest) error {
	return b.stopExpected(ctx, req.StopRequest, "", req.ExpectedProviderIdentity)
}

func (b *backend) stopExpected(ctx context.Context, req core.StopRequest, root string, expected core.ProviderIdentityExpectation) error {
	claim, err := b.claim(req.ID)
	if err != nil {
		return err
	}
	if err := core.ValidateLeaseTargetProviderIdentity(core.LeaseTarget{LeaseID: claim.LeaseID, Server: core.Server{CloudID: claim.CloudID, ImmutableID: claim.CloudImmutableID, Name: claim.Slug, Labels: map[string]string{"slug": claim.Slug}}}, expected); err != nil {
		return err
	}
	c, err := b.connect()
	if err != nil {
		return err
	}
	kind := leaseKind
	if claim.FixedCreateIntent.Retention != nil {
		kind.TerminalIdentityLabels = []string{"crabbox_lease", "crabbox_fingerprint", "crabbox_attempt"}
	}
	if claim.FixedCreateIntent.State == "acquired" && (claim.FixedCreateIntent.Journal == nil || claim.FixedCreateIntent.Journal.Phase != "deleting") {
		release, err := b.interruptActivity(ctx, claim, root)
		if err != nil {
			return err
		}
		defer release()
	}
	return core.DeleteFixedResource(ctx, kind, claim, core.FixedLeaseOperations[sandbox]{
		Release:      &core.FixedReleasePolicy{RepoRoot: root, SkipTerminalObservation: true},
		ObserveExact: b.observe,
		DeleteExact: func(ctx context.Context, _ *core.FixedTransaction, s sandbox) error {
			if err := c.Delete(ctx, s.ID); err != nil && !isMissing(err) {
				return err
			}
			for {
				_, err := c.Get(ctx, s.ID)
				if isMissing(err) {
					return nil
				}
				if err != nil {
					return err
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Second):
				}
			}
		},
	})
}

func (b *backend) Run(ctx context.Context, req core.RunRequest) (core.RunResult, error) {
	if req.ID == "" || !req.Keep || !req.NoSync {
		return core.RunResult{}, fmt.Errorf("ACA Sandbox execution requires a kept lease and --no-sync; workspace preparation is caller-owned")
	}
	claim, err := b.claim(req.ID)
	if err != nil {
		return core.RunResult{}, err
	}
	req.ID = claim.LeaseID
	if err := core.CheckLeaseClaimRepositoryOwner(claim.LeaseID, claim, req.Repo.Root, false); err != nil {
		return core.RunResult{}, err
	}
	deadline, err := b.executionDeadline(claim)
	if err != nil {
		return core.RunResult{}, err
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	c, err := b.connect()
	if err != nil {
		return core.RunResult{}, err
	}
	return shared.RunDelegatedSandbox(ctx, req, shared.DelegatedSandboxLifecycle{
		Provider: providerName, Runtime: b.rt, Workdir: "/workspace", TTL: b.cfg.TTL, IdleTimeout: b.cfg.IdleTimeout,
		Resolve: func(context.Context) (shared.DelegatedSandbox, error) {
			return shared.DelegatedSandbox{LeaseID: claim.LeaseID, Slug: claim.Slug,
				CleanupCommand: "crabbox stop --provider azure-sandbox --id " + core.ShellQuote(claim.LeaseID)}, nil
		},
		Workspace: func() shared.SandboxWorkspace {
			return shared.WorkspaceOperations{EnsureFunc: func(context.Context) error { return nil }}
		},
		Command: func(context.Context) (shared.DelegatedSandboxCommand, error) {
			commandArgs, shellMode := req.Command, req.ShellMode
			literalArgs := req.CommandLiteralArgs
			if req.Script != nil {
				commandArgs = append([]string{"bash", "-c", string(req.Script.Data), "crabbox-script"}, req.Command...)
				shellMode = false
				literalArgs = nil
			}
			intent, err := core.ParseCommandIntent(commandArgs, shellMode, literalArgs)
			if err != nil {
				return shared.DelegatedSandboxCommand{}, err
			}
			command := shared.ShellWorkspaceCommand("/workspace", req.Env, intent, "bash", "-c")
			return shared.DelegatedSandboxCommand{Text: intent.ShellScript(), Run: func(ctx context.Context, stdout, stderr io.Writer) (int, error) {
				var out execResult
				err := withActivity(ctx, claim, func(ctx context.Context) error {
					if _, err := b.ensureRunning(ctx, claim); err != nil {
						return err
					}
					var err error
					out, err = c.Exec(ctx, claim.CloudID, command, "")
					return err
				})
				if err != nil {
					return 1, err
				}
				if _, err := io.WriteString(stdout, out.Stdout); err != nil {
					return 1, err
				}
				if _, err := io.WriteString(stderr, out.Stderr); err != nil {
					return 1, err
				}
				return *out.ExitCode, nil
			}}, nil
		},
	})
}

func (b *backend) Heartbeat(ctx context.Context, req core.LeaseHeartbeatRequest) (core.LeaseHeartbeatResult, error) {
	claim, err := b.claim(req.ID)
	if err != nil {
		return core.LeaseHeartbeatResult{}, err
	}
	deadline, err := b.executionDeadline(claim)
	if err != nil {
		return core.LeaseHeartbeatResult{}, err
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	err = withActivity(ctx, claim, func(ctx context.Context) error {
		s, err := b.ensureRunning(ctx, claim)
		if err != nil {
			return err
		}
		out, err := b.api.Exec(ctx, s.ID, "true", "")
		if err != nil {
			return err
		}
		if *out.ExitCode != 0 {
			return fmt.Errorf("ACA Sandbox heartbeat failed")
		}
		return nil
	})
	return core.LeaseHeartbeatResult{LeaseID: claim.LeaseID, Slug: claim.Slug, State: "Running", LastTouchedAt: time.Now()}, err
}

// Repository preparation transfers regular files through the existing cp seam.
func (b *backend) Copy(ctx context.Context, req core.CopyRequest) error {
	prefix, destination, ok := strings.Cut(req.Destination, ":")
	if !ok || !strings.EqualFold(prefix, "SANDBOX") || !strings.HasPrefix(destination, "/") {
		return fmt.Errorf("ACA Sandbox cp currently supports local regular-file uploads to SANDBOX:/absolute/path")
	}
	claim, err := b.claim(req.ID)
	if err != nil {
		return err
	}
	if req.RepoRoot == "" {
		return fmt.Errorf("ACA Sandbox upload requires its repository owner")
	}
	if err := core.CheckLeaseClaimRepositoryOwner(claim.LeaseID, claim, req.RepoRoot, false); err != nil {
		return err
	}
	deadline, err := b.executionDeadline(claim)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	info, err := os.Lstat(req.Source)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 && !req.FollowLink {
		return fmt.Errorf("ACA Sandbox upload of a symlink requires -L")
	}
	if req.FollowLink && info.Mode()&os.ModeSymlink != 0 {
		info, err = os.Stat(req.Source)
		if err != nil {
			return err
		}
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("ACA Sandbox upload requires a regular file")
	}
	input, err := os.Open(req.Source)
	if err != nil {
		return err
	}
	defer input.Close()
	info, err = input.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("ACA Sandbox upload requires a regular file")
	}
	return withActivity(ctx, claim, func(ctx context.Context) error {
		s, err := b.ensureRunning(ctx, claim)
		if err != nil {
			return err
		}
		return b.api.Upload(ctx, s.ID, destination, input)
	})
}
