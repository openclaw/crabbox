package awslambdamicrovm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
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
	spec       ProviderSpec
	cfg        Config
	rt         Runtime
	newControl func(context.Context, Config) (controlPlane, error)
	newRunner  func(controlPlane, Config, Runtime) runnerAPI
}

func newBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	return &backend{
		spec:       spec,
		cfg:        cfg,
		rt:         rt,
		newControl: newControlPlane,
		newRunner: func(control controlPlane, cfg Config, rt Runtime) runnerAPI {
			return newRunnerClient(control, rt.HTTP, cfg.AWSRegion)
		},
	}
}

func (b *backend) Spec() ProviderSpec { return b.spec }

func (b *backend) Warmup(ctx context.Context, req WarmupRequest) error {
	if req.ActionsRunner {
		return exit(2, "--actions-runner is not supported for provider=%s", providerName)
	}
	started := now(b.rt)
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
	total := now(b.rt).Sub(started)
	fmt.Fprintf(b.rt.Stdout, "warmup complete total=%s\n", total.Round(time.Millisecond))
	if req.TimingJSON {
		return writeTimingJSON(b.rt.Stderr, timingReport{Provider: providerName, LeaseID: leaseID, Slug: slug, TotalMs: total.Milliseconds(), ExitCode: 0})
	}
	return nil
}

func (b *backend) Run(ctx context.Context, req RunRequest) (result RunResult, retErr error) {
	if err := delegatedSyncOptionsError(b.spec, req); err != nil {
		return RunResult{}, err
	}
	started := now(b.rt)
	control, runner, err := b.clients(ctx)
	if err != nil {
		return RunResult{}, err
	}
	leaseID, slug := "", ""
	var vm microVM
	var server Server
	acquired := false
	if req.ID == "" {
		leaseID, slug, vm, err = b.create(ctx, control, runner, req.Repo, req.RequestedSlug, req.Keep || req.KeepOnFailure, req.Reclaim)
		if err != nil {
			return RunResult{}, err
		}
		server = b.server(vm, leaseID, slug, req.Keep || req.KeepOnFailure, nil)
		acquired = true
		fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=%s microvm=%s region=%s image_version=%s\n", leaseID, slug, providerName, vm.ID, b.cfg.AWSRegion, vm.ImageVersion)
	} else {
		var claim LeaseClaim
		claim, vm, server, err = b.resolve(ctx, control, req.ID)
		if err != nil {
			return RunResult{}, err
		}
		unlockOperation, err := lockAWSLambdaMicroVMLeaseOperation(ctx, claim.LeaseID)
		if err != nil {
			return RunResult{}, err
		}
		defer unlockOperation()
		claim, vm, server, err = b.resolve(ctx, control, claim.LeaseID)
		if err != nil {
			return RunResult{}, err
		}
		if vm.ImageARN != b.cfg.AWSLambdaMicroVM.Image || (b.cfg.AWSLambdaMicroVM.ImageVersion != "" && vm.ImageVersion != b.cfg.AWSLambdaMicroVM.ImageVersion) {
			return RunResult{}, exit(4, "%s image identity mismatch for lease %s", providerName, claim.LeaseID)
		}
		leaseID, slug = claim.LeaseID, claim.Slug
		server.Labels = touchLeaseLabels(server.Labels, b.cfg, strings.ToLower(vm.State), now(b.rt))
		if err := claimLease(leaseID, slug, b.scope(), req.Options.Pond, req.Repo.Root, b.cfg.IdleTimeout, req.Reclaim, server); err != nil {
			return RunResult{}, err
		}
	}

	shouldStop := acquired && !req.Keep
	session := &RunSessionHandle{
		Provider:       providerName,
		LeaseID:        leaseID,
		Slug:           slug,
		Reused:         !acquired,
		Kept:           !shouldStop,
		CleanupCommand: "crabbox stop --provider " + providerName + " " + shellQuote(leaseID),
	}
	defer func() {
		if !shouldStop {
			session.Kept = true
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if cleanupErr := control.Terminate(cleanupCtx, vm.ID); cleanupErr != nil && !isNotFound(cleanupErr) {
			session.Kept = true
			if retErr == nil {
				retErr = fmt.Errorf("terminate AWS Lambda MicroVM %s: %w", vm.ID, cleanupErr)
			} else {
				retErr = errors.Join(retErr, cleanupErr)
			}
			return
		}
		removeLeaseClaim(leaseID)
		session.Kept = false
	}()

	fmt.Fprintf(b.rt.Stderr, "provider=%s lease=%s microvm=%s workdir=%s\n", providerName, leaseID, vm.ID, b.cfg.AWSLambdaMicroVM.Workdir)
	syncDuration := time.Duration(0)
	syncPhases := []timingPhase{{Name: "sync", Skipped: true, Reason: "--no-sync"}}
	if !req.NoSync {
		syncPhases, syncDuration, err = b.syncWorkspace(ctx, runner, vm, req)
	} else {
		var exitCode int
		exitCode, err = runner.Exec(ctx, vm, "mkdir -p "+shellQuote(b.cfg.AWSLambdaMicroVM.Workdir), "/", nil, io.Discard, b.rt.Stderr)
		if err == nil && exitCode != 0 {
			err = exit(exitCode, "%s workspace preparation exited %d", providerName, exitCode)
		}
	}
	if err != nil {
		handleDelegatedRunFailure(b.rt.Stderr, req, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return RunResult{Provider: providerName, LeaseID: leaseID, Slug: slug, Total: now(b.rt).Sub(started), SyncDelegated: true, Session: session}, err
	}
	if !req.NoSync {
		fmt.Fprintf(b.rt.Stderr, "sync complete in %s\n", syncDuration.Round(time.Millisecond))
	}
	if req.SyncOnly {
		result = RunResult{Provider: providerName, LeaseID: leaseID, Slug: slug, Total: now(b.rt).Sub(started), SyncDelegated: true, Session: session}
		fmt.Fprintf(b.rt.Stdout, "synced %s\n", b.cfg.AWSLambdaMicroVM.Workdir)
		if req.TimingJSON {
			retErr = writeTimingJSON(b.rt.Stderr, timingReportWithRunResult(timingReport{Provider: providerName, LeaseID: leaseID, Slug: slug, SyncDelegated: true, SyncMs: syncDuration.Milliseconds(), SyncPhases: syncPhases, SyncSkipped: req.NoSync, TotalMs: result.Total.Milliseconds()}, result, nil))
		}
		return result, retErr
	}

	command := shellScriptFromArgv(req.Command)
	if req.ShellMode {
		command = strings.Join(req.Command, " ")
	}
	if strings.TrimSpace(command) == "" {
		return RunResult{}, exit(2, "provider=%s requires a command", providerName)
	}
	if req.EnvSummary {
		printEnvForwardingSummary(b.rt.Stderr, req.Options.EnvAllow, req.Env)
	}
	commandStarted := now(b.rt)
	exitCode, commandErr := runner.Exec(ctx, vm, command, b.cfg.AWSLambdaMicroVM.Workdir, req.Env, b.rt.Stdout, b.rt.Stderr)
	commandDuration := now(b.rt).Sub(commandStarted)
	result = RunResult{
		Provider: providerName, LeaseID: leaseID, Slug: slug,
		ExitCode: exitCode, Command: commandDuration, Total: now(b.rt).Sub(started),
		SyncDelegated: true, CommandText: strings.Join(req.Command, " "), Session: session,
	}
	fmt.Fprintf(b.rt.Stderr, "%s run summary sync=%s command=%s total=%s exit=%d\n", providerName, syncDuration.Round(time.Millisecond), commandDuration.Round(time.Millisecond), result.Total.Round(time.Millisecond), exitCode)
	if req.TimingJSON {
		if timingErr := writeTimingJSON(b.rt.Stderr, timingReportWithRunResult(timingReport{Provider: providerName, LeaseID: leaseID, Slug: slug, SyncDelegated: true, SyncMs: syncDuration.Milliseconds(), SyncPhases: syncPhases, SyncSkipped: req.NoSync, CommandMs: commandDuration.Milliseconds(), TotalMs: result.Total.Milliseconds(), ExitCode: exitCode, Label: strings.TrimSpace(req.Label)}, result, commandErr)); timingErr != nil {
			return result, timingErr
		}
	}
	if commandErr != nil {
		handleDelegatedRunFailure(b.rt.Stderr, req, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return result, fmt.Errorf("%s run failed: %w", providerName, commandErr)
	}
	if exitCode != 0 {
		handleDelegatedRunFailure(b.rt.Stderr, req, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return result, ExitError{Code: exitCode, Message: fmt.Sprintf("%s run exited %d", providerName, exitCode)}
	}
	server.Labels = touchLeaseLabels(server.Labels, b.cfg, strings.ToLower(vm.State), now(b.rt))
	if err := claimLease(leaseID, slug, b.scope(), req.Options.Pond, req.Repo.Root, b.cfg.IdleTimeout, true, server); err != nil {
		return result, err
	}
	return result, nil
}

func (b *backend) List(ctx context.Context, _ ListRequest) ([]LeaseView, error) {
	control, _, err := b.clients(ctx)
	if err != nil {
		return nil, err
	}
	claims, err := listLeaseClaims()
	if err != nil {
		return nil, err
	}
	servers := make([]LeaseView, 0, len(claims))
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

func (b *backend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	control, _, err := b.clients(ctx)
	if err != nil {
		return StatusView{}, err
	}
	claim, vm, server, err := b.resolve(ctx, control, req.ID)
	if err != nil {
		return StatusView{}, err
	}
	deadline := now(b.rt).Add(req.WaitTimeout)
	if req.WaitTimeout <= 0 {
		deadline = now(b.rt).Add(lifecycleWaitTimeout)
	}
	for req.Wait && !microVMReady(vm.State) {
		if microVMTerminal(vm.State) {
			break
		}
		if now(b.rt).After(deadline) {
			return StatusView{}, exit(5, "timed out waiting for AWS Lambda MicroVM %s", vm.ID)
		}
		if err := sleepContext(ctx, 2*time.Second); err != nil {
			return StatusView{}, err
		}
		vm, err = control.Get(ctx, vm.ID)
		if err != nil {
			return StatusView{}, err
		}
		server = b.server(vm, claim.LeaseID, claim.Slug, claim.Labels["keep"] == "true", claim.Labels)
	}
	return StatusView{ID: claim.LeaseID, Slug: claim.Slug, Provider: providerName, TargetOS: targetLinux, State: strings.ToLower(vm.State), ServerID: vm.ID, ServerType: server.ServerType.Name, Host: vm.Endpoint, Network: "public", Ready: microVMReady(vm.State), Labels: server.Labels}, nil
}

func (b *backend) Stop(ctx context.Context, req StopRequest) error {
	control, _, err := b.clients(ctx)
	if err != nil {
		return err
	}
	claim, ok, err := resolveLeaseClaim(req.ID)
	if err != nil {
		return err
	}
	if !ok || claim.ProviderScope != b.scope() {
		return exit(4, "%s lease not found: %s", providerName, req.ID)
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
		return exit(4, "%s lease not found: %s", providerName, req.ID)
	}
	if err := control.Terminate(ctx, claim.CloudID); err != nil {
		if !isNotFound(err) || !b.cfg.AWSLambdaMicroVM.ForgetMissing {
			return err
		}
	}
	removeLeaseClaim(claim.LeaseID)
	fmt.Fprintf(b.rt.Stderr, "released lease=%s microvm=%s\n", claim.LeaseID, claim.CloudID)
	return nil
}

func (b *backend) Pause(ctx context.Context, req PauseRequest) error {
	return b.changeState(ctx, req.ID, "SUSPENDED")
}

func (b *backend) Resume(ctx context.Context, req ResumeRequest) error {
	return b.changeState(ctx, req.ID, "RUNNING")
}

func (b *backend) Cleanup(ctx context.Context, req CleanupRequest) error {
	servers, err := b.List(ctx, ListRequest{Options: req.Options})
	if err != nil {
		return err
	}
	for _, server := range servers {
		shouldDelete, reason := shouldCleanupServer(server, now(b.rt))
		if !shouldDelete {
			fmt.Fprintf(b.rt.Stderr, "skip microvm id=%s reason=%s\n", server.CloudID, reason)
			continue
		}
		fmt.Fprintf(b.rt.Stderr, "terminate microvm id=%s lease=%s\n", server.CloudID, server.Labels["lease"])
		if req.DryRun {
			continue
		}
		if err := b.Stop(ctx, StopRequest{ID: server.Labels["lease"]}); err != nil {
			return err
		}
	}
	return nil
}

func (b *backend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	control, _, err := b.clients(ctx)
	if err != nil {
		return DoctorResult{}, err
	}
	if err := control.Probe(ctx, b.cfg.AWSLambdaMicroVM.Image, b.cfg.AWSLambdaMicroVM.ImageVersion); err != nil {
		return DoctorResult{}, err
	}
	claims, err := listLeaseClaims()
	if err != nil {
		return DoctorResult{}, err
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

func coreInventoryDoctorResult(provider string, leases int) DoctorResult {
	return DoctorResult{Provider: provider, Message: fmt.Sprintf("auth=ready control_plane=ready inventory=ready api=list mutation=false leases=%d runtime=unchecked", leases)}
}

func (b *backend) clients(ctx context.Context) (controlPlane, runnerAPI, error) {
	control, err := b.newControl(ctx, b.cfg)
	if err != nil {
		return nil, nil, err
	}
	return control, b.newRunner(control, b.cfg, b.rt), nil
}

func (b *backend) create(ctx context.Context, control controlPlane, runner runnerAPI, repo Repo, requestedSlug string, keep, reclaim bool) (string, string, microVM, error) {
	leaseID := newLeaseID()
	slug, err := allocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return "", "", microVM{}, err
	}
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s region=%s image=%s keep=%t\n", providerName, leaseID, slug, b.cfg.AWSRegion, b.cfg.AWSLambdaMicroVM.Image, keep)
	vm, err := control.Run(ctx, b.runRequest(leaseID))
	if err != nil {
		return "", "", microVM{}, err
	}
	createdID := vm.ID
	rollback := true
	defer func() {
		if rollback {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = control.Terminate(cleanupCtx, createdID)
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

func (b *backend) resolve(ctx context.Context, control controlPlane, identifier string) (LeaseClaim, microVM, Server, error) {
	claim, ok, err := resolveLeaseClaim(identifier)
	if err != nil {
		return LeaseClaim{}, microVM{}, Server{}, err
	}
	if !ok || claim.ProviderScope != b.scope() || claim.CloudID == "" {
		return LeaseClaim{}, microVM{}, Server{}, exit(4, "%s lease not found: %s", providerName, identifier)
	}
	vm, err := control.Get(ctx, claim.CloudID)
	if err != nil {
		return LeaseClaim{}, microVM{}, Server{}, err
	}
	return claim, vm, b.server(vm, claim.LeaseID, claim.Slug, claim.Labels["keep"] == "true", claim.Labels), nil
}

func (b *backend) waitReady(ctx context.Context, control controlPlane, runner runnerAPI, vm microVM) (microVM, error) {
	deadline := now(b.rt).Add(lifecycleWaitTimeout)
	for {
		current, err := control.Get(ctx, vm.ID)
		if err != nil {
			return microVM{}, err
		}
		vm = current
		if microVMTerminal(vm.State) {
			return microVM{}, exit(5, "AWS Lambda MicroVM %s entered %s: %s", vm.ID, vm.State, vm.StateReason)
		}
		if strings.EqualFold(vm.State, "RUNNING") {
			healthCtx, cancel := context.WithTimeout(ctx, runnerHealthProbeTimeout)
			err := runner.Health(healthCtx, vm)
			cancel()
			if err == nil {
				return vm, nil
			}
		}
		if now(b.rt).After(deadline) {
			return microVM{}, exit(5, "timed out waiting for AWS Lambda MicroVM %s runner readiness", vm.ID)
		}
		if err := sleepContext(ctx, 2*time.Second); err != nil {
			return microVM{}, err
		}
	}
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
	deadline := now(b.rt).Add(lifecycleWaitTimeout)
	for {
		vm, err = control.Get(ctx, vm.ID)
		if err != nil {
			return err
		}
		if strings.EqualFold(vm.State, target) {
			fmt.Fprintf(b.rt.Stderr, "%s lease=%s microvm=%s\n", strings.ToLower(target), claim.LeaseID, vm.ID)
			return nil
		}
		if microVMTerminal(vm.State) || now(b.rt).After(deadline) {
			return exit(5, "AWS Lambda MicroVM %s did not reach %s (state=%s)", vm.ID, target, vm.State)
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

func (b *backend) server(vm microVM, leaseID, slug string, keep bool, existing map[string]string) Server {
	labels := directLeaseLabels(b.cfg, leaseID, slug, keep, now(b.rt))
	for key, value := range existing {
		labels[key] = value
	}
	labels["state"] = strings.ToLower(vm.State)
	labels["aws_region"] = b.cfg.AWSRegion
	labels["image_arn"] = vm.ImageARN
	labels["image_version"] = vm.ImageVersion
	labels["endpoint"] = vm.Endpoint
	server := Server{CloudID: vm.ID, Provider: providerName, Name: slug, Status: strings.ToLower(vm.State), Labels: labels}
	server.PublicNet.IPv4.IP = vm.Endpoint
	server.ServerType.Name = vm.ImageVersion
	return server
}

func serverFromClaim(claim LeaseClaim) Server {
	labels := make(map[string]string, len(claim.Labels))
	for key, value := range claim.Labels {
		labels[key] = value
	}
	server := Server{CloudID: claim.CloudID, Provider: providerName, Name: claim.Slug, Status: labels["state"], Labels: labels}
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
