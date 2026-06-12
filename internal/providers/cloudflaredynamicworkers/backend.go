package cloudflaredynamicworkers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

type backend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

func NewBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = providerName
	cfg.TargetOS = targetWorker
	return &backend{spec: spec, cfg: cfg, rt: rt}
}

func (b *backend) Spec() ProviderSpec { return b.spec }

func (b *backend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	client, err := newLoaderAPI(b.cfg, b.rt)
	if err != nil {
		return DoctorResult{}, err
	}
	readiness, err := client.Readiness(ctx)
	if err != nil {
		return DoctorResult{}, providerError("readiness", err)
	}
	checks := []DoctorCheck{
		{Status: "pass", Check: "loader-api", Message: "readiness endpoint reachable"},
	}
	status := "pass"
	if !readiness.OK || readiness.Runner != providerName || !readiness.LoaderBinding {
		status = "fail"
		if !readiness.OK {
			checks = append(checks, DoctorCheck{Status: "fail", Check: "ok", Message: "readiness did not report ok=true"})
		}
		if readiness.Runner != providerName {
			checks = append(checks, DoctorCheck{Status: "fail", Check: "runner", Message: fmt.Sprintf("readiness runner=%s", blank(readiness.Runner, "-"))})
		}
		if !readiness.LoaderBinding {
			checks = append(checks, DoctorCheck{Status: "fail", Check: "loader-binding", Message: "Dynamic Workers binding unavailable"})
		}
	}
	return DoctorResult{
		Provider: providerName,
		Status:   status,
		Message:  fmt.Sprintf("auth=ready control_plane=ready api=readiness mutation=false runner=%s loader_binding=%t compatibility_date=%s egress=%s", blank(readiness.Runner, "-"), readiness.LoaderBinding, blank(readiness.CompatibilityDate, "-"), blank(readiness.Egress, "-")),
		Checks:   checks,
	}, nil
}

func (b *backend) Warmup(ctx context.Context, req WarmupRequest) error {
	if req.ActionsRunner {
		return exit(2, "--actions-runner is not supported for provider=%s", providerName)
	}
	started := now(b.rt)
	client, err := newLoaderAPI(b.cfg, b.rt)
	if err != nil {
		return err
	}
	readiness, err := client.Readiness(ctx)
	if err != nil {
		return providerError("readiness", err)
	}
	if !readiness.OK || readiness.Runner != providerName || !readiness.LoaderBinding {
		return exit(5, "%s readiness failed runner=%s loader_binding=%t", providerName, blank(readiness.Runner, "-"), readiness.LoaderBinding)
	}
	leaseID := ""
	slug := ""
	if req.Keep || strings.TrimSpace(req.RequestedSlug) != "" {
		leaseID = newLeaseID()
		slug, err = allocateClaimLeaseSlug(leaseID, req.RequestedSlug)
		if err != nil {
			return err
		}
		server := runServer(leaseID, slug, runStatus{ID: leaseID, Status: "ready"}, map[string]string{
			"cache_mode": "warmup",
			"remote":     "false",
		})
		if err := claimLease(leaseID, slug, b.cfg, req.Repo.Root, b.cfg.IdleTimeout, req.Reclaim, server); err != nil {
			return err
		}
		fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s local_claim=true\n", leaseID, slug, providerName)
	}
	total := now(b.rt).Sub(started)
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
	if err := rejectDelegatedSyncOptions(b.spec, req); err != nil {
		return RunResult{}, err
	}
	if req.Script == nil || len(req.Script.Data) == 0 {
		return RunResult{}, exit(2, "%s requires --script or --script-stdin module source", providerName)
	}
	started := now(b.rt)
	client, err := newLoaderAPI(b.cfg, b.rt)
	if err != nil {
		return RunResult{}, err
	}
	cacheMode := normalizeCacheMode(b.cfg.CloudflareDynamicWorkers.CacheMode)
	if cacheMode == "" {
		cacheMode = "stable"
	}
	leaseID, slug, reused, err := b.runIdentity(req, cacheMode)
	if err != nil {
		return RunResult{}, err
	}
	loaderReq := b.buildRunRequest(req, leaseID, cacheMode)
	if req.EnvSummary {
		printEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, req.Env)
	}
	commandStarted := now(b.rt)
	run, err := client.Run(ctx, loaderReq)
	commandDuration := now(b.rt).Sub(commandStarted)
	if err != nil {
		result := RunResult{ExitCode: 1, Command: commandDuration, Total: now(b.rt).Sub(started), Provider: providerName, LeaseID: leaseID, Slug: slug}
		shouldStop := false
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, false, &shouldStop)
		return result, ExitError{Code: 1, Message: fmt.Sprintf("%s run failed: %v", providerName, err)}
	}
	if strings.TrimSpace(run.ID) != "" {
		leaseID = run.ID
	}
	writeRunOutput(b.rt.Stdout, b.rt.Stderr, run)
	exitCode := run.ExitCode
	if exitCode == 0 && !runSucceeded(run.Status) {
		exitCode = 1
	}
	total := now(b.rt).Sub(started)
	result := RunResult{
		ExitCode:    exitCode,
		Command:     commandDuration,
		Total:       total,
		Provider:    providerName,
		LeaseID:     leaseID,
		Slug:        slug,
		CommandText: req.Script.Source,
		Session: (&coreRunSessionHandle{
			Provider:       providerName,
			LeaseID:        leaseID,
			Slug:           slug,
			Reused:         reused,
			Kept:           req.Keep || cacheMode == "explicit",
			RunID:          leaseID,
			CleanupCommand: fmt.Sprintf("crabbox stop --provider %s --id %s", providerName, leaseID),
		}).toCore(),
	}
	if req.Keep || cacheMode == "explicit" {
		server := runServer(leaseID, slug, runStatus{ID: leaseID, Status: run.Status, Metadata: run.Metadata}, map[string]string{
			"cache_mode": cacheMode,
			"egress":     normalizeEgress(b.cfg.CloudflareDynamicWorkers.Egress),
		})
		if err := claimLease(leaseID, slug, b.cfg, req.Repo.Root, b.cfg.IdleTimeout, req.Reclaim, server); err != nil {
			return result, err
		}
	}
	if req.TimingJSON {
		if err := writeTimingJSON(b.rt.Stderr, timingReport{
			Provider:  providerName,
			LeaseID:   leaseID,
			Slug:      slug,
			CommandMs: commandDuration.Milliseconds(),
			TotalMs:   total.Milliseconds(),
			ExitCode:  exitCode,
			Label:     strings.TrimSpace(req.Label),
		}); err != nil {
			return result, err
		}
	}
	if exitCode != 0 {
		shouldStop := false
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, false, &shouldStop)
		return result, ExitError{Code: exitCode, Message: fmt.Sprintf("%s run exited %d", providerName, exitCode)}
	}
	return result, nil
}

func (b *backend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	claims, err := providerClaims()
	if err != nil {
		return nil, err
	}
	if !req.Refresh {
		views := make([]LeaseView, 0, len(claims))
		for _, claim := range claims {
			views = append(views, claimServer(claim, "unknown"))
		}
		return views, nil
	}
	client, err := newLoaderAPI(b.cfg, b.rt)
	if err != nil {
		return nil, err
	}
	views := make([]LeaseView, 0, len(claims))
	for _, claim := range claims {
		status, err := client.Status(ctx, claim.LeaseID)
		if err != nil {
			if notFoundError(err) {
				views = append(views, claimServer(claim, "missing"))
				continue
			}
			fmt.Fprintf(b.rt.Stderr, "warning: %s status failed for %s: %v\n", providerName, claim.LeaseID, err)
			views = append(views, claimServer(claim, "unknown"))
			continue
		}
		views = append(views, runServer(claim.LeaseID, blank(claim.Slug, newLeaseSlug(claim.LeaseID)), status, mergeRunLabels(claim.Labels, status.Metadata)))
	}
	return views, nil
}

func (b *backend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	client, err := newLoaderAPI(b.cfg, b.rt)
	if err != nil {
		return StatusView{}, err
	}
	leaseID, slug, err := b.resolveRunID(req.ID, "", false)
	if err != nil {
		return StatusView{}, err
	}
	deadline := now(b.rt).Add(req.WaitTimeout)
	if req.WaitTimeout <= 0 {
		deadline = now(b.rt).Add(5 * time.Minute)
	}
	for {
		status, err := client.Status(ctx, leaseID)
		if err != nil {
			if notFoundError(err) {
				return statusView(leaseID, slug, runStatus{ID: leaseID, Status: "missing"}), nil
			}
			return StatusView{}, providerError("status", err)
		}
		view := statusView(leaseID, slug, status)
		if !req.Wait || view.Ready || terminalState(view.State) {
			return view, nil
		}
		if now(b.rt).After(deadline) {
			return StatusView{}, exit(5, "timed out waiting for %s run %s to become ready", providerName, leaseID)
		}
		select {
		case <-ctx.Done():
			return StatusView{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (b *backend) Stop(ctx context.Context, req StopRequest) error {
	client, err := newLoaderAPI(b.cfg, b.rt)
	if err != nil {
		return err
	}
	leaseID, _, err := b.resolveRunID(req.ID, "", false)
	if err != nil {
		return err
	}
	if err := client.Delete(ctx, leaseID); err != nil {
		if notFoundError(err) {
			removeLeaseClaim(leaseID)
			fmt.Fprintf(b.rt.Stdout, "removed stale %s claim %s reason=not-found\n", providerName, leaseID)
			return nil
		}
		return providerError("delete metadata", err)
	}
	removeLeaseClaim(leaseID)
	fmt.Fprintf(b.rt.Stdout, "stopped %s provider=%s loader_metadata=%s\n", leaseID, providerName, leaseID)
	return nil
}

func (b *backend) Cleanup(ctx context.Context, req CleanupRequest) error {
	client, err := newLoaderAPI(b.cfg, b.rt)
	if err != nil {
		return err
	}
	claims, err := providerClaims()
	if err != nil {
		return err
	}
	removed := 0
	for _, claim := range claims {
		status, err := client.Status(ctx, claim.LeaseID)
		if err != nil {
			if !notFoundError(err) {
				fmt.Fprintf(b.rt.Stderr, "warning: %s status failed for %s: %v\n", providerName, claim.LeaseID, err)
				continue
			}
			if req.DryRun {
				fmt.Fprintf(b.rt.Stdout, "would remove stale %s claim %s slug=%s reason=not-found\n", providerName, claim.LeaseID, blank(claim.Slug, "-"))
				continue
			}
			removeLeaseClaim(claim.LeaseID)
			removed++
			fmt.Fprintf(b.rt.Stdout, "removed stale %s claim %s slug=%s reason=not-found\n", providerName, claim.LeaseID, blank(claim.Slug, "-"))
			continue
		}
		if !terminalState(status.Status) {
			continue
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would remove stale %s claim %s slug=%s state=%s\n", providerName, claim.LeaseID, blank(claim.Slug, "-"), status.Status)
			continue
		}
		removeLeaseClaim(claim.LeaseID)
		removed++
		fmt.Fprintf(b.rt.Stdout, "removed stale %s claim %s slug=%s state=%s\n", providerName, claim.LeaseID, blank(claim.Slug, "-"), status.Status)
	}
	if !req.DryRun {
		fmt.Fprintf(b.rt.Stdout, "%s cleanup removed=%d checked=%d\n", providerName, removed, len(claims))
	}
	return nil
}

func (b *backend) runIdentity(req RunRequest, cacheMode string) (string, string, bool, error) {
	if cacheMode == "explicit" {
		if strings.TrimSpace(req.ID) == "" {
			return "", "", false, exit(2, "%s cache=explicit requires --id", providerName)
		}
		leaseID, slug, err := b.resolveRunID(req.ID, req.Repo.Root, req.Reclaim)
		return leaseID, slug, true, err
	}
	if strings.TrimSpace(req.ID) != "" {
		leaseID, slug, err := b.resolveRunID(req.ID, req.Repo.Root, req.Reclaim)
		return leaseID, slug, true, err
	}
	if cacheMode == "stable" {
		leaseID := stableRunID(req.Script.Data, b.cfg.CloudflareDynamicWorkers, req.Env)
		return leaseID, newLeaseSlug(leaseID), false, nil
	}
	leaseID := ""
	slug := ""
	if req.Keep {
		leaseID = newLeaseID()
		var err error
		slug, err = allocateClaimLeaseSlug(leaseID, req.RequestedSlug)
		if err != nil {
			return "", "", false, err
		}
	}
	return leaseID, slug, false, nil
}

func (b *backend) resolveRunID(identifier, repoRoot string, reclaim bool) (string, string, error) {
	claim, ok, err := resolveLeaseClaim(identifier)
	if err != nil {
		return "", "", err
	}
	if ok {
		if repoRoot != "" {
			server := claimServer(claim, blank(claim.Labels["state"], "unknown"))
			if err := claimLease(claim.LeaseID, claim.Slug, b.cfg, repoRoot, time.Duration(claim.IdleTimeoutSeconds)*time.Second, reclaim, server); err != nil {
				return "", "", err
			}
		}
		return claim.LeaseID, blank(claim.Slug, newLeaseSlug(claim.LeaseID)), nil
	}
	value := strings.TrimSpace(identifier)
	if value == "" {
		return "", "", exit(2, "%s id is required", providerName)
	}
	return value, newLeaseSlug(value), nil
}

func (b *backend) buildRunRequest(req RunRequest, leaseID, cacheMode string) runRequest {
	cfg := b.cfg.CloudflareDynamicWorkers
	return runRequest{
		ID:                 leaseID,
		CacheMode:          cacheMode,
		Module:             moduleSource{Name: req.Script.Source, Source: string(req.Script.Data)},
		CompatibilityDate:  strings.TrimSpace(cfg.CompatibilityDate),
		CompatibilityFlags: append([]string(nil), cfg.CompatibilityFlags...),
		Egress:             normalizeEgress(cfg.Egress),
		Limits:             limits{CPUMs: cfg.CPUMs, Subrequests: cfg.Subrequests},
		Env:                req.Env,
		Metadata:           runMetadata(cfg.Metadata, req),
		TimeoutMS:          durationMillisecondsCeil(time.Duration(cfg.TimeoutSecs) * time.Second),
	}
}

func runMetadata(configured map[string]string, req RunRequest) map[string]string {
	out := map[string]string{
		"provider": providerName,
		"source":   req.Script.Source,
	}
	if strings.TrimSpace(req.Label) != "" {
		out["label"] = strings.TrimSpace(req.Label)
	}
	for key, value := range configured {
		key = strings.TrimSpace(key)
		if key != "" {
			out[key] = value
		}
	}
	return out
}

func mergeRunLabels(local, live map[string]string) map[string]string {
	labels := make(map[string]string, len(local)+len(live))
	for key, value := range local {
		labels[key] = value
	}
	for key, value := range live {
		labels[key] = value
	}
	return labels
}

func writeRunOutput(stdout, stderr io.Writer, run runResponse) {
	if run.Stdout != "" && stdout != nil {
		_, _ = io.WriteString(stdout, run.Stdout)
	}
	if run.Body != "" && stdout != nil {
		_, _ = io.WriteString(stdout, run.Body)
	}
	if run.Stderr != "" && stderr != nil {
		_, _ = io.WriteString(stderr, run.Stderr)
	}
	if run.Logs != "" && stderr != nil {
		_, _ = io.WriteString(stderr, run.Logs)
	}
}

func stableRunID(source []byte, cfg CloudflareDynamicWorkersConfig, env map[string]string) string {
	h := sha256.New()
	_, _ = h.Write(source)
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(strings.TrimSpace(cfg.CompatibilityDate)))
	_, _ = h.Write([]byte{0})
	flags := append([]string(nil), cfg.CompatibilityFlags...)
	sort.Strings(flags)
	_, _ = h.Write([]byte(strings.Join(flags, ",")))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(normalizeEgress(cfg.Egress)))
	_, _ = h.Write([]byte(fmt.Sprintf("\x00%d\x00%d", cfg.CPUMs, cfg.Subrequests)))
	envKeys := make([]string, 0, len(env))
	for key := range env {
		envKeys = append(envKeys, key)
	}
	sort.Strings(envKeys)
	for _, key := range envKeys {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(key))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(env[key]))
	}
	return "cfdw_" + hex.EncodeToString(h.Sum(nil))[:24]
}

func normalizeCacheMode(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func normalizeEgress(value string) string {
	if strings.TrimSpace(value) == "" {
		return "blocked"
	}
	return strings.ToLower(strings.TrimSpace(value))
}

func durationMillisecondsCeil(duration time.Duration) int64 {
	if duration <= 0 {
		return 0
	}
	return int64((duration + time.Millisecond - 1) / time.Millisecond)
}

func providerClaims() ([]LeaseClaim, error) {
	claims, err := listLeaseClaims()
	if err != nil {
		return nil, err
	}
	out := make([]LeaseClaim, 0, len(claims))
	for _, claim := range claims {
		if claim.Provider == providerName {
			out = append(out, claim)
		}
	}
	return out, nil
}

func claimServer(claim LeaseClaim, state string) Server {
	return runServer(claim.LeaseID, blank(claim.Slug, newLeaseSlug(claim.LeaseID)), runStatus{ID: claim.LeaseID, Status: state}, claim.Labels)
}

func runServer(leaseID, slug string, status runStatus, extra map[string]string) Server {
	labels := map[string]string{
		"provider": providerName,
		"lease":    leaseID,
		"slug":     blank(slug, newLeaseSlug(leaseID)),
		"target":   targetWorker,
		"state":    blank(status.Status, "unknown"),
	}
	for key, value := range extra {
		if strings.TrimSpace(key) != "" {
			labels[key] = value
		}
	}
	labels["state"] = blank(status.Status, "unknown")
	server := Server{
		Provider: providerName,
		CloudID:  leaseID,
		Name:     leaseID,
		Status:   labels["state"],
		Labels:   labels,
	}
	server.ServerType.Name = "dynamic-worker"
	return server
}

func statusView(leaseID, slug string, status runStatus) StatusView {
	server := runServer(leaseID, slug, status, status.Metadata)
	return StatusView{
		ID:         leaseID,
		Slug:       blank(slug, newLeaseSlug(leaseID)),
		Provider:   providerName,
		TargetOS:   targetWorker,
		State:      server.Status,
		ServerID:   blank(status.ID, leaseID),
		ServerType: server.ServerType.Name,
		Ready:      readyState(server.Status),
		Labels:     server.Labels,
	}
}

func readyState(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "ready", "running", "completed", "succeeded", "success":
		return true
	default:
		return false
	}
}

func runSucceeded(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", "completed", "succeeded", "success", "ok":
		return true
	default:
		return false
	}
}

func terminalState(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "missing", "expired", "deleted", "stopped", "failed", "error", "completed", "succeeded", "success", "ok":
		return true
	default:
		return false
	}
}

func notFoundError(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *apiError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "404") || strings.Contains(text, "not found")
}

type coreRunSessionHandle struct {
	Provider       string
	LeaseID        string
	Slug           string
	Reused         bool
	Kept           bool
	RunID          string
	CleanupCommand string
}

func (h *coreRunSessionHandle) toCore() *RunSessionHandle {
	return &RunSessionHandle{
		Provider:       h.Provider,
		LeaseID:        h.LeaseID,
		Slug:           h.Slug,
		Reused:         h.Reused,
		Kept:           h.Kept,
		RunID:          h.RunID,
		CleanupCommand: h.CleanupCommand,
	}
}
