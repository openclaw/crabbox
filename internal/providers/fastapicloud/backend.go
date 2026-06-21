package fastapicloud

import (
	"context"
	"net/url"
	"strings"
)

func NewFastAPICloudBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = providerName
	return &fastAPICloudBackend{spec: spec, cfg: cfg, rt: rt}
}

type fastAPICloudBackend struct {
	spec   ProviderSpec
	cfg    Config
	rt     Runtime
	client fastAPICloudAPI
}

func (b *fastAPICloudBackend) Spec() ProviderSpec { return b.spec }

func (b *fastAPICloudBackend) Warmup(ctx context.Context, req WarmupRequest) error {
	_ = ctx
	_ = req
	return exit(2, "provider=%s does not support warmup; create and deploy the FastAPI Cloud app out-of-band", providerName)
}

func (b *fastAPICloudBackend) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	_ = ctx
	if err := rejectFastAPICloudRunOptions(req); err != nil {
		return RunResult{}, err
	}
	if len(req.Command) == 0 {
		return RunResult{}, exit(2, "missing command")
	}
	return RunResult{}, exit(2, "provider=%s cannot execute arbitrary run commands; deploy with fastapi deploy or FastAPI Cloud CI", providerName)
}

func (b *fastAPICloudBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	_ = req
	client, err := b.api()
	if err != nil {
		return nil, err
	}
	if teamID := strings.TrimSpace(b.cfg.FastAPICloud.TeamID); teamID != "" {
		apps, err := client.ListApps(ctx, teamID)
		if err != nil {
			return nil, err
		}
		servers := make([]Server, 0, len(apps))
		for _, app := range apps {
			servers = append(servers, fastAPICloudServer(app))
		}
		return servers, nil
	}
	if appID := strings.TrimSpace(b.cfg.FastAPICloud.AppID); appID != "" {
		app, err := client.GetApp(ctx, appID)
		if err != nil {
			return nil, err
		}
		return []LeaseView{fastAPICloudServer(app)}, nil
	}
	return nil, exit(2, "provider=%s list requires --fastapi-cloud-team-id or --fastapi-cloud-app-id", providerName)
}

func (b *fastAPICloudBackend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	if strings.TrimSpace(b.cfg.FastAPICloud.AppID) != "" {
		if _, err := b.Status(ctx, StatusRequest{}); err != nil {
			return DoctorResult{}, err
		}
		return inventoryDoctorResult(providerName, 1), nil
	}
	servers, err := b.List(ctx, ListRequest{})
	if err != nil {
		return DoctorResult{}, err
	}
	return inventoryDoctorResult(providerName, len(servers)), nil
}

func (b *fastAPICloudBackend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	appID := strings.TrimSpace(req.ID)
	if appID == "" {
		appID = strings.TrimSpace(b.cfg.FastAPICloud.AppID)
	}
	if appID == "" {
		return StatusView{}, exit(2, "provider=%s status requires --id <fastapi-cloud-app-id> or --fastapi-cloud-app-id", providerName)
	}
	client, err := b.api()
	if err != nil {
		return StatusView{}, err
	}
	app, err := client.GetApp(ctx, appID)
	if err != nil {
		return StatusView{}, err
	}
	deployment, ok, err := client.LatestDeployment(ctx, appID)
	if err != nil {
		return StatusView{}, err
	}
	state := "no-deployment"
	ready := false
	if ok {
		state = deployment.Status.State()
		ready = deployment.Status.IsReady()
	}
	view := StatusView{
		ID:         app.ID,
		Slug:       app.Slug,
		Provider:   providerName,
		TargetOS:   targetLinux,
		State:      state,
		ServerID:   app.ID,
		ServerType: "fastapi-cloud-app",
		Host:       hostFromAppURL(app.URL),
		Network:    networkPublic,
		Ready:      ready,
		Labels:     fastAPICloudLabels(app, deployment, ok),
	}
	return view, nil
}

func (b *fastAPICloudBackend) Stop(ctx context.Context, req StopRequest) error {
	_ = ctx
	_ = req
	return exit(2, "provider=%s does not support stop; FastAPI Cloud does not expose app stop/delete through this provider", providerName)
}

func (b *fastAPICloudBackend) api() (fastAPICloudAPI, error) {
	if b.client != nil {
		return b.client, nil
	}
	return newFastAPICloudClient(b.cfg, b.rt)
}

func fastAPICloudServer(app fastAPICloudApp) Server {
	return Server{
		CloudID:  app.ID,
		Provider: providerName,
		Name:     blank(app.Name, app.Slug),
		Labels:   fastAPICloudLabels(app, fastAPICloudDeployment{}, false),
	}
}

func fastAPICloudLabels(app fastAPICloudApp, deployment fastAPICloudDeployment, hasDeployment bool) map[string]string {
	labels := map[string]string{}
	addLabel(labels, "teamId", app.TeamID)
	addLabel(labels, "slug", app.Slug)
	addLabel(labels, "directory", app.Directory)
	addLabel(labels, "url", app.URL)
	addLabel(labels, "region", app.Region)
	addLabel(labels, "updatedAt", app.UpdatedAt)
	if hasDeployment {
		addLabel(labels, "deploymentId", deployment.ID)
		addLabel(labels, "deploymentSlug", deployment.Slug)
		addLabel(labels, "deploymentStatus", string(deployment.Status.Normalized()))
		addLabel(labels, "deploymentCreatedAt", deployment.CreatedAt)
		addLabel(labels, "deploymentUrl", deployment.URL)
		addLabel(labels, "deploymentDashboardUrl", deployment.DashboardURL)
	}
	return labels
}

func addLabel(labels map[string]string, key, value string) {
	if strings.TrimSpace(value) != "" {
		labels[key] = value
	}
}

func hostFromAppURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err == nil && parsed.Hostname() != "" {
		return parsed.Hostname()
	}
	return strings.TrimSpace(raw)
}

func rejectFastAPICloudRunOptions(req RunRequest) error {
	if req.Keep {
		return exit(2, "provider=%s lifecycle is owned by FastAPI Cloud; --keep is not supported", providerName)
	}
	if req.Reclaim {
		return exit(2, "provider=%s lifecycle is owned by FastAPI Cloud; --reclaim is not supported", providerName)
	}
	if !req.NoSync {
		return exit(2, "provider=%s does not support workspace sync; pass --no-sync", providerName)
	}
	if req.SyncOnly {
		return exit(2, "provider=%s does not support sync; --sync-only is rejected", providerName)
	}
	if req.ChecksumSync {
		return exit(2, "provider=%s does not support sync; --checksum is rejected", providerName)
	}
	if req.ForceSyncLarge {
		return exit(2, "provider=%s does not support sync; --force-sync-large is rejected", providerName)
	}
	if req.FullResync {
		return exit(2, "provider=%s does not support sync; --full-resync is rejected", providerName)
	}
	if req.ShellMode {
		return exit(2, "provider=%s cannot open an interactive shell; --shell is not supported", providerName)
	}
	if req.EnvSummary {
		return exit(2, "provider=%s cannot forward per-run environment variables", providerName)
	}
	return nil
}
