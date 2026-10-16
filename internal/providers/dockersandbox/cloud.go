package dockersandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type cloudBackend struct {
	spec core.ProviderSpec
	cfg  core.Config
	rt   core.Runtime
}

func (b *cloudBackend) Spec() core.ProviderSpec { return b.spec }

func cloudTTL(cfg core.Config) time.Duration {
	if cfg.TTL > 0 {
		return cfg.TTL
	}
	return time.Hour
}

func validateCloudConfig(cfg core.Config) error {
	if cfg.DockerSandbox.Clone || len(cfg.DockerSandbox.ExtraWorkspaces) > 0 {
		return core.Exit(2, "Docker cloud has no host mounts: --docker-sandbox-clone and extra workspaces are local-only")
	}
	if cfg.TTL < 0 || cloudTTL(cfg) > 24*time.Hour {
		return core.Exit(2, "Docker cloud TTL must be positive and at most 24h")
	}
	if cfg.DockerSandbox.Template != "" && (cfg.DockerSandbox.CPUs > 0 || cfg.DockerSandbox.Memory != "" || len(cfg.DockerSandbox.Kit) > 0) {
		return core.Exit(2, "Docker cloud templates supply their own sizing; do not combine a template with CPUs, memory or kits")
	}
	if cfg.DockerSandbox.CPUs != 0 && cfg.DockerSandbox.CPUs != 1 && cfg.DockerSandbox.CPUs != 2 && cfg.DockerSandbox.CPUs != 4 && cfg.DockerSandbox.CPUs != 8 && cfg.DockerSandbox.CPUs != 16 {
		return core.Exit(2, "Docker cloud CPU count must be 1, 2, 4, 8 or 16")
	}
	memory := strings.ToLower(strings.TrimSpace(cfg.DockerSandbox.Memory))
	if memory != "" {
		sizes := map[string]int{"2g": 1, "2048m": 1, "4g": 2, "4096m": 2, "8g": 4, "8192m": 4, "16g": 8, "16384m": 8, "32g": 16, "32768m": 16}
		cpus, ok := sizes[memory]
		if !ok || (cfg.DockerSandbox.CPUs > 0 && int(cfg.DockerSandbox.CPUs) != cpus) {
			return core.Exit(2, "Docker cloud memory must match a supported CPU shape: 1/2g, 2/4g, 4/8g, 8/16g or 16/32g")
		}
	}
	_, err := cloudWorkdir(cfg)
	return err
}

func cloudWorkdir(cfg core.Config) (string, error) {
	return shared.CleanPOSIXWorkspacePath("Docker cloud workdir", core.Blank(strings.TrimSpace(cfg.DockerSandbox.Workdir), "/home/agent/workspace/project"), "/home/agent", "/home/agent/workspace")
}

func cloudCleanupCommand(id string) string {
	return "crabbox stop --provider docker-sandbox --docker-sandbox-target cloud " + id
}

func cloudCommandEnv(env map[string]string) ([]string, error) {
	keys := make([]string, 0, len(env))
	for key := range env {
		if !core.IsRunExecutionMetadataEnvName(key) {
			return nil, core.Exit(2, "Docker cloud user env forwarding is unsupported; configure cloud secrets through sbx or set env.allow: []")
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var args []string
	for _, key := range keys {
		args = append(args, "--env", key+"="+env[key])
	}
	return args, nil
}

func validateCloudClaim(claim core.LeaseClaim) error {
	if claim.Provider != providerName || claim.ProviderScope != cloudScope || !cloudIDPattern.MatchString(claim.CloudID) || claim.LeaseID != cloudLeasePrefix+claim.CloudID || !strings.HasPrefix(claim.Labels["sandbox"], namePrefix) || claim.Labels["docker_sandbox_target"] != "cloud" {
		return core.Exit(4, "Docker cloud requires an exact Crabbox cloud claim; local claims and sandbox names cannot authorize cloud operations")
	}
	_, err := shared.CleanPOSIXWorkspacePath("Docker cloud claimed workdir", claim.Labels["workdir"], "/home/agent", "/home/agent/workspace")
	return err
}

func resolveCloudClaim(id string) (core.LeaseClaim, error) {
	claim, ok, _, err := core.ResolveLeaseClaimForProviderScopeWithExact(id, providerName, cloudScope)
	if err != nil {
		return claim, err
	}
	if !ok {
		return claim, core.Exit(4, "Docker cloud sandbox is not claimed in the current Crabbox scope; use its cloud slug or lease ID")
	}
	return claim, validateCloudClaim(claim)
}

func (b *cloudBackend) live(ctx context.Context, c *sbxCLI, claim core.LeaseClaim) (sandboxRecord, error) {
	if err := validateCloudClaim(claim); err != nil {
		return sandboxRecord{}, err
	}
	records, err := c.cloudList(ctx)
	if err != nil {
		return sandboxRecord{}, err
	}
	for _, r := range records {
		if r.ID == claim.CloudID {
			if r.Name != claim.Labels["sandbox"] {
				return sandboxRecord{}, core.Exit(4, "Docker cloud identity changed; refusing access")
			}
			return r, nil
		}
	}
	return sandboxRecord{}, core.Exit(4, "Docker cloud sandbox is absent from the current account inventory (expired, deleted or another account); claim retained for recovery")
}

func (b *cloudBackend) create(ctx context.Context, c *sbxCLI, repo core.Repo, reclaim bool, slug string) (core.LeaseClaim, error) {
	if err := core.ValidateDirectLeaseCreation(b.cfg); err != nil {
		return core.LeaseClaim{}, err
	}
	if err := validateCreateRepo(b.cfg, repo); err != nil {
		return core.LeaseClaim{}, err
	}
	name := newSandboxName(repo)
	id, err := c.cloudCreate(ctx, name)
	if err != nil {
		return core.LeaseClaim{}, err
	}
	records, err := c.cloudList(ctx)
	if err != nil {
		return core.LeaseClaim{}, fmt.Errorf("Docker cloud create identity unconfirmed for %s (%s); inspect sbx --cloud ls before cleanup: %w", name, id, err)
	}
	created := false
	for _, record := range records {
		if record.ID == id && record.Name == name {
			created = true
		}
	}
	if !created {
		return core.LeaseClaim{}, core.Exit(4, "Docker cloud create identity unconfirmed for %s (%s); inspect sbx --cloud ls before cleanup", name, id)
	}
	leaseID := cloudLeasePrefix + id
	slug, err = core.AllocateClaimLeaseSlug(leaseID, slug)
	if err != nil {
		return core.LeaseClaim{}, b.rollbackCreate(c, id, err)
	}
	labels := map[string]string{"provider": providerName, "lease": leaseID, "slug": slug, "sandbox": name, "docker_sandbox_target": "cloud", "workdir": mustCloudWorkdir(b.cfg), "expires_at": core.ClockNow(b.rt.Clock).Add(cloudTTL(b.cfg)).UTC().Format(time.RFC3339), "on_timeout": "delete"}
	server := core.Server{Provider: providerName, CloudID: id, Name: name, Labels: labels}
	claim, err := core.ClaimLeaseTargetForRepoConfigScopeIfUnchangedDurable(leaseID, slug, b.cfg, cloudScope, server, core.SSHTarget{}, repo.Root, b.cfg.IdleTimeout, reclaim, core.LeaseClaim{}, false)
	if err != nil {
		return core.LeaseClaim{}, b.rollbackCreate(c, id, err)
	}
	fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=docker-sandbox target=cloud sandbox=%s ttl=%s on_timeout=delete (billable compute); idle enforced by crabbox cleanup\n", leaseID, slug, id, cloudTTL(b.cfg))
	return claim, nil
}

func mustCloudWorkdir(cfg core.Config) string { workdir, _ := cloudWorkdir(cfg); return workdir }

func (b *cloudBackend) rollbackCreate(c *sbxCLI, id string, primary error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	err := c.remove(ctx, id)
	if err == nil {
		err = b.confirmRemoved(ctx, c, id)
	}
	if err != nil {
		return errors.Join(primary, fmt.Errorf("Docker cloud allocation cleanup unconfirmed; inspect sbx --cloud ls and retry sbx --cloud rm --force %s: %w", id, err))
	}
	return primary
}

func (b *cloudBackend) confirmRemoved(ctx context.Context, c *sbxCLI, id string) error {
	records, err := c.cloudList(ctx)
	if err != nil {
		return err
	}
	for _, r := range records {
		if r.ID == id {
			return core.Exit(5, "Docker cloud deletion still pending for %s; retry cleanup after checking sbx --cloud ls", id)
		}
	}
	return nil
}

func (b *cloudBackend) remove(ctx context.Context, c *sbxCLI, claim core.LeaseClaim) error {
	if claim.Labels["docker_sandbox_delete_requested"] != "true" {
		labels := shared.CloneLabels(claim.Labels)
		labels["docker_sandbox_delete_requested"], labels["state"] = "true", "deleting"
		var err error
		claim, err = core.UpdateLeaseClaimLabelsIfUnchangedAfter(claim.LeaseID, claim, labels, func() error {
			if _, err := b.live(ctx, c, claim); err != nil {
				return err
			}
			return c.remove(ctx, claim.CloudID)
		})
		if err != nil {
			return err
		}
	}
	// A recorded accepted deletion permits settling an absent ID on retry.
	// Mere inventory absence without this marker never retires another account's claim.
	return core.CleanupLeaseClaimIfUnchangedAfterContext(ctx, claim.LeaseID, claim, true, func() error { return b.confirmRemoved(ctx, c, claim.CloudID) })
}

func (b *cloudBackend) workspace(c *sbxCLI, claim *core.LeaseClaim, req core.RunRequest) core.ArchiveWorkspace {
	w := core.NewArchiveWorkspace(b.cfg, b.rt, req, providerName, mustCloudWorkdir(b.cfg))
	w.CleanWorkdir = func(p string) (string, error) {
		return shared.CleanPOSIXWorkspacePath("Docker cloud workdir", p, "/home/agent", "/home/agent/workspace")
	}
	w.Upload = func(ctx context.Context, dest string, reader io.Reader) error {
		file, ok := reader.(*os.File)
		if !ok {
			return errors.New("Docker cloud archive upload requires a prepared file")
		}
		return core.WithLeaseClaimUnchangedContext(ctx, claim.LeaseID, *claim, func() error { return c.copy(ctx, file.Name(), claim.CloudID+":"+dest, false) })
	}
	w.Exec = func(ctx context.Context, command string) error {
		return core.WithLeaseClaimUnchangedContext(ctx, claim.LeaseID, *claim, func() error {
			code, err := c.execStream(ctx, claim.CloudID, "", "", []string{"sh", "-lc", command}, io.Discard, b.rt.Stderr)
			if err != nil {
				return err
			}
			if code != 0 {
				return core.Exit(code, "Docker cloud workspace operation exited %d", code)
			}
			return nil
		})
	}
	return w
}

func (b *cloudBackend) Run(ctx context.Context, req core.RunRequest) (core.RunResult, error) {
	c, err := newSBXCLI(b.cfg, b.rt)
	if err != nil {
		return core.RunResult{}, err
	}
	var claim core.LeaseClaim
	var artifacts []core.RunArtifact
	handle := func(unlock func()) shared.DelegatedSandbox {
		return shared.DelegatedSandbox{LeaseID: claim.LeaseID, Slug: claim.Slug, CleanupCommand: cloudCleanupCommand(claim.LeaseID), Unlock: unlock}
	}
	result, runErr := shared.RunDelegatedSandbox(ctx, req, shared.DelegatedSandboxLifecycle{
		Provider: providerName, Runtime: b.rt, Workdir: mustCloudWorkdir(b.cfg), TTL: cloudTTL(b.cfg), IdleTimeout: b.cfg.IdleTimeout, CleanupTimeout: 90 * time.Second,
		Preflight: func(ctx context.Context) error {
			if err := validateConfig(b.cfg); err != nil {
				return err
			}
			if err := rejectRunOptions(b.spec, req); err != nil {
				return err
			}
			if _, err := cloudCommandEnv(req.Env); err != nil {
				return err
			}
			if req.ID == "" {
				if err := core.ValidateDirectLeaseCreation(b.cfg); err != nil {
					return err
				}
			}
			return c.cloudReady(ctx)
		},
		Workspace: func() shared.SandboxWorkspace { return b.workspace(c, &claim, req) },
		Acquire: func(ctx context.Context) (shared.DelegatedSandbox, error) {
			var err error
			claim, err = b.create(ctx, c, req.Repo, req.Reclaim, req.RequestedSlug)
			if err != nil {
				return shared.DelegatedSandbox{}, err
			}
			unlock, err := shared.LockLeaseOperation(ctx, providerName, claim.LeaseID)
			if err != nil {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
				defer cancel()
				return handle(nil), errors.Join(err, b.remove(cleanupCtx, c, claim))
			}
			return handle(unlock), nil
		},
		Resolve: func(ctx context.Context) (shared.DelegatedSandbox, error) {
			var err error
			claim, err = resolveCloudClaim(req.ID)
			if err != nil {
				return shared.DelegatedSandbox{}, err
			}
			unlock, err := shared.LockLeaseOperation(ctx, providerName, claim.LeaseID)
			if err != nil {
				return shared.DelegatedSandbox{}, err
			}
			// Re-read after acquiring the operation lock, then bind repository admission.
			claim, err = resolveCloudClaim(claim.LeaseID)
			if err == nil {
				server := core.Server{Provider: providerName, CloudID: claim.CloudID, Name: claim.Labels["sandbox"], Labels: shared.CloneLabels(claim.Labels)}
				claim, err = core.ClaimLeaseTargetForRepoConfigScopeIfUnchangedDurableAfterContext(ctx, claim.LeaseID, claim.Slug, b.cfg, cloudScope, server, core.SSHTarget{}, req.Repo.Root, b.cfg.IdleTimeout, req.Reclaim, claim, true, func() error { _, e := b.live(ctx, c, claim); return e })
			}
			if err != nil {
				unlock()
				return shared.DelegatedSandbox{}, err
			}
			return handle(unlock), nil
		},
		AdmitReuse: func(ctx context.Context) error {
			r, err := b.live(ctx, c, claim)
			if err != nil {
				return err
			}
			if !isReadyState(r.State) {
				return core.Exit(4, "Docker cloud sandbox is %s, not running; inspect with sbx --cloud ls", r.State)
			}
			if mustCloudWorkdir(b.cfg) != claim.Labels["workdir"] {
				return core.Exit(2, "Docker cloud workdir differs from the retained claim")
			}
			return shared.AuthorizeClaimActivity(claim)
		},
		Command: func(ctx context.Context) (shared.DelegatedSandboxCommand, error) {
			intent, err := core.ParseCommandIntent(req.Command, req.ShellMode, req.CommandLiteralArgs)
			if err != nil {
				return shared.DelegatedSandboxCommand{}, err
			}
			envArgs, err := cloudCommandEnv(req.Env)
			if err != nil {
				return shared.DelegatedSandboxCommand{}, err
			}
			return shared.DelegatedSandboxCommand{Text: strings.Join(req.Command, " "), OutputScope: core.RunOutputProvider, Run: func(ctx context.Context, stdout, stderr io.Writer) (int, error) {
				var code int
				err := core.WithLeaseClaimUnchangedContext(ctx, claim.LeaseID, claim, func() error {
					var e error
					args := append([]string{"exec", "--workdir", claim.Labels["workdir"]}, envArgs...)
					args = append(args, claim.CloudID)
					args = append(args, intent.Argv("sh", "-lc")...)
					code, e = c.runStreamed(ctx, args, stdout, stderr)
					return e
				})
				if err == nil && code == 0 && core.HasDelegatedRunDownloadRequests(req) {
					artifacts, err = core.MaterializeDelegatedRunDownloads(ctx, cloudRunDownloader{b, c, claim}, req, claim.LeaseID, b.rt.Stderr)
				}
				return code, err
			}}, nil
		},
		Retained: func(ctx context.Context) error {
			_, err := core.UpdateLeaseClaimLabelsAndLastUsedIfUnchanged(claim.LeaseID, claim, claim.Labels, core.ClockNow(b.rt.Clock))
			return err
		},
		Cleanup: func(ctx context.Context) error { return b.remove(ctx, c, claim) },
	})
	result.Artifacts = artifacts
	return result, runErr
}

func (b *cloudBackend) Warmup(ctx context.Context, req core.WarmupRequest) error {
	if req.ActionsRunner {
		return core.Exit(2, "Docker cloud does not support --actions-runner")
	}
	result, err := b.Run(ctx, core.RunRequest{Repo: req.Repo, Options: req.Options, Keep: true, SyncOnly: true, Reclaim: req.Reclaim, RequestedSlug: req.RequestedSlug})
	if err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=docker-sandbox target=cloud\n", result.LeaseID, result.Slug)
	return shared.CompleteWarmup(b.rt, req.TimingJSON, shared.WarmupCompletion{Provider: providerName, LeaseID: result.LeaseID, Slug: result.Slug, Total: result.Total})
}

func (b *cloudBackend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	c, err := newSBXCLI(b.cfg, b.rt)
	if err == nil {
		err = c.cloudReady(ctx)
	}
	result := core.DoctorResult{Provider: providerName, Status: "ok", Message: "target=cloud readiness checked; no compute allocated", Checks: []core.DoctorCheck{doctorCheck("sbx_cloud", err, nil)}}
	if err != nil {
		result.Status = "error"
		result.Message = "target=cloud blocked"
	}
	return result, err
}

func cloudServer(claim core.LeaseClaim, r sandboxRecord) core.Server {
	if claim.Labels["docker_sandbox_delete_requested"] == "true" {
		r.State = "deleting"
	}
	s := serverFromClaimRecord(claim, r)
	s.CloudID = r.ID
	s.Labels["docker_sandbox_target"] = "cloud"
	s.Labels["expires_at"] = claim.Labels["expires_at"]
	s.Labels["on_timeout"] = "delete"
	return s
}

func (b *cloudBackend) List(ctx context.Context, _ core.ListRequest) ([]core.LeaseView, error) {
	c, err := newSBXCLI(b.cfg, b.rt)
	if err != nil {
		return nil, err
	}
	records, err := c.cloudList(ctx)
	if err != nil {
		return nil, err
	}
	claims, err := core.ListLeaseClaimsContext(ctx)
	if err != nil {
		return nil, err
	}
	var out []core.LeaseView
	for _, claim := range claims {
		if validateCloudClaim(claim) != nil {
			continue
		}
		for _, r := range records {
			if r.ID == claim.CloudID && r.Name == claim.Labels["sandbox"] {
				out = append(out, cloudServer(claim, r))
				break
			}
		}
	}
	return out, nil
}

func (b *cloudBackend) Status(ctx context.Context, req core.StatusRequest) (core.StatusView, error) {
	c, err := newSBXCLI(b.cfg, b.rt)
	if err != nil {
		return core.StatusView{}, err
	}
	claim, err := resolveCloudClaim(req.ID)
	if err != nil {
		return core.StatusView{}, err
	}
	var record sandboxRecord
	err = core.WithLeaseClaimUnchangedContext(ctx, claim.LeaseID, claim, func() error { var e error; record, e = b.live(ctx, c, claim); return e })
	if err != nil {
		return core.StatusView{}, err
	}
	view := statusFromRecord(claim.LeaseID, claim.Slug, record)
	view.ServerID = record.ID
	view.Labels = cloudServer(claim, record).Labels
	if claim.Labels["docker_sandbox_delete_requested"] == "true" {
		view.State, view.Ready = "deleting", false
	}
	return view, nil
}

func (b *cloudBackend) Stop(ctx context.Context, req core.StopRequest) error {
	c, err := newSBXCLI(b.cfg, b.rt)
	if err != nil {
		return err
	}
	claim, err := resolveCloudClaim(req.ID)
	if err != nil {
		return err
	}
	unlock, err := shared.LockLeaseOperation(ctx, providerName, claim.LeaseID)
	if err != nil {
		return err
	}
	defer unlock()
	claim, err = resolveCloudClaim(claim.LeaseID)
	if err != nil {
		return err
	}
	if err := b.remove(ctx, c, claim); err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stderr, "released lease=%s sandbox=%s target=cloud\n", claim.LeaseID, claim.CloudID)
	return nil
}

func (b *cloudBackend) Copy(ctx context.Context, req core.CopyRequest) error {
	c, err := newSBXCLI(b.cfg, b.rt)
	if err != nil {
		return err
	}
	claim, err := resolveCloudClaim(req.ID)
	if err != nil {
		return err
	}
	src, dst, err := rewriteSandboxCopyPaths(claim.CloudID, req.Source, req.Destination)
	if err != nil {
		return err
	}
	for _, value := range []string{src, dst} {
		if strings.HasPrefix(value, claim.CloudID+":") && !path.IsAbs(strings.TrimPrefix(value, claim.CloudID+":")) {
			return core.Exit(2, "Docker cloud copy requires an absolute sandbox path")
		}
	}
	return core.WithLeaseClaimUnchangedContext(ctx, claim.LeaseID, claim, func() error {
		if _, err := b.live(ctx, c, claim); err != nil {
			return err
		}
		return c.copy(ctx, src, dst, req.FollowLink)
	})
}

func (b *cloudBackend) Ports(ctx context.Context, req core.PortsRequest) (string, error) {
	for _, v := range append(append([]string{}, req.Publish...), req.Unpublish...) {
		n := strings.TrimSuffix(v, "/tcp")
		port, err := strconv.Atoi(n)
		if err != nil || port < 1 || port > 65535 {
			return "", core.Exit(2, "Docker cloud ports require a sandbox port (1-65535, optional /tcp); host bindings are unsupported")
		}
	}
	c, err := newSBXCLI(b.cfg, b.rt)
	if err != nil {
		return "", err
	}
	claim, err := resolveCloudClaim(req.ID)
	if err != nil {
		return "", err
	}
	var out string
	err = core.WithLeaseClaimUnchangedContext(ctx, claim.LeaseID, claim, func() error {
		if _, err := b.live(ctx, c, claim); err != nil {
			return err
		}
		var e error
		out, e = c.ports(ctx, claim.CloudID, req.Publish, req.Unpublish, req.JSON)
		return e
	})
	return out, err
}

func (b *cloudBackend) Cleanup(ctx context.Context, req core.CleanupRequest) error {
	c, err := newSBXCLI(b.cfg, b.rt)
	if err != nil {
		return err
	}
	claims, err := core.ListLeaseClaimsContext(ctx)
	if err != nil {
		return err
	}
	for _, claim := range claims {
		if validateCloudClaim(claim) != nil {
			continue
		}
		if err := b.cleanupClaim(ctx, c, claim.LeaseID, req.DryRun); err != nil {
			return err
		}
	}
	return nil
}

func (b *cloudBackend) cleanupClaim(ctx context.Context, c *sbxCLI, leaseID string, dryRun bool) error {
	unlock, err := shared.LockLeaseOperation(ctx, providerName, leaseID)
	if err != nil {
		return err
	}
	defer unlock()
	claim, err := resolveCloudClaim(leaseID)
	if err != nil {
		fmt.Fprintf(b.rt.Stderr, "skip %s: claim changed or removed\n", leaseID)
		return nil
	}
	if claim.Labels["docker_sandbox_delete_requested"] == "true" {
		if dryRun {
			fmt.Fprintf(b.rt.Stderr, "would confirm pending deletion %s\n", leaseID)
			return nil
		}
		return b.remove(ctx, c, claim)
	}
	due, reason := shared.ClaimIdleCleanupDue(claim, core.ClockNow(b.rt.Clock))
	if deadline, err := time.Parse(time.RFC3339, claim.Labels["expires_at"]); err == nil && !core.ClockNow(b.rt.Clock).Before(deadline) {
		due, reason = true, "TTL"
	}
	if !due {
		fmt.Fprintf(b.rt.Stderr, "skip %s: %s\n", claim.LeaseID, reason)
		return nil
	}
	if _, err := b.live(ctx, c, claim); err != nil {
		fmt.Fprintf(b.rt.Stderr, "skip %s: %v\n", claim.LeaseID, err)
		return nil
	}
	if dryRun {
		fmt.Fprintf(b.rt.Stderr, "would remove %s: %s\n", claim.LeaseID, reason)
		return nil
	}
	if err := b.remove(ctx, c, claim); err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stderr, "removed %s: %s\n", claim.LeaseID, reason)
	return nil
}

type cloudRunDownloader struct {
	b     *cloudBackend
	c     *sbxCLI
	claim core.LeaseClaim
}

func (d cloudRunDownloader) Spec() core.ProviderSpec { return d.b.spec }
func (d cloudRunDownloader) FetchRunFile(ctx context.Context, req core.DelegatedRunDownloadRequest) ([]byte, error) {
	if req.LeaseID != d.claim.LeaseID {
		return nil, core.Exit(4, "Docker cloud download claim mismatch")
	}
	var data []byte
	err := core.WithLeaseClaimUnchangedContext(ctx, d.claim.LeaseID, d.claim, func() error {
		var err error
		data, err = d.c.cloudReadFile(ctx, d.claim.CloudID, path.Join(d.claim.Labels["workdir"], req.RemotePath), req.MaxBytes)
		return err
	})
	return data, err
}
