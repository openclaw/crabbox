package railway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const (
	railwayClaimServiceLabel     = "railwayServiceId"
	railwayClaimProjectLabel     = "railwayProjectId"
	railwayClaimEnvironmentLabel = "railwayEnvironmentId"
	railwayClaimDeploymentLabel  = "railwayDeploymentId"
)

func NewRailwayBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) core.Backend {
	cfg.Provider = providerName
	return &railwayBackend{spec: spec, cfg: cfg, rt: rt}
}

type railwayBackend struct {
	spec   core.ProviderSpec
	cfg    core.Config
	rt     core.Runtime
	client railwayAPI
}

func (b *railwayBackend) Spec() core.ProviderSpec { return b.spec }

func (b *railwayBackend) Warmup(ctx context.Context, req core.WarmupRequest) error {
	_ = ctx
	_ = req
	// Warmup is rejected because Railway services and projects must be created
	// out-of-band (the provider would otherwise leak billable resources if a
	// warmup were triggered accidentally). Use the Railway dashboard or CLI to
	// create the service, then point crabbox at it with --id <serviceId>.
	return core.Exit(2, "provider=%s does not support warmup; create the Railway service out-of-band", providerName)
}

func (b *railwayBackend) Run(ctx context.Context, req core.RunRequest) (core.RunResult, error) {
	_ = ctx
	if err := shared.RejectServiceRunOptions(req, providerName, "lifecycle is owned by Railway", "runs the Railway service start command"); err != nil {
		return core.RunResult{}, err
	}
	if req.ID == "" {
		return core.RunResult{}, core.Exit(2, "provider=%s requires --id <railway-service-id>", providerName)
	}
	if len(req.Command) == 0 {
		return core.RunResult{}, core.Exit(2, "missing command")
	}
	return core.RunResult{}, core.Exit(2, "provider=%s cannot execute arbitrary run commands; Railway only runs the service's configured start command", providerName)
}

func (b *railwayBackend) List(ctx context.Context, req core.ListRequest) ([]core.LeaseView, error) {
	_ = req
	client, err := b.api()
	if err != nil {
		return nil, err
	}
	services, err := client.ListServices(ctx)
	if err != nil {
		return nil, err
	}
	servers := make([]core.Server, 0, len(services))
	for _, s := range services {
		servers = append(servers, core.Server{
			CloudID:  s.ID,
			Provider: providerName,
			Name:     s.Name,
			Labels:   map[string]string{"projectId": s.ProjectID},
		})
	}
	return servers, nil
}

func (b *railwayBackend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	if _, _, err := b.requireProjectEnv(); err != nil {
		return core.DoctorResult{}, err
	}
	servers, err := b.List(ctx, core.ListRequest{})
	if err != nil {
		return core.DoctorResult{}, err
	}
	return core.InventoryDoctorResult(providerName, len(servers)), nil
}

func (b *railwayBackend) Status(ctx context.Context, req core.StatusRequest) (core.StatusView, error) {
	if req.ID == "" {
		return core.StatusView{}, core.Exit(2, "provider=%s status requires --id <railway-service-id>", providerName)
	}
	projectID, environmentID, err := b.requireProjectEnv()
	if err != nil {
		// Status accepts the legacy combined message because callers historically
		// piped --id-only requests through here.
		return core.StatusView{}, core.Exit(2, "provider=%s status requires --railway-project and --railway-environment", providerName)
	}
	client, err := b.api()
	if err != nil {
		return core.StatusView{}, err
	}

	// GetService and LatestDeployment are independent reads; fan them out in
	// parallel so a slow Railway region doesn't double the wall-clock cost of
	// a status check. Done with a WaitGroup rather than errgroup because the
	// repository does not depend on golang.org/x/sync.
	var (
		wg         sync.WaitGroup
		service    railwayService
		deployment railwayDeployment
		serviceErr error
		deployErr  error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		service, serviceErr = client.GetService(ctx, req.ID)
	}()
	go func() {
		defer wg.Done()
		deployment, deployErr = client.LatestDeployment(ctx, projectID, environmentID, req.ID)
	}()
	wg.Wait()
	if serviceErr != nil {
		return core.StatusView{}, serviceErr
	}
	if deployErr != nil {
		return core.StatusView{}, deployErr
	}

	view := core.StatusView{
		ID:         service.ID,
		Slug:       service.Name,
		Provider:   providerName,
		TargetOS:   targetLinux,
		State:      deployment.Status.State(),
		ServerID:   service.ID,
		ServerType: "railway-service",
		Network:    networkPublic,
		Ready:      deployment.Status.IsReady(),
		Labels:     map[string]string{"projectId": service.ProjectID},
	}
	return view, nil
}

func (b *railwayBackend) Stop(ctx context.Context, req core.StopRequest) error {
	claim, err := b.resolveStopClaim(req.ID)
	if err != nil {
		return err
	}
	service, deployment, err := b.railwayStopTarget(ctx, req.ID)
	if err != nil {
		return err
	}
	if deployment.ID != claim.Labels[railwayClaimDeploymentLabel] {
		return core.Exit(4, "provider=%s service=%s latest deployment changed from claimed %s to %s; inspect it and rerun stop --reclaim to adopt the new deployment", providerName, req.ID, claim.Labels[railwayClaimDeploymentLabel], deployment.ID)
	}
	return b.stopClaimedDeployment(ctx, service, deployment, claim)
}

func (b *railwayBackend) ReclaimAndStop(ctx context.Context, req core.StopRequest) error {
	if req.ID == "" {
		return core.Exit(2, "provider=%s stop requires --id <railway-service-id>", providerName)
	}
	service, deployment, err := b.railwayStopTarget(ctx, req.ID)
	if err != nil {
		return err
	}
	claimID := railwayClaimID(b.cfg, req.ID)
	previous, previousExists, err := core.ResolveLeaseClaim(claimID)
	if err != nil {
		return err
	}
	labels := railwayClaimLabels(b.cfg, req.ID, deployment.ID)
	claim, err := claimLeaseTargetForConfigIfUnchanged(
		claimID,
		b.cfg, core.Server{CloudID: req.ID, Provider: providerName, Name: service.Name, Labels: labels}, previous,
		previousExists,
	)
	if err != nil {
		return err
	}
	if err := validateRailwayStopClaim(b.cfg, req.ID, claim); err != nil {
		return err
	}
	return b.stopClaimedDeployment(ctx, service, deployment, claim)
}

func (b *railwayBackend) resolveStopClaim(serviceID string) (core.LeaseClaim, error) {
	if serviceID == "" {
		return core.LeaseClaim{}, core.Exit(2, "provider=%s stop requires --id <railway-service-id>", providerName)
	}
	if _, _, err := b.requireProjectEnv(); err != nil {
		return core.LeaseClaim{}, core.Exit(2, "provider=%s stop requires --railway-project and --railway-environment", providerName)
	}
	claim, ok, err := resolveLeaseClaimForProviderCloudID(serviceID)
	if err != nil {
		return core.LeaseClaim{}, err
	}
	if !ok {
		return core.LeaseClaim{}, core.Exit(4, "provider=%s service=%s is not claimed; inspect the configured project and environment, then use stop --reclaim for explicit one-deployment adoption", providerName, serviceID)
	}
	if err := validateRailwayStopClaim(b.cfg, serviceID, claim); err != nil {
		return core.LeaseClaim{}, err
	}
	return claim, nil
}

func (b *railwayBackend) railwayStopTarget(ctx context.Context, serviceID string) (railwayService, railwayDeployment, error) {
	if serviceID == "" {
		return railwayService{}, railwayDeployment{}, core.Exit(2, "provider=%s stop requires --id <railway-service-id>", providerName)
	}
	projectID, environmentID, err := b.requireProjectEnv()
	if err != nil {
		return railwayService{}, railwayDeployment{}, core.Exit(2, "provider=%s stop requires --railway-project and --railway-environment", providerName)
	}
	client, err := b.api()
	if err != nil {
		return railwayService{}, railwayDeployment{}, err
	}
	service, err := client.GetService(ctx, serviceID)
	if err != nil {
		return railwayService{}, railwayDeployment{}, err
	}
	if service.ID != serviceID || service.ProjectID != projectID {
		return railwayService{}, railwayDeployment{}, core.Exit(4, "provider=%s service=%s does not belong to configured project=%s", providerName, serviceID, projectID)
	}
	deployment, err := client.LatestDeployment(ctx, projectID, environmentID, serviceID)
	if err != nil {
		return railwayService{}, railwayDeployment{}, err
	}
	if deployment.ID == "" {
		return railwayService{}, railwayDeployment{}, core.Exit(5, "provider=%s service=%s has no deployment to stop", providerName, serviceID)
	}
	return service, deployment, nil
}

func (b *railwayBackend) stopClaimedDeployment(ctx context.Context, service railwayService, deployment railwayDeployment, claim core.LeaseClaim) error {
	client, err := b.api()
	if err != nil {
		return err
	}
	updated, err := core.UpdateLeaseClaimLabelsIfUnchangedAfter(claim.LeaseID, claim, claim.Labels, func() error {
		return client.StopDeployment(ctx, deployment.ID)
	})
	if err != nil {
		return err
	}
	if err := core.RemoveLeaseClaimIfUnchanged(updated.LeaseID, updated); err != nil {
		return fmt.Errorf("remove stopped Railway claim: %w", err)
	}
	fmt.Fprintf(b.rt.Stderr, "stopped %s service=%s deployment=%s\n", providerName, service.ID, deployment.ID)
	return nil
}

func railwayClaimID(cfg core.Config, serviceID string) string {
	sum := sha256.Sum256([]byte(providerClaimScope(cfg) + "\x00" + strings.TrimSpace(serviceID)))
	return "railway_" + hex.EncodeToString(sum[:16])
}

func railwayClaimLabels(cfg core.Config, serviceID, deploymentID string) map[string]string {
	return map[string]string{
		railwayClaimServiceLabel:     strings.TrimSpace(serviceID),
		railwayClaimProjectLabel:     strings.TrimSpace(cfg.Railway.ProjectID),
		railwayClaimEnvironmentLabel: strings.TrimSpace(cfg.Railway.EnvironmentID),
		railwayClaimDeploymentLabel:  strings.TrimSpace(deploymentID),
	}
}

func validateRailwayStopClaim(cfg core.Config, serviceID string, claim core.LeaseClaim) error {
	expectedID := railwayClaimID(cfg, serviceID)
	expectedLabels := railwayClaimLabels(cfg, serviceID, claim.Labels[railwayClaimDeploymentLabel])
	if claim.LeaseID != expectedID || claim.Provider != providerName || claim.CloudID != serviceID || claim.ProviderScope != providerClaimScope(cfg) {
		return core.Exit(4, "provider=%s service=%s local claim does not match the configured endpoint, project, and environment; use stop --reclaim only after inspecting the target", providerName, serviceID)
	}
	if expectedLabels[railwayClaimDeploymentLabel] == "" {
		return core.Exit(4, "provider=%s service=%s local claim has no exact deployment binding", providerName, serviceID)
	}
	for key, expected := range expectedLabels {
		if claim.Labels[key] != expected {
			return core.Exit(4, "provider=%s service=%s local claim %s mismatch", providerName, serviceID, key)
		}
	}
	return nil
}

func (b *railwayBackend) api() (railwayAPI, error) {
	if b.client != nil {
		return b.client, nil
	}
	return newRailwayClient(b.cfg, b.rt)
}

// requireProjectEnv reads and trims the Railway project + environment ids and
// returns a CLI-facing exit error when either is missing. Callers route the
// error directly out to the user.
func (b *railwayBackend) requireProjectEnv() (string, string, error) {
	projectID := strings.TrimSpace(b.cfg.Railway.ProjectID)
	environmentID := strings.TrimSpace(b.cfg.Railway.EnvironmentID)
	if projectID == "" {
		return "", "", core.Exit(2, "provider=%s requires --railway-project or RAILWAY_PROJECT_ID", providerName)
	}
	if environmentID == "" {
		return "", "", core.Exit(2, "provider=%s requires --railway-environment or RAILWAY_ENVIRONMENT_ID", providerName)
	}
	return projectID, environmentID, nil
}
