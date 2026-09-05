package codesandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type codeSandboxBackend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

func (b *codeSandboxBackend) Spec() ProviderSpec { return b.spec }

func (b *codeSandboxBackend) Warmup(ctx context.Context, req WarmupRequest) error {
	if req.ActionsRunner {
		return exit(2, "--actions-runner is not supported for provider=%s", providerName)
	}
	started := b.now()
	api, err := newCodeSandboxClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	leaseID, sandboxID, slug, err := b.createSandbox(ctx, api, req.Repo, req.Reclaim, req.RequestedSlug)
	if err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s sandbox=%s\n", leaseID, slug, providerName, sandboxID)
	if !req.Keep {
		fmt.Fprintf(b.rt.Stderr, "warning: codesandbox warmup keeps the sandbox until explicit stop\n")
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

func (b *codeSandboxBackend) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if err := delegatedSyncOptionsError(b.spec, req); err != nil {
		return RunResult{}, err
	}
	workdir, err := codeSandboxWorkdir(b.cfg)
	if err != nil {
		return RunResult{}, err
	}
	if !req.SyncOnly && (len(req.Command) == 0 || (len(req.Command) == 1 && strings.TrimSpace(req.Command[0]) == "")) {
		return RunResult{}, exit(2, "missing command")
	}
	var api codeSandboxAPI
	var leaseID, sandboxID, slug string
	boundSandbox := func() shared.DelegatedSandbox {
		fmt.Fprintf(b.rt.Stderr, "provider=%s lease=%s sandbox=%s workdir=%s\n", providerName, leaseID, sandboxID, workdir)
		return shared.DelegatedSandbox{LeaseID: leaseID, Slug: slug, CleanupCommand: codeSandboxCleanupCommand(leaseID)}
	}
	return shared.RunDelegatedSandbox(ctx, req, shared.DelegatedSandboxLifecycle{
		Provider: providerName, Runtime: b.rt, Workdir: workdir,
		IdleTimeout: b.cfg.IdleTimeout, TTL: b.cfg.TTL, CleanupTimeout: codeSandboxCleanupTimeout,
		Preflight: func(context.Context) error {
			var err error
			api, err = newCodeSandboxClient(b.cfg, b.rt)
			return err
		},
		PrepareArchive: func(ctx context.Context) (*core.PreparedArchive, error) {
			return core.PrepareDelegatedArchive(ctx, core.DelegatedArchivePreparationRequest{
				Config: b.cfg, Repo: req.Repo, ForceSyncLarge: req.ForceSyncLarge,
				TempPattern: "crabbox-codesandbox-sync-*.tgz", Stderr: b.rt.Stderr, Now: b.now,
			})
		},
		Acquire: func(ctx context.Context) (shared.DelegatedSandbox, error) {
			var err error
			leaseID, sandboxID, slug, err = b.createSandbox(ctx, api, req.Repo, req.Reclaim, req.RequestedSlug)
			if err != nil {
				return shared.DelegatedSandbox{}, err
			}
			fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=%s sandbox=%s\n", leaseID, slug, providerName, sandboxID)
			return boundSandbox(), nil
		},
		Resolve: func(ctx context.Context) (shared.DelegatedSandbox, error) {
			var claim LeaseClaim
			var err error
			leaseID, sandboxID, slug, claim, err = resolveLeaseID(req.ID)
			if err != nil {
				return shared.DelegatedSandbox{}, err
			}
			sb, err := api.GetSandbox(ctx, sandboxID)
			if err != nil {
				return shared.DelegatedSandbox{}, err
			}
			if err := validateCodeSandboxSandboxOwnership(claim, sb); err != nil {
				return shared.DelegatedSandbox{}, err
			}
			if req.Repo.Root != "" {
				if err := claimLeaseForRepoProviderScopePond(leaseID, slug, providerName, claim.ProviderScope, b.cfg.Pond, req.Repo.Root,
					timeoutOrDefault(b.cfg.IdleTimeout, time.Duration(claim.IdleTimeoutSeconds)*time.Second), req.Reclaim); err != nil {
					return shared.DelegatedSandbox{}, err
				}
			}
			return boundSandbox(), nil
		},
		Sync: func(ctx context.Context, prepared *core.PreparedArchive) ([]core.TimingPhase, time.Duration, error) {
			return b.syncWorkspace(ctx, api, sandboxID, req, workdir, prepared)
		},
		NoSync: func(ctx context.Context) error {
			return b.ensureWorkspace(ctx, api, sandboxID, workdir)
		},
		Command: func(context.Context) (shared.DelegatedSandboxCommand, error) {
			intent, err := core.ParseCommandIntent(req.Command, req.ShellMode, req.CommandLiteralArgs)
			if err != nil {
				return shared.DelegatedSandboxCommand{}, err
			}
			command := intent.Argv("bash", "-lc")
			if req.EnvSummary || strings.TrimSpace(os.Getenv("CRABBOX_ENV_ALLOW")) != "" {
				printEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, req.Env)
			}
			return shared.DelegatedSandboxCommand{
				Text: strings.Join(command, " "),
				Run: func(ctx context.Context) (int, error) {
					return b.execCommand(ctx, api, sandboxID, workdir, command, req.Env)
				},
			}, nil
		},
		Cleanup: func(ctx context.Context) error {
			if err := api.DeleteSandbox(ctx, sandboxID); err != nil {
				return err
			}
			removeLeaseClaim(leaseID)
			return nil
		},
	})
}

func (b *codeSandboxBackend) List(ctx context.Context, _ ListRequest) ([]LeaseView, error) {
	api, err := newCodeSandboxClient(b.cfg, b.rt)
	if err != nil {
		return nil, err
	}
	claims, err := listCodeSandboxLeaseClaims()
	if err != nil {
		return nil, err
	}
	servers := make([]Server, 0, len(claims))
	for _, claim := range claims {
		if claim.Provider != providerName || !strings.HasPrefix(claim.LeaseID, leasePrefix) {
			continue
		}
		if validateCodeSandboxClaimScope(claim) != nil {
			continue
		}
		sandboxID := strings.TrimPrefix(claim.LeaseID, leasePrefix)
		sb, getErr := api.GetSandbox(ctx, sandboxID)
		state := ""
		if getErr != nil {
			state = "missing-or-inaccessible"
		} else {
			if err := validateCodeSandboxSandboxOwnership(claim, sb); err != nil {
				return nil, err
			}
			state = blank(sb.State, "unknown")
		}
		servers = append(servers, codeSandboxServerView(claim, SandboxSummary{ID: sandboxID, Title: sb.Title, State: state, URL: sb.URL}))
	}
	return servers, nil
}

func (b *codeSandboxBackend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	api, err := newCodeSandboxClient(b.cfg, b.rt)
	if err != nil {
		return StatusView{}, err
	}
	leaseID, sandboxID, slug, claim, err := resolveLeaseID(req.ID)
	if err != nil {
		return StatusView{}, err
	}
	waitTimeout := req.WaitTimeout
	if waitTimeout <= 0 {
		waitTimeout = 5 * time.Minute
	}
	deadline := b.now().Add(waitTimeout)
	pollCtx := ctx
	cancel := func() {}
	if req.Wait {
		pollCtx, cancel = context.WithTimeout(ctx, waitTimeout)
	}
	defer cancel()
	for {
		sb, getErr := api.GetSandbox(pollCtx, sandboxID)
		if getErr != nil {
			if req.Wait && errors.Is(pollCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				return StatusView{}, exit(5, "timed out waiting for codesandbox sandbox %s to become ready", sandboxID)
			}
			if ctx.Err() != nil {
				return StatusView{}, ctx.Err()
			}
			return StatusView{}, getErr
		}
		if err := validateCodeSandboxSandboxOwnership(claim, sb); err != nil {
			return StatusView{}, err
		}
		state := strings.ToLower(strings.TrimSpace(blank(sb.State, "unknown")))
		view := StatusView{
			ID:       leaseID,
			Slug:     slug,
			Provider: providerName,
			TargetOS: targetLinux,
			State:    state,
			ServerID: sandboxID,
			Host:     sb.URL,
			Pond:     claim.Pond,
			Network:  NetworkPublic,
			Ready:    isReadyState(state),
			Labels: map[string]string{
				"provider": providerName,
				"lease":    leaseID,
				"pond":     claim.Pond,
				"state":    state,
			},
		}
		if !req.Wait || view.Ready {
			return view, nil
		}
		if isTerminalState(state) {
			return StatusView{}, exit(5, "codesandbox sandbox %s entered terminal state %q before becoming ready", sandboxID, state)
		}
		if b.now().After(deadline) {
			return StatusView{}, exit(5, "timed out waiting for codesandbox sandbox %s to become ready", sandboxID)
		}
		select {
		case <-pollCtx.Done():
			if errors.Is(pollCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				return StatusView{}, exit(5, "timed out waiting for codesandbox sandbox %s to become ready", sandboxID)
			}
			return StatusView{}, pollCtx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (b *codeSandboxBackend) Stop(ctx context.Context, req StopRequest) error {
	api, err := newCodeSandboxClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	leaseID, sandboxID, _, claim, err := resolveLeaseID(req.ID)
	if err != nil {
		return err
	}
	sb, err := api.GetSandbox(ctx, sandboxID)
	if err != nil {
		return err
	}
	if err := validateCodeSandboxSandboxOwnership(claim, sb); err != nil {
		return err
	}
	if err := api.DeleteSandbox(ctx, sandboxID); err != nil {
		return err
	}
	removeLeaseClaim(leaseID)
	fmt.Fprintf(b.rt.Stderr, "released lease=%s sandbox=%s\n", leaseID, sandboxID)
	return nil
}

func (b *codeSandboxBackend) Cleanup(ctx context.Context, req CleanupRequest) error {
	api, err := newCodeSandboxClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	claims, err := listCodeSandboxLeaseClaims()
	if err != nil {
		return err
	}
	now := b.now().UTC()
	checked := 0
	removed := 0
	for _, listed := range claims {
		if listed.Provider != providerName || !strings.HasPrefix(listed.LeaseID, leasePrefix) {
			continue
		}
		claim, err := readLeaseClaim(listed.LeaseID)
		if err != nil {
			return err
		}
		if claim.LeaseID == "" || claim.Provider != providerName || !strings.HasPrefix(claim.LeaseID, leasePrefix) {
			continue
		}
		if err := validateCodeSandboxClaimScope(claim); err != nil {
			return err
		}
		checked++
		due, reason := claimCleanupDue(claim, now)
		sandboxID := strings.TrimPrefix(claim.LeaseID, leasePrefix)
		if !due {
			fmt.Fprintf(b.rt.Stderr, "skip sandbox=%s lease=%s reason=%s\n", sandboxID, claim.LeaseID, reason)
			continue
		}
		sb, err := api.GetSandbox(ctx, sandboxID)
		if err != nil {
			return err
		}
		if err := validateCodeSandboxSandboxOwnership(claim, sb); err != nil {
			return err
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would delete sandbox=%s lease=%s reason=%s\n", sandboxID, claim.LeaseID, reason)
			continue
		}
		if err := api.DeleteSandbox(ctx, sandboxID); err != nil {
			return err
		}
		if err := removeLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
			return err
		}
		fmt.Fprintf(b.rt.Stdout, "delete sandbox=%s lease=%s reason=%s\n", sandboxID, claim.LeaseID, reason)
		removed++
	}
	if !req.DryRun {
		fmt.Fprintf(b.rt.Stdout, "%s cleanup removed=%d checked=%d\n", providerName, removed, checked)
	}
	return nil
}

func (b *codeSandboxBackend) Pause(ctx context.Context, req PauseRequest) error {
	api, err := newCodeSandboxClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	leaseID, sandboxID, _, claim, err := resolveLeaseID(req.ID)
	if err != nil {
		return err
	}
	sb, err := api.GetSandbox(ctx, sandboxID)
	if err != nil {
		return err
	}
	if err := validateCodeSandboxSandboxOwnership(claim, sb); err != nil {
		return err
	}
	if err := api.HibernateSandbox(ctx, sandboxID); err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stderr, "paused lease=%s sandbox=%s\n", leaseID, sandboxID)
	return nil
}

func (b *codeSandboxBackend) Resume(ctx context.Context, req ResumeRequest) error {
	api, err := newCodeSandboxClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	leaseID, sandboxID, _, claim, err := resolveLeaseID(req.ID)
	if err != nil {
		return err
	}
	sb, err := api.GetSandbox(ctx, sandboxID)
	if err != nil {
		return err
	}
	if err := validateCodeSandboxSandboxOwnership(claim, sb); err != nil {
		return err
	}
	resumed, err := api.ResumeSandbox(ctx, sandboxID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(resumed.ID) != "" && strings.TrimSpace(resumed.ID) != sandboxID {
		return exit(4, "codesandbox resumed sandbox %q does not match local claim %q", resumed.ID, claim.LeaseID)
	}
	fmt.Fprintf(b.rt.Stderr, "resumed lease=%s sandbox=%s\n", leaseID, sandboxID)
	return nil
}

func (b *codeSandboxBackend) Ports(ctx context.Context, req PortsRequest) (string, error) {
	if len(req.Unpublish) > 0 {
		return "", exit(2, "provider=codesandbox does not support ports --unpublish; stop the process inside the sandbox instead")
	}
	api, err := newCodeSandboxClient(b.cfg, b.rt)
	if err != nil {
		return "", err
	}
	_, sandboxID, _, claim, err := resolveLeaseID(req.ID)
	if err != nil {
		return "", err
	}
	sb, err := api.GetSandbox(ctx, sandboxID)
	if err != nil {
		return "", err
	}
	if err := validateCodeSandboxSandboxOwnership(claim, sb); err != nil {
		return "", err
	}
	ports := make([]PortInfo, 0, len(req.Publish))
	if len(req.Publish) == 0 {
		ports, err = api.ListPorts(ctx, sandboxID)
		if err != nil {
			return "", err
		}
	} else {
		for _, spec := range req.Publish {
			port, err := parseCodeSandboxPortSpec(spec)
			if err != nil {
				return "", err
			}
			info, err := api.WaitForPortURL(ctx, sandboxID, port)
			if err != nil {
				return "", err
			}
			ports = append(ports, info)
		}
	}
	if req.JSON {
		data, err := json.Marshal(ports)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	lines := make([]string, 0, len(ports))
	for _, port := range ports {
		host := strings.TrimSpace(port.Host)
		if host == "" {
			host = strings.TrimSpace(port.URL)
		}
		if host == "" {
			host = "-"
		}
		lines = append(lines, fmt.Sprintf("%d %s", port.Port, host))
	}
	return strings.Join(lines, "\n"), nil
}

func (b *codeSandboxBackend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	token, source, ok := authFromEnv()
	if !ok {
		return DoctorResult{}, missingAuthError{}
	}
	client, err := newCodeSandboxClient(b.cfg, b.rt)
	if err != nil {
		return DoctorResult{}, err
	}
	result := DoctorResult{
		Provider: providerName,
		Checks: []DoctorCheck{
			doctorCheck("codesandbox_auth", nil, map[string]string{
				"source":   source,
				"redacted": "true",
			}),
		},
	}
	listed, err := client.ListSandboxes(ctx, ListSandboxesRequest{Limit: doctorListLimit(b.cfg.CodeSandbox)})
	if err != nil {
		err = fmt.Errorf("%s", redactToken(err.Error(), token))
		result.Checks = append(result.Checks, doctorCheck("codesandbox_sandbox_list", err, map[string]string{
			"mutation": "false",
			"limit":    fmt.Sprint(doctorListLimit(b.cfg.CodeSandbox)),
		}))
		result.Status = "error"
		result.Message = "auth=ready control_plane=blocked inventory=blocked api=list mutation=false"
		return result, err
	}
	result.Checks = append(result.Checks, doctorCheck("codesandbox_sandbox_list", nil, map[string]string{
		"mutation":   "false",
		"limit":      fmt.Sprint(doctorListLimit(b.cfg.CodeSandbox)),
		"totalCount": fmt.Sprint(listed.TotalCount),
	}))
	result.Status = "ok"
	result.Message = inventoryDoctorResult(providerName, len(listed.Sandboxes)).Message
	return result, nil
}

func (b *codeSandboxBackend) execCommand(ctx context.Context, api codeSandboxAPI, sandboxID, workdir string, command []string, env map[string]string) (int, error) {
	if len(command) == 0 {
		return 2, errors.New("missing command")
	}
	res, err := api.RunCommand(ctx, sandboxID, CommandRequest{
		Command: command,
		Cwd:     workdir,
		Env:     env,
		Timeout: b.execTimeoutSecs(),
	})
	if err != nil {
		return 1, err
	}
	if res.Stdout != "" {
		_, _ = io.WriteString(b.rt.Stdout, res.Stdout)
	}
	if res.Stderr != "" {
		_, _ = io.WriteString(b.rt.Stderr, res.Stderr)
	}
	return res.ExitCode, nil
}

func codeSandboxServerView(claim LeaseClaim, sb SandboxSummary) Server {
	state := blank(sb.State, "unknown")
	sandboxID := strings.TrimPrefix(claim.LeaseID, leasePrefix)
	if strings.TrimSpace(sb.ID) != "" {
		sandboxID = sb.ID
	}
	name := blank(sb.Title, sandboxID)
	return Server{
		Provider: providerName,
		CloudID:  sandboxID,
		Name:     name,
		Status:   state,
		Labels: map[string]string{
			"provider": providerName,
			"lease":    claim.LeaseID,
			"slug":     claim.Slug,
			"pond":     claim.Pond,
			"target":   targetLinux,
			"state":    state,
		},
	}
}

func timeoutOrDefault(primary, fallback time.Duration) time.Duration {
	if primary > 0 {
		return primary
	}
	return fallback
}

var _ interface {
	Warmup(context.Context, WarmupRequest) error
	Run(context.Context, RunRequest) (RunResult, error)
	List(context.Context, ListRequest) ([]LeaseView, error)
	Status(context.Context, StatusRequest) (StatusView, error)
	Stop(context.Context, StopRequest) error
	Cleanup(context.Context, CleanupRequest) error
	Pause(context.Context, PauseRequest) error
	Resume(context.Context, ResumeRequest) error
	Ports(context.Context, PortsRequest) (string, error)
	Doctor(context.Context, DoctorRequest) (DoctorResult, error)
} = (*codeSandboxBackend)(nil)

func parseCodeSandboxPortSpec(spec string) (int, error) {
	value := strings.TrimSpace(spec)
	if value == "" {
		return 0, exit(2, "codesandbox port spec must not be empty")
	}
	if strings.ContainsAny(value, ":/") {
		return 0, exit(2, "codesandbox ports only support a sandbox port number, got %q", spec)
	}
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, exit(2, "codesandbox port must be an integer between 1 and 65535, got %q", spec)
	}
	return port, nil
}
