package cloudflaredynamicworkers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
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
	if !readiness.OK || readiness.Runner != providerName || !readiness.LoaderBinding || !readiness.CoordinatorBinding || !readiness.DurableRunMetadata {
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
		if !readiness.CoordinatorBinding {
			checks = append(checks, DoctorCheck{Status: "fail", Check: "coordinator-binding", Message: "RUN_COORDINATOR Durable Object binding unavailable"})
		}
		if !readiness.DurableRunMetadata {
			checks = append(checks, DoctorCheck{Status: "fail", Check: "run-metadata", Message: "RUNS KV binding unavailable"})
		}
	}
	return DoctorResult{
		Provider: providerName,
		Status:   status,
		Message:  fmt.Sprintf("auth=ready control_plane=ready api=readiness mutation=false runner=%s loader_binding=%t coordinator_binding=%t durable_run_metadata=%t compatibility_date=%s egress=%s", blank(readiness.Runner, "-"), readiness.LoaderBinding, readiness.CoordinatorBinding, readiness.DurableRunMetadata, blank(readiness.CompatibilityDate, "-"), blank(readiness.Egress, "-")),
		Checks:   checks,
	}, nil
}

func (b *backend) Warmup(context.Context, WarmupRequest) error {
	return exit(2, "provider=%s requires module source; use crabbox run --script <file>", providerName)
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
	if normalizeEgress(b.cfg.CloudflareDynamicWorkers.Egress) == "intercept" && cacheMode != "one-shot" {
		return RunResult{}, exit(2, "%s egress=intercept requires cache=one-shot because gateway context is run-scoped", providerName)
	}
	leaseID, workerID, slug, reused, err := b.runIdentity(req, cacheMode)
	if err != nil {
		return RunResult{}, err
	}
	loaderReq := b.buildRunRequest(req, leaseID, workerID, cacheMode)
	if req.EnvSummary {
		printEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, req.Env)
	}
	commandStarted := now(b.rt)
	run, err := client.Run(ctx, loaderReq)
	commandDuration := now(b.rt).Sub(commandStarted)
	if err != nil {
		total := now(b.rt).Sub(started)
		timingWritten := false
		result := RunResult{
			ExitCode:    1,
			Command:     commandDuration,
			Total:       total,
			Provider:    providerName,
			LeaseID:     leaseID,
			Slug:        slug,
			CommandText: req.Script.Source,
		}
		var apiErr *apiError
		var contractErr *responseContractError
		keepRequested := req.Keep || req.KeepOnFailure || cacheMode == "explicit"
		definitiveRejection := errors.As(err, &apiErr) &&
			!errors.As(err, &contractErr) &&
			apiErr.StatusCode >= http.StatusBadRequest &&
			apiErr.StatusCode < http.StatusInternalServerError &&
			apiErr.StatusCode != http.StatusRequestTimeout
		if leaseID != "" && keepRequested && !definitiveRejection {
			probeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			status, statusErr := client.Status(probeCtx, leaseID)
			cancel()
			if statusErr != nil {
				status = runStatus{ID: leaseID, WorkerID: workerID, Status: "unknown"}
			} else if strings.TrimSpace(status.WorkerID) == "" {
				status.WorkerID = workerID
			}
			if strings.TrimSpace(slug) == "" {
				slug = newLeaseSlug(leaseID)
				result.Slug = slug
			}
			server := runServer(leaseID, slug, status, mergeRunLabels(status.Metadata, map[string]string{
				"cache_mode": cacheMode,
				"egress":     normalizeEgress(b.cfg.CloudflareDynamicWorkers.Egress),
				"uncertain":  "true",
			}))
			if claimErr := claimLease(leaseID, slug, b.cfg, req.Repo.Root, b.cfg.IdleTimeout, req.Reclaim, server); claimErr != nil {
				return result, claimErr
			}
			result.Session = (&coreRunSessionHandle{
				Provider:       providerName,
				LeaseID:        leaseID,
				Slug:           slug,
				Reused:         reused,
				Kept:           true,
				RunID:          leaseID,
				CleanupCommand: fmt.Sprintf("crabbox stop --provider %s --id %s", providerName, leaseID),
			}).toCore()
			fmt.Fprintf(b.rt.Stderr, "kept uncertain run=%s slug=%s after request error\n", leaseID, slug)
			fmt.Fprintf(b.rt.Stderr, "inspect: crabbox status --provider %s --id %s\n", providerName, slug)
			fmt.Fprintf(b.rt.Stderr, "stop: crabbox stop --provider %s --id %s\n", providerName, slug)
			total = now(b.rt).Sub(started)
			result.Total = total
			if req.TimingJSON {
				report := timingReportWithProviderError(timingReportWithRunResult(timingReport{
					Provider:  providerName,
					LeaseID:   leaseID,
					Slug:      slug,
					CommandMs: commandDuration.Milliseconds(),
					TotalMs:   total.Milliseconds(),
					ExitCode:  1,
					Label:     strings.TrimSpace(req.Label),
				}, result, err))
				if timingErr := writeTimingJSON(b.rt.Stderr, report); timingErr != nil {
					return result, timingErr
				}
				timingWritten = true
			}
		}
		cleanupID := leaseID
		if cleanupID == "" {
			cleanupID = strings.TrimSpace(run.ID)
		}
		if cleanupID != "" && !keepRequested && errors.As(err, &contractErr) {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			cleanupErr := client.DeleteAcknowledgedComplete(cleanupCtx, cleanupID)
			cancel()
			if cleanupErr != nil && !notFoundError(cleanupErr) {
				fmt.Fprintf(b.rt.Stderr, "warning: %s malformed response cleanup failed for %s: %v\n", providerName, cleanupID, cleanupErr)
			}
		}
		if req.TimingJSON && !timingWritten {
			report := timingReportWithProviderError(timingReportWithRunResult(timingReport{
				Provider:  providerName,
				LeaseID:   leaseID,
				Slug:      slug,
				CommandMs: commandDuration.Milliseconds(),
				TotalMs:   result.Total.Milliseconds(),
				ExitCode:  result.ExitCode,
				Label:     strings.TrimSpace(req.Label),
			}, result, err))
			if timingErr := writeTimingJSON(b.rt.Stderr, report); timingErr != nil {
				return result, timingErr
			}
		}
		return result, ExitError{Code: 1, Message: fmt.Sprintf("%s run failed: %v", providerName, err)}
	}
	if strings.TrimSpace(run.ID) != "" {
		leaseID = run.ID
		if strings.TrimSpace(slug) == "" {
			slug = newLeaseSlug(leaseID)
		}
	}
	if strings.TrimSpace(run.WorkerID) == "" {
		run.WorkerID = workerID
	}
	if cacheMode == "explicit" {
		fmt.Fprintf(b.rt.Stderr, "dynamic worker run=%s worker=%s\n", leaseID, run.WorkerID)
	}
	writeRunOutput(b.rt.Stdout, b.rt.Stderr, run)
	claimStatus := runStatus{
		ID:       leaseID,
		WorkerID: run.WorkerID,
		Status:   run.Status,
		Metadata: run.Metadata,
	}
	claimLabels := map[string]string{
		"cache_mode": cacheMode,
		"egress":     normalizeEgress(b.cfg.CloudflareDynamicWorkers.Egress),
	}
	if run.LifecycleUncertain {
		probeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		status, statusErr := client.Status(probeCtx, leaseID)
		cancel()
		if statusErr != nil {
			claimStatus.Status = "unknown"
		} else {
			if strings.TrimSpace(status.WorkerID) == "" {
				status.WorkerID = run.WorkerID
			}
			claimStatus = status
		}
		claimLabels = mergeRunLabels(claimLabels, map[string]string{"uncertain": "true"})
		fmt.Fprintf(b.rt.Stderr, "warning: %s lifecycle reconciliation pending for run %s\n", providerName, leaseID)
	}
	exitCode := run.ExitCode
	if exitCode == 0 && !runSucceeded(run.Status) {
		exitCode = 1
	}
	total := now(b.rt).Sub(started)
	keepRun := req.Keep || cacheMode == "explicit" || (req.KeepOnFailure && exitCode != 0)
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
			Kept:           keepRun,
			RunID:          leaseID,
			CleanupCommand: fmt.Sprintf("crabbox stop --provider %s --id %s", providerName, leaseID),
		}).toCore(),
	}
	if keepRun {
		server := runServer(
			leaseID,
			slug,
			claimStatus,
			mergeRunLabels(run.Metadata, mergeRunLabels(claimStatus.Metadata, claimLabels)),
		)
		if err := claimLease(leaseID, slug, b.cfg, req.Repo.Root, b.cfg.IdleTimeout, req.Reclaim, server); err != nil {
			return result, err
		}
	}
	var cleanupErr error
	if !keepRun && !run.LifecycleUncertain && strings.TrimSpace(leaseID) != "" {
		cleanupErr = client.DeleteAcknowledgedComplete(ctx, leaseID)
		if cleanupErr != nil && notFoundError(cleanupErr) {
			cleanupErr = nil
		}
		if cleanupErr != nil {
			fmt.Fprintf(b.rt.Stderr, "warning: %s completed run metadata cleanup failed for %s: %v\n", providerName, leaseID, cleanupErr)
		}
	}
	if req.TimingJSON {
		var runErr error
		if exitCode != 0 {
			runErr = ExitError{Code: exitCode, Message: fmt.Sprintf("%s run exited %d", providerName, exitCode)}
		}
		if err := writeTimingJSON(b.rt.Stderr, timingReportWithRunResult(timingReport{
			Provider:  providerName,
			LeaseID:   leaseID,
			Slug:      slug,
			CommandMs: commandDuration.Milliseconds(),
			TotalMs:   total.Milliseconds(),
			ExitCode:  exitCode,
			Label:     strings.TrimSpace(req.Label),
		}, result, runErr)); err != nil {
			return result, err
		}
	}
	if exitCode != 0 {
		if req.KeepOnFailure {
			fmt.Fprintf(b.rt.Stderr, "keep-on-failure: kept run=%s slug=%s\n", leaseID, slug)
			fmt.Fprintf(b.rt.Stderr, "inspect: crabbox status --provider %s --id %s\n", providerName, slug)
			fmt.Fprintf(b.rt.Stderr, "stop: crabbox stop --provider %s --id %s\n", providerName, slug)
		}
		return result, ExitError{Code: exitCode, Message: fmt.Sprintf("%s run exited %d", providerName, exitCode)}
	}
	return result, nil
}

func (b *backend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	claims, err := providerClaims(b.cfg)
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
	leaseID, slug, _, _, err := b.resolveRunID(req.ID, "", false)
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
	leaseID, _, claim, claimed, err := b.resolveRunID(req.ID, "", false)
	if err != nil {
		return err
	}
	if err := client.Delete(ctx, leaseID); err != nil {
		if notFoundError(err) {
			if claimed {
				if err := removeLeaseClaimIfUnchanged(leaseID, claim); err != nil {
					return err
				}
				fmt.Fprintf(b.rt.Stdout, "removed stale %s claim %s reason=not-found\n", providerName, leaseID)
			}
			return nil
		}
		return providerError("delete metadata", err)
	}
	if claimed {
		if err := removeLeaseClaimIfUnchanged(leaseID, claim); err != nil {
			return err
		}
	}
	fmt.Fprintf(b.rt.Stdout, "stopped %s provider=%s loader_metadata=%s\n", leaseID, providerName, leaseID)
	return nil
}

func (b *backend) Cleanup(ctx context.Context, req CleanupRequest) error {
	client, err := newLoaderAPI(b.cfg, b.rt)
	if err != nil {
		return err
	}
	claims, err := providerClaims(b.cfg)
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
			if err := removeLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
				fmt.Fprintf(b.rt.Stderr, "warning: %s claim removal failed for %s: %v\n", providerName, claim.LeaseID, err)
				continue
			}
			removed++
			fmt.Fprintf(b.rt.Stdout, "removed stale %s claim %s slug=%s reason=not-found\n", providerName, claim.LeaseID, blank(claim.Slug, "-"))
			continue
		}
		if !terminalState(status.Status) {
			continue
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would delete terminal %s metadata and remove claim %s slug=%s state=%s\n", providerName, claim.LeaseID, blank(claim.Slug, "-"), status.Status)
			continue
		}
		if err := client.Delete(ctx, claim.LeaseID); err != nil && !notFoundError(err) {
			fmt.Fprintf(b.rt.Stderr, "warning: %s metadata delete failed for %s: %v\n", providerName, claim.LeaseID, err)
			continue
		}
		if err := removeLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
			fmt.Fprintf(b.rt.Stderr, "warning: %s claim removal failed for %s: %v\n", providerName, claim.LeaseID, err)
			continue
		}
		removed++
		fmt.Fprintf(b.rt.Stdout, "deleted terminal %s metadata and removed claim %s slug=%s state=%s\n", providerName, claim.LeaseID, blank(claim.Slug, "-"), status.Status)
	}
	if !req.DryRun {
		fmt.Fprintf(b.rt.Stdout, "%s cleanup removed=%d checked=%d\n", providerName, removed, len(claims))
	}
	return nil
}

func (b *backend) runIdentity(req RunRequest, cacheMode string) (string, string, string, bool, error) {
	if cacheMode == "explicit" {
		if strings.TrimSpace(req.ID) == "" {
			return "", "", "", false, exit(2, "%s cache=explicit requires --id", providerName)
		}
		leaseID := newLeaseID()
		slug, err := allocateClaimLeaseSlug(leaseID, req.RequestedSlug)
		if err != nil {
			return "", "", "", false, err
		}
		return leaseID, strings.TrimSpace(req.ID), slug, true, nil
	}
	if strings.TrimSpace(req.ID) != "" {
		return "", "", "", false, exit(2, "%s --id requires cache=explicit", providerName)
	}
	if cacheMode == "stable" {
		leaseID := newLeaseID()
		workerID := stableRunID(workerModuleName(req.Script), req.Script.Data, b.cfg.CloudflareDynamicWorkers, req.Env)
		slug := newLeaseSlug(leaseID)
		if req.Keep || req.KeepOnFailure {
			var err error
			slug, err = allocateClaimLeaseSlug(leaseID, req.RequestedSlug)
			if err != nil {
				return "", "", "", false, err
			}
		}
		return leaseID, workerID, slug, false, nil
	}
	leaseID := ""
	slug := ""
	if req.Keep || req.KeepOnFailure {
		leaseID = newLeaseID()
		var err error
		slug, err = allocateClaimLeaseSlug(leaseID, req.RequestedSlug)
		if err != nil {
			return "", "", "", false, err
		}
	}
	return leaseID, "", slug, false, nil
}

func (b *backend) resolveRunID(identifier, repoRoot string, reclaim bool) (string, string, LeaseClaim, bool, error) {
	claim, ok, err := resolveLeaseClaim(identifier, b.cfg)
	if err != nil {
		return "", "", LeaseClaim{}, false, err
	}
	if ok {
		if repoRoot != "" {
			server := claimServer(claim, blank(claim.Labels["state"], "unknown"))
			if err := claimLease(claim.LeaseID, claim.Slug, b.cfg, repoRoot, time.Duration(claim.IdleTimeoutSeconds)*time.Second, reclaim, server); err != nil {
				return "", "", LeaseClaim{}, false, err
			}
		}
		return claim.LeaseID, blank(claim.Slug, newLeaseSlug(claim.LeaseID)), claim, true, nil
	}
	value := strings.TrimSpace(identifier)
	if value == "" {
		return "", "", LeaseClaim{}, false, exit(2, "%s id is required", providerName)
	}
	return value, newLeaseSlug(value), LeaseClaim{}, false, nil
}

func (b *backend) buildRunRequest(req RunRequest, leaseID, workerID, cacheMode string) runRequest {
	cfg := b.cfg.CloudflareDynamicWorkers
	return runRequest{
		ID:                 leaseID,
		WorkerID:           workerID,
		CacheMode:          cacheMode,
		RetainMetadata:     req.Keep || cacheMode == "explicit",
		RetainOnFailure:    req.KeepOnFailure,
		Module:             moduleSource{Name: workerModuleName(req.Script), Source: string(req.Script.Data)},
		CompatibilityDate:  effectiveCompatibilityDate(cfg.CompatibilityDate),
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
		"source":   workerModuleName(req.Script),
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
	if stderr != nil {
		writeStderrPart(stderr, run.Stderr)
		writeStderrPart(stderr, run.Logs)
	}
}

func writeStderrPart(stderr io.Writer, value string) {
	if value == "" {
		return
	}
	_, _ = io.WriteString(stderr, value)
	if !strings.HasSuffix(value, "\n") {
		_, _ = io.WriteString(stderr, "\n")
	}
}

func workerModuleName(script *RunScriptSpec) string {
	if script == nil {
		return "index.js"
	}
	if strings.TrimSpace(script.Source) == "stdin" {
		return "index.js"
	}
	candidate := strings.ReplaceAll(strings.TrimSpace(script.RemotePath), "\\", "/")
	if candidate == "" {
		candidate = strings.ReplaceAll(strings.TrimSpace(script.Source), "\\", "/")
	}
	name := path.Base(candidate)
	var cleaned strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			cleaned.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			cleaned.WriteRune(r)
		case r >= '0' && r <= '9':
			cleaned.WriteRune(r)
		case r == '.', r == '_', r == '-':
			cleaned.WriteRune(r)
		}
	}
	result := cleaned.String()
	if result == "" || result == "." || strings.Contains(result, "..") {
		return "index.js"
	}
	const maxModuleNameLength = 256
	if len(result) > maxModuleNameLength {
		ext := path.Ext(result)
		if ext == "" || len(ext) > 16 {
			ext = ".js"
		}
		stem := strings.TrimSuffix(result, path.Ext(result))
		maxStemLength := maxModuleNameLength - len(ext)
		if len(stem) > maxStemLength {
			stem = stem[:maxStemLength]
		}
		stem = strings.TrimRight(stem, ".")
		if stem == "" {
			return "index.js"
		}
		result = stem + ext
	}
	return result
}

func stableRunID(moduleName string, source []byte, cfg CloudflareDynamicWorkersConfig, env map[string]string) string {
	h := sha256.New()
	_, _ = h.Write([]byte(strings.TrimSpace(moduleName)))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(source)
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(effectiveCompatibilityDate(cfg.CompatibilityDate)))
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

func effectiveCompatibilityDate(value string) string {
	return blank(strings.TrimSpace(value), defaultCompatibilityDate)
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

func providerClaims(cfg Config) ([]LeaseClaim, error) {
	scope, err := loaderClaimScope(cfg)
	if err != nil {
		return nil, err
	}
	claims, err := listLeaseClaims()
	if err != nil {
		return nil, err
	}
	out := make([]LeaseClaim, 0, len(claims))
	for _, claim := range claims {
		if claim.Provider == providerName && claim.ProviderScope == scope {
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
	if strings.TrimSpace(status.WorkerID) != "" {
		labels["worker_id"] = status.WorkerID
	}
	for key, value := range extra {
		if strings.TrimSpace(key) != "" {
			labels[key] = value
		}
	}
	labels["provider"] = providerName
	labels["lease"] = leaseID
	labels["slug"] = blank(slug, newLeaseSlug(leaseID))
	labels["target"] = targetWorker
	labels["state"] = blank(status.Status, "unknown")
	if strings.TrimSpace(status.WorkerID) != "" {
		labels["worker_id"] = status.WorkerID
	} else {
		delete(labels, "worker_id")
	}
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
	case "completed", "succeeded", "success", "ok":
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
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusNotFound
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
