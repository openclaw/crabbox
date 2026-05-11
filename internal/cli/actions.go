package cli

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"path"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type GitHubRepo struct {
	Owner string
	Name  string
}

func (r GitHubRepo) Slug() string {
	if r.Owner == "" || r.Name == "" {
		return ""
	}
	return r.Owner + "/" + r.Name
}

func (a App) actionsHydrate(ctx context.Context, args []string) error {
	started := time.Now()
	defaults := defaultConfig()
	fs := newFlagSet("actions hydrate", a.Stderr)
	provider := fs.String("provider", defaults.Provider, providerHelpAll())
	providerFlags := registerProviderFlags(fs, defaults)
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	leaseIDFlag := fs.String("id", "", "existing lease id or slug")
	repoFlag := fs.String("repo", "", "GitHub repository owner/name")
	workflowFlag := fs.String("workflow", "", "workflow file/name/id")
	jobFlag := fs.String("job", "", "expected hydrate workflow job/input name")
	refFlag := fs.String("ref", "", "workflow ref")
	waitTimeout := fs.Duration("wait-timeout", 20*time.Minute, "time to wait for Actions hydration")
	keepAliveMinutes := fs.Int("keep-alive-minutes", 90, "minutes for workflow to keep the job alive")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	timingJSON := fs.Bool("timing-json", false, "print final timing as JSON")
	fieldFlags := stringListFlag{}
	fs.Var(&fieldFlags, "f", "workflow input key=value")
	fs.Var(&fieldFlags, "field", "workflow input key=value")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *leaseIDFlag == "" {
		return exit(2, "actions hydrate requires --id")
	}
	cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{})
	if err != nil {
		return err
	}
	if err := applyProviderFlags(&cfg, fs, providerFlags); err != nil {
		return err
	}
	repo, err := findRepo()
	if err != nil {
		return err
	}
	if *repoFlag != "" {
		cfg.Actions.Repo = *repoFlag
	}
	if *workflowFlag != "" {
		cfg.Actions.Workflow = *workflowFlag
	}
	if *jobFlag != "" {
		cfg.Actions.Job = *jobFlag
	}
	if *refFlag != "" {
		cfg.Actions.Ref = *refFlag
	}
	if cfg.Actions.Workflow == "" {
		return exit(2, "actions hydrate requires --workflow or actions.workflow")
	}
	ghRepo, err := resolveGitHubRepo(repo, cfg.Actions.Repo)
	if err != nil {
		return err
	}
	server, target, leaseID, slug, err := a.resolveLeaseTargetForActions(ctx, cfg, *leaseIDFlag)
	if err != nil {
		return err
	}
	if err := claimLeaseForRepoConfig(leaseID, slug, cfg, repo.Root, cfg.IdleTimeout, *reclaim); err != nil {
		return err
	}
	backend, err := loadBackend(cfg, runtimeForApp(a))
	if err != nil {
		return err
	}
	if coord := backendCoordinator(backend); coord != nil {
		stopHeartbeat := startCoordinatorHeartbeat(ctx, coord, leaseID, cfg.IdleTimeout, nil, leaseTelemetryCollectorForTarget(target), a.Stderr)
		defer stopHeartbeat()
	} else if sshBackend, ok := backend.(SSHLeaseBackend); ok {
		_, err := sshBackend.Touch(ctx, TouchRequest{Lease: LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, State: blank(server.Labels["state"], "ready"), IdleTimeout: cfg.IdleTimeout})
		if err != nil {
			fmt.Fprintf(a.Stderr, "warning: touch failed for %s: %v\n", leaseID, err)
		}
	}
	label := githubActionsLeaseLabel(leaseID)
	if err := a.registerGitHubActionsRunner(ctx, cfg, target, leaseID, slug, ghRepo, "", nil); err != nil {
		return err
	}
	if err := clearActionsHydrationState(ctx, target, leaseID); err != nil {
		return err
	}
	ref := actionsRef(cfg, repo)
	extraFields := mergeWorkflowInputFields(cfg.Actions.Fields, fieldFlags)
	fields := actionsHydrateFields(leaseID, label, cfg.Actions.Job, *keepAliveMinutes, extraFields)
	if inputs, ok, err := githubWorkflowDispatchInputs(ctx, repo.Root, ghRepo, cfg.Actions.Workflow, ref); err != nil {
		fmt.Fprintf(a.Stderr, "warning: inspect workflow inputs failed: %v\n", err)
	} else if ok {
		filtered, dropped := filterWorkflowInputs(fields, inputs)
		for _, field := range dropped {
			fmt.Fprintf(a.Stderr, "warning: workflow %s does not declare input %s; omitting it\n", cfg.Actions.Workflow, fieldName(field))
		}
		fields = filtered
		for _, required := range []string{"crabbox_id", "crabbox_runner_label", "crabbox_keep_alive_minutes"} {
			if !inputs[required] {
				return exit(2, "workflow %s at %s does not declare required hydrate input %s", cfg.Actions.Workflow, ref, required)
			}
		}
	}
	expectedJob := cfg.Actions.Job
	if !workflowFieldsContain(fields, "crabbox_job") {
		expectedJob = ""
	}
	if err := dispatchGitHubActionsWorkflow(ctx, repo.Root, ghRepo, cfg.Actions.Workflow, ref, fields); err != nil {
		if expectedJob != "" && strings.Contains(err.Error(), "Unexpected input") {
			fields = dropWorkflowField(fields, "crabbox_job")
			expectedJob = ""
			fmt.Fprintf(a.Stderr, "warning: retrying workflow dispatch without crabbox_job for compatibility\n")
			if retryErr := dispatchGitHubActionsWorkflow(ctx, repo.Root, ghRepo, cfg.Actions.Workflow, ref, fields); retryErr != nil {
				return retryErr
			}
		} else {
			return err
		}
	}
	fmt.Fprintf(a.Stdout, "dispatched workflow=%s repo=%s ref=%s runner_label=%s\n", cfg.Actions.Workflow, ghRepo.Slug(), ref, label)
	state, err := waitForActionsHydration(ctx, target, leaseID, expectedJob, *waitTimeout, a.Stderr)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "actions hydrated id=%s slug=%s workspace=%s run_id=%s\n", leaseID, blank(slug, "-"), state.Workspace, blank(state.RunID, "-"))
	fmt.Fprintf(a.Stdout, "actions hydrate complete total=%s\n", time.Since(started).Round(time.Millisecond))
	if *timingJSON {
		total := time.Since(started)
		if err := writeTimingJSON(a.Stderr, timingReport{
			Provider:      cfg.Provider,
			LeaseID:       leaseID,
			Slug:          slug,
			TotalMs:       total.Milliseconds(),
			ExitCode:      0,
			ActionsRunURL: actionsRunURL(ghRepo, state.RunID),
		}); err != nil {
			return err
		}
	}
	return nil
}

func (a App) actionsRegister(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("actions register", a.Stderr)
	provider := fs.String("provider", defaults.Provider, providerHelpAll())
	providerFlags := registerProviderFlags(fs, defaults)
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	leaseIDFlag := fs.String("id", "", "existing lease id or slug")
	repoFlag := fs.String("repo", "", "GitHub repository owner/name")
	nameFlag := fs.String("name", "", "runner name")
	labelsFlag := fs.String("labels", "", "comma-separated extra runner labels")
	versionFlag := fs.String("version", "", "actions/runner version or latest")
	ephemeralFlag := fs.Bool("ephemeral", true, "register runner as ephemeral")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *leaseIDFlag == "" {
		return exit(2, "actions register requires --id")
	}
	cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{})
	if err != nil {
		return err
	}
	if err := applyProviderFlags(&cfg, fs, providerFlags); err != nil {
		return err
	}
	repo, err := findRepo()
	if err != nil {
		return err
	}
	if *repoFlag != "" {
		cfg.Actions.Repo = *repoFlag
	}
	if *versionFlag != "" {
		cfg.Actions.RunnerVersion = *versionFlag
	}
	if flagWasSet(fs, "ephemeral") {
		cfg.Actions.Ephemeral = *ephemeralFlag
	}
	extraLabels := splitCommaList(*labelsFlag)
	ghRepo, err := resolveGitHubRepo(repo, cfg.Actions.Repo)
	if err != nil {
		return err
	}
	server, target, leaseID, slug, err := a.resolveLeaseTargetForActions(ctx, cfg, *leaseIDFlag)
	if err != nil {
		return err
	}
	if err := claimLeaseForRepoConfig(leaseID, slug, cfg, repo.Root, cfg.IdleTimeout, *reclaim); err != nil {
		return err
	}
	a.touchLeaseTargetBestEffort(ctx, cfg, LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, "")
	return a.registerGitHubActionsRunner(ctx, cfg, target, leaseID, slug, ghRepo, *nameFlag, extraLabels)
}

func (a App) actionsDispatch(ctx context.Context, args []string) error {
	fs := newFlagSet("actions dispatch", a.Stderr)
	repoFlag := fs.String("repo", "", "GitHub repository owner/name")
	workflowFlag := fs.String("workflow", "", "workflow file/name/id")
	refFlag := fs.String("ref", "", "workflow ref")
	fieldFlags := stringListFlag{}
	fs.Var(&fieldFlags, "f", "workflow input key=value")
	fs.Var(&fieldFlags, "field", "workflow input key=value")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	repo, err := findRepo()
	if err != nil {
		return err
	}
	if *repoFlag != "" {
		cfg.Actions.Repo = *repoFlag
	}
	if *workflowFlag != "" {
		cfg.Actions.Workflow = *workflowFlag
	}
	if *refFlag != "" {
		cfg.Actions.Ref = *refFlag
	}
	ghRepo, err := resolveGitHubRepo(repo, cfg.Actions.Repo)
	if err != nil {
		return err
	}
	if cfg.Actions.Workflow == "" {
		return exit(2, "actions dispatch requires --workflow or actions.workflow")
	}
	ref := actionsRef(cfg, repo)
	if err := dispatchGitHubActionsWorkflow(ctx, repo.Root, ghRepo, cfg.Actions.Workflow, ref, fieldFlags); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "dispatched workflow=%s repo=%s ref=%s\n", cfg.Actions.Workflow, ghRepo.Slug(), ref)
	return nil
}

func (a App) registerGitHubActionsRunner(ctx context.Context, cfg Config, target SSHTarget, leaseID, slug string, ghRepo GitHubRepo, nameOverride string, extraLabels []string) error {
	if !supportsActionsRunnerTarget(target) {
		return exit(2, "actions runner registration currently supports Linux and Windows WSL2 targets only")
	}
	token, err := githubActionsRegistrationToken(ctx, ghRepo)
	if err != nil {
		return err
	}
	name := nameOverride
	if name == "" {
		name = leaseProviderName(leaseID, slug)
	}
	labels := githubActionsRunnerLabels(cfg, leaseID, slug, extraLabels)
	script := githubActionsRunnerInstallScript(cfg.Actions.RunnerVersion, cfg.Actions.Ephemeral)
	remote := fmt.Sprintf("RUNNER_REPO=%s RUNNER_NAME=%s RUNNER_LABELS=%s RUNNER_TOKEN=%s bash -s", shellQuote(ghRepo.Slug()), shellQuote(name), shellQuote(strings.Join(labels, ",")), shellQuote(token))
	if err := runSSHInputQuiet(ctx, target, remote, script); err != nil {
		return exit(7, "register GitHub Actions runner on %s: %v", target.Host, err)
	}
	fmt.Fprintf(a.Stdout, "actions runner registered repo=%s name=%s labels=%s ephemeral=%t\n", ghRepo.Slug(), name, strings.Join(labels, ","), cfg.Actions.Ephemeral)
	return nil
}

func supportsActionsRunnerTarget(target SSHTarget) bool {
	return target.TargetOS == "" || target.TargetOS == targetLinux || isWindowsWSL2Target(target)
}

func (a App) resolveLeaseTargetForActions(ctx context.Context, cfg Config, id string) (Server, SSHTarget, string, string, error) {
	server, target, leaseID, err := a.resolveLeaseTarget(ctx, cfg, id)
	return server, target, leaseID, serverSlug(server), err
}

func dispatchGitHubActionsWorkflow(ctx context.Context, dir string, repo GitHubRepo, workflow, ref string, fields []string) error {
	cmdArgs := []string{"workflow", "run", workflow, "--repo", repo.Slug(), "--ref", ref}
	for _, field := range fields {
		if !strings.Contains(field, "=") {
			return exit(2, "workflow input must be key=value: %s", field)
		}
		cmdArgs = append(cmdArgs, "-f", field)
	}
	return runGH(ctx, dir, cmdArgs...)
}

func actionsHydrateFields(leaseID, label, job string, keepAliveMinutes int, extra []string) []string {
	fields := []string{
		"crabbox_id=" + leaseID,
		"crabbox_runner_label=" + label,
		fmt.Sprintf("crabbox_keep_alive_minutes=%d", keepAliveMinutes),
	}
	if job != "" {
		fields = append(fields, "crabbox_job="+job)
	}
	fields = append(fields, extra...)
	return fields
}

func mergeWorkflowInputFields(base, override []string) []string {
	fields := append([]string{}, base...)
	index := map[string]int{}
	for i, field := range fields {
		if name := fieldName(field); name != "" {
			index[name] = i
		}
	}
	for _, field := range override {
		name := fieldName(field)
		if name != "" {
			if existing, ok := index[name]; ok {
				fields[existing] = field
				continue
			}
			index[name] = len(fields)
		}
		fields = append(fields, field)
	}
	return fields
}

func githubWorkflowDispatchInputs(ctx context.Context, dir string, repo GitHubRepo, workflow, ref string) (map[string]bool, bool, error) {
	workflow = strings.TrimPrefix(workflow, "/")
	if !strings.HasPrefix(workflow, ".github/workflows/") {
		return nil, false, nil
	}
	out, err := ghOutput(ctx, dir, "api", "repos/"+repo.Slug()+"/contents/"+workflow+"?ref="+url.QueryEscape(ref), "--jq", ".content")
	if err != nil {
		return nil, false, err
	}
	encoded := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, out)
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, false, err
	}
	inputs, ok, err := parseWorkflowDispatchInputs(data)
	return inputs, ok, err
}

func parseWorkflowDispatchInputs(data []byte) (map[string]bool, bool, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, false, err
	}
	root := mappingValue(&doc, "")
	if root == nil {
		return nil, false, nil
	}
	on := mappingValue(root, "on")
	if on == nil {
		return nil, false, nil
	}
	dispatch := mappingValue(on, "workflow_dispatch")
	if dispatch == nil {
		return nil, false, nil
	}
	inputsNode := mappingValue(dispatch, "inputs")
	if inputsNode == nil || inputsNode.Kind != yaml.MappingNode {
		return map[string]bool{}, true, nil
	}
	inputs := map[string]bool{}
	for i := 0; i+1 < len(inputsNode.Content); i += 2 {
		inputs[inputsNode.Content[i].Value] = true
	}
	return inputs, true, nil
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		if key == "" {
			return node.Content[0]
		}
		return mappingValue(node.Content[0], key)
	}
	if key == "" {
		if node.Kind == yaml.MappingNode {
			return node
		}
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func filterWorkflowInputs(fields []string, inputs map[string]bool) ([]string, []string) {
	filtered := make([]string, 0, len(fields))
	dropped := []string{}
	for _, field := range fields {
		name := fieldName(field)
		if inputs[name] {
			filtered = append(filtered, field)
		} else {
			dropped = append(dropped, field)
		}
	}
	return filtered, dropped
}

func workflowFieldsContain(fields []string, name string) bool {
	for _, field := range fields {
		if fieldName(field) == name {
			return true
		}
	}
	return false
}

func dropWorkflowField(fields []string, name string) []string {
	out := fields[:0]
	for _, field := range fields {
		if fieldName(field) != name {
			out = append(out, field)
		}
	}
	return out
}

func fieldName(field string) string {
	name, _, _ := strings.Cut(field, "=")
	return name
}

func actionsRef(cfg Config, repo Repo) string {
	if cfg.Actions.Ref != "" {
		return cfg.Actions.Ref
	}
	if repo.BaseRef != "" {
		return repo.BaseRef
	}
	return "main"
}

func githubActionsRunnerLabels(cfg Config, leaseID, slug string, extra []string) []string {
	labels := []string{
		"crabbox",
		githubActionsLeaseLabel(leaseID),
		"crabbox-profile-" + sanitizeGitHubRunnerLabel(cfg.Profile),
		"crabbox-class-" + sanitizeGitHubRunnerLabel(cfg.Class),
	}
	if slug = normalizeLeaseSlug(slug); slug != "" {
		labels = append(labels, "crabbox-"+sanitizeGitHubRunnerLabel(slug))
	}
	labels = append(labels, cfg.Actions.RunnerLabels...)
	labels = append(labels, extra...)
	return appendUniqueStrings(nil, labels...)
}

func githubActionsLeaseLabel(leaseID string) string {
	return "crabbox-" + sanitizeGitHubRunnerLabel(leaseID)
}

type actionsHydrationState struct {
	Workspace    string
	RunID        string
	ReadyAt      string
	Job          string
	EnvFile      string
	ServicesFile string
}

func waitForActionsHydration(ctx context.Context, target SSHTarget, leaseID, expectedJob string, timeout time.Duration, stderr io.Writer) (actionsHydrationState, error) {
	deadline := time.Now().Add(timeout)
	for {
		state, err := readActionsHydrationState(ctx, target, leaseID)
		if err == nil && state.Workspace != "" {
			if expectedJob != "" && state.Job != "" && state.Job != expectedJob {
				return actionsHydrationState{}, exit(5, "GitHub Actions hydration marker for %s came from job %q, expected %q", leaseID, state.Job, expectedJob)
			}
			return state, nil
		}
		if ctx.Err() != nil {
			return actionsHydrationState{}, ctx.Err()
		}
		if time.Now().After(deadline) {
			return actionsHydrationState{}, exit(5, "timed out waiting for GitHub Actions hydration marker for %s", leaseID)
		}
		fmt.Fprintf(stderr, "waiting for GitHub Actions hydration marker id=%s...\n", leaseID)
		time.Sleep(10 * time.Second)
	}
}

func readActionsHydrationState(ctx context.Context, target SSHTarget, leaseID string) (actionsHydrationState, error) {
	out, err := runSSHOutput(ctx, target, remoteReadActionsHydrationState(leaseID))
	if err != nil {
		return actionsHydrationState{}, err
	}
	return parseActionsHydrationState(out), nil
}

func clearActionsHydrationState(ctx context.Context, target SSHTarget, leaseID string) error {
	if err := runSSHQuiet(ctx, target, remoteClearActionsHydrationState(leaseID)); err != nil {
		return exit(7, "clear GitHub Actions hydration marker on %s: %v", target.Host, err)
	}
	return nil
}

func writeActionsHydrationStop(ctx context.Context, target SSHTarget, leaseID string) error {
	if err := runSSHQuiet(ctx, target, remoteWriteActionsHydrationStop(leaseID)); err != nil {
		return exit(7, "write GitHub Actions hydration stop marker on %s: %v", target.Host, err)
	}
	return nil
}

func parseActionsHydrationState(value string) actionsHydrationState {
	state := actionsHydrationState{}
	for _, line := range strings.Split(value, "\n") {
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "WORKSPACE":
			state.Workspace = strings.TrimSpace(val)
		case "RUN_ID":
			state.RunID = strings.TrimSpace(val)
		case "READY_AT":
			state.ReadyAt = strings.TrimSpace(val)
		case "JOB":
			state.Job = strings.TrimSpace(val)
		case "ENV_FILE":
			state.EnvFile = strings.TrimSpace(val)
		case "SERVICES_FILE":
			state.ServicesFile = strings.TrimSpace(val)
		}
	}
	return state
}

func remoteReadActionsHydrationState(leaseID string) string {
	return "cat \"$HOME\"/" + shellQuote(actionsHydrationStatePath(leaseID)) + " 2>/dev/null || true"
}

func remoteClearActionsHydrationState(leaseID string) string {
	return "rm -f \"$HOME\"/" + shellQuote(actionsHydrationStatePath(leaseID)) + " \"$HOME\"/" + shellQuote(actionsHydrationEnvPath(leaseID)) + " \"$HOME\"/" + shellQuote(actionsHydrationServicesPath(leaseID)) + " \"$HOME\"/" + shellQuote(actionsHydrationStopPath(leaseID))
}

func actionsHydrationStatePath(leaseID string) string {
	return actionsHydrationDir() + "/" + leaseID + ".env"
}

func actionsHydrationEnvPath(leaseID string) string {
	return actionsHydrationDir() + "/" + leaseID + ".env.sh"
}

func actionsHydrationServicesPath(leaseID string) string {
	return actionsHydrationDir() + "/" + leaseID + ".services"
}

func actionsHydrationStopPath(leaseID string) string {
	return actionsHydrationDir() + "/" + leaseID + ".stop"
}

func actionsHydrationDir() string {
	return ".crabbox/actions"
}

func actionsRunURL(repo GitHubRepo, runID string) string {
	if repo.Slug() == "" || runID == "" {
		return ""
	}
	return "https://github.com/" + repo.Slug() + "/actions/runs/" + runID
}

func remoteWriteActionsHydrationStop(leaseID string) string {
	return "mkdir -p \"$HOME\"/" + shellQuote(actionsHydrationDir()) + " && touch \"$HOME\"/" + shellQuote(actionsHydrationStopPath(leaseID))
}

func githubActionsRunnerInstallScript(version string, ephemeral bool) string {
	if version == "" {
		version = "latest"
	}
	ephemeralArg := ""
	if ephemeral {
		ephemeralArg = "--ephemeral"
	}
	return fmt.Sprintf(`set -euo pipefail
if [ -z "${RUNNER_REPO:-}" ] || [ -z "${RUNNER_NAME:-}" ] || [ -z "${RUNNER_TOKEN:-}" ]; then
  echo "missing runner env" >&2
  exit 2
fi
version=%s
arch="$(uname -m)"
case "$arch" in
  x86_64|amd64) runner_arch=x64 ;;
  aarch64|arm64) runner_arch=arm64 ;;
  *) echo "unsupported runner arch: $arch" >&2; exit 2 ;;
esac
if [ "$(id -u)" = 0 ]; then
  export RUNNER_ALLOW_RUNASROOT=1
fi
if [ "$version" = latest ]; then
  version="$(curl -fsSL https://api.github.com/repos/actions/runner/releases/latest | jq -r '.tag_name' | sed 's/^v//')"
fi
runner_dir="$HOME/actions-runner"
mkdir -p "$runner_dir"
cd "$runner_dir"
if [ ! -x ./config.sh ] || [ ! -f ".crabbox-runner-version-$version-$runner_arch" ]; then
  rm -rf ./*
  curl -fsSL -o actions-runner.tar.gz "https://github.com/actions/runner/releases/download/v${version}/actions-runner-linux-${runner_arch}-${version}.tar.gz"
  tar xzf actions-runner.tar.gz
  rm actions-runner.tar.gz
  touch ".crabbox-runner-version-$version-$runner_arch"
fi
if [ -f .runner ]; then
  ./config.sh remove --unattended --token "$RUNNER_TOKEN" || true
fi
if command -v apt-get >/dev/null 2>&1 && grep -qi microsoft /proc/version 2>/dev/null; then
  sudo rm -rf /var/lib/apt/lists/*
  sudo apt-get update >/tmp/crabbox-actions-runner-apt-update.log 2>&1
fi
sudo ./bin/installdependencies.sh >/tmp/crabbox-actions-runner-deps.log 2>&1 || true
./config.sh --unattended --replace %s --url "https://github.com/${RUNNER_REPO}" --token "$RUNNER_TOKEN" --name "$RUNNER_NAME" --labels "$RUNNER_LABELS"
cat >"$HOME/actions-runner/run-crabbox.sh" <<'RUNNER'
#!/usr/bin/env bash
set -euo pipefail
if [ "$(id -u)" = 0 ]; then
  export RUNNER_ALLOW_RUNASROOT=1
fi
cd "$HOME/actions-runner"
exec ./run.sh
RUNNER
chmod +x "$HOME/actions-runner/run-crabbox.sh"
sudo tee /etc/systemd/system/crabbox-actions-runner.service >/dev/null <<SERVICE
[Unit]
Description=Crabbox GitHub Actions runner
After=network-online.target docker.service
Wants=network-online.target

[Service]
User=$(id -un)
WorkingDirectory=$HOME/actions-runner
ExecStart=$HOME/actions-runner/run-crabbox.sh
Restart=no

[Install]
WantedBy=multi-user.target
SERVICE
sudo systemctl daemon-reload
sudo systemctl enable --now crabbox-actions-runner.service
`, shellQuote(version), ephemeralArg)
}

func githubActionsRegistrationToken(ctx context.Context, repo GitHubRepo) (string, error) {
	out, err := ghOutput(ctx, "", "api", "-X", "POST", "repos/"+repo.Slug()+"/actions/runners/registration-token", "--jq", ".token")
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(out)
	if token == "" {
		return "", exit(3, "GitHub returned an empty runner registration token for %s", repo.Slug())
	}
	return token, nil
}

func resolveGitHubRepo(repo Repo, override string) (GitHubRepo, error) {
	if override != "" {
		return parseGitHubRepo(override)
	}
	return parseGitHubRepo(repo.RemoteURL)
}

var scpLikeGitHubRemote = regexp.MustCompile(`^[^@]+@github\.com:([^/]+)/(.+)$`)

func parseGitHubRepo(value string) (GitHubRepo, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return GitHubRepo{}, exit(2, "GitHub repo is unknown; set actions.repo or pass --repo owner/name")
	}
	if !strings.Contains(value, "://") {
		if match := scpLikeGitHubRemote.FindStringSubmatch(value); match != nil {
			return cleanGitHubRepo(match[1], match[2])
		}
		parts := strings.Split(strings.TrimSuffix(value, ".git"), "/")
		if len(parts) == 2 {
			return cleanGitHubRepo(parts[0], parts[1])
		}
	}
	u, err := url.Parse(value)
	if err == nil && strings.EqualFold(u.Host, "github.com") {
		parts := strings.Split(strings.Trim(path.Clean(u.Path), "/"), "/")
		if len(parts) >= 2 {
			return cleanGitHubRepo(parts[0], parts[1])
		}
	}
	return GitHubRepo{}, exit(2, "unsupported GitHub repo %q; expected owner/name or github.com remote", value)
}

func cleanGitHubRepo(owner, name string) (GitHubRepo, error) {
	owner = strings.TrimSpace(owner)
	name = strings.TrimSuffix(strings.TrimSpace(name), ".git")
	if owner == "" || name == "" {
		return GitHubRepo{}, exit(2, "invalid GitHub repo owner/name")
	}
	return GitHubRepo{Owner: owner, Name: name}, nil
}

func sanitizeGitHubRunnerLabel(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "unknown"
	}
	return out
}

func ghOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", exit(3, "gh %s: %v\n%s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func runGH(ctx context.Context, dir string, args ...string) error {
	_, err := ghOutput(ctx, dir, args...)
	return err
}

type stringListFlag []string

func (f *stringListFlag) String() string {
	return strings.Join(*f, ",")
}

func (f *stringListFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}
