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
	spec core.ProviderSpec
	cfg  core.Config
	rt   core.Runtime
}

func (b *codeSandboxBackend) Spec() core.ProviderSpec { return b.spec }

func (b *codeSandboxBackend) Warmup(ctx context.Context, req core.WarmupRequest) error {
	if req.ActionsRunner {
		return core.Exit(2, "--actions-runner is not supported for provider=%s", providerName)
	}
	started := core.ClockNow(b.rt.Clock)
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
	total := core.ClockNow(b.rt.Clock).Sub(started)
	return shared.CompleteWarmup(b.rt, req.TimingJSON, shared.WarmupCompletion{
		Provider: providerName,
		LeaseID:  leaseID,
		Slug:     slug,
		Total:    total,
	})
}

func (b *codeSandboxBackend) Run(ctx context.Context, req core.RunRequest) (core.RunResult, error) {
	if err := core.RejectDelegatedSyncOptionsForSpec(b.spec, req); err != nil {
		return core.RunResult{}, err
	}
	workdir, err := codeSandboxWorkdir(b.cfg)
	if err != nil {
		return core.RunResult{}, err
	}
	if !req.SyncOnly && (len(req.Command) == 0 || (len(req.Command) == 1 && strings.TrimSpace(req.Command[0]) == "")) {
		return core.RunResult{}, core.Exit(2, "missing command")
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
				TempPattern: "crabbox-codesandbox-sync-*.tgz", Stderr: b.rt.Stderr, Now: func() time.Time { return core.ClockNow(b.rt.Clock) },
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
			var claim core.LeaseClaim
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
				if err := core.ClaimLeaseForRepoProviderScopePond(leaseID, slug, providerName, claim.ProviderScope, b.cfg.Pond, req.Repo.Root,
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
				core.PrintEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, req.Env)
			}
			return shared.DelegatedSandboxCommand{
				Text: strings.Join(command, " "),
				Run: func(ctx context.Context, stdout, stderr io.Writer) (int, error) {
					return b.execCommand(ctx, api, sandboxID, workdir, command, req.Env, stdout, stderr)
				},
			}, nil
		},
		Cleanup: func(ctx context.Context) error {
			if err := api.DeleteSandbox(ctx, sandboxID); err != nil {
				return err
			}
			core.RemoveLeaseClaim(leaseID)
			return nil
		},
	})
}

func (b *codeSandboxBackend) List(ctx context.Context, _ core.ListRequest) ([]core.LeaseView, error) {
	api, err := newCodeSandboxClient(b.cfg, b.rt)
	if err != nil {
		return nil, err
	}
	claims, err := listCodeSandboxLeaseClaims()
	if err != nil {
		return nil, err
	}
	servers := make([]core.Server, 0, len(claims))
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

func (b *codeSandboxBackend) Status(ctx context.Context, req core.StatusRequest) (core.StatusView, error) {
	api, err := newCodeSandboxClient(b.cfg, b.rt)
	if err != nil {
		return core.StatusView{}, err
	}
	leaseID, sandboxID, slug, claim, err := resolveLeaseID(req.ID)
	if err != nil {
		return core.StatusView{}, err
	}
	waitTimeout := req.WaitTimeout
	if waitTimeout <= 0 {
		waitTimeout = 5 * time.Minute
	}
	deadline := core.ClockNow(b.rt.Clock).Add(waitTimeout)
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
				return core.StatusView{}, core.Exit(5, "timed out waiting for codesandbox sandbox %s to become ready", sandboxID)
			}
			if ctx.Err() != nil {
				return core.StatusView{}, ctx.Err()
			}
			return core.StatusView{}, getErr
		}
		if err := validateCodeSandboxSandboxOwnership(claim, sb); err != nil {
			return core.StatusView{}, err
		}
		state := strings.ToLower(strings.TrimSpace(blank(sb.State, "unknown")))
		view := core.StatusView{
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
			return core.StatusView{}, core.Exit(5, "codesandbox sandbox %s entered terminal state %q before becoming ready", sandboxID, state)
		}
		if core.ClockNow(b.rt.Clock).After(deadline) {
			return core.StatusView{}, core.Exit(5, "timed out waiting for codesandbox sandbox %s to become ready", sandboxID)
		}
		select {
		case <-pollCtx.Done():
			if errors.Is(pollCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				return core.StatusView{}, core.Exit(5, "timed out waiting for codesandbox sandbox %s to become ready", sandboxID)
			}
			return core.StatusView{}, pollCtx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (b *codeSandboxBackend) Stop(ctx context.Context, req core.StopRequest) error {
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
	core.RemoveLeaseClaim(leaseID)
	fmt.Fprintf(b.rt.Stderr, "released lease=%s sandbox=%s\n", leaseID, sandboxID)
	return nil
}

func (b *codeSandboxBackend) Cleanup(ctx context.Context, req core.CleanupRequest) error {
	api, err := newCodeSandboxClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	claims, err := listCodeSandboxLeaseClaims()
	if err != nil {
		return err
	}
	now := core.ClockNow(b.rt.Clock).UTC()
	checked := 0
	removed := 0
	for _, listed := range claims {
		if listed.Provider != providerName || !strings.HasPrefix(listed.LeaseID, leasePrefix) {
			continue
		}
		claim, err := core.ReadLeaseClaim(listed.LeaseID)
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
		due, reason := shared.ClaimIdleCleanupDue(claim, now)
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
		if err := core.RemoveLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
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

func (b *codeSandboxBackend) Pause(ctx context.Context, req core.PauseRequest) error {
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

func (b *codeSandboxBackend) Resume(ctx context.Context, req core.ResumeRequest) error {
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
		return core.Exit(4, "codesandbox resumed sandbox %q does not match local claim %q", resumed.ID, claim.LeaseID)
	}
	fmt.Fprintf(b.rt.Stderr, "resumed lease=%s sandbox=%s\n", leaseID, sandboxID)
	return nil
}

func (b *codeSandboxBackend) Ports(ctx context.Context, req core.PortsRequest) (string, error) {
	if len(req.Unpublish) > 0 {
		return "", core.Exit(2, "provider=codesandbox does not support ports --unpublish; stop the process inside the sandbox instead")
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

func (b *codeSandboxBackend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	token, source, ok := authFromEnv()
	if !ok {
		return core.DoctorResult{}, missingAuthError{}
	}
	client, err := newCodeSandboxClient(b.cfg, b.rt)
	if err != nil {
		return core.DoctorResult{}, err
	}
	result := core.DoctorResult{
		Provider: providerName,
		Checks: []core.DoctorCheck{
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
	result.Message = core.InventoryDoctorResult(providerName, len(listed.Sandboxes)).Message
	return result, nil
}

func (b *codeSandboxBackend) execCommand(ctx context.Context, api codeSandboxAPI, sandboxID, workdir string, command []string, env map[string]string, stdout, stderr io.Writer) (int, error) {
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
		_, _ = io.WriteString(stdout, res.Stdout)
	}
	if res.Stderr != "" {
		_, _ = io.WriteString(stderr, res.Stderr)
	}
	return res.ExitCode, nil
}

func codeSandboxServerView(claim core.LeaseClaim, sb SandboxSummary) core.Server {
	state := blank(sb.State, "unknown")
	sandboxID := strings.TrimPrefix(claim.LeaseID, leasePrefix)
	if strings.TrimSpace(sb.ID) != "" {
		sandboxID = sb.ID
	}
	name := blank(sb.Title, sandboxID)
	return core.Server{
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
	Warmup(context.Context, core.WarmupRequest) error
	Run(context.Context, core.RunRequest) (core.RunResult, error)
	List(context.Context, core.ListRequest) ([]core.LeaseView, error)
	Status(context.Context, core.StatusRequest) (core.StatusView, error)
	Stop(context.Context, core.StopRequest) error
	Cleanup(context.Context, core.CleanupRequest) error
	Pause(context.Context, core.PauseRequest) error
	Resume(context.Context, core.ResumeRequest) error
	Ports(context.Context, core.PortsRequest) (string, error)
	Doctor(context.Context, core.DoctorRequest) (core.DoctorResult, error)
} = (*codeSandboxBackend)(nil)

func parseCodeSandboxPortSpec(spec string) (int, error) {
	value := strings.TrimSpace(spec)
	if value == "" {
		return 0, core.Exit(2, "codesandbox port spec must not be empty")
	}
	if strings.ContainsAny(value, ":/") {
		return 0, core.Exit(2, "codesandbox ports only support a sandbox port number, got %q", spec)
	}
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, core.Exit(2, "codesandbox port must be an integer between 1 and 65535, got %q", spec)
	}
	return port, nil
}
