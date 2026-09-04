package railway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
)

const (
	railwayClaimServiceLabel     = "railwayServiceId"
	railwayClaimProjectLabel     = "railwayProjectId"
	railwayClaimEnvironmentLabel = "railwayEnvironmentId"
	railwayClaimDeploymentLabel  = "railwayDeploymentId"
)

func NewRailwayBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = providerName
	return &railwayBackend{spec: spec, cfg: cfg, rt: rt}
}

type railwayBackend struct {
	spec   ProviderSpec
	cfg    Config
	rt     Runtime
	client railwayAPI
}

func (b *railwayBackend) Spec() ProviderSpec { return b.spec }

func (b *railwayBackend) Warmup(ctx context.Context, req WarmupRequest) error {
	_ = ctx
	_ = req
	// Warmup is rejected because Railway services and projects must be created
	// out-of-band (the provider would otherwise leak billable resources if a
	// warmup were triggered accidentally). Use the Railway dashboard or CLI to
	// create the service, then point crabbox at it with --id <serviceId>.
	return exit(2, "provider=%s does not support warmup; create the Railway service out-of-band", providerName)
}

func (b *railwayBackend) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	_ = ctx
	if err := rejectRailwayRunOptions(req); err != nil {
		return RunResult{}, err
	}
	if req.ID == "" {
		return RunResult{}, exit(2, "provider=%s requires --id <railway-service-id>", providerName)
	}
	if len(req.Command) == 0 {
		return RunResult{}, exit(2, "missing command")
	}
	return RunResult{}, exit(2, "provider=%s cannot execute arbitrary run commands; Railway only runs the service's configured start command", providerName)
}

func (b *railwayBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	_ = req
	client, err := b.api()
	if err != nil {
		return nil, err
	}
	services, err := client.ListServices(ctx)
	if err != nil {
		return nil, err
	}
	servers := make([]Server, 0, len(services))
	for _, s := range services {
		servers = append(servers, Server{
			CloudID:  s.ID,
			Provider: providerName,
			Name:     s.Name,
			Labels:   map[string]string{"projectId": s.ProjectID},
		})
	}
	return servers, nil
}

func (b *railwayBackend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	if _, _, err := b.requireProjectEnv(); err != nil {
		return DoctorResult{}, err
	}
	servers, err := b.List(ctx, ListRequest{})
	if err != nil {
		return DoctorResult{}, err
	}
	return inventoryDoctorResult(providerName, len(servers)), nil
}

func (b *railwayBackend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	if req.ID == "" {
		return StatusView{}, exit(2, "provider=%s status requires --id <railway-service-id>", providerName)
	}
	projectID, environmentID, err := b.requireProjectEnv()
	if err != nil {
		// Status accepts the legacy combined message because callers historically
		// piped --id-only requests through here.
		return StatusView{}, exit(2, "provider=%s status requires --railway-project and --railway-environment", providerName)
	}
	client, err := b.api()
	if err != nil {
		return StatusView{}, err
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
		return StatusView{}, serviceErr
	}
	if deployErr != nil {
		return StatusView{}, deployErr
	}

	view := StatusView{
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

func (b *railwayBackend) Stop(ctx context.Context, req StopRequest) error {
	claim, err := b.resolveStopClaim(req.ID)
	if err != nil {
		return err
	}
	service, deployment, err := b.railwayStopTarget(ctx, req.ID)
	if err != nil {
		return err
	}
	if deployment.ID != claim.Labels[railwayClaimDeploymentLabel] {
		return exit(4, "provider=%s service=%s latest deployment changed from claimed %s to %s; inspect it and rerun stop --reclaim to adopt the new deployment", providerName, req.ID, claim.Labels[railwayClaimDeploymentLabel], deployment.ID)
	}
	return b.stopClaimedDeployment(ctx, service, deployment, claim)
}

func (b *railwayBackend) ReclaimAndStop(ctx context.Context, req StopRequest) error {
	if req.ID == "" {
		return exit(2, "provider=%s stop requires --id <railway-service-id>", providerName)
	}
	service, deployment, err := b.railwayStopTarget(ctx, req.ID)
	if err != nil {
		return err
	}
	claimID := railwayClaimID(b.cfg, req.ID)
	previous, previousExists, err := resolveLeaseClaim(claimID)
	if err != nil {
		return err
	}
	labels := railwayClaimLabels(b.cfg, req.ID, deployment.ID)
	claim, err := claimLeaseTargetForConfigIfUnchanged(
		claimID,
		b.cfg,
		Server{CloudID: req.ID, Provider: providerName, Name: service.Name, Labels: labels},
		previous,
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

func (b *railwayBackend) resolveStopClaim(serviceID string) (LeaseClaim, error) {
	if serviceID == "" {
		return LeaseClaim{}, exit(2, "provider=%s stop requires --id <railway-service-id>", providerName)
	}
	if _, _, err := b.requireProjectEnv(); err != nil {
		return LeaseClaim{}, exit(2, "provider=%s stop requires --railway-project and --railway-environment", providerName)
	}
	claim, ok, err := resolveLeaseClaimForProviderCloudID(serviceID)
	if err != nil {
		return LeaseClaim{}, err
	}
	if !ok {
		return LeaseClaim{}, exit(4, "provider=%s service=%s is not claimed; inspect the configured project and environment, then use stop --reclaim for explicit one-deployment adoption", providerName, serviceID)
	}
	if err := validateRailwayStopClaim(b.cfg, serviceID, claim); err != nil {
		return LeaseClaim{}, err
	}
	return claim, nil
}

func (b *railwayBackend) railwayStopTarget(ctx context.Context, serviceID string) (railwayService, railwayDeployment, error) {
	if serviceID == "" {
		return railwayService{}, railwayDeployment{}, exit(2, "provider=%s stop requires --id <railway-service-id>", providerName)
	}
	projectID, environmentID, err := b.requireProjectEnv()
	if err != nil {
		return railwayService{}, railwayDeployment{}, exit(2, "provider=%s stop requires --railway-project and --railway-environment", providerName)
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
		return railwayService{}, railwayDeployment{}, exit(4, "provider=%s service=%s does not belong to configured project=%s", providerName, serviceID, projectID)
	}
	deployment, err := client.LatestDeployment(ctx, projectID, environmentID, serviceID)
	if err != nil {
		return railwayService{}, railwayDeployment{}, err
	}
	if deployment.ID == "" {
		return railwayService{}, railwayDeployment{}, exit(5, "provider=%s service=%s has no deployment to stop", providerName, serviceID)
	}
	return service, deployment, nil
}

func (b *railwayBackend) stopClaimedDeployment(ctx context.Context, service railwayService, deployment railwayDeployment, claim LeaseClaim) error {
	client, err := b.api()
	if err != nil {
		return err
	}
	updated, err := updateLeaseClaimLabelsIfUnchangedAfter(claim.LeaseID, claim, claim.Labels, func() error {
		return client.StopDeployment(ctx, deployment.ID)
	})
	if err != nil {
		return err
	}
	if err := removeLeaseClaimIfUnchanged(updated.LeaseID, updated); err != nil {
		return fmt.Errorf("remove stopped Railway claim: %w", err)
	}
	fmt.Fprintf(b.rt.Stderr, "stopped %s service=%s deployment=%s\n", providerName, service.ID, deployment.ID)
	return nil
}

func railwayClaimID(cfg Config, serviceID string) string {
	sum := sha256.Sum256([]byte(providerClaimScope(cfg) + "\x00" + strings.TrimSpace(serviceID)))
	return "railway_" + hex.EncodeToString(sum[:16])
}

func railwayClaimLabels(cfg Config, serviceID, deploymentID string) map[string]string {
	return map[string]string{
		railwayClaimServiceLabel:     strings.TrimSpace(serviceID),
		railwayClaimProjectLabel:     strings.TrimSpace(cfg.Railway.ProjectID),
		railwayClaimEnvironmentLabel: strings.TrimSpace(cfg.Railway.EnvironmentID),
		railwayClaimDeploymentLabel:  strings.TrimSpace(deploymentID),
	}
}

func validateRailwayStopClaim(cfg Config, serviceID string, claim LeaseClaim) error {
	expectedID := railwayClaimID(cfg, serviceID)
	expectedLabels := railwayClaimLabels(cfg, serviceID, claim.Labels[railwayClaimDeploymentLabel])
	if claim.LeaseID != expectedID || claim.Provider != providerName || claim.CloudID != serviceID || claim.ProviderScope != providerClaimScope(cfg) {
		return exit(4, "provider=%s service=%s local claim does not match the configured endpoint, project, and environment; use stop --reclaim only after inspecting the target", providerName, serviceID)
	}
	if expectedLabels[railwayClaimDeploymentLabel] == "" {
		return exit(4, "provider=%s service=%s local claim has no exact deployment binding", providerName, serviceID)
	}
	for key, expected := range expectedLabels {
		if claim.Labels[key] != expected {
			return exit(4, "provider=%s service=%s local claim %s mismatch", providerName, serviceID, key)
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
		return "", "", exit(2, "provider=%s requires --railway-project or RAILWAY_PROJECT_ID", providerName)
	}
	if environmentID == "" {
		return "", "", exit(2, "provider=%s requires --railway-environment or RAILWAY_ENVIRONMENT_ID", providerName)
	}
	return projectID, environmentID, nil
}

func rejectRailwayRunOptions(req RunRequest) error {
	if req.Keep {
		return exit(2, "provider=%s lifecycle is owned by Railway; --keep is not supported", providerName)
	}
	if req.Reclaim {
		return exit(2, "provider=%s lifecycle is owned by Railway; --reclaim is not supported", providerName)
	}
	if !req.NoSync {
		// Railway does not expose a workspace-sync surface; mirror other
		// delegated-only providers and require --no-sync explicitly so callers
		// understand the deploy runs whatever the service is already configured
		// to run.
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
		return exit(2, "provider=%s runs the Railway service start command; --shell is not supported", providerName)
	}
	if req.EnvSummary {
		return exit(2, "provider=%s cannot forward per-run environment variables", providerName)
	}
	return nil
}
