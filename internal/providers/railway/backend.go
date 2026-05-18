package railway

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// Polling configuration for Run(). Railway has no synchronous exec endpoint,
// so we trigger a deploy and then poll the new deployment's status until it
// reaches a terminal state. The interval grows exponentially with jitter so a
// long deployment doesn't hammer the API while a short one still feels snappy.
const (
	railwayPollInitialInterval  = 5 * time.Second
	railwayPollMaxInterval      = 30 * time.Second
	railwayPollOverallTimeout   = 30 * time.Minute
	railwayDeployResolveTimeout = 2 * time.Minute
	railwayPollJitterFraction   = 0.2
	railwayPollLogLimit         = 500
)

// railwayPollRand seeds a private rand.Source on first use so jitter doesn't
// rely on the global generator (which other tests may reseed) while staying
// dependency-free.
var (
	railwayPollRandOnce sync.Once
	railwayPollRand     *rand.Rand
	railwayPollRandMu   sync.Mutex
)

func railwayJitter(d time.Duration) time.Duration {
	railwayPollRandOnce.Do(func() {
		railwayPollRand = rand.New(rand.NewSource(time.Now().UnixNano()))
	})
	railwayPollRandMu.Lock()
	defer railwayPollRandMu.Unlock()
	// Centered jitter in [-fraction, +fraction] * d.
	delta := (railwayPollRand.Float64()*2 - 1) * railwayPollJitterFraction
	jittered := time.Duration(float64(d) * (1 + delta))
	if jittered <= 0 {
		return d
	}
	return jittered
}

func NewRailwayBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = providerName
	return &railwayBackend{spec: spec, cfg: cfg, rt: rt}
}

type railwayBackend struct {
	spec   ProviderSpec
	cfg    Config
	rt     Runtime
	client railwayAPI

	// pollOverride lets tests shrink the polling interval without changing the
	// production defaults. When zero, railwayPollInitialInterval is used.
	pollInitialOverride time.Duration
	// pollOverallOverride lets tests shorten the overall poll timeout.
	pollOverallOverride time.Duration
	// deployResolveOverride lets tests shorten boolean-trigger deployment resolution.
	deployResolveOverride time.Duration
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
	if err := rejectRailwayRunOptions(req); err != nil {
		return RunResult{}, err
	}
	if req.ID == "" {
		return RunResult{}, exit(2, "provider=%s requires --id <railway-service-id>", providerName)
	}
	projectID, environmentID, err := b.requireProjectEnv()
	if err != nil {
		return RunResult{}, err
	}
	if len(req.Command) == 0 {
		return RunResult{}, exit(2, "missing command")
	}
	client, err := b.api()
	if err != nil {
		return RunResult{}, err
	}
	started := b.now()
	fmt.Fprintf(b.rt.Stderr, "running on %s service=%s command=%s (start command is owned by the Railway service)\n", providerName, req.ID, strings.Join(req.Command, " "))

	previousDeployment, previousErr := client.LatestDeployment(ctx, projectID, environmentID, req.ID)
	deploymentID, err := client.TriggerDeploy(ctx, projectID, environmentID, req.ID)
	if err != nil {
		return RunResult{}, ExitError{Code: 1, Message: fmt.Sprintf("%s trigger deploy failed: %v", providerName, err)}
	}
	deploymentID = strings.TrimSpace(deploymentID)
	if deploymentID == "" {
		if previousErr != nil {
			return RunResult{ExitCode: 1}, ExitError{Code: 1, Message: fmt.Sprintf("%s read latest deployment before trigger failed: %v", providerName, previousErr)}
		}
		deploymentID, err = b.resolveTriggeredDeployment(ctx, client, projectID, environmentID, req.ID, previousDeployment.ID)
		if err != nil {
			return RunResult{ExitCode: 1}, ExitError{Code: 1, Message: fmt.Sprintf("%s resolve triggered deployment failed: %v", providerName, err)}
		}
	}

	logs := &railwayLogStreamer{out: b.rt.Stdout}
	finalStatus, pollErr := b.pollDeployment(ctx, client, deploymentID, logs)
	if pollErr != nil {
		commandDuration := b.now().Sub(started)
		return RunResult{ExitCode: 1, Command: commandDuration, Total: commandDuration}, ExitError{Code: 1, Message: fmt.Sprintf("%s deployment %s polling failed: %v", providerName, deploymentID, pollErr)}
	}

	commandDuration := b.now().Sub(started)
	result := RunResult{
		ExitCode: finalStatus.ExitCode(),
		Command:  commandDuration,
		Total:    commandDuration,
	}
	fmt.Fprintf(b.rt.Stderr, "%s run summary command=%s total=%s exit=%d status=%s\n", providerName, result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), result.ExitCode, finalStatus)
	if req.TimingJSON {
		if err := writeTimingJSON(b.rt.Stderr, timingReport{
			Provider:  providerName,
			CommandMs: commandDuration.Milliseconds(),
			TotalMs:   result.Total.Milliseconds(),
			ExitCode:  result.ExitCode,
		}); err != nil {
			return result, err
		}
	}
	if result.ExitCode != 0 {
		return result, ExitError{Code: result.ExitCode, Message: fmt.Sprintf("%s deployment status=%s", providerName, finalStatus)}
	}
	return result, nil
}

func (b *railwayBackend) resolveTriggeredDeployment(ctx context.Context, client railwayAPI, projectID, environmentID, serviceID, previousID string) (string, error) {
	overall := railwayDeployResolveTimeout
	if b.deployResolveOverride > 0 {
		overall = b.deployResolveOverride
	}
	deadlineCtx, cancel := context.WithTimeout(ctx, overall)
	defer cancel()

	interval := railwayPollInitialInterval
	if b.pollInitialOverride > 0 {
		interval = b.pollInitialOverride
	}
	for {
		deployment, err := client.LatestDeployment(deadlineCtx, projectID, environmentID, serviceID)
		if err != nil {
			if deadlineCtx.Err() != nil {
				return "", fmt.Errorf("deployment resolution cancelled: %w", deadlineCtx.Err())
			}
			return "", err
		}
		deploymentID := strings.TrimSpace(deployment.ID)
		if deploymentID != "" && deploymentID != strings.TrimSpace(previousID) {
			return deploymentID, nil
		}
		sleepFor := railwayJitter(interval)
		select {
		case <-deadlineCtx.Done():
			return "", fmt.Errorf("deployment resolution cancelled: %w", deadlineCtx.Err())
		case <-time.After(sleepFor):
		}
		if interval < railwayPollMaxInterval {
			interval *= 2
			if interval > railwayPollMaxInterval {
				interval = railwayPollMaxInterval
			}
		}
	}
}

// pollDeployment polls a specific deployment until it reaches a terminal state
// or the overall timeout / parent context expires. Returns the final observed
// status on success.
func (b *railwayBackend) pollDeployment(ctx context.Context, client railwayAPI, deploymentID string, logs *railwayLogStreamer) (railwayDeploymentStatus, error) {
	overall := railwayPollOverallTimeout
	if b.pollOverallOverride > 0 {
		overall = b.pollOverallOverride
	}
	deadlineCtx, cancel := context.WithTimeout(ctx, overall)
	defer cancel()

	initial := railwayPollInitialInterval
	if b.pollInitialOverride > 0 {
		initial = b.pollInitialOverride
	}
	interval := initial

	for {
		dep, err := client.Deployment(deadlineCtx, deploymentID)
		if err != nil {
			// Bubble up context errors as-is so the caller can tell timeout from
			// transport failures.
			if deadlineCtx.Err() != nil {
				return "", fmt.Errorf("polling cancelled: %w", deadlineCtx.Err())
			}
			return "", err
		}
		if dep.Status.IsTerminal() {
			if logs != nil {
				if err := logs.Flush(deadlineCtx, client, deploymentID); err != nil {
					return "", err
				}
			}
			return dep.Status, nil
		}
		if logs != nil {
			if err := logs.Flush(deadlineCtx, client, deploymentID); err != nil {
				return "", err
			}
		}
		// Backoff with ±jitter, capped at railwayPollMaxInterval.
		sleepFor := railwayJitter(interval)
		select {
		case <-deadlineCtx.Done():
			return "", fmt.Errorf("polling cancelled: %w", deadlineCtx.Err())
		case <-time.After(sleepFor):
		}
		if interval < railwayPollMaxInterval {
			interval *= 2
			if interval > railwayPollMaxInterval {
				interval = railwayPollMaxInterval
			}
		}
	}
}

type railwayLogStreamer struct {
	out            io.Writer
	buildSeen      []string
	deploymentSeen []string
}

func (s *railwayLogStreamer) Flush(ctx context.Context, client railwayAPI, deploymentID string) error {
	buildLogs, err := client.BuildLogs(ctx, deploymentID, railwayPollLogLimit)
	if err != nil {
		return fmt.Errorf("fetch build logs: %w", err)
	}
	s.buildSeen = printNewRailwayLogs(s.out, buildLogs, s.buildSeen)

	deploymentLogs, err := client.DeploymentLogs(ctx, deploymentID, railwayPollLogLimit)
	if err != nil {
		return fmt.Errorf("fetch deployment logs: %w", err)
	}
	s.deploymentSeen = printNewRailwayLogs(s.out, deploymentLogs, s.deploymentSeen)
	return nil
}

func printNewRailwayLogs(out io.Writer, lines []string, seen []string) []string {
	start := overlappingRailwayLogLines(seen, lines)
	for _, line := range lines[start:] {
		fmt.Fprintln(out, line)
	}
	next := make([]string, len(lines))
	copy(next, lines)
	return next
}

func overlappingRailwayLogLines(previous, current []string) int {
	n := len(previous)
	if len(current) < n {
		n = len(current)
	}
	for overlap := n; overlap > 0; overlap-- {
		match := true
		previousStart := len(previous) - overlap
		for i := 0; i < overlap; i++ {
			if previous[previousStart+i] != current[i] {
				match = false
				break
			}
		}
		if match {
			return overlap
		}
	}
	return 0
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
	if req.ID == "" {
		return exit(2, "provider=%s stop requires --id <railway-service-id>", providerName)
	}
	projectID, environmentID, err := b.requireProjectEnv()
	if err != nil {
		return exit(2, "provider=%s stop requires --railway-project and --railway-environment", providerName)
	}
	client, err := b.api()
	if err != nil {
		return err
	}
	deployment, err := client.LatestDeployment(ctx, projectID, environmentID, req.ID)
	if err != nil {
		return err
	}
	if deployment.ID == "" {
		return exit(5, "provider=%s service=%s has no deployment to stop", providerName, req.ID)
	}
	return client.StopDeployment(ctx, deployment.ID)
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

func (b *railwayBackend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now()
	}
	return time.Now()
}
