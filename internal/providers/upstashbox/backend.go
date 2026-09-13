package upstashbox

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type backend struct {
	spec core.ProviderSpec
	cfg  core.Config
	rt   core.Runtime
}

func NewBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) core.Backend {
	cfg.Provider = providerName
	return &backend{spec: spec, cfg: cfg, rt: rt}
}

func (b *backend) Spec() core.ProviderSpec { return b.spec }

func (b *backend) Warmup(ctx context.Context, req core.WarmupRequest) error {
	if req.ActionsRunner {
		return core.Exit(2, "--actions-runner is not supported for provider=%s", providerName)
	}
	started := core.ClockNow(b.rt.Clock)
	client, err := newAPI(b.cfg, b.rt)
	if err != nil {
		return err
	}
	leaseID, box, slug, err := b.createBox(ctx, client, req.Repo, req.Keep, req.Reclaim, req.RequestedSlug)
	if err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s box=%s name=%s\n", leaseID, slug, providerName, box.ID, box.Name)
	if !req.Keep {
		fmt.Fprintf(b.rt.Stderr, "warning: upstash-box warmup keeps the box until explicit stop\n")
	}
	total := core.ClockNow(b.rt.Clock).Sub(started)
	return shared.CompleteWarmup(b.rt, req.TimingJSON, shared.WarmupCompletion{
		Provider: providerName,
		LeaseID:  leaseID,
		Slug:     slug,
		Total:    total,
	})
}

func (b *backend) Run(ctx context.Context, req core.RunRequest) (core.RunResult, error) {
	workdir, validationErr := cleanWorkdir(workdir(b.cfg))
	folder := ""
	if validationErr == nil {
		folder, validationErr = workspaceFolder(workdir)
	}
	var client api
	var leaseID, boxID, slug string
	session := func() shared.DelegatedSandbox {
		return shared.DelegatedSandbox{LeaseID: leaseID, Slug: slug, CleanupCommand: upstashBoxCleanupCommand(leaseID)}
	}
	return shared.RunDelegatedSandbox(ctx, req, shared.DelegatedSandboxLifecycle{
		Provider: providerName, Runtime: b.rt, Workdir: workdir,
		IdleTimeout: b.cfg.IdleTimeout, TTL: b.cfg.TTL, CleanupTimeout: upstashBoxCleanupTimeout,
		Preflight: func(context.Context) error {
			if validationErr != nil {
				return validationErr
			}
			var err error
			client, err = newAPI(b.cfg, b.rt)
			return err
		},
		PrepareArchive: func(ctx context.Context) (*core.PreparedArchive, error) {
			return b.prepareArchive(ctx, req)
		},
		Acquire: func(ctx context.Context) (shared.DelegatedSandbox, error) {
			var box boxData
			var err error
			leaseID, box, slug, err = b.createBox(ctx, client, req.Repo, req.Keep, req.Reclaim, req.RequestedSlug)
			if err != nil {
				return shared.DelegatedSandbox{}, err
			}
			boxID = box.ID
			fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=%s box=%s name=%s\n", leaseID, slug, providerName, box.ID, box.Name)
			return session(), nil
		},
		Resolve: func(ctx context.Context) (shared.DelegatedSandbox, error) {
			var err error
			leaseID, boxID, slug, err = b.resolveBoxID(ctx, client, req.ID, req.Repo.Root, req.Reclaim)
			if err != nil {
				return shared.DelegatedSandbox{}, err
			}
			return session(), nil
		},
		Sync: func(ctx context.Context, archive *core.PreparedArchive) ([]core.TimingPhase, time.Duration, error) {
			return b.syncWorkspace(ctx, client, boxID, req, workdir, folder, archive)
		},
		NoSync: func(ctx context.Context) error {
			return b.prepareWorkspace(ctx, client, boxID, folder)
		},
		Command: func(ctx context.Context) (shared.DelegatedSandboxCommand, error) {
			intent, err := core.ParseCommandIntent(req.Command, req.ShellMode, req.CommandLiteralArgs)
			if err != nil {
				return shared.DelegatedSandboxCommand{}, err
			}
			command := intent.ShellSource()
			if req.EnvSummary {
				core.PrintEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, req.Env)
			}
			var closeCommand func(context.Context) error
			if len(req.Env) > 0 {
				envPath, cleanup, err := uploadEnvProfile(ctx, client, leaseID, boxID, slug, req.Env)
				if cleanup != nil {
					closeCommand = func(ctx context.Context) error {
						if err := cleanup(ctx); err != nil {
							return shared.ExitErrorWithCause(5, err.Error(), err)
						}
						return nil
					}
				}
				if err != nil {
					return shared.DelegatedSandboxCommand{Close: closeCommand}, err
				}
				command = shared.ShellScriptWithEnvProfile(command, envPath)
			}
			return shared.DelegatedSandboxCommand{Text: strings.Join(req.Command, " "), Close: closeCommand,
				Run: func(ctx context.Context, stdout, _ io.Writer) (int, error) {
					req.Observation.OmitStream("stderr", "provider-combines-output")
					return client.ExecStream(ctx, boxID, command, folder, stdout)
				}}, nil
		},
		Cleanup: func(ctx context.Context) error {
			return b.deleteClaimedBox(ctx, client, leaseID, boxID, slug)
		},
	})
}

func (b *backend) List(ctx context.Context, req core.ListRequest) ([]core.LeaseView, error) {
	_ = req
	client, err := newAPI(b.cfg, b.rt)
	if err != nil {
		return nil, err
	}
	boxes, err := client.ListBoxes(ctx)
	if err != nil {
		return nil, err
	}
	servers := make([]core.Server, 0, len(boxes))
	for _, box := range boxes {
		if isCrabboxBox(box) {
			servers = append(servers, boxToServer(b.cfg, box))
		}
	}
	return servers, nil
}

func (b *backend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	servers, err := b.List(ctx, core.ListRequest{})
	if err != nil {
		return core.DoctorResult{}, err
	}
	return core.InventoryDoctorResult(providerName, len(servers)), nil
}

func (b *backend) Status(ctx context.Context, req core.StatusRequest) (core.StatusView, error) {
	client, err := newAPI(b.cfg, b.rt)
	if err != nil {
		return core.StatusView{}, err
	}
	return shared.PollDelegatedStatus(ctx, shared.DelegatedStatusRequest{
		ID:          req.ID,
		Provider:    providerName,
		TargetOS:    targetLinux,
		Network:     networkPublic,
		Wait:        req.Wait,
		WaitTimeout: req.WaitTimeout,
		Now:         func() time.Time { return core.ClockNow(b.rt.Clock) },
		Resolve: func(id string) (string, string, string, error) {
			return b.resolveBoxID(ctx, client, id, "", false)
		},
		Get: func(getCtx context.Context, boxID string) (shared.DelegatedStatusResource, error) {
			box, err := client.GetBox(getCtx, boxID)
			if err != nil {
				return shared.DelegatedStatusResource{}, err
			}
			server := boxToServer(b.cfg, box)
			return shared.DelegatedStatusResource{
				State:      box.Status,
				ServerID:   box.ID,
				ServerType: server.ServerType.Name,
				Ready:      statusReady(box.Status),
				Labels:     server.Labels,
			}, nil
		},
		TimeoutError: func(boxID string) error {
			return core.Exit(5, "timed out waiting for upstash-box %s to become ready", boxID)
		},
	})
}

func (b *backend) Stop(ctx context.Context, req core.StopRequest) error {
	client, err := newAPI(b.cfg, b.rt)
	if err != nil {
		return err
	}
	leaseID, boxID, slug, err := b.resolveBoxID(ctx, client, req.ID, "", false)
	if err != nil {
		return err
	}
	if err := b.deleteClaimedBox(ctx, client, leaseID, boxID, slug); err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stderr, "released lease=%s box=%s\n", leaseID, boxID)
	return nil
}

func (b *backend) deleteClaimedBox(ctx context.Context, client api, leaseID, boxID, slug string) error {
	binding := shared.ClaimBinding{
		Provider:       providerName,
		ProviderScope:  upstashBoxClaimScope(b.cfg),
		LeaseID:        leaseID,
		Slug:           slug,
		CloudID:        boxID,
		RequiredLabels: map[string]string{"box_id": boxID},
	}
	claim, err := shared.RequireExactClaim(binding)
	if err != nil {
		return err
	}
	return shared.RemoveExactClaimAfter(claim, binding, func() error {
		box, err := client.GetBox(ctx, boxID)
		if err != nil {
			if isNotFound(err) {
				return nil
			}
			return err
		}
		if box.ID != boxID || !isCrabboxBox(box) || boxLeaseID(box) != leaseID || boxSlug(leaseID, box) != slug {
			return core.Exit(2, "provider=%s box %s no longer matches its exact local ownership claim", providerName, boxID)
		}
		return client.DeleteBoxes(ctx, []string{boxID})
	})
}

func (b *backend) createBox(ctx context.Context, client api, repo core.Repo, keep, reclaim bool, requestedSlug string) (string, boxData, string, error) {
	leaseID := core.NewLeaseID()
	slug, err := core.AllocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return "", boxData{}, "", err
	}
	name := upstashBoxName(leaseID, slug)
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s name=%s runtime=%s size=%s keep_alive=%t\n", providerName, leaseID, slug, name, runtimeName(b.cfg), sizeName(b.cfg), b.cfg.UpstashBox.KeepAlive)
	box, err := client.CreateBox(ctx, createRequest{
		Name:      name,
		Runtime:   runtimeName(b.cfg),
		Size:      sizeName(b.cfg),
		KeepAlive: b.cfg.UpstashBox.KeepAlive,
	})
	if err != nil {
		return "", boxData{}, "", err
	}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, slug, providerName, upstashBoxClaimScope(b.cfg), "", repo.Root, b.cfg.IdleTimeout, reclaim, boxToServer(b.cfg, box), core.SSHTarget{}); err != nil {
		cleanupCtx, cancel := upstashBoxCleanupContext()
		cleanupErr := client.DeleteBoxes(cleanupCtx, []string{box.ID})
		cancel()
		if cleanupErr != nil {
			return "", boxData{}, "", fmt.Errorf("%w; cleanup failed for upstash-box %s: %v", err, box.ID, cleanupErr)
		}
		return "", boxData{}, "", err
	}
	return leaseID, box, slug, nil
}

func (b *backend) resolveBoxID(ctx context.Context, client api, id, repoRoot string, reclaim bool) (string, string, string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", "", "", core.Exit(2, "provider=%s requires a Crabbox lease id, slug, or Upstash Box id", providerName)
	}
	if claim, ok, err := core.ResolveLeaseClaim(id); err != nil {
		return "", "", "", err
	} else if ok && claim.Provider == providerName {
		if repoRoot == "" {
			if strings.TrimSpace(claim.CloudID) == "" {
				return "", "", "", core.Exit(2, "provider=%s lease=%s has no exact local ownership claim for an immutable box ID", providerName, claim.LeaseID)
			}
			return claim.LeaseID, claim.CloudID, claim.Slug, nil
		}
		box, err := resolveBoxByLease(ctx, client, claim.LeaseID)
		if err != nil {
			return "", "", "", err
		}
		if repoRoot != "" {
			if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(claim.LeaseID, claim.Slug, providerName, upstashBoxClaimScope(b.cfg), "", repoRoot, time.Duration(claim.IdleTimeoutSeconds)*time.Second, reclaim, boxToServer(b.cfg, box), core.SSHTarget{}); err != nil {
				return "", "", "", err
			}
		}
		return claim.LeaseID, box.ID, claim.Slug, nil
	}
	if strings.HasPrefix(id, "cbx_") {
		box, err := resolveBoxByLease(ctx, client, id)
		if err != nil {
			return "", "", "", err
		}
		return id, box.ID, boxSlug(id, box), nil
	}
	if box, err := client.GetBox(ctx, id); err == nil && isCrabboxBox(box) {
		leaseID := boxLeaseID(box)
		return leaseID, box.ID, boxSlug(leaseID, box), nil
	} else if err != nil && !isNotFound(err) {
		return "", "", "", err
	}
	box, err := resolveBoxBySlug(ctx, client, id)
	if err != nil {
		return "", "", "", err
	}
	leaseID := boxLeaseID(box)
	return leaseID, box.ID, boxSlug(leaseID, box), nil
}

func resolveBoxByLease(ctx context.Context, client api, leaseID string) (boxData, error) {
	boxes, err := client.ListBoxes(ctx)
	if err != nil {
		return boxData{}, err
	}
	for _, box := range boxes {
		if isCrabboxBox(box) && boxLeaseID(box) == leaseID {
			return box, nil
		}
	}
	return boxData{}, core.Exit(4, "upstash-box lease %q was not found", leaseID)
}

func resolveBoxBySlug(ctx context.Context, client api, slug string) (boxData, error) {
	boxes, err := client.ListBoxes(ctx)
	if err != nil {
		return boxData{}, err
	}
	for _, box := range boxes {
		if isCrabboxBox(box) && boxSlug(boxLeaseID(box), box) == slug {
			return box, nil
		}
	}
	return boxData{}, core.Exit(4, "upstash-box %q was not found", slug)
}

func boxToServer(cfg core.Config, box boxData) core.Server {
	leaseID := boxLeaseID(box)
	labels := core.DirectLeaseLabels(cfg, leaseID, boxSlug(leaseID, box), providerName, "", box.KeepAlive, time.Now().UTC())
	labels["box_id"] = box.ID
	labels["box_name"] = box.Name
	labels["runtime"] = core.Blank(box.Runtime, runtimeName(cfg))
	labels["size"] = core.Blank(box.Size, sizeName(cfg))
	labels["state"] = box.Status
	server := core.Server{
		Provider: providerName,
		CloudID:  box.ID,
		Name:     core.Blank(box.Name, box.ID),
		Status:   box.Status,
		Labels:   labels,
	}
	server.ServerType.Name = core.Blank(box.Size, sizeName(cfg))
	server.PublicNet.IPv4.IP = boxBaseHost(cfg)
	return server
}

func boxBaseHost(cfg core.Config) string {
	raw := core.Blank(strings.TrimSpace(cfg.UpstashBox.BaseURL), core.UpstashBoxConfigDefaultBaseURL)
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return raw
	}
	return parsed.Host
}

func upstashBoxClaimScope(cfg core.Config) string {
	raw := core.Blank(strings.TrimSpace(cfg.UpstashBox.BaseURL), core.UpstashBoxConfigDefaultBaseURL)
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "endpoint:" + strings.TrimRight(raw, "/")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return "endpoint:" + parsed.String()
}

var boxNamePattern = regexp.MustCompile(`^crabbox-(.+)-([0-9a-f]{12})$`)

func isCrabboxBox(box boxData) bool {
	return boxNamePattern.MatchString(strings.TrimSpace(box.Name))
}

func boxLeaseID(box boxData) string {
	if match := boxNamePattern.FindStringSubmatch(strings.TrimSpace(box.Name)); len(match) == 3 {
		return "cbx_" + match[2]
	}
	return "upstash_" + box.ID
}

func boxSlug(leaseID string, box boxData) string {
	if match := boxNamePattern.FindStringSubmatch(strings.TrimSpace(box.Name)); len(match) == 3 {
		return match[1]
	}
	return core.NewLeaseSlug(leaseID)
}

func statusReady(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "running", "ready", "idle", "paused":
		return true
	default:
		return false
	}
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "404") || strings.Contains(msg, "not found")
}

func runtimeName(cfg core.Config) string {
	return core.Blank(strings.TrimSpace(cfg.UpstashBox.Runtime), core.UpstashBoxConfigDefaultRuntime)
}

func upstashBoxName(leaseID, slug string) string {
	slug = strings.Trim(strings.ToLower(strings.TrimSpace(slug)), "-")
	if slug == "" {
		slug = core.NewLeaseSlug(leaseID)
	}
	return "crabbox-" + slug + "-" + strings.TrimPrefix(leaseID, "cbx_")
}

func sizeName(cfg core.Config) string {
	return core.Blank(strings.TrimSpace(cfg.UpstashBox.Size), core.UpstashBoxConfigDefaultSize)
}

func workdir(cfg core.Config) string {
	return core.Blank(strings.TrimSpace(cfg.UpstashBox.Workdir), core.UpstashBoxConfigDefaultWorkdir)
}

func cleanWorkdir(workdir string) (string, error) {
	return shared.CleanPOSIXWorkspacePath("upstash-box workdir", workdir, "/workspace", "/workspace/home")
}

const workspaceRoot = "/workspace/home"

func workspaceFolder(workdir string) (string, error) {
	clean, err := cleanWorkdir(workdir)
	if err != nil {
		return "", err
	}
	prefix := workspaceRoot + "/"
	if !strings.HasPrefix(clean, prefix) {
		return "", core.Exit(2, "upstash-box workdir %q must be under %s", clean, workspaceRoot)
	}
	return strings.TrimPrefix(clean, prefix), nil
}

func workspacePath(name string) string {
	return path.Join(workspaceRoot, name)
}
