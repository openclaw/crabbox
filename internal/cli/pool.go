package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var ansiEscapePattern = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

func (a App) list(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("list", a.Stderr)
	provider := fs.String("provider", defaults.Provider, providerHelpAll())
	jsonOut := fs.Bool("json", false, "print JSON")
	providerFlags := registerProviderFlags(fs, defaults)
	targetFlags := registerTargetFlags(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	cfg.Provider = *provider
	if err := applyProviderFlags(&cfg, fs, providerFlags); err != nil {
		return err
	}
	if err := applyTargetFlagOverrides(&cfg, fs, targetFlags); err != nil {
		return err
	}
	backend, err := loadBackend(cfg, runtimeForApp(a))
	if err != nil {
		return err
	}
	if *jsonOut {
		if jsonBackend, ok := backend.(JSONListBackend); ok {
			view, err := jsonBackend.ListJSON(ctx, ListRequest{Options: leaseOptionsFromConfig(cfg)})
			if err != nil {
				return err
			}
			a.syncExternalRunnersBestEffort(ctx, cfg, backend)
			return json.NewEncoder(a.Stdout).Encode(view)
		}
	}
	var servers []Server
	switch b := backend.(type) {
	case SSHLeaseBackend:
		servers, err = b.List(ctx, ListRequest{Options: leaseOptionsFromConfig(cfg)})
	case DelegatedRunBackend:
		servers, err = b.List(ctx, ListRequest{Options: leaseOptionsFromConfig(cfg)})
	default:
		return exit(2, "provider=%s does not support list", backend.Spec().Name)
	}
	if err != nil {
		return err
	}
	a.syncExternalRunnersBestEffort(ctx, cfg, backend)
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(servers)
	}
	renderServerList(a.Stdout, servers)
	return nil
}

func (a App) syncExternalRunnersBestEffort(ctx context.Context, cfg Config, backend Backend) {
	if !isBlacksmithProvider(cfg.Provider) {
		return
	}
	client, ok, err := newCoordinatorClient(cfg)
	if err != nil || !ok {
		return
	}
	jsonBackend, ok := backend.(JSONListBackend)
	if !ok {
		return
	}
	view, err := jsonBackend.ListJSON(ctx, ListRequest{Options: leaseOptionsFromConfig(cfg), All: true})
	if err != nil {
		fmt.Fprintf(a.Stderr, "warning: external runner portal sync skipped: %v\n", err)
		return
	}
	runners, err := coordinatorExternalRunnersFromListView(view)
	if err != nil {
		fmt.Fprintf(a.Stderr, "warning: external runner portal sync skipped: %v\n", err)
		return
	}
	enrichExternalRunnerActionsBestEffort(ctx, cfg, runners)
	if _, err := client.SyncExternalRunners(ctx, "blacksmith-testbox", runners); err != nil {
		fmt.Fprintf(a.Stderr, "warning: external runner portal sync failed: %v\n", err)
	}
}

func coordinatorExternalRunnersFromListView(view any) ([]CoordinatorExternalRunner, error) {
	data, err := json.Marshal(view)
	if err != nil {
		return nil, err
	}
	var runners []CoordinatorExternalRunner
	if err := json.Unmarshal(data, &runners); err != nil {
		return nil, err
	}
	for i := range runners {
		runners[i].Provider = "blacksmith-testbox"
		if runners[i].CreatedAt == "" {
			runners[i].CreatedAt = runners[i].Created
		}
	}
	return runners, nil
}

type externalRunnerActionsRun struct {
	DatabaseID   int64  `json:"databaseId"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
	CreatedAt    string `json:"createdAt"`
	UpdatedAt    string `json:"updatedAt"`
	HeadBranch   string `json:"headBranch"`
	URL          string `json:"url"`
	WorkflowName string `json:"workflowName"`
	DisplayTitle string `json:"displayTitle"`
	Name         string `json:"name"`
}

func enrichExternalRunnerActionsBestEffort(ctx context.Context, cfg Config, runners []CoordinatorExternalRunner) {
	cache := map[string][]externalRunnerActionsRun{}
	for i := range runners {
		repo, ok := externalRunnerGitHubRepo(cfg, runners[i])
		if !ok || runners[i].Workflow == "" {
			continue
		}
		key := repo.Slug() + "\x00" + runners[i].Workflow + "\x00" + runners[i].Ref
		runs, seen := cache[key]
		if !seen {
			var err error
			runs, err = externalRunnerGitHubRuns(ctx, repo, runners[i].Workflow, runners[i].Ref)
			if err != nil {
				cache[key] = nil
				continue
			}
			cache[key] = runs
		}
		run, ok := matchExternalRunnerActionRun(runners[i], runs)
		if !ok {
			runners[i].ActionsRepo = repo.Slug()
			runners[i].ActionsWorkflowURL = externalRunnerWorkflowURL(repo, runners[i].Workflow)
			continue
		}
		runners[i].ActionsRepo = repo.Slug()
		runners[i].ActionsRunID = strconv.FormatInt(run.DatabaseID, 10)
		runners[i].ActionsRunURL = run.URL
		runners[i].ActionsRunStatus = run.Status
		runners[i].ActionsRunConclusion = run.Conclusion
		runners[i].ActionsWorkflowName = run.WorkflowName
		runners[i].ActionsWorkflowURL = externalRunnerWorkflowURL(repo, runners[i].Workflow)
	}
}

func externalRunnerGitHubRepo(cfg Config, runner CoordinatorExternalRunner) (GitHubRepo, bool) {
	if strings.Contains(runner.Repo, "/") {
		repo, err := parseGitHubRepo(runner.Repo)
		return repo, err == nil
	}
	owner := strings.TrimSpace(cfg.Blacksmith.Org)
	if owner == "" && cfg.Actions.Repo != "" {
		if repo, err := parseGitHubRepo(cfg.Actions.Repo); err == nil {
			owner = repo.Owner
		}
	}
	if runner.Repo == "" {
		return GitHubRepo{}, false
	}
	if owner == "" {
		repo, err := parseGitHubRepo(runner.Repo + "/" + runner.Repo)
		return repo, err == nil
	}
	repo, err := parseGitHubRepo(owner + "/" + runner.Repo)
	return repo, err == nil
}

func externalRunnerGitHubRuns(ctx context.Context, repo GitHubRepo, workflow, ref string) ([]externalRunnerActionsRun, error) {
	args := []string{
		"run", "list",
		"--repo", repo.Slug(),
		"--workflow", workflow,
		"--limit", "30",
		"--json", "databaseId,status,conclusion,createdAt,updatedAt,headBranch,url,workflowName,displayTitle,name",
	}
	if ref != "" {
		args = append(args, "--branch", ref)
	}
	out, err := ghOutput(ctx, "", args...)
	if err != nil {
		return nil, err
	}
	var runs []externalRunnerActionsRun
	if err := json.Unmarshal([]byte(stripANSI(out)), &runs); err != nil {
		return nil, err
	}
	return runs, nil
}

func matchExternalRunnerActionRun(runner CoordinatorExternalRunner, runs []externalRunnerActionsRun) (externalRunnerActionsRun, bool) {
	if len(runs) == 0 {
		return externalRunnerActionsRun{}, false
	}
	runnerTime, hasRunnerTime := parseExternalRunnerTime(runner.CreatedAt)
	bestIndex := -1
	bestDelta := int64(0)
	for i, run := range runs {
		if runner.Ref != "" && run.HeadBranch != "" && run.HeadBranch != runner.Ref {
			continue
		}
		if !hasRunnerTime {
			return run, true
		}
		runTime, ok := parseExternalRunnerTime(run.CreatedAt)
		if !ok {
			continue
		}
		delta := runTime.Sub(runnerTime)
		if delta < 0 {
			delta = -delta
		}
		if delta > 6*time.Hour {
			continue
		}
		deltaMillis := delta.Milliseconds()
		if bestIndex < 0 || deltaMillis < bestDelta {
			bestIndex = i
			bestDelta = deltaMillis
		}
	}
	if bestIndex < 0 {
		return externalRunnerActionsRun{}, false
	}
	return runs[bestIndex], true
}

func parseExternalRunnerTime(value string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func externalRunnerWorkflowURL(repo GitHubRepo, workflow string) string {
	if repo.Slug() == "" || workflow == "" {
		return ""
	}
	workflow = strings.TrimPrefix(strings.TrimSpace(workflow), "/")
	if strings.HasPrefix(workflow, ".github/workflows/") {
		workflow = path.Base(workflow)
	}
	if !strings.HasSuffix(workflow, ".yml") && !strings.HasSuffix(workflow, ".yaml") && !allDigits(workflow) {
		return ""
	}
	return "https://github.com/" + repo.Slug() + "/actions/workflows/" + url.PathEscape(workflow)
}

func allDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func stripANSI(value string) string {
	return ansiEscapePattern.ReplaceAllString(value, "")
}

func activeCoordinatorLeaseIDs(leases []CoordinatorLease) map[string]struct{} {
	ids := make(map[string]struct{}, len(leases))
	for _, lease := range leases {
		if lease.ID != "" {
			ids[lease.ID] = struct{}{}
		}
	}
	return ids
}

func coordinatorMachineOrphanField(labels map[string]string, activeLeaseIDs map[string]struct{}) string {
	leaseID := labels["lease"]
	if leaseID == "" {
		return " orphan=missing-lease-label"
	}
	if _, ok := activeLeaseIDs[leaseID]; !ok {
		return " orphan=no-active-lease"
	}
	return ""
}

func (a App) cleanup(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("machine cleanup", a.Stderr)
	provider := fs.String("provider", defaults.Provider, "provider: hetzner, aws, azure, gcp, proxmox, or namespace-devbox")
	dryRun := fs.Bool("dry-run", false, "only print")
	providerFlags := registerProviderFlags(fs, defaults)
	targetFlags := registerTargetFlags(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	cfg.Provider = *provider
	if err := applyProviderFlags(&cfg, fs, providerFlags); err != nil {
		return err
	}
	if err := applyTargetFlagOverrides(&cfg, fs, targetFlags); err != nil {
		return err
	}
	backend, err := loadBackend(cfg, runtimeForApp(a))
	if err != nil {
		return err
	}
	if backendCoordinator(backend) != nil {
		return exit(2, "machine cleanup is disabled when a coordinator is configured; coordinator TTL alarms own brokered cleanup")
	}
	cleaner, ok := backend.(CleanupBackend)
	if !ok {
		return exit(2, "machine cleanup is not supported for provider=%s", cfg.Provider)
	}
	return cleaner.Cleanup(ctx, CleanupRequest{Options: leaseOptionsFromConfig(cfg), DryRun: *dryRun})
}

func shouldCleanupServer(server Server, now time.Time) (bool, string) {
	labels := server.Labels
	if labels == nil {
		return false, "missing labels"
	}
	if strings.EqualFold(labels["keep"], "true") {
		return false, "keep=true"
	}
	state := strings.ToLower(labels["state"])
	switch state {
	case "running", "provisioning":
		expiresAt, ok := cleanupExpiry(labels)
		if ok && now.After(expiresAt.Add(12*time.Hour)) {
			return true, "stale state=" + state
		}
		return false, "state=" + state
	case "leased", "ready", "active":
		expiresAt, ok := cleanupExpiry(labels)
		if ok && now.After(expiresAt) {
			return true, "expired state=" + state
		}
		return false, "state=" + state
	}
	if state == "failed" || state == "released" || state == "expired" {
		return true, "state=" + state
	}
	expiresAt, ok := cleanupExpiry(labels)
	if !ok {
		return false, "missing expires_at"
	}
	if now.Before(expiresAt) {
		return false, "not expired"
	}
	return true, "expired"
}

func ShouldCleanupServer(server Server, now time.Time) (bool, string) {
	return shouldCleanupServer(server, now)
}

func cleanupExpiry(labels map[string]string) (time.Time, bool) {
	for _, key := range []string{"expires_at", "ttl"} {
		value := strings.TrimSpace(labels[key])
		if value == "" {
			continue
		}
		if parsed, ok := parseLeaseLabelTime(value); ok {
			return parsed, true
		}
	}
	return time.Time{}, false
}

func directLeaseExpiresAt(now time.Time, cfg Config) time.Time {
	return directLeaseExpiresAtFrom(now, now, cfg.TTL, cfg.IdleTimeout)
}
