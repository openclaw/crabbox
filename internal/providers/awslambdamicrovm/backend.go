package awslambdamicrovm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const (
	lifecycleWaitTimeout     = 5 * time.Minute
	runnerHealthProbeTimeout = 10 * time.Second
)

type runnerAPI interface {
	Health(context.Context, microVM) error
	Upload(context.Context, microVM, string, io.Reader) error
	Exec(context.Context, microVM, string, string, map[string]string, io.Writer, io.Writer) (int, error)
}

type backend struct {
	spec       core.ProviderSpec
	cfg        core.Config
	rt         core.Runtime
	newControl func(context.Context, core.Config) (controlPlane, error)
	newRunner  func(controlPlane, core.Config, core.Runtime) (runnerAPI, error)
}

func newBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) core.Backend {
	return &backend{
		spec:       spec,
		cfg:        cfg,
		rt:         rt,
		newControl: newControlPlane,
		newRunner: func(control controlPlane, cfg core.Config, rt core.Runtime) (runnerAPI, error) {
			return newRunnerClient(control, rt.HTTP, cfg.AWSRegion)
		},
	}
}

func (b *backend) Spec() core.ProviderSpec { return b.spec }

func (b *backend) Warmup(ctx context.Context, req core.WarmupRequest) error {
	if req.ActionsRunner {
		return core.Exit(2, "--actions-runner is not supported for provider=%s", providerName)
	}
	started := core.ClockNow(b.rt.Clock)
	control, runner, err := b.clients(ctx)
	if err != nil {
		return err
	}
	leaseID, slug, vm, err := b.create(ctx, control, runner, req.Repo, req.RequestedSlug, true, req.Reclaim)
	if err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s microvm=%s region=%s image_version=%s\n", leaseID, slug, providerName, vm.ID, b.cfg.AWSRegion, vm.ImageVersion)
	if !req.Keep {
		fmt.Fprintf(b.rt.Stderr, "warning: %s warmup keeps the MicroVM until explicit stop\n", providerName)
	}
	total := core.ClockNow(b.rt.Clock).Sub(started)
	return shared.CompleteWarmup(b.rt, req.TimingJSON, shared.WarmupCompletion{
		Provider: providerName,
		LeaseID:  leaseID,
		Slug:     slug,
		Total:    total,
	})
}

func (b *backend) Run(ctx context.Context, req core.RunRequest) (result core.RunResult, retErr error) {
	if err := core.RejectDelegatedSyncOptionsForSpec(b.spec, req); err != nil {
		return core.RunResult{}, err
	}
	started := core.ClockNow(b.rt.Clock)
	control, runner, err := b.clients(ctx)
	if err != nil {
		return core.RunResult{}, err
	}
	var prepared *core.PreparedArchive
	if req.ID == "" && !req.NoSync {
		prepared, err = core.PrepareDelegatedArchive(ctx, core.DelegatedArchivePreparationRequest{
			Config: b.cfg, Repo: req.Repo, ForceSyncLarge: req.ForceSyncLarge,
			TempPattern: "crabbox-aws-lambda-microvm-sync-*.tgz", Stderr: b.rt.Stderr,
			Now: func() time.Time { return core.ClockNow(b.rt.Clock) },
		})
		if err != nil {
			return core.RunResult{}, err
		}
		defer prepared.Close()
	}
	leaseID, slug := "", ""
	var vm microVM
	var server core.Server
	acquired := false
	if req.ID == "" {
		leaseID, slug, vm, err = b.create(ctx, control, runner, req.Repo, req.RequestedSlug, req.Keep || req.KeepOnFailure, req.Reclaim)
		if err != nil {
			return core.RunResult{}, err
		}
		server = b.server(vm, leaseID, slug, req.Keep || req.KeepOnFailure, nil)
		acquired = true
		fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=%s microvm=%s region=%s image_version=%s\n", leaseID, slug, providerName, vm.ID, b.cfg.AWSRegion, vm.ImageVersion)
	} else {
		var claim core.LeaseClaim
		claim, vm, server, err = b.resolve(ctx, control, req.ID)
		if err != nil {
			return core.RunResult{}, err
		}
		unlockOperation, err := lockAWSLambdaMicroVMLeaseOperation(ctx, claim.LeaseID)
		if err != nil {
			return core.RunResult{}, err
		}
		defer unlockOperation()
		claim, vm, server, err = b.resolve(ctx, control, claim.LeaseID)
		if err != nil {
			return core.RunResult{}, err
		}
		if vm.ImageARN != b.cfg.AWSLambdaMicroVM.Image || (b.cfg.AWSLambdaMicroVM.ImageVersion != "" && vm.ImageVersion != b.cfg.AWSLambdaMicroVM.ImageVersion) {
			return core.RunResult{}, core.Exit(4, "%s image identity mismatch for lease %s", providerName, claim.LeaseID)
		}
		leaseID, slug = claim.LeaseID, claim.Slug
		server.Labels = core.TouchDirectLeaseLabels(server.Labels, b.cfg, strings.ToLower(vm.State), core.ClockNow(b.rt.Clock))
		if err := claimLease(leaseID, slug, b.scope(), req.Options.Pond, req.Repo.Root, b.cfg.IdleTimeout, req.Reclaim, server); err != nil {
			return core.RunResult{}, err
		}
	}

	shouldStop := acquired && !req.Keep
	session := &core.RunSessionHandle{
		Provider:       providerName,
		LeaseID:        leaseID,
		Slug:           slug,
		Reused:         !acquired,
		Kept:           !shouldStop,
		CleanupCommand: "crabbox stop --provider " + providerName + " " + core.ShellQuote(leaseID),
	}
	result = core.RunResult{Provider: providerName, LeaseID: leaseID, Slug: slug, SyncDelegated: true, Session: session}
	syncDuration := time.Duration(0)
	syncPhases := []core.TimingPhase{{Name: "sync", Skipped: true, Reason: "--no-sync"}}
	commandRan := false
	defer func() {
		// Preserve the native primary outcome before termination or reporting
		// can introduce another error, and publish timing only after cleanup.
		result, retErr = shared.PinDelegatedRunFailure(result, retErr)
		if shouldStop {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			cleanupErr := control.Terminate(cleanupCtx, vm.ID)
			cancel()
			if cleanupErr != nil && !isNotFound(cleanupErr) {
				session.Kept = true
				result, retErr = shared.AppendDelegatedRunFailure(result, retErr, fmt.Errorf("terminate AWS Lambda MicroVM %s: %w", vm.ID, cleanupErr), 1)
			} else {
				core.RemoveLeaseClaim(leaseID)
				session.Kept = false
			}
		} else {
			session.Kept = true
		}
		result.Total = core.ClockNow(b.rt.Clock).Sub(started)
		result = core.FinalizeRunResult(result, retErr)
		if commandRan {
			fmt.Fprintf(b.rt.Stderr, "%s run summary sync=%s command=%s total=%s exit=%d\n", providerName, syncDuration.Round(time.Millisecond), result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), result.ExitCode)
		}
		if req.TimingJSON {
			timingErr := core.WriteTimingJSON(b.rt.Stderr, core.TimingReportWithRunResult(core.TimingReport{Provider: providerName, LeaseID: leaseID, Slug: slug, SyncDelegated: true, SyncMs: syncDuration.Milliseconds(), SyncPhases: syncPhases, SyncSkipped: req.NoSync, CommandMs: result.Command.Milliseconds(), TotalMs: result.Total.Milliseconds(), ExitCode: result.ExitCode, Label: strings.TrimSpace(req.Label)}, result, retErr))
			result, retErr = shared.AppendDelegatedRunFailure(result, retErr, timingErr, 1)
		}
	}()

	fmt.Fprintf(b.rt.Stderr, "provider=%s lease=%s microvm=%s workdir=%s\n", providerName, leaseID, vm.ID, b.cfg.AWSLambdaMicroVM.Workdir)
	if !req.NoSync {
		syncPhases, syncDuration, err = b.syncWorkspace(ctx, runner, vm, req, prepared)
	} else {
		var exitCode int
		exitCode, err = runner.Exec(ctx, vm, "mkdir -p "+core.ShellQuote(b.cfg.AWSLambdaMicroVM.Workdir), "/", nil, io.Discard, b.rt.Stderr)
		if err == nil && exitCode != 0 {
			err = core.Exit(exitCode, "%s workspace preparation exited %d", providerName, exitCode)
		}
	}
	if err != nil {
		handleDelegatedRunFailure(b.rt.Stderr, req, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return result, err
	}
	if !req.NoSync {
		fmt.Fprintf(b.rt.Stderr, "sync complete in %s\n", syncDuration.Round(time.Millisecond))
	}
	if req.SyncOnly {
		fmt.Fprintf(b.rt.Stdout, "synced %s\n", b.cfg.AWSLambdaMicroVM.Workdir)
		return result, nil
	}

	command := core.ShellScriptFromArgv(req.Command)
	if req.ShellMode {
		command = strings.Join(req.Command, " ")
	}
	if strings.TrimSpace(command) == "" {
		return result, core.Exit(2, "provider=%s requires a command", providerName)
	}
	if req.EnvSummary {
		printEnvForwardingSummary(b.rt.Stderr, req.Options.EnvAllow, req.Env)
	}
	commandStarted := core.ClockNow(b.rt.Clock)
	req.Observation.Phase(core.RunPhaseCommand)
	stdout, stderr := req.Observation.CommandWriters(b.rt.Stdout, b.rt.Stderr, core.RunOutputWorkload)
	exitCode, commandErr := runner.Exec(ctx, vm, command, b.cfg.AWSLambdaMicroVM.Workdir, req.Env, stdout, stderr)
	result.Command = core.ClockNow(b.rt.Clock).Sub(commandStarted)
	result.CommandText = strings.Join(req.Command, " ")
	commandRan = true
	outcome := shared.FinalizeDelegatedCommandOutcome(exitCode, commandErr)
	result.ExitCode, result.Status, result.ErrorKind = outcome.ExitCode, outcome.Status, outcome.ErrorKind
	if commandErr != nil {
		handleDelegatedRunFailure(b.rt.Stderr, req, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return result, shared.ExitErrorWithCause(result.ExitCode, fmt.Sprintf("%s run failed: %v", providerName, commandErr), commandErr)
	}
	if exitCode != 0 {
		handleDelegatedRunFailure(b.rt.Stderr, req, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return result, core.ExitError{Code: exitCode, Message: fmt.Sprintf("%s run exited %d", providerName, exitCode)}
	}
	server.Labels = core.TouchDirectLeaseLabels(server.Labels, b.cfg, strings.ToLower(vm.State), core.ClockNow(b.rt.Clock))
	if err := claimLease(leaseID, slug, b.scope(), req.Options.Pond, req.Repo.Root, b.cfg.IdleTimeout, true, server); err != nil {
		failure, failureErr := shared.PinDelegatedRunFailure(core.RunResult{}, err)
		result.ExitCode, result.Status, result.ErrorKind = failure.ExitCode, failure.Status, failure.ErrorKind
		return result, failureErr
	}
	return result, nil
}

func (b *backend) List(ctx context.Context, _ core.ListRequest) ([]core.LeaseView, error) {
	control, _, err := b.clients(ctx)
	if err != nil {
		return nil, err
	}
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return nil, err
	}
	servers := make([]core.LeaseView, 0, len(claims))
	for _, claim := range claims {
		if claim.Provider != providerName || claim.ProviderScope != b.scope() {
			continue
		}
		vm, getErr := control.Get(ctx, claim.CloudID)
		if getErr != nil {
			if !isNotFound(getErr) {
				return nil, getErr
			}
			server := serverFromClaim(claim)
			server.Status = "missing-or-inaccessible"
			server.Labels["state"] = server.Status
			servers = append(servers, server)
			continue
		}
		servers = append(servers, b.server(vm, claim.LeaseID, claim.Slug, claim.Labels["keep"] == "true", claim.Labels))
	}
	return servers, nil
}

func (b *backend) Status(ctx context.Context, req core.StatusRequest) (core.StatusView, error) {
	control, _, err := b.clients(ctx)
	if err != nil {
		return core.StatusView{}, err
	}
	claim, vm, server, err := b.resolve(ctx, control, req.ID)
	if err != nil {
		return core.StatusView{}, err
	}
	deadline := core.ClockNow(b.rt.Clock).Add(req.WaitTimeout)
	if req.WaitTimeout <= 0 {
		deadline = core.ClockNow(b.rt.Clock).Add(lifecycleWaitTimeout)
	}
	for req.Wait && !microVMReady(vm.State) {
		if microVMTerminal(vm.State) {
			break
		}
		if core.ClockNow(b.rt.Clock).After(deadline) {
			return core.StatusView{}, core.Exit(5, "timed out waiting for AWS Lambda MicroVM %s", vm.ID)
		}
		if err := sleepContext(ctx, 2*time.Second); err != nil {
			return core.StatusView{}, err
		}
		vm, err = control.Get(ctx, vm.ID)
		if err != nil {
			return core.StatusView{}, err
		}
		server = b.server(vm, claim.LeaseID, claim.Slug, claim.Labels["keep"] == "true", claim.Labels)
	}
	return core.StatusView{ID: claim.LeaseID, Slug: claim.Slug, Provider: providerName, TargetOS: targetLinux, State: strings.ToLower(vm.State), ServerID: vm.ID, ServerType: server.ServerType.Name, Host: vm.Endpoint, Network: "public", Ready: microVMReady(vm.State), Labels: server.Labels}, nil
}

func (b *backend) Stop(ctx context.Context, req core.StopRequest) error {
	control, _, err := b.clients(ctx)
	if err != nil {
		return err
	}
	claim, ok, err := resolveLeaseClaim(req.ID)
	if err != nil {
		return err
	}
	if !ok || claim.ProviderScope != b.scope() {
		return core.Exit(4, "%s lease not found: %s", providerName, req.ID)
	}
	unlockOperation, err := lockAWSLambdaMicroVMLeaseOperation(ctx, claim.LeaseID)
	if err != nil {
		return err
	}
	defer unlockOperation()
	claim, ok, err = resolveLeaseClaim(claim.LeaseID)
	if err != nil {
		return err
	}
	if !ok || claim.ProviderScope != b.scope() {
		return core.Exit(4, "%s lease not found: %s", providerName, req.ID)
	}
	if err := control.Terminate(ctx, claim.CloudID); err != nil {
		if !isNotFound(err) || !b.cfg.AWSLambdaMicroVM.ForgetMissing {
			return err
		}
	}
	core.RemoveLeaseClaim(claim.LeaseID)
	fmt.Fprintf(b.rt.Stderr, "released lease=%s microvm=%s\n", claim.LeaseID, claim.CloudID)
	return nil
}

func (b *backend) Pause(ctx context.Context, req core.PauseRequest) error {
	return b.changeState(ctx, req.ID, "SUSPENDED")
}

func (b *backend) Resume(ctx context.Context, req core.ResumeRequest) error {
	return b.changeState(ctx, req.ID, "RUNNING")
}

func (b *backend) Cleanup(ctx context.Context, req core.CleanupRequest) error {
	servers, err := b.List(ctx, core.ListRequest{Options: req.Options})
	if err != nil {
		return err
	}
	for _, server := range servers {
		shouldDelete, reason := core.ShouldCleanupServer(server, core.ClockNow(b.rt.Clock))
		if !shouldDelete {
			fmt.Fprintf(b.rt.Stderr, "skip microvm id=%s reason=%s\n", server.CloudID, reason)
			continue
		}
		fmt.Fprintf(b.rt.Stderr, "terminate microvm id=%s lease=%s\n", server.CloudID, server.Labels["lease"])
		if req.DryRun {
			continue
		}
		if err := b.Stop(ctx, core.StopRequest{ID: server.Labels["lease"]}); err != nil {
			return err
		}
	}
	return nil
}

func (b *backend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	control, _, err := b.clients(ctx)
	if err != nil {
		return core.DoctorResult{}, err
	}
	if err := control.Probe(ctx, b.cfg.AWSLambdaMicroVM.Image, b.cfg.AWSLambdaMicroVM.ImageVersion); err != nil {
		return core.DoctorResult{}, err
	}
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return core.DoctorResult{}, err
	}
	count := 0
	for _, claim := range claims {
		if claim.Provider == providerName && claim.ProviderScope == b.scope() {
			count++
		}
	}
	result := coreInventoryDoctorResult(providerName, count)
	result.Message += fmt.Sprintf(" region=%s image=%s", b.cfg.AWSRegion, b.cfg.AWSLambdaMicroVM.Image)
	return result, nil
}

func coreInventoryDoctorResult(provider string, leases int) core.DoctorResult {
	return core.DoctorResult{Provider: provider, Message: fmt.Sprintf("auth=ready control_plane=ready inventory=ready api=list mutation=false leases=%d runtime=unchecked", leases)}
}

func (b *backend) clients(ctx context.Context) (controlPlane, runnerAPI, error) {
	control, err := b.newControl(ctx, b.cfg)
	if err != nil {
		return nil, nil, err
	}
	runner, err := b.newRunner(control, b.cfg, b.rt)
	if err != nil {
		return nil, nil, err
	}
	return control, runner, nil
}

func (b *backend) create(ctx context.Context, control controlPlane, runner runnerAPI, repo core.Repo, requestedSlug string, keep, reclaim bool) (leaseID, slug string, vm microVM, retErr error) {
	leaseID = core.NewLeaseID()
	slug, err := core.AllocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return "", "", microVM{}, err
	}
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s region=%s image=%s keep=%t\n", providerName, leaseID, slug, b.cfg.AWSRegion, b.cfg.AWSLambdaMicroVM.Image, keep)
	vm, err = control.Run(ctx, b.runRequest(leaseID))
	if err != nil {
		return "", "", microVM{}, err
	}
	createdVM := vm
	createdLeaseID, createdSlug := leaseID, slug
	rollback := true
	defer func() {
		if !rollback {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if cleanupErr := control.Terminate(cleanupCtx, createdVM.ID); cleanupErr != nil && !isNotFound(cleanupErr) {
			recoveryServer := b.server(createdVM, createdLeaseID, createdSlug, false, nil)
			if claimErr := claimLease(createdLeaseID, createdSlug, b.scope(), b.cfg.Pond, repo.Root, b.cfg.IdleTimeout, reclaim, recoveryServer); claimErr != nil {
				rollbackErr := fmt.Errorf("rollback termination failed for AWS Lambda MicroVM %s and recovery claim %s could not be persisted: %w", createdVM.ID, createdLeaseID, errors.Join(cleanupErr, claimErr))
				fmt.Fprintf(b.rt.Stderr, "warning: %v; terminate the MicroVM manually\n", rollbackErr)
				retErr = errors.Join(retErr, rollbackErr)
				return
			}
			rollbackErr := fmt.Errorf("rollback termination failed for AWS Lambda MicroVM %s; recovery claim %s remains for cleanup: %w", createdVM.ID, createdLeaseID, cleanupErr)
			fmt.Fprintf(b.rt.Stderr, "warning: %v; retry with crabbox stop --provider %s %s\n", rollbackErr, providerName, createdLeaseID)
			retErr = errors.Join(retErr, rollbackErr)
		}
	}()
	vm, err = b.waitReady(ctx, control, runner, vm)
	if err != nil {
		return "", "", microVM{}, err
	}
	server := b.server(vm, leaseID, slug, keep, nil)
	if err := claimLease(leaseID, slug, b.scope(), b.cfg.Pond, repo.Root, b.cfg.IdleTimeout, reclaim, server); err != nil {
		return "", "", microVM{}, err
	}
	rollback = false
	return leaseID, slug, vm, nil
}

func (b *backend) resolve(ctx context.Context, control controlPlane, identifier string) (core.LeaseClaim, microVM, core.Server, error) {
	claim, ok, err := resolveLeaseClaim(identifier)
	if err != nil {
		return core.LeaseClaim{}, microVM{}, core.Server{}, err
	}
	if !ok || claim.ProviderScope != b.scope() || claim.CloudID == "" {
		return core.LeaseClaim{}, microVM{}, core.Server{}, core.Exit(4, "%s lease not found: %s", providerName, identifier)
	}
	vm, err := control.Get(ctx, claim.CloudID)
	if err != nil {
		return core.LeaseClaim{}, microVM{}, core.Server{}, err
	}
	return claim, vm, b.server(vm, claim.LeaseID, claim.Slug, claim.Labels["keep"] == "true", claim.Labels), nil
}

func (b *backend) waitReady(ctx context.Context, control controlPlane, runner runnerAPI, vm microVM) (microVM, error) {
	deadline := core.ClockNow(b.rt.Clock).Add(lifecycleWaitTimeout)
	result, err := shared.Poll(context.WithoutCancel(ctx), 0, 2*time.Second,
		func(context.Context, time.Duration) error { return sleepContext(ctx, 2*time.Second) },
		func(context.Context) (microVM, error) { return control.Get(ctx, vm.ID) },
		func(_ context.Context, current microVM, fetchErr error) (bool, error) {
			if fetchErr != nil {
				return false, fetchErr
			}
			vm = current
			if microVMTerminal(vm.State) {
				return false, core.Exit(5, "AWS Lambda MicroVM %s entered %s: %s", vm.ID, vm.State, vm.StateReason)
			}
			if strings.EqualFold(vm.State, "RUNNING") {
				healthCtx, cancel := context.WithTimeout(ctx, runnerHealthProbeTimeout)
				err := runner.Health(healthCtx, vm)
				cancel()
				if err == nil {
					return true, nil
				}
			}
			if core.ClockNow(b.rt.Clock).After(deadline) {
				return false, core.Exit(5, "timed out waiting for AWS Lambda MicroVM %s runner readiness", vm.ID)
			}
			return false, nil
		}, nil)
	if err != nil {
		return microVM{}, err
	}
	return result.Value, nil
}

func (b *backend) changeState(ctx context.Context, identifier, target string) error {
	control, _, err := b.clients(ctx)
	if err != nil {
		return err
	}
	claim, vm, _, err := b.resolve(ctx, control, identifier)
	if err != nil {
		return err
	}
	unlockOperation, err := lockAWSLambdaMicroVMLeaseOperation(ctx, claim.LeaseID)
	if err != nil {
		return err
	}
	defer unlockOperation()
	claim, vm, _, err = b.resolve(ctx, control, claim.LeaseID)
	if err != nil {
		return err
	}
	if strings.EqualFold(vm.State, target) {
		return nil
	}
	if target == "SUSPENDED" {
		err = control.Suspend(ctx, vm.ID)
	} else {
		err = control.Resume(ctx, vm.ID)
	}
	if err != nil {
		return err
	}
	deadline := core.ClockNow(b.rt.Clock).Add(lifecycleWaitTimeout)
	for {
		vm, err = control.Get(ctx, vm.ID)
		if err != nil {
			return err
		}
		if strings.EqualFold(vm.State, target) {
			fmt.Fprintf(b.rt.Stderr, "%s lease=%s microvm=%s\n", strings.ToLower(target), claim.LeaseID, vm.ID)
			return nil
		}
		if microVMTerminal(vm.State) || core.ClockNow(b.rt.Clock).After(deadline) {
			return core.Exit(5, "AWS Lambda MicroVM %s did not reach %s (state=%s)", vm.ID, target, vm.State)
		}
		if err := sleepContext(ctx, 2*time.Second); err != nil {
			return err
		}
	}
}

func (b *backend) runRequest(leaseID string) runMicroVMRequest {
	idle := durationSeconds(b.cfg.IdleTimeout)
	maximum := durationSeconds(b.cfg.TTL)
	if maximum <= 0 || maximum > 28800 {
		maximum = 28800
	}
	suspended := maximum
	if suspended <= 0 {
		suspended = 28800
	}
	ingress := append([]string(nil), b.cfg.AWSLambdaMicroVM.IngressConnectors...)
	if len(ingress) == 0 {
		ingress = []string{managedConnectorARN(b.cfg.AWSRegion, "ALL_INGRESS")}
	}
	egress := append([]string(nil), b.cfg.AWSLambdaMicroVM.EgressConnectors...)
	if len(egress) == 0 {
		egress = []string{managedConnectorARN(b.cfg.AWSRegion, "INTERNET_EGRESS")}
	}
	return runMicroVMRequest{ImageARN: b.cfg.AWSLambdaMicroVM.Image, ImageVersion: b.cfg.AWSLambdaMicroVM.ImageVersion, ExecutionRoleARN: b.cfg.AWSLambdaMicroVM.ExecutionRoleARN, ClientToken: leaseID, IngressConnectors: ingress, EgressConnectors: egress, IdleSeconds: idle, SuspendedSeconds: suspended, MaximumSeconds: maximum}
}

func (b *backend) scope() string {
	return b.cfg.AWSRegion
}

func (b *backend) server(vm microVM, leaseID, slug string, keep bool, existing map[string]string) core.Server {
	labels := directLeaseLabels(b.cfg, leaseID, slug, keep, core.ClockNow(b.rt.Clock))
	for key, value := range existing {
		labels[key] = value
	}
	labels["state"] = strings.ToLower(vm.State)
	labels["aws_region"] = b.cfg.AWSRegion
	labels["image_arn"] = vm.ImageARN
	labels["image_version"] = vm.ImageVersion
	labels["endpoint"] = vm.Endpoint
	server := core.Server{CloudID: vm.ID, Provider: providerName, Name: slug, Status: strings.ToLower(vm.State), Labels: labels}
	server.PublicNet.IPv4.IP = vm.Endpoint
	server.ServerType.Name = vm.ImageVersion
	return server
}

func serverFromClaim(claim core.LeaseClaim) core.Server {
	labels := make(map[string]string, len(claim.Labels))
	for key, value := range claim.Labels {
		labels[key] = value
	}
	server := core.Server{CloudID: claim.CloudID, Provider: providerName, Name: claim.Slug, Status: labels["state"], Labels: labels}
	server.PublicNet.IPv4.IP = labels["endpoint"]
	server.ServerType.Name = labels["image_version"]
	return server
}

func managedConnectorARN(region, name string) string {
	return fmt.Sprintf("arn:aws:lambda:%s:aws:network-connector:aws-network-connector:%s", region, name)
}

func durationSeconds(value time.Duration) int32 {
	if value <= 0 {
		return 0
	}
	seconds := value / time.Second
	if seconds > 28800 {
		seconds = 28800
	}
	if seconds < 1 {
		seconds = 1
	}
	return int32(seconds)
}

func microVMReady(state string) bool {
	return strings.EqualFold(state, "RUNNING") || strings.EqualFold(state, "SUSPENDED")
}

func microVMTerminal(state string) bool {
	return strings.EqualFold(state, "TERMINATED") || strings.EqualFold(state, "TERMINATING")
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
