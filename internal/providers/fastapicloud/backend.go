package fastapicloud

import (
	"context"
	"net/url"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func NewFastAPICloudBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) core.Backend {
	cfg.Provider = providerName
	return &fastAPICloudBackend{spec: spec, cfg: cfg, rt: rt}
}

type fastAPICloudBackend struct {
	spec   core.ProviderSpec
	cfg    core.Config
	rt     core.Runtime
	client fastAPICloudAPI
}

func (b *fastAPICloudBackend) Spec() core.ProviderSpec { return b.spec }

func (b *fastAPICloudBackend) Warmup(ctx context.Context, req core.WarmupRequest) error {
	_ = ctx
	_ = req
	return core.Exit(2, "provider=%s does not support warmup; create and deploy the FastAPI Cloud app out-of-band", providerName)
}

func (b *fastAPICloudBackend) Run(ctx context.Context, req core.RunRequest) (core.RunResult, error) {
	_ = ctx
	if err := shared.RejectServiceRunOptions(req, providerName, "lifecycle is owned by FastAPI Cloud", "cannot open an interactive shell"); err != nil {
		return core.RunResult{}, err
	}
	if len(req.Command) == 0 {
		return core.RunResult{}, core.Exit(2, "missing command")
	}
	return core.RunResult{}, core.Exit(2, "provider=%s cannot execute arbitrary run commands; deploy with fastapi deploy or FastAPI Cloud CI", providerName)
}

func (b *fastAPICloudBackend) List(ctx context.Context, req core.ListRequest) ([]core.LeaseView, error) {
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
		servers := make([]core.Server, 0, len(apps))
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
		return []core.LeaseView{fastAPICloudServer(app)}, nil
	}
	return nil, core.Exit(2, "provider=%s list requires --fastapi-cloud-team-id or --fastapi-cloud-app-id", providerName)
}

func (b *fastAPICloudBackend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	if strings.TrimSpace(b.cfg.FastAPICloud.AppID) != "" {
		if _, err := b.Status(ctx, core.StatusRequest{}); err != nil {
			return core.DoctorResult{}, err
		}
		return core.InventoryDoctorResult(providerName, 1), nil
	}
	servers, err := b.List(ctx, core.ListRequest{})
	if err != nil {
		return core.DoctorResult{}, err
	}
	return core.InventoryDoctorResult(providerName, len(servers)), nil
}

func (b *fastAPICloudBackend) Status(ctx context.Context, req core.StatusRequest) (core.StatusView, error) {
	appID := strings.TrimSpace(req.ID)
	if appID == "" {
		appID = strings.TrimSpace(b.cfg.FastAPICloud.AppID)
	}
	if appID == "" {
		return core.StatusView{}, core.Exit(2, "provider=%s status requires --id <fastapi-cloud-app-id> or --fastapi-cloud-app-id", providerName)
	}
	client, err := b.api()
	if err != nil {
		return core.StatusView{}, err
	}
	app, err := client.GetApp(ctx, appID)
	if err != nil {
		return core.StatusView{}, err
	}
	deployment, ok, err := client.LatestDeployment(ctx, appID)
	if err != nil {
		return core.StatusView{}, err
	}
	state := "no-deployment"
	ready := false
	if ok {
		state = deployment.Status.State()
		ready = deployment.Status.IsReady()
	}
	view := core.StatusView{
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

func (b *fastAPICloudBackend) Stop(ctx context.Context, req core.StopRequest) error {
	_ = ctx
	_ = req
	return core.Exit(2, "provider=%s does not support stop; FastAPI Cloud does not expose app stop/delete through this provider", providerName)
}

func (b *fastAPICloudBackend) api() (fastAPICloudAPI, error) {
	if b.client != nil {
		return b.client, nil
	}
	return newFastAPICloudClient(b.cfg, b.rt)
}

func fastAPICloudServer(app fastAPICloudApp) core.Server {
	return core.Server{
		CloudID:  app.ID,
		Provider: providerName,
		Name:     core.Blank(app.Name, app.Slug),
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
