package smolvm

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
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
	claim, machine, err := b.createMachine(ctx, client, req.Repo, true, req.RequestedSlug)
	if err != nil {
		return err
	}
	leaseID, slug := claim.LeaseID, claim.Slug
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
	effectiveKeep := req.Keep || b.cfg.Smolvm.Keep
	lifecycleReq := req
	lifecycleReq.Keep = effectiveKeep
	var client api
	var claim core.LeaseClaim
	session := func() shared.DelegatedSandbox {
		return shared.DelegatedSandbox{
			LeaseID: claim.LeaseID, Slug: claim.Slug,
			CleanupCommand: smolvmCleanupCommand(claim.LeaseID),
		}
	}
	return shared.RunDelegatedSandbox(ctx, lifecycleReq, shared.DelegatedSandboxLifecycle{
		Provider: providerName, Runtime: b.rt, Workdir: workdir,
		IdleTimeout: b.cfg.IdleTimeout, TTL: b.cfg.TTL, CleanupTimeout: smolvmControlTimeout,
		Preflight: func(context.Context) error {
			var err error
			client, err = newAPI(b.cfg, b.rt)
			return err
		},
		PrepareArchive: func(ctx context.Context) (*core.PreparedArchive, error) { return b.prepareArchive(ctx, req) },
		Acquire: func(ctx context.Context) (shared.DelegatedSandbox, error) {
			var machine machineData
			var err error
			claim, machine, err = b.createMachine(ctx, client, req.Repo, effectiveKeep, req.RequestedSlug)
			if err != nil {
				return shared.DelegatedSandbox{}, err
			}
			fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=%s machine=%s name=%s\n", claim.LeaseID, claim.Slug, providerName, machine.ID, machine.Name)
			return session(), nil
		},
		Resolve: func(ctx context.Context) (shared.DelegatedSandbox, error) {
			var err error
			claim, err = b.reuseMachine(ctx, client, req.ID, req.Repo.Root, req.Reclaim)
			if err != nil {
				return shared.DelegatedSandbox{}, err
			}
			return session(), nil
		},
		Sync: func(ctx context.Context, prepared *core.PreparedArchive) ([]core.TimingPhase, time.Duration, error) {
			return b.syncWorkspace(ctx, client, claim.CloudID, req, folder, prepared)
		},
		NoSync: func(ctx context.Context) error { return b.prepareWorkspace(ctx, client, claim.CloudID, folder, false) },
		Command: func(ctx context.Context) (shared.DelegatedSandboxCommand, error) {
			intent, err := core.ParseCommandIntent(req.Command, req.ShellMode, req.CommandLiteralArgs)
			if err != nil {
				return shared.DelegatedSandboxCommand{}, err
			}
			command := intent.ShellSource()
			if req.EnvSummary {
				printEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, req.Env)
			}
			var closeCommand func(context.Context) error
			if len(req.Env) > 0 {
				envPath, cleanup, err := b.uploadEnvProfile(ctx, client, claim, req.Env, workdir)
				if cleanup != nil {
					closeCommand = func(ctx context.Context) error {
						// Profile cleanup has its own shorter budget and remains best-effort.
						cleanupCtx, cancel := context.WithTimeout(ctx, envProfileCleanupTimeout)
						defer cancel()
						cleanup(cleanupCtx)
						return nil
					}
				}
				if err != nil {
					return shared.DelegatedSandboxCommand{Close: closeCommand}, err
				}
				command = shared.ShellScriptWithEnvProfile(command, envPath)
			}
			return shared.DelegatedSandboxCommand{
				Text:  strings.Join(req.Command, " "),
				Close: closeCommand,
				Run: func(ctx context.Context) (int, error) {
					return client.ExecStream(ctx, claim.CloudID, command, folder, b.rt.Stdout)
				},
			}, nil
		},
		Cleanup: func(ctx context.Context) error { return b.deleteOwnedMachine(ctx, client, claim) },
	})
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
	return shared.PollDelegatedStatus(ctx, shared.DelegatedStatusRequest{
		ID:          req.ID,
		Provider:    providerName,
		TargetOS:    targetLinux,
		Network:     networkPublic,
		Wait:        req.Wait,
		WaitTimeout: req.WaitTimeout,
		Now:         b.now,
		Resolve: func(id string) (string, string, string, error) {
			return b.resolveMachineID(ctx, client, id)
		},
		Get: func(getCtx context.Context, machineID string) (shared.DelegatedStatusResource, error) {
			machine, err := client.GetMachine(getCtx, machineID)
			if err != nil {
				return shared.DelegatedStatusResource{}, err
			}
			server := machineToServer(b.cfg, machine)
			return shared.DelegatedStatusResource{
				State:      machine.State,
				ServerID:   machine.ID,
				ServerType: server.ServerType.Name,
				Ready:      statusReady(machine.State),
				Labels:     server.Labels,
			}, nil
		},
		TimeoutError: func(machineID string) error {
			return exit(5, "timed out waiting for smolvm %s to become ready", machineID)
		},
	})
}

func (b *backend) Stop(ctx context.Context, req StopRequest) error {
	client, err := newAPI(b.cfg, b.rt)
	if err != nil {
		return err
	}
	claim, err := b.resolveOwnedMachine(req.ID)
	if err != nil {
		return err
	}
	cleanupCtx, cancel := context.WithTimeout(ctx, smolvmControlTimeout)
	defer cancel()
	if err := b.deleteOwnedMachine(cleanupCtx, client, claim); err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stderr, "released lease=%s machine=%s\n", claim.LeaseID, claim.CloudID)
	return nil
}

func (b *backend) createMachine(ctx context.Context, client api, repo Repo, keep bool, requestedSlug string) (claim core.LeaseClaim, machine machineData, resultErr error) {
	leaseID := newLeaseID()
	slug, err := allocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return core.LeaseClaim{}, machineData{}, err
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
	machine, err = client.CreateMachine(ctx, creq)
	if err != nil {
		return core.LeaseClaim{}, machineData{}, err
	}
	original := machine
	if machine.Name != name || validateMachineIdentity(machine, machine) != nil {
		return core.LeaseClaim{}, machineData{}, exit(2, "smolvm create returned an incomplete or unexpected identity; retaining machine id=%q name=%q", machine.ID, machine.Name)
	}
	// Publish before start so failures retain an exact cleanup receipt. A failed
	// publication permits rollback only while the lease still has no claim.
	defer func() {
		if resultErr == nil {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), smolvmControlTimeout)
		defer cancel()
		var cleanupErr error
		if claim.LeaseID != "" {
			cleanupErr = b.deleteOwnedMachine(cleanupCtx, client, claim)
		} else {
			cleanupErr = core.CleanupLeaseClaimIfUnchangedAfter(leaseID, core.LeaseClaim{}, false, func() error {
				return deleteExactMachine(cleanupCtx, client, original)
			})
		}
		if cleanupErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("smolvm rollback retained machine=%s lease=%s: %w", original.ID, leaseID, cleanupErr))
		}
	}()
	claim, err = b.publishMachineClaim(leaseID, slug, original, repo)
	if err != nil {
		return claim, machine, err
	}
	if err := client.StartMachine(ctx, original.ID); err != nil {
		return claim, machine, fmt.Errorf("smolvm start %s: %w", original.ID, err)
	}
	deadline := time.Now().Add(5 * time.Minute)
	for {
		st := strings.ToLower(strings.TrimSpace(machine.State))
		if createStatusReady(st) {
			break
		}
		if st == "error" || st == "failed" || st == "erroring" {
			return claim, machine, exit(5, "smolvm machine failed for %s status=%s", original.ID, machine.State)
		}
		if time.Now().After(deadline) {
			return claim, machine, exit(5, "smolvm start timed out for %s status=%s", original.ID, machine.State)
		}
		select {
		case <-ctx.Done():
			return claim, machine, ctx.Err()
		case <-time.After(2 * time.Second):
		}
		next, err := client.GetMachine(ctx, original.ID)
		if err != nil {
			return claim, machine, err
		}
		if err := validateMachineIdentity(next, original); err != nil {
			return claim, machine, err
		}
		machine = next
	}
	return claim, machine, nil
}

func (b *backend) resolveMachineID(ctx context.Context, client api, id string) (string, string, string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", "", "", exit(2, "provider=%s requires a Crabbox lease id, slug, or smolvm machine id/name", providerName)
	}
	if claim, ok, err := resolveLeaseClaim(id); err != nil {
		return "", "", "", err
	} else if ok && claim.Provider == providerName {
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
		return finishResolvedMachine(machine)
	}
	if machine, err := client.GetMachine(ctx, id); err == nil && isCrabboxMachine(machine) {
		return finishResolvedMachine(machine)
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
		return finishResolvedMachine(m)
	}
	return finishResolvedMachine(machine)
}

func finishResolvedMachine(machine machineData) (string, string, string, error) {
	leaseID := machineLeaseID(machine)
	return leaseID, machine.ID, machineSlug(leaseID, machine), nil
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
