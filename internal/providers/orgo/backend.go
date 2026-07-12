package orgo

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	orgoCreatedWorkspaceLabel = "orgo_workspace_created"
	orgoWorkspaceLabel        = "orgo_workspace_id"
	orgoReadyTimeout          = 5 * time.Minute
	orgoReadyPollInterval     = 250 * time.Millisecond
	orgoCleanupTimeout        = 30 * time.Second
)

var orgoEnvNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func NewOrgoBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = providerName
	applyOrgoDefaults(&cfg)
	return &orgoBackend{spec: spec, cfg: cfg, rt: rt}
}

type orgoBackend struct {
	spec   ProviderSpec
	cfg    Config
	rt     Runtime
	client orgoAPI
}

type orgoLease struct {
	LeaseID          string
	Slug             string
	Computer         orgoComputer
	CreatedWorkspace string
}

func (b *orgoBackend) Spec() ProviderSpec { return b.spec }

func (b *orgoBackend) Warmup(ctx context.Context, req WarmupRequest) error {
	if req.ActionsRunner {
		return exit(2, "--actions-runner is not supported for provider=%s", providerName)
	}
	if req.Options.Tailscale.Enabled {
		return exit(2, "provider=%s is delegated-run only and does not support Tailscale options", providerName)
	}
	started := b.now()
	client, err := b.api()
	if err != nil {
		return err
	}
	lease, err := b.createComputer(ctx, client, req.Repo, req.RequestedSlug, req.Reclaim)
	if err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s computer=%s workspace=%s\n", lease.LeaseID, lease.Slug, providerName, lease.Computer.ID, lease.Computer.WorkspaceID)
	if !req.Keep {
		fmt.Fprintf(b.rt.Stderr, "warning: orgo warmup keeps the computer until explicit stop\n")
	}
	total := b.now().Sub(started)
	fmt.Fprintf(b.rt.Stdout, "warmup complete total=%s\n", total.Round(time.Millisecond))
	if req.TimingJSON {
		return writeTimingJSON(b.rt.Stderr, timingReport{
			Provider: providerName,
			LeaseID:  lease.LeaseID,
			Slug:     lease.Slug,
			TotalMs:  total.Milliseconds(),
			ExitCode: 0,
		})
	}
	return nil
}

func (b *orgoBackend) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if err := b.rejectRunOptions(req); err != nil {
		return RunResult{}, err
	}
	if len(req.Command) == 0 {
		return RunResult{}, exit(2, "missing command")
	}
	started := b.now()
	client, err := b.api()
	if err != nil {
		return RunResult{}, err
	}
	lease := orgoLease{}
	acquired := false
	if strings.TrimSpace(req.ID) == "" {
		lease, err = b.createComputer(ctx, client, req.Repo, req.RequestedSlug, req.Reclaim)
		if err != nil {
			return RunResult{}, err
		}
		acquired = true
		fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=%s computer=%s workspace=%s\n", lease.LeaseID, lease.Slug, providerName, lease.Computer.ID, lease.Computer.WorkspaceID)
	} else {
		lease, err = b.resolveComputer(ctx, client, req.ID)
		if err != nil {
			return RunResult{}, err
		}
		lease.Computer, err = b.ensureComputerRunning(ctx, client, lease.Computer)
		if err != nil {
			return RunResult{}, err
		}
	}

	shouldStop := acquired && !req.Keep
	defer func() {
		if !shouldStop {
			return
		}
		if err := b.cleanupLease(client, lease); err != nil {
			fmt.Fprintf(b.rt.Stderr, "warning: orgo cleanup failed for %s: %v\n", lease.Computer.ID, err)
		}
	}()

	command, err := b.buildCommand(req)
	if err != nil {
		return RunResult{}, err
	}
	commandText := orgoCommandText(req)
	if req.EnvSummary {
		printEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, req.Env)
	}
	commandStarted := b.now()
	exitCode, runErr := client.RunBash(ctx, lease.Computer.ID, command, b.rt.Stdout, b.rt.Stderr)
	commandDuration := b.now().Sub(commandStarted)
	result := RunResult{
		ExitCode:      exitCode,
		Command:       commandDuration,
		Total:         b.now().Sub(started),
		SyncDelegated: true,
		Provider:      providerName,
		LeaseID:       lease.LeaseID,
		Slug:          lease.Slug,
		CommandText:   commandText,
	}
	fmt.Fprintf(b.rt.Stderr, "orgo run summary sync_delegated=true command=%s total=%s exit=%d\n", commandDuration.Round(time.Millisecond), result.Total.Round(time.Millisecond), result.ExitCode)
	if req.TimingJSON {
		if err := writeTimingJSON(b.rt.Stderr, timingReport{
			Provider:      providerName,
			LeaseID:       lease.LeaseID,
			Slug:          lease.Slug,
			SyncDelegated: true,
			SyncSkipped:   true,
			CommandMs:     commandDuration.Milliseconds(),
			TotalMs:       result.Total.Milliseconds(),
			ExitCode:      result.ExitCode,
			Label:         strings.TrimSpace(req.Label),
		}); err != nil {
			return result, err
		}
	}
	if runErr != nil {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, lease.LeaseID, lease.Slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return result, runErr
	}
	if result.ExitCode != 0 {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, lease.LeaseID, lease.Slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return result, ExitError{Code: result.ExitCode, Message: fmt.Sprintf("%s computer exit=%d", providerName, result.ExitCode)}
	}
	return result, nil
}

func (b *orgoBackend) List(ctx context.Context, _ ListRequest) ([]LeaseView, error) {
	client, err := b.api()
	if err != nil {
		return nil, err
	}
	computers, err := b.listComputers(ctx, client)
	if err != nil {
		return nil, err
	}
	claimsByComputer := map[string]LeaseClaim{}
	if claims, err := listLeaseClaims(); err == nil {
		for _, claim := range claims {
			if claim.Provider == providerName && strings.TrimSpace(claim.CloudID) != "" {
				claimsByComputer[claim.CloudID] = claim
			}
		}
	}
	servers := make([]Server, 0, len(computers))
	for _, computer := range computers {
		claim := claimsByComputer[computer.ID]
		servers = append(servers, orgoComputerServer(computer, claim))
	}
	return servers, nil
}

func (b *orgoBackend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	if strings.TrimSpace(req.ID) == "" {
		return StatusView{}, exit(2, "provider=%s status requires --id <computer-id-or-slug>", providerName)
	}
	client, err := b.api()
	if err != nil {
		return StatusView{}, err
	}
	lease, err := b.resolveComputer(ctx, client, req.ID)
	if err != nil {
		return StatusView{}, err
	}
	timeout := req.WaitTimeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	deadline := b.now().Add(timeout)
	for {
		view := orgoStatusView(lease)
		if !req.Wait || view.Ready {
			return view, nil
		}
		switch view.State {
		case "error", "failed", "deleted":
			return view, exit(5, "orgo computer %s entered %s state", lease.Computer.ID, view.State)
		}
		if b.now().After(deadline) {
			return StatusView{}, exit(5, "timed out waiting for orgo computer %s to become ready", lease.Computer.ID)
		}
		select {
		case <-ctx.Done():
			return StatusView{}, ctx.Err()
		case <-time.After(1 * time.Second):
		}
		computer, err := client.GetComputer(ctx, lease.Computer.ID)
		if err != nil {
			return StatusView{}, err
		}
		lease.Computer = computer
	}
}

func (b *orgoBackend) Stop(ctx context.Context, req StopRequest) error {
	if strings.TrimSpace(req.ID) == "" {
		return exit(2, "provider=%s stop requires --id <computer-id-or-slug>", providerName)
	}
	client, err := b.api()
	if err != nil {
		return err
	}
	lease, err := b.resolveClaimedComputer(ctx, client, req.ID)
	if err != nil {
		return err
	}
	return b.deleteLease(ctx, client, lease)
}

func (b *orgoBackend) resolveClaimedComputer(ctx context.Context, client orgoAPI, identifier string) (orgoLease, error) {
	id := strings.TrimSpace(identifier)
	claim, ok, err := resolveLeaseClaimForProviderCloudID(id)
	if err != nil {
		return orgoLease{}, err
	}
	if !ok {
		claim, ok, err = resolveLeaseClaimForProvider(id)
		if err != nil {
			return orgoLease{}, err
		}
	}
	if !ok {
		return orgoLease{}, exit(4, "provider=%s refuses to stop unclaimed computer %s", providerName, id)
	}
	computerID := strings.TrimSpace(claim.CloudID)
	if computerID == "" {
		return orgoLease{}, exit(4, "provider=%s claim %s has no computer identity", providerName, claim.LeaseID)
	}
	lease := orgoLease{
		LeaseID: claim.LeaseID,
		Slug:    claim.Slug,
		Computer: orgoComputer{
			ID:          computerID,
			WorkspaceID: strings.TrimSpace(claim.Labels[orgoWorkspaceLabel]),
		},
		CreatedWorkspace: strings.TrimSpace(claim.Labels[orgoCreatedWorkspaceLabel]),
	}
	computer, err := client.GetComputer(ctx, computerID)
	if err != nil {
		if !isOrgoNotFound(err) {
			return orgoLease{}, err
		}
		workspaceID := lease.Computer.WorkspaceID
		if workspaceID == "" {
			return orgoLease{}, err
		}
		workspace, workspaceErr := client.GetWorkspace(ctx, workspaceID)
		if workspaceErr != nil {
			return orgoLease{}, errors.Join(err, fmt.Errorf("verify orgo workspace inventory: %w", workspaceErr))
		}
		if workspace.Computers == nil {
			return orgoLease{}, errors.Join(err, errors.New("verify orgo workspace inventory: response omitted computers"))
		}
		for _, candidate := range workspace.Computers {
			if candidate.ID == computerID {
				return orgoLease{}, err
			}
		}
		// A readable, complete workspace inventory proves the claimed computer is
		// absent. Keep the workspace identity so a prior partial cleanup can retry.
		lease.Computer.ID = ""
		return lease, nil
	}
	if computer.WorkspaceID == "" {
		computer.WorkspaceID = lease.Computer.WorkspaceID
	}
	lease.Computer = computer
	return lease, nil
}

func (b *orgoBackend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	client, err := b.api()
	if err != nil {
		return DoctorResult{}, err
	}
	computers, err := b.listComputers(ctx, client)
	if err != nil {
		return DoctorResult{}, err
	}
	return inventoryDoctorResult(providerName, len(computers)), nil
}

func (b *orgoBackend) api() (orgoAPI, error) {
	if b.client != nil {
		return b.client, nil
	}
	client, err := newOrgoClient(b.cfg, b.rt)
	if err != nil {
		return nil, err
	}
	b.client = client
	return client, nil
}

func (b *orgoBackend) createComputer(ctx context.Context, client orgoAPI, repo Repo, requestedSlug string, reclaim bool) (orgoLease, error) {
	leaseID := newLeaseID()
	slug, err := allocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return orgoLease{}, err
	}
	workspaceID := strings.TrimSpace(b.cfg.Orgo.WorkspaceID)
	createdWorkspace := ""
	if workspaceID == "" {
		workspaceName := "crabbox-" + leaseID
		workspace, err := client.CreateWorkspace(ctx, workspaceName)
		if err != nil {
			return orgoLease{}, fmt.Errorf("create orgo workspace name=%q: %w", workspaceName, err)
		}
		workspaceID = workspace.ID
		createdWorkspace = workspace.ID
	}
	req := b.createComputerRequest(workspaceID, leaseID)
	computer, err := client.CreateComputer(ctx, req)
	if err != nil {
		err = fmt.Errorf("create orgo computer workspace=%s name=%q: %w", workspaceID, req.Name, err)
		if createdWorkspace != "" {
			if cleanupErr := b.cleanupLease(client, orgoLease{CreatedWorkspace: createdWorkspace}); cleanupErr != nil {
				return orgoLease{}, errors.Join(err, fmt.Errorf("rollback orgo workspace %s: %w", createdWorkspace, cleanupErr))
			}
		}
		return orgoLease{}, err
	}
	if computer.WorkspaceID == "" {
		computer.WorkspaceID = workspaceID
	}
	computer, err = b.waitForComputerRunning(ctx, client, computer, false)
	if err != nil {
		cleanupErr := b.cleanupLease(client, orgoLease{
			Computer:         computer,
			CreatedWorkspace: createdWorkspace,
		})
		if cleanupErr != nil {
			return orgoLease{}, errors.Join(err, fmt.Errorf("rollback orgo computer %s workspace %s: %w", computer.ID, computer.WorkspaceID, cleanupErr))
		}
		return orgoLease{}, err
	}
	lease := orgoLease{LeaseID: leaseID, Slug: slug, Computer: computer, CreatedWorkspace: createdWorkspace}
	if err := b.claimLease(repo, lease, reclaim); err != nil {
		if cleanupErr := b.cleanupLease(client, lease); cleanupErr != nil {
			return orgoLease{}, errors.Join(err, fmt.Errorf("rollback orgo computer %s workspace %s: %w", computer.ID, computer.WorkspaceID, cleanupErr))
		}
		return orgoLease{}, err
	}
	return lease, nil
}

func (b *orgoBackend) ensureComputerRunning(ctx context.Context, client orgoAPI, computer orgoComputer) (orgoComputer, error) {
	return b.waitForComputerRunning(ctx, client, computer, true)
}

func (b *orgoBackend) waitForComputerRunning(ctx context.Context, client orgoAPI, computer orgoComputer, startStopped bool) (orgoComputer, error) {
	deadline := b.now().Add(orgoReadyTimeout)
	startRequested := false
	for {
		state := normalizeOrgoStatus(computer.Status)
		switch state {
		case "running":
			return computer, nil
		case "error", "failed", "deleted":
			return computer, exit(5, "orgo computer %s entered %s state while starting", computer.ID, state)
		case "stopped", "suspended":
			if startStopped && !startRequested {
				if err := client.StartComputer(ctx, computer.ID); err != nil {
					return computer, err
				}
				startRequested = true
				computer.Status = "starting"
				continue
			}
		}
		if !b.now().Before(deadline) {
			return computer, exit(5, "timed out waiting for orgo computer %s to become running (last state=%s)", computer.ID, state)
		}
		select {
		case <-ctx.Done():
			return computer, ctx.Err()
		case <-time.After(orgoReadyPollInterval):
		}
		refreshed, err := client.GetComputer(ctx, computer.ID)
		if err != nil {
			return computer, err
		}
		if refreshed.WorkspaceID == "" {
			refreshed.WorkspaceID = computer.WorkspaceID
		}
		computer = refreshed
	}
}

func (b *orgoBackend) createComputerRequest(workspaceID, leaseID string) orgoCreateComputerRequest {
	cfg := b.cfg.Orgo
	return orgoCreateComputerRequest{
		WorkspaceID: workspaceID,
		Name:        "crabbox-" + leaseID,
		OS:          "linux",
		RAMGB:       cfg.RAMGB,
		CPUs:        cfg.CPUs,
		DiskGB:      cfg.DiskGB,
		Resolution:  strings.TrimSpace(cfg.Resolution),
	}
}

func (b *orgoBackend) claimLease(repo Repo, lease orgoLease, reclaim bool) error {
	labels := map[string]string{
		"provider":           providerName,
		orgoWorkspaceLabel:   lease.Computer.WorkspaceID,
		"orgo_instance_id":   lease.Computer.InstanceID,
		"orgo_connectionURL": lease.Computer.ConnectionURL,
		"target":             targetLinux,
	}
	if lease.CreatedWorkspace != "" {
		labels[orgoCreatedWorkspaceLabel] = lease.CreatedWorkspace
	}
	server := orgoComputerServer(lease.Computer, LeaseClaim{
		LeaseID: lease.LeaseID,
		Slug:    lease.Slug,
		Labels:  labels,
	})
	return claimLeaseForRepoProviderEndpoint(lease.LeaseID, lease.Slug, repo.Root, b.cfg.IdleTimeout, reclaim, server)
}

func (b *orgoBackend) resolveComputer(ctx context.Context, client orgoAPI, identifier string) (orgoLease, error) {
	id := strings.TrimSpace(identifier)
	leaseID, slug := id, ""
	createdWorkspace := ""
	workspaceID := ""
	// Exact provider resource identity wins over friendly slug matching. This is
	// required for destructive operations when a slug happens to equal another
	// computer's ID.
	claim, ok, err := resolveLeaseClaimForProviderCloudID(id)
	if err != nil {
		return orgoLease{}, err
	}
	if !ok {
		claim, ok, err = resolveLeaseClaimForProvider(id)
		if err != nil {
			return orgoLease{}, err
		}
	}
	if ok {
		leaseID = claim.LeaseID
		slug = claim.Slug
		createdWorkspace = strings.TrimSpace(claim.Labels[orgoCreatedWorkspaceLabel])
		workspaceID = strings.TrimSpace(claim.Labels[orgoWorkspaceLabel])
		if strings.TrimSpace(claim.CloudID) != "" {
			id = claim.CloudID
		}
	}
	computer, err := client.GetComputer(ctx, id)
	if err != nil {
		return orgoLease{}, err
	}
	if computer.WorkspaceID == "" {
		computer.WorkspaceID = workspaceID
	}
	if slug == "" {
		slug = computer.Name
	}
	return orgoLease{LeaseID: leaseID, Slug: slug, Computer: computer, CreatedWorkspace: createdWorkspace}, nil
}

func (b *orgoBackend) deleteLease(ctx context.Context, client orgoAPI, lease orgoLease) error {
	var computerErr error
	if strings.TrimSpace(lease.Computer.ID) != "" {
		if err := client.DeleteComputer(ctx, lease.Computer.ID); err != nil {
			computerErr = err
		}
	}
	cleanupErr := computerErr
	if strings.TrimSpace(lease.CreatedWorkspace) != "" {
		if err := client.DeleteWorkspace(ctx, lease.CreatedWorkspace); err != nil {
			cleanupErr = errors.Join(computerErr, err)
		} else {
			// Deleting a Crabbox-created workspace cascades to its computers, so a
			// successful workspace delete supersedes an earlier computer error.
			cleanupErr = nil
		}
	}
	if cleanupErr == nil && strings.HasPrefix(lease.LeaseID, "cbx_") {
		removeLeaseClaim(lease.LeaseID)
	}
	return cleanupErr
}

func isOrgoNotFound(err error) bool {
	var exitErr ExitError
	return errors.As(err, &exitErr) && exitErr.Code == 4
}

func (b *orgoBackend) cleanupLease(client orgoAPI, lease orgoLease) error {
	ctx, cancel := context.WithTimeout(context.Background(), orgoCleanupTimeout)
	defer cancel()
	return b.deleteLease(ctx, client, lease)
}

func (b *orgoBackend) listComputers(ctx context.Context, client orgoAPI) ([]orgoComputer, error) {
	workspaceID := strings.TrimSpace(b.cfg.Orgo.WorkspaceID)
	if workspaceID != "" {
		workspace, err := client.GetWorkspace(ctx, workspaceID)
		if err != nil {
			return nil, err
		}
		return orgoComputersForWorkspace(workspace), nil
	}
	workspaces, err := client.ListWorkspaces(ctx)
	if err != nil {
		return nil, err
	}
	var computers []orgoComputer
	for _, workspace := range workspaces {
		if workspace.Computers == nil {
			full, err := client.GetWorkspace(ctx, workspace.ID)
			if err != nil {
				return nil, err
			}
			workspace = full
		}
		computers = append(computers, orgoComputersForWorkspace(workspace)...)
	}
	return computers, nil
}

func orgoComputersForWorkspace(workspace orgoWorkspace) []orgoComputer {
	computers := append([]orgoComputer(nil), workspace.Computers...)
	for i := range computers {
		if computers[i].WorkspaceID == "" {
			computers[i].WorkspaceID = workspace.ID
		}
	}
	return computers
}

func (b *orgoBackend) buildCommand(req RunRequest) (string, error) {
	command := orgoCommandText(req)
	if len(req.Env) == 0 {
		return command, nil
	}
	names := make([]string, 0, len(req.Env))
	for name := range req.Env {
		if !orgoEnvNamePattern.MatchString(name) {
			return "", exit(2, "provider=%s cannot forward invalid env var name %q", providerName, name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	var bld strings.Builder
	for _, name := range names {
		fmt.Fprintf(&bld, "export %s=%s\n", name, shellQuote(req.Env[name]))
	}
	bld.WriteString(command)
	return bld.String(), nil
}

func orgoCommandText(req RunRequest) string {
	if req.ShellMode {
		return strings.Join(req.Command, " ")
	}
	return shellScriptFromArgv(req.Command)
}

func (b *orgoBackend) rejectRunOptions(req RunRequest) error {
	if err := rejectDelegatedSyncOptionsForSpec(b.spec, req); err != nil {
		return err
	}
	if req.Options.Tailscale.Enabled {
		return exit(2, "provider=%s is delegated-run only and does not support Tailscale options", providerName)
	}
	if req.Options.Desktop || req.Options.Browser || req.Options.Code {
		return exit(2, "provider=%s does not support desktop, browser, or code-server options", providerName)
	}
	return nil
}

func orgoComputerServer(computer orgoComputer, claim LeaseClaim) Server {
	labels := map[string]string{
		"provider":         providerName,
		orgoWorkspaceLabel: computer.WorkspaceID,
		"target":           targetLinux,
	}
	if claim.LeaseID != "" {
		labels["lease"] = claim.LeaseID
	}
	if claim.Slug != "" {
		labels["slug"] = claim.Slug
	}
	for key, value := range claim.Labels {
		if value != "" {
			labels[key] = value
		}
	}
	server := Server{
		CloudID:  computer.ID,
		Provider: providerName,
		Name:     blank(computer.Name, computer.ID),
		Status:   normalizeOrgoStatus(computer.Status),
		Labels:   labels,
	}
	server.ServerType.Name = "orgo-computer"
	if strings.TrimSpace(computer.ConnectionURL) != "" {
		server.PublicNet.IPv4.IP = computer.ConnectionURL
	} else {
		server.PublicNet.IPv4.IP = computer.Hostname
	}
	return server
}

func orgoStatusView(lease orgoLease) StatusView {
	state := normalizeOrgoStatus(lease.Computer.Status)
	return StatusView{
		ID:         blank(lease.LeaseID, lease.Computer.ID),
		Slug:       lease.Slug,
		Provider:   providerName,
		TargetOS:   targetLinux,
		State:      state,
		ServerID:   lease.Computer.ID,
		ServerType: "orgo-computer",
		Host:       blank(lease.Computer.ConnectionURL, lease.Computer.Hostname),
		Network:    networkPublic,
		Ready:      state == "running",
		Labels: map[string]string{
			orgoWorkspaceLabel: lease.Computer.WorkspaceID,
			"orgo_instance_id": lease.Computer.InstanceID,
		},
	}
}

func normalizeOrgoStatus(status string) string {
	status = strings.ToLower(strings.TrimSpace(status))
	if status == "" {
		return "unknown"
	}
	return status
}

func applyOrgoDefaults(cfg *Config) {
	cfg.Provider = providerName
	if cfg.TargetOS == "" {
		cfg.TargetOS = targetLinux
	}
	if strings.TrimSpace(cfg.Orgo.APIBase) == "" {
		cfg.Orgo.APIBase = defaultAPIBase
	}
	if cfg.Orgo.RAMGB <= 0 {
		cfg.Orgo.RAMGB = 4
	}
	if cfg.Orgo.CPUs <= 0 {
		cfg.Orgo.CPUs = 1
	}
	if cfg.Orgo.DiskGB <= 0 {
		cfg.Orgo.DiskGB = 8
	}
	if strings.TrimSpace(cfg.Orgo.Resolution) == "" {
		cfg.Orgo.Resolution = "1280x720x24"
	}
}

func (b *orgoBackend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now()
	}
	return time.Now()
}
