package smolvm

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

type backend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

func NewBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = providerName
	return &backend{spec: spec, cfg: cfg, rt: rt}
}

func (b *backend) Spec() ProviderSpec { return b.spec }

func (b *backend) Warmup(ctx context.Context, req WarmupRequest) error {
	if req.ActionsRunner {
		return exit(2, "--actions-runner is not supported for provider=%s", providerName)
	}
	started := b.now()
	client, err := newAPI(b.cfg, b.rt)
	if err != nil {
		return err
	}
	leaseID, machine, slug, err := b.createMachine(ctx, client, req.Repo, true, req.Reclaim, req.RequestedSlug)
	if err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s machine=%s name=%s\n", leaseID, slug, providerName, machine.ID, machine.Name)
	if !req.Keep {
		fmt.Fprintf(b.rt.Stderr, "warning: smolvm warmup keeps the machine until explicit stop\n")
	}
	total := b.now().Sub(started)
	fmt.Fprintf(b.rt.Stdout, "warmup complete total=%s\n", total.Round(time.Millisecond))
	if req.TimingJSON {
		return writeTimingJSON(b.rt.Stderr, timingReport{
			Provider: providerName,
			LeaseID:  leaseID,
			Slug:     slug,
			TotalMs:  total.Milliseconds(),
			ExitCode: 0,
		})
	}
	return nil
}

func (b *backend) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	workdir, err := cleanWorkdir(workdir(b.cfg))
	if err != nil {
		return RunResult{}, err
	}
	folder, err := workspaceFolder(workdir)
	if err != nil {
		return RunResult{}, err
	}
	started := b.now()
	client, err := newAPI(b.cfg, b.rt)
	if err != nil {
		return RunResult{}, err
	}
	effectiveKeep := req.Keep || b.cfg.Smolvm.Keep
	leaseID, machineID, slug := "", "", ""
	acquired := false
	if req.ID == "" {
		var machine machineData
		leaseID, machine, slug, err = b.createMachine(ctx, client, req.Repo, effectiveKeep, req.Reclaim, req.RequestedSlug)
		if err != nil {
			return RunResult{}, err
		}
		machineID = machine.ID
		fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=%s machine=%s name=%s\n", leaseID, slug, providerName, machine.ID, machine.Name)
		acquired = true
	} else {
		leaseID, machineID, slug, err = b.resolveMachineID(ctx, client, req.ID, req.Repo.Root, req.Reclaim)
		if err != nil {
			return RunResult{}, err
		}
	}
	shouldStop := acquired && !effectiveKeep
	session := &RunSessionHandle{
		Provider:       providerName,
		LeaseID:        leaseID,
		Slug:           slug,
		Reused:         !acquired,
		Kept:           !shouldStop,
		CleanupCommand: smolvmCleanupCommand(leaseID),
	}
	if shouldStop {
		defer func() {
			if !shouldStop {
				session.Kept = true
				return
			}
			if err := client.DeleteMachine(context.Background(), machineID); err != nil {
				fmt.Fprintf(b.rt.Stderr, "warning: smolvm delete failed for %s: %v\n", machineID, err)
				session.Kept = true
				return
			}
			removeLeaseClaim(leaseID)
			session.Kept = false
		}()
	}

	syncDuration := time.Duration(0)
	syncPhases := []timingPhase{{Name: "sync", Skipped: true, Reason: "--no-sync"}}
	if !req.NoSync {
		syncPhases, syncDuration, err = b.syncWorkspace(ctx, client, machineID, req, workdir, folder)
		if err != nil {
			return RunResult{Provider: providerName, LeaseID: leaseID, Slug: slug, Total: b.now().Sub(started), SyncDelegated: true, Session: session}, err
		}
		fmt.Fprintf(b.rt.Stderr, "sync complete in %s\n", syncDuration.Round(time.Millisecond))
	} else if err := b.prepareWorkspace(ctx, client, machineID, folder, false); err != nil {
		return RunResult{Provider: providerName, LeaseID: leaseID, Slug: slug, Total: b.now().Sub(started), SyncDelegated: true, Session: session}, err
	}
	if req.SyncOnly {
		result := RunResult{Provider: providerName, LeaseID: leaseID, Slug: slug, Total: b.now().Sub(started), SyncDelegated: true, Session: session}
		fmt.Fprintf(b.rt.Stdout, "synced %s\n", workdir)
		if req.TimingJSON {
			err := writeTimingJSON(b.rt.Stderr, timingReportWithRunResult(timingReport{
				Provider:      providerName,
				LeaseID:       leaseID,
				Slug:          slug,
				SyncDelegated: true,
				SyncMs:        syncDuration.Milliseconds(),
				SyncPhases:    syncPhases,
				SyncSkipped:   req.NoSync,
				TotalMs:       result.Total.Milliseconds(),
				ExitCode:      0,
				Label:         strings.TrimSpace(req.Label),
			}, result, nil))
			return result, err
		}
		return result, nil
	}

	command, err := buildCommand(req.Command, req.ShellMode)
	if err != nil {
		return RunResult{Provider: providerName, LeaseID: leaseID, Slug: slug, Total: b.now().Sub(started), SyncDelegated: true, Session: session}, err
	}
	if req.EnvSummary {
		printEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, req.Env)
	}
	if len(req.Env) > 0 {
		envPath := path.Join(workdir, ".crabbox-env-"+leaseID+".sh")
		if err := client.WriteFile(ctx, machineID, envPath, shellEnvProfile(req.Env)); err != nil {
			return RunResult{Provider: providerName, LeaseID: leaseID, Slug: slug, Total: b.now().Sub(started), SyncDelegated: true, Session: session}, err
		}
		defer func() {
			_, _ = client.Exec(context.Background(), machineID, "rm -f "+shellQuote(envPath), "")
		}()
		command = ". " + shellQuote(envPath) + " && " + command
	}
	commandStarted := b.now()
	exitCode, commandErr := client.ExecStream(ctx, machineID, command, folder, b.rt.Stdout)
	commandDuration := b.now().Sub(commandStarted)
	result := RunResult{
		ExitCode:      exitCode,
		Command:       commandDuration,
		Total:         b.now().Sub(started),
		SyncDelegated: true,
		Provider:      providerName,
		LeaseID:       leaseID,
		Slug:          slug,
		CommandText:   strings.Join(req.Command, " "),
		Session:       session,
	}
	if req.NoSync {
		fmt.Fprintf(b.rt.Stderr, "smolvm run summary sync_skipped=true command=%s total=%s exit=%d\n", result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), exitCode)
	} else {
		fmt.Fprintf(b.rt.Stderr, "smolvm run summary sync=%s command=%s total=%s exit=%d\n", syncDuration.Round(time.Millisecond), result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), exitCode)
	}
	if req.TimingJSON {
		if err := writeTimingJSON(b.rt.Stderr, timingReportWithRunResult(timingReport{
			Provider:      providerName,
			LeaseID:       leaseID,
			Slug:          slug,
			SyncDelegated: true,
			SyncMs:        syncDuration.Milliseconds(),
			SyncPhases:    syncPhases,
			SyncSkipped:   req.NoSync,
			CommandMs:     commandDuration.Milliseconds(),
			TotalMs:       result.Total.Milliseconds(),
			ExitCode:      exitCode,
			Label:         strings.TrimSpace(req.Label),
		}, result, commandErr)); err != nil {
			return result, err
		}
	}
	if commandErr != nil {
		failureReq := req
		failureReq.Keep = effectiveKeep
		handleDelegatedRunFailure(b.rt.Stderr, failureReq, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return result, ExitError{Code: 1, Message: fmt.Sprintf("smolvm run failed: %v", commandErr)}
	}
	if exitCode != 0 {
		failureReq := req
		failureReq.Keep = effectiveKeep
		handleDelegatedRunFailure(b.rt.Stderr, failureReq, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return result, ExitError{Code: exitCode, Message: fmt.Sprintf("smolvm run exited %d", exitCode)}
	}
	return result, nil
}

func smolvmCleanupCommand(leaseID string) string {
	return "crabbox stop --provider " + providerName + " --id " + shellQuote(leaseID)
}

func (b *backend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	_ = req
	client, err := newAPI(b.cfg, b.rt)
	if err != nil {
		return nil, err
	}
	machines, err := client.ListMachines(ctx)
	if err != nil {
		return nil, err
	}
	servers := make([]Server, 0, len(machines))
	for _, m := range machines {
		if isCrabboxMachine(m) {
			servers = append(servers, machineToServer(b.cfg, m))
		}
	}
	return servers, nil
}

func (b *backend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	servers, err := b.List(ctx, ListRequest{})
	if err != nil {
		return DoctorResult{}, err
	}
	return inventoryDoctorResult(providerName, len(servers)), nil
}

func (b *backend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	client, err := newAPI(b.cfg, b.rt)
	if err != nil {
		return StatusView{}, err
	}
	leaseID, machineID, slug, err := b.resolveMachineID(ctx, client, req.ID, "", false)
	if err != nil {
		return StatusView{}, err
	}
	deadline := b.now().Add(req.WaitTimeout)
	if req.WaitTimeout <= 0 {
		deadline = b.now().Add(5 * time.Minute)
	}
	for {
		machine, err := client.GetMachine(ctx, machineID)
		if err != nil {
			return StatusView{}, err
		}
		server := machineToServer(b.cfg, machine)
		view := StatusView{
			ID:         leaseID,
			Slug:       blank(slug, server.Labels["slug"]),
			Provider:   providerName,
			TargetOS:   targetLinux,
			State:      machine.State,
			ServerID:   machine.ID,
			ServerType: server.ServerType.Name,
			Network:    networkPublic,
			Ready:      statusReady(machine.State),
			Labels:     server.Labels,
		}
		if !req.Wait || view.Ready {
			return view, nil
		}
		if b.now().After(deadline) {
			return StatusView{}, exit(5, "timed out waiting for smolvm %s to become ready", machineID)
		}
		select {
		case <-ctx.Done():
			return StatusView{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (b *backend) Stop(ctx context.Context, req StopRequest) error {
	client, err := newAPI(b.cfg, b.rt)
	if err != nil {
		return err
	}
	leaseID, machineID, _, err := b.resolveMachineID(ctx, client, req.ID, "", false)
	if err != nil {
		return err
	}
	if err := client.DeleteMachine(ctx, machineID); err != nil {
		return err
	}
	removeLeaseClaim(leaseID)
	fmt.Fprintf(b.rt.Stderr, "released lease=%s machine=%s\n", leaseID, machineID)
	return nil
}

func (b *backend) createMachine(ctx context.Context, client api, repo Repo, keep, reclaim bool, requestedSlug string) (string, machineData, string, error) {
	leaseID := newLeaseID()
	slug, err := allocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return "", machineData{}, "", err
	}
	name := machineName(leaseID, slug)
	cpus := cpusValue(b.cfg)
	mem := memoryValue(b.cfg)
	netMode := networkMode(b.cfg)
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s name=%s image=%s cpus=%d memory_mb=%d network=%s keep=%t\n", providerName, leaseID, slug, name, imageName(b.cfg), cpus, mem, netMode, keep)
	creq := createRequest{
		Name: name,
		Source: smolvmMachineSource{
			Type:      "image",
			Reference: imageName(b.cfg),
		},
		Resources: smolvmMachineResources{
			CPUs:     cpus,
			MemoryMB: mem,
		},
		Network: &smolvmMachineNetwork{
			Mode: netMode,
		},
		Workdir: workdir(b.cfg),
	}
	if !keep {
		creq.Ephemeral = true
		creq.TTLSeconds = 3600 // reasonable default; backend stop will delete anyway
	}
	machine, err := client.CreateMachine(ctx, creq)
	if err != nil {
		return "", machineData{}, "", err
	}
	// Machines are created stopped; start explicitly for delegated run/warmup.
	if err := client.StartMachine(ctx, machine.ID); err != nil {
		_ = client.DeleteMachine(context.Background(), machine.ID)
		return "", machineData{}, "", fmt.Errorf("smolvm start %s: %w", machine.ID, err)
	}
	// Poll until ready (started/running etc).
	deadline := time.Now().Add(5 * time.Minute)
	for {
		st := strings.ToLower(strings.TrimSpace(machine.State))
		if createStatusReady(st) {
			break
		}
		if st == "error" || st == "failed" || st == "erroring" {
			_ = client.DeleteMachine(context.Background(), machine.ID)
			return "", machineData{}, "", exit(5, "smolvm machine failed for %s status=%s", machine.ID, machine.State)
		}
		if time.Now().After(deadline) {
			_ = client.DeleteMachine(context.Background(), machine.ID)
			return "", machineData{}, "", exit(5, "smolvm start timed out for %s status=%s", machine.ID, machine.State)
		}
		select {
		case <-ctx.Done():
			_ = client.DeleteMachine(context.Background(), machine.ID)
			return "", machineData{}, "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
		next, err := client.GetMachine(ctx, machine.ID)
		if err == nil {
			machine = next
		}
	}
	if err := claimLeaseForRepoProvider(leaseID, slug, providerName, repo.Root, b.cfg.IdleTimeout, reclaim); err != nil {
		_ = client.DeleteMachine(context.Background(), machine.ID)
		return "", machineData{}, "", err
	}
	return leaseID, machine, slug, nil
}

func (b *backend) resolveMachineID(ctx context.Context, client api, id, repoRoot string, reclaim bool) (string, string, string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", "", "", exit(2, "provider=%s requires a Crabbox lease id, slug, or smolvm machine id/name", providerName)
	}
	if claim, ok, err := resolveLeaseClaim(id); err != nil {
		return "", "", "", err
	} else if ok && claim.Provider == providerName {
		if repoRoot != "" {
			if err := claimLeaseForRepoProvider(claim.LeaseID, claim.Slug, providerName, repoRoot, time.Duration(claim.IdleTimeoutSeconds)*time.Second, reclaim); err != nil {
				return "", "", "", err
			}
		}
		machine, err := resolveMachineByLease(ctx, client, claim.LeaseID)
		if err != nil {
			return "", "", "", err
		}
		return claim.LeaseID, machine.ID, claim.Slug, nil
	}
	if strings.HasPrefix(id, "cbx_") {
		machine, err := resolveMachineByLease(ctx, client, id)
		if err != nil {
			return "", "", "", err
		}
		return b.finishResolvedMachine(machine, repoRoot, reclaim)
	}
	if machine, err := client.GetMachine(ctx, id); err == nil && isCrabboxMachine(machine) {
		return b.finishResolvedMachine(machine, repoRoot, reclaim)
	} else if err != nil && !isNotFound(err) {
		return "", "", "", err
	}
	// try by slug or direct name
	machine, err := resolveMachineBySlug(ctx, client, id)
	if err != nil {
		// last try: treat id as the machine name/id directly
		m, gerr := client.GetMachine(ctx, id)
		if gerr != nil || !isCrabboxMachine(m) {
			return "", "", "", err
		}
		return b.finishResolvedMachine(m, repoRoot, reclaim)
	}
	return b.finishResolvedMachine(machine, repoRoot, reclaim)
}

func (b *backend) finishResolvedMachine(machine machineData, repoRoot string, reclaim bool) (string, string, string, error) {
	leaseID := machineLeaseID(machine)
	slug := machineSlug(leaseID, machine)
	if repoRoot != "" {
		if err := claimLeaseForRepoProvider(leaseID, slug, providerName, repoRoot, b.cfg.IdleTimeout, reclaim); err != nil {
			return "", "", "", err
		}
	}
	return leaseID, machine.ID, slug, nil
}

func resolveMachineByLease(ctx context.Context, client api, leaseID string) (machineData, error) {
	machines, err := client.ListMachines(ctx)
	if err != nil {
		return machineData{}, err
	}
	for _, m := range machines {
		if isCrabboxMachine(m) && machineLeaseID(m) == leaseID {
			return m, nil
		}
	}
	return machineData{}, exit(4, "smolvm lease %q was not found", leaseID)
}

func resolveMachineBySlug(ctx context.Context, client api, slug string) (machineData, error) {
	machines, err := client.ListMachines(ctx)
	if err != nil {
		return machineData{}, err
	}
	for _, m := range machines {
		if isCrabboxMachine(m) && machineSlug(machineLeaseID(m), m) == slug {
			return m, nil
		}
	}
	return machineData{}, exit(4, "smolvm %q was not found", slug)
}

func (b *backend) now() time.Time {
	return now(b.rt)
}

func machineToServer(cfg Config, m machineData) Server {
	leaseID := machineLeaseID(m)
	labels := directLeaseLabels(cfg, leaseID, machineSlug(leaseID, m), providerName, "", cfg.Smolvm.Keep, time.Now().UTC())
	labels["machine_id"] = m.ID
	labels["machine_name"] = m.Name
	labels["image"] = blank(m.Source.Reference, imageName(cfg))
	if m.Resources.CPUs > 0 {
		labels["cpus"] = fmt.Sprintf("%d", m.Resources.CPUs)
	}
	if m.Resources.MemoryMB > 0 {
		labels["memory_mb"] = fmt.Sprintf("%d", m.Resources.MemoryMB)
	}
	labels["state"] = m.State
	server := Server{
		Provider: providerName,
		CloudID:  m.ID,
		Name:     blank(m.Name, m.ID),
		Status:   m.State,
		Labels:   labels,
	}
	server.ServerType.Name = fmt.Sprintf("smolvm-%d-%d", cpusValue(cfg), memoryValue(cfg))
	server.PublicNet.IPv4.IP = machineBaseHost(cfg)
	return server
}

func machineBaseHost(cfg Config) string {
	raw := blank(strings.TrimSpace(cfg.Smolvm.BaseURL), "https://api.smolmachines.com")
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return raw
	}
	return parsed.Host
}

var machineNamePattern = regexp.MustCompile(`^crabbox-(.+)-([0-9a-f]{12})$`)

func isCrabboxMachine(m machineData) bool {
	return machineNamePattern.MatchString(strings.TrimSpace(m.Name))
}

func machineLeaseID(m machineData) string {
	if match := machineNamePattern.FindStringSubmatch(strings.TrimSpace(m.Name)); len(match) == 3 {
		return "cbx_" + match[2]
	}
	return "smolvm_" + m.ID
}

func machineSlug(leaseID string, m machineData) string {
	if match := machineNamePattern.FindStringSubmatch(strings.TrimSpace(m.Name)); len(match) == 3 {
		return match[1]
	}
	return newLeaseSlug(leaseID)
}

func statusReady(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", "running", "ready", "idle", "active", "started", "paused":
		return true
	default:
		return false
	}
}

func imageName(cfg Config) string {
	return blank(strings.TrimSpace(cfg.Smolvm.Image), "alpine")
}

func machineName(leaseID, slug string) string {
	slug = strings.Trim(strings.ToLower(strings.TrimSpace(slug)), "-")
	if slug == "" {
		slug = newLeaseSlug(leaseID)
	}
	return "crabbox-" + slug + "-" + strings.TrimPrefix(leaseID, "cbx_")
}

func cpusValue(cfg Config) int {
	if cfg.Smolvm.CPUs > 0 {
		return cfg.Smolvm.CPUs
	}
	return 2
}

func memoryValue(cfg Config) int {
	if cfg.Smolvm.MemoryMB > 0 {
		return cfg.Smolvm.MemoryMB
	}
	return 2048
}

func networkMode(cfg Config) string {
	n := strings.ToLower(strings.TrimSpace(cfg.Smolvm.Network))
	if n == "" {
		return "blocked"
	}
	if n == "open" || n == "public" {
		return "open"
	}
	return "blocked"
}

func workdir(cfg Config) string {
	return blank(strings.TrimSpace(cfg.Smolvm.Workdir), "/workspace")
}

func cleanWorkdir(workdir string) (string, error) {
	trimmed := strings.TrimSpace(workdir)
	if trimmed == "" {
		return "", exit(2, "smolvm workdir is empty")
	}
	clean := path.Clean(trimmed)
	if !strings.HasPrefix(clean, "/") {
		return "", exit(2, "smolvm workdir %q must resolve to an absolute path", workdir)
	}
	switch clean {
	case "/", "/bin", "/dev", "/etc", "/home", "/lib", "/lib64", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var":
		return "", exit(2, "smolvm workdir %q is too broad; choose a dedicated subdirectory", clean)
	}
	return clean, nil
}

func buildCommand(command []string, shellMode bool) (string, error) {
	if len(command) == 0 {
		return "", errors.New("missing command")
	}
	var script string
	if shellMode {
		script = strings.Join(command, " ")
	} else if shouldUseShell(command) || leadingEnvAssignment(command) {
		script = shellScriptFromArgv(command)
	} else {
		script = "exec " + strings.Join(shellWords(command), " ")
	}
	return script, nil
}

const workspaceRoot = "/workspace"

func workspaceFolder(workdir string) (string, error) {
	clean, err := cleanWorkdir(workdir)
	if err != nil {
		return "", err
	}
	prefix := workspaceRoot + "/"
	if !strings.HasPrefix(clean, prefix) {
		// allow exact /workspace too; return absolute for reliable mkdir/exec workdir across API calls
		if clean == workspaceRoot {
			return "/workspace", nil
		}
		return "", exit(2, "smolvm workdir %q must be under %s or exactly %s", clean, workspaceRoot, workspaceRoot)
	}
	return clean, nil
}

func shellEnvProfile(env map[string]string) string {
	var b strings.Builder
	keys := make([]string, 0, len(env))
	for key := range env {
		if !validEnvName(key) {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	b.WriteString("set -a\n")
	for _, key := range keys {
		b.WriteString(key)
		b.WriteString("=")
		b.WriteString(shellQuote(env[key]))
		b.WriteByte('\n')
	}
	b.WriteString("set +a\n")
	return b.String()
}

func validEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}
