package dockersandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

var randomBytes = rand.Read
var statusPollInterval = 2 * time.Second

type backend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

func NewBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = providerName
	return &backend{spec: spec, cfg: cfg, rt: rt}
}

func (b *backend) Spec() ProviderSpec { return b.spec }

func (b *backend) Warmup(ctx context.Context, req WarmupRequest) error {
	if req.ActionsRunner {
		return exit(2, "--actions-runner is not supported for provider=%s", providerName)
	}
	started := b.now()
	cli, err := newSBXCLI(b.cfg, b.rt)
	if err != nil {
		return err
	}
	leaseID, sandboxName, slug, err := b.createSandbox(ctx, cli, req.Repo, req.Reclaim, req.RequestedSlug)
	if err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s sandbox=%s name=%s\n", leaseID, slug, providerName, sandboxName, sandboxName)
	if !req.Keep {
		fmt.Fprintf(b.rt.Stderr, "warning: docker-sandbox warmup keeps the sandbox until explicit stop\n")
	}
	total := b.now().Sub(started)
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
	if err := rejectRunOptions(b.spec, req); err != nil {
		return RunResult{}, err
	}
	workdir, err := dockerSandboxWorkdir(b.cfg, req.Repo.Root)
	if err != nil {
		return RunResult{}, err
	}
	started := b.now()
	cli, err := newSBXCLI(b.cfg, b.rt)
	if err != nil {
		return RunResult{}, err
	}
	leaseID, sandboxName, slug := "", "", ""
	acquired := false
	if req.ID == "" {
		leaseID, sandboxName, slug, err = b.createSandbox(ctx, cli, req.Repo, req.Reclaim, req.RequestedSlug)
		if err != nil {
			return RunResult{}, err
		}
		fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=%s sandbox=%s name=%s\n", leaseID, slug, providerName, sandboxName, sandboxName)
		acquired = true
	} else {
		leaseID, sandboxName, slug, err = resolveLeaseID(req.ID, req.Repo.Root, req.Reclaim, b.cfg.IdleTimeout)
		if err != nil {
			return RunResult{}, err
		}
	}
	shouldStop := acquired && !req.Keep
	if shouldStop {
		defer func() {
			if !shouldStop {
				return
			}
			if removeErr := cli.remove(context.Background(), sandboxName); removeErr != nil {
				fmt.Fprintf(b.rt.Stderr, "warning: docker-sandbox rm failed for %s: %v\n", sandboxName, removeErr)
				return
			}
			removeLeaseClaim(leaseID)
		}()
	}
	command, err := buildCommand(req.Command, req.ShellMode)
	if err != nil {
		return RunResult{}, err
	}
	if req.EnvSummary || strings.TrimSpace(os.Getenv("CRABBOX_ENV_ALLOW")) != "" {
		printEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, req.Env)
	}
	envFile := ""
	if len(req.Env) > 0 {
		var cleanup func()
		envFile, cleanup, err = writeDockerSandboxEnvFile(req.Env)
		if err != nil {
			return RunResult{}, err
		}
		defer cleanup()
	}
	fmt.Fprintf(b.rt.Stderr, "provider=%s lease=%s sandbox=%s workdir=%s sync_delegated=true\n", providerName, leaseID, sandboxName, workdir)
	commandStart := b.now()
	exitCode, runErr := cli.execStream(ctx, sandboxName, workdir, envFile, command, b.rt.Stdout, b.rt.Stderr)
	commandDuration := b.now().Sub(commandStart)
	result := RunResult{
		ExitCode:      exitCode,
		Command:       commandDuration,
		Total:         b.now().Sub(started),
		SyncDelegated: true,
		Provider:      providerName,
		LeaseID:       leaseID,
		Slug:          slug,
		CommandText:   strings.Join(req.Command, " "),
		Session: (&coreRunSessionHandle{
			Provider:       providerName,
			LeaseID:        leaseID,
			Slug:           slug,
			Reused:         !acquired,
			Kept:           !shouldStop,
			CleanupCommand: fmt.Sprintf("crabbox stop --provider %s %s", providerName, slug),
		}).handle(),
	}
	fmt.Fprintf(b.rt.Stderr, "docker-sandbox run summary sync_delegated=true command=%s total=%s exit=%d\n", commandDuration.Round(time.Millisecond), result.Total.Round(time.Millisecond), exitCode)
	if req.TimingJSON {
		if err := writeTimingJSON(b.rt.Stderr, timingReport{
			Provider:      providerName,
			LeaseID:       leaseID,
			Slug:          slug,
			SyncDelegated: true,
			SyncPhases:    []timingPhase{{Name: "sync", Skipped: true, Reason: "provider-delegated workspace"}},
			SyncSkipped:   true,
			CommandMs:     commandDuration.Milliseconds(),
			TotalMs:       result.Total.Milliseconds(),
			ExitCode:      exitCode,
			Label:         strings.TrimSpace(req.Label),
		}); err != nil {
			return result, err
		}
	}
	if runErr != nil {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		result.Session.Kept = !shouldStop
		return result, exit(1, "docker-sandbox run failed: %v", runErr)
	}
	if exitCode != 0 {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		result.Session.Kept = !shouldStop
		return result, exit(exitCode, "docker-sandbox run exited %d", exitCode)
	}
	if acquired && b.cfg.DockerSandbox.Clone && !req.Keep {
		shouldStop = false
		result.Session.Kept = true
		fmt.Fprintf(b.rt.Stderr, "docker-sandbox clone run kept sandbox to preserve unfetched commits; cleanup manually with: %s\n", result.Session.CleanupCommand)
	}
	result.Session.Kept = !shouldStop
	return result, nil
}

type coreRunSessionHandle struct {
	Provider       string
	LeaseID        string
	Slug           string
	Reused         bool
	Kept           bool
	CleanupCommand string
}

func (h coreRunSessionHandle) handle() *core.RunSessionHandle {
	return &core.RunSessionHandle{
		Provider:       h.Provider,
		LeaseID:        h.LeaseID,
		Slug:           h.Slug,
		Reused:         h.Reused,
		Kept:           h.Kept,
		CleanupCommand: h.CleanupCommand,
	}
}

func (b *backend) List(ctx context.Context, _ ListRequest) ([]LeaseView, error) {
	cli, err := newSBXCLI(b.cfg, b.rt)
	if err != nil {
		return nil, err
	}
	live, err := cli.list(ctx)
	if err != nil {
		return nil, err
	}
	byName := map[string]sandboxRecord{}
	for _, record := range live {
		if record.Name != "" {
			byName[record.Name] = record
		}
	}
	claims, err := listLeaseClaims()
	if err != nil {
		return nil, err
	}
	var servers []Server
	for _, claim := range claims {
		if claim.Provider != providerName {
			continue
		}
		name := sandboxNameFromLeaseID(claim.LeaseID)
		record, ok := byName[name]
		if !ok {
			continue
		}
		servers = append(servers, serverFromClaimRecord(claim, record))
	}
	return servers, nil
}

func (b *backend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	cli, err := newSBXCLI(b.cfg, b.rt)
	if err != nil {
		return DoctorResult{}, err
	}
	result := DoctorResult{Provider: providerName}
	version, versionErr := cli.version(ctx)
	result.Checks = append(result.Checks, doctorCheck("sbx_version", versionErr, map[string]string{"version": version}))
	if versionErr == nil {
		result.Checks = append(result.Checks, dockerSandboxCompatibilityCheck(version))
	}
	records, listErr := cli.list(ctx)
	result.Checks = append(result.Checks, doctorCheck("sbx_inventory", listErr, map[string]string{"leases": fmt.Sprint(len(records))}))
	if _, diagnoseErr := cli.diagnose(ctx); diagnoseErr == nil {
		result.Checks = append(result.Checks, DoctorCheck{Status: "ok", Check: "sbx_diagnose", Message: "diagnose completed", Details: map[string]string{"mutation": "false"}})
	} else {
		result.Checks = append(result.Checks, DoctorCheck{Status: "warn", Check: "sbx_diagnose", Message: diagnoseErr.Error(), Details: map[string]string{"mutation": "false", "optional": "true"}})
	}
	if versionErr != nil || listErr != nil {
		result.Status = "error"
		result.Message = "cli=blocked control_plane=blocked inventory=blocked api=list mutation=false"
		if versionErr != nil {
			return result, versionErr
		}
		return result, listErr
	}
	result.Status = "ok"
	result.Message = fmt.Sprintf("cli=ready control_plane=ready inventory=ready api=list mutation=false leases=%d runtime=unchecked", len(records))
	return result, nil
}

func (b *backend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	cli, err := newSBXCLI(b.cfg, b.rt)
	if err != nil {
		return StatusView{}, err
	}
	leaseID, sandboxName, slug, err := resolveLeaseID(req.ID, "", false, 0)
	if err != nil {
		return StatusView{}, err
	}
	deadline := b.now().Add(req.WaitTimeout)
	if req.WaitTimeout <= 0 {
		deadline = b.now().Add(5 * time.Minute)
	}
	for {
		records, err := cli.list(ctx)
		if err != nil {
			return StatusView{}, err
		}
		record, ok := findRecord(records, sandboxName)
		if !ok {
			return StatusView{}, exit(4, "docker-sandbox sandbox %q is not present in sbx inventory", sandboxName)
		}
		view := statusFromRecord(leaseID, slug, record)
		if !req.Wait || view.Ready || dockerSandboxTerminalState(view.State) {
			return view, nil
		}
		remaining := deadline.Sub(b.now())
		if remaining <= 0 {
			return StatusView{}, exit(5, "timed out waiting for docker-sandbox sandbox %s to become ready", sandboxName)
		}
		sleepFor := statusPollInterval
		if remaining < sleepFor {
			sleepFor = remaining
		}
		timer := time.NewTimer(sleepFor)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return StatusView{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (b *backend) Stop(ctx context.Context, req StopRequest) error {
	cli, err := newSBXCLI(b.cfg, b.rt)
	if err != nil {
		return err
	}
	leaseID, sandboxName, _, err := resolveLeaseID(req.ID, "", false, 0)
	if err != nil {
		return err
	}
	if err := cli.remove(ctx, sandboxName); err != nil {
		if !dockerSandboxRemoveNotFoundError(err) {
			return err
		}
	}
	removeLeaseClaim(leaseID)
	fmt.Fprintf(b.rt.Stderr, "released lease=%s sandbox=%s\n", leaseID, sandboxName)
	return nil
}

func (b *backend) Ports(ctx context.Context, req PortsRequest) (string, error) {
	cli, err := newSBXCLI(b.cfg, b.rt)
	if err != nil {
		return "", err
	}
	_, sandboxName, _, err := resolveLeaseID(req.ID, "", false, 0)
	if err != nil {
		return "", err
	}
	return cli.ports(ctx, sandboxName, req.Publish, req.Unpublish, req.JSON)
}

func (b *backend) Copy(ctx context.Context, req CopyRequest) error {
	cli, err := newSBXCLI(b.cfg, b.rt)
	if err != nil {
		return err
	}
	_, sandboxName, _, err := resolveLeaseID(req.ID, "", false, 0)
	if err != nil {
		return err
	}
	src, dst, err := rewriteSandboxCopyPaths(sandboxName, req.Source, req.Destination)
	if err != nil {
		return err
	}
	return cli.copy(ctx, src, dst, req.FollowLink)
}

func dockerSandboxRemoveNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "not found") ||
		strings.Contains(text, "no such sandbox") ||
		strings.Contains(text, "no such container")
}

func rewriteSandboxCopyPaths(sandboxName, src, dst string) (string, string, error) {
	src = strings.TrimSpace(src)
	dst = strings.TrimSpace(dst)
	srcSandbox, srcPath := parseSandboxPathSpec(src)
	dstSandbox, dstPath := parseSandboxPathSpec(dst)
	if srcSandbox == "" && dstSandbox == "" {
		return "", "", exit(2, "copy requires one side to use SANDBOX:PATH")
	}
	if srcSandbox != "" && dstSandbox != "" {
		return "", "", exit(2, "copy does not support sandbox-to-sandbox transfers")
	}
	if srcSandbox != "" {
		if !strings.EqualFold(srcSandbox, "SANDBOX") {
			return "", "", exit(2, "copy source must use SANDBOX:PATH")
		}
		src = sandboxName + ":" + srcPath
	}
	if dstSandbox != "" {
		if !strings.EqualFold(dstSandbox, "SANDBOX") {
			return "", "", exit(2, "copy destination must use SANDBOX:PATH")
		}
		dst = sandboxName + ":" + dstPath
	}
	return src, dst, nil
}

func parseSandboxPathSpec(value string) (string, string) {
	if len(value) < 2 {
		return "", ""
	}
	idx := strings.IndexByte(value, ':')
	if idx <= 0 {
		return "", ""
	}
	prefix := strings.TrimSpace(value[:idx])
	if !strings.EqualFold(prefix, "SANDBOX") {
		return "", ""
	}
	return prefix, value[idx+1:]
}

func (b *backend) createSandbox(ctx context.Context, cli *sbxCLI, repo Repo, reclaim bool, requestedSlug string) (string, string, string, error) {
	if err := validateCreateRepo(b.cfg, repo); err != nil {
		return "", "", "", err
	}
	sandboxName := newSandboxName(repo)
	if err := cli.create(ctx, sandboxName, repo); err != nil {
		return "", "", "", err
	}
	leaseID := leasePrefix + sandboxName
	slug, err := allocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		_ = cli.remove(context.Background(), sandboxName)
		return "", "", "", err
	}
	if err := claimLeaseForRepoProviderPond(leaseID, slug, providerName, b.cfg.Pond, repo.Root, b.cfg.IdleTimeout, reclaim); err != nil {
		_ = cli.remove(context.Background(), sandboxName)
		return "", "", "", err
	}
	return leaseID, sandboxName, slug, nil
}

func resolveLeaseID(id, repoRoot string, reclaim bool, idleTimeout time.Duration) (string, string, string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", "", "", exit(2, "provider=docker-sandbox requires a Crabbox-created sandbox slug or lease id")
	}
	probes := []string{id}
	if !strings.HasPrefix(id, leasePrefix) {
		probes = append(probes, leasePrefix+id)
	}
	for _, probe := range probes {
		claim, ok, err := resolveLeaseClaimForProvider(probe, providerName)
		if err != nil {
			return "", "", "", err
		}
		if !ok {
			continue
		}
		if repoRoot != "" {
			if err := claimLeaseForRepoProviderPond(claim.LeaseID, claim.Slug, providerName, claim.Pond, repoRoot, timeoutOrDefault(idleTimeout, time.Duration(claim.IdleTimeoutSeconds)*time.Second), reclaim); err != nil {
				return "", "", "", err
			}
		}
		slug := claim.Slug
		if strings.TrimSpace(slug) == "" {
			slug = newLeaseSlug(claim.LeaseID)
		}
		return claim.LeaseID, sandboxNameFromLeaseID(claim.LeaseID), slug, nil
	}
	return "", "", "", exit(4, "docker-sandbox sandbox %q is not claimed by Crabbox; use a Crabbox slug or %s<sandbox-name>", id, leasePrefix)
}

func rejectRunOptions(spec ProviderSpec, req RunRequest) error {
	if err := core.RejectDelegatedSyncOptionsForSpec(spec, req); err != nil {
		return err
	}
	if req.Options.Desktop || req.Options.Browser || req.Options.Code {
		return exit(2, "provider=%s does not support desktop, browser, or code-server options in v1", providerName)
	}
	if req.Options.Tailscale.Enabled {
		return exit(2, "provider=%s is delegated-run only and does not support Tailscale options", providerName)
	}
	if !req.ApplyLocalPatch && strings.TrimSpace(req.Repo.Root) == "" {
		return exit(2, "provider=%s requires a local workspace", providerName)
	}
	return nil
}

func validateCreateRepo(cfg Config, repo Repo) error {
	if strings.TrimSpace(repo.Root) == "" {
		return exit(2, "provider=%s requires a local workspace", providerName)
	}
	if cfg.DockerSandbox.Clone {
		cmd := exec.Command("git", "rev-parse", "--git-dir", "--git-common-dir", "--is-inside-work-tree")
		cmd.Dir = repo.Root
		output, err := cmd.CombinedOutput()
		if err != nil {
			return exit(2, "docker-sandbox --clone requires a normal Git repository workspace: %v: %s", err, strings.TrimSpace(string(output)))
		}
		lines := strings.Split(strings.TrimSpace(string(output)), "\n")
		if len(lines) != 3 || strings.TrimSpace(lines[2]) != "true" {
			return exit(2, "docker-sandbox --clone requires a normal Git repository workspace: git rev-parse output was %q", strings.TrimSpace(string(output)))
		}
		resolveGitPath := func(value string) string {
			value = strings.TrimSpace(value)
			if filepath.IsAbs(value) {
				return filepath.Clean(value)
			}
			return filepath.Clean(filepath.Join(repo.Root, value))
		}
		if resolveGitPath(lines[0]) != resolveGitPath(lines[1]) {
			return exit(2, "docker-sandbox --clone requires a normal Git repository workspace: linked Git worktrees are not supported")
		}
	}
	return nil
}

func buildCommand(command []string, shellMode bool) ([]string, error) {
	if len(command) == 0 {
		return nil, errors.New("missing command")
	}
	if shellMode {
		return []string{"sh", "-lc", strings.Join(command, " ")}, nil
	}
	if len(command) == 1 && shouldUseShell(command) {
		return []string{"sh", "-lc", command[0]}, nil
	}
	if shouldUseShell(command) || leadingEnvAssignment(command) {
		return []string{"sh", "-lc", shellScriptFromArgv(command)}, nil
	}
	return command, nil
}

func dockerSandboxAgent(cfg Config) string {
	agent := strings.TrimSpace(cfg.DockerSandbox.Agent)
	if agent == "" {
		return defaultAgent
	}
	return agent
}

func dockerSandboxWorkdir(cfg Config, repoRoot string) (string, error) {
	workdir := strings.TrimSpace(cfg.DockerSandbox.Workdir)
	if workdir == "" {
		workdir = repoRoot
	}
	if workdir == "" {
		workdir = defaultWorkdir
	}
	clean := path.Clean(workdir)
	if !strings.HasPrefix(clean, "/") {
		return "", exit(2, "docker-sandbox workdir %q must be an absolute path", workdir)
	}
	return clean, nil
}

func sandboxNameFromLeaseID(leaseID string) string {
	return strings.TrimPrefix(leaseID, leasePrefix)
}

func newSandboxName(repo Repo) string {
	base := normalizeLeaseSlug(repo.Name)
	if base == "" {
		base = "crabbox"
	}
	base = strings.TrimPrefix(base, strings.TrimSuffix(namePrefix, "-")+"-")
	maxBase := maxSandboxNameLen - len(namePrefix) - 1 - sandboxNameSuffixLen
	if maxBase < 1 {
		maxBase = 1
	}
	if len(base) > maxBase {
		base = strings.Trim(base[:maxBase], "-")
	}
	if base == "" {
		base = "crabbox"
	}
	return namePrefix + base + "-" + randomSuffix()
}

func randomSuffix() string {
	var b [3]byte
	if _, err := randomBytes(b[:]); err != nil {
		value := fmt.Sprintf("%x", time.Now().UnixNano())
		if len(value) > sandboxNameSuffixLen {
			return value[:sandboxNameSuffixLen]
		}
		return value
	}
	return hex.EncodeToString(b[:])
}

func serverFromClaimRecord(claim core.LeaseClaim, record sandboxRecord) Server {
	state := blank(record.State, "unknown")
	labels := map[string]string{
		"provider":  providerName,
		"lease":     claim.LeaseID,
		"slug":      claim.Slug,
		"target":    targetLinux,
		"state":     state,
		"sandbox":   record.Name,
		"agent":     blank(record.Agent, defaultAgent),
		"workspace": record.Workspace,
	}
	server := Server{
		Provider: providerName,
		CloudID:  record.Name,
		Name:     record.Name,
		Status:   state,
		Labels:   labels,
	}
	server.ServerType.Name = providerName
	return server
}

func statusFromRecord(leaseID, slug string, record sandboxRecord) StatusView {
	state := blank(record.State, "unknown")
	return StatusView{
		ID:         leaseID,
		Slug:       slug,
		Provider:   providerName,
		TargetOS:   targetLinux,
		State:      state,
		ServerID:   record.Name,
		ServerType: "docker-sandbox",
		Network:    NetworkPublic,
		Ready:      isReadyState(state),
		Labels: map[string]string{
			"provider":  providerName,
			"lease":     leaseID,
			"slug":      slug,
			"state":     state,
			"sandbox":   record.Name,
			"agent":     blank(record.Agent, defaultAgent),
			"workspace": record.Workspace,
		},
	}
}

func findRecord(records []sandboxRecord, name string) (sandboxRecord, bool) {
	for _, record := range records {
		if record.Name == name || record.ID == name {
			return record, true
		}
	}
	return sandboxRecord{}, false
}

func isReadyState(state string) bool {
	switch strings.TrimSpace(strings.ToLower(state)) {
	case "running", "ready", "started", "active":
		return true
	default:
		return false
	}
}

func dockerSandboxTerminalState(state string) bool {
	switch strings.TrimSpace(strings.ToLower(state)) {
	case "expired", "failed", "released", "stopped", "stopped_with_code", "terminated":
		return true
	default:
		return false
	}
}

func timeoutOrDefault(primary, fallback time.Duration) time.Duration {
	if primary > 0 {
		return primary
	}
	return fallback
}

func doctorCheck(name string, err error, details map[string]string) DoctorCheck {
	if details == nil {
		details = map[string]string{}
	}
	details["mutation"] = "false"
	if err != nil {
		return DoctorCheck{Status: "error", Check: name, Message: err.Error(), Details: details}
	}
	return DoctorCheck{Status: "ok", Check: name, Message: "ready", Details: details}
}

func dockerSandboxCompatibilityCheck(version string) DoctorCheck {
	details := map[string]string{
		"mutation": "false",
		"baseline": baselineSBX,
		"version":  version,
	}
	if sbxVersionMatchesBaseline(version) {
		return DoctorCheck{
			Status:  "ok",
			Check:   "sbx_compatibility",
			Message: fmt.Sprintf("matches documented compatibility baseline %s", baselineSBX),
			Details: details,
		}
	}
	return DoctorCheck{
		Status:  "warn",
		Check:   "sbx_compatibility",
		Message: fmt.Sprintf("best-effort compatibility; validated baseline is %s", baselineSBX),
		Details: details,
	}
}

func sbxVersionMatchesBaseline(version string) bool {
	text := strings.TrimSpace(version)
	if text == "" {
		return false
	}
	baseline := strings.TrimPrefix(baselineSBX, "v")
	for _, field := range strings.Fields(text) {
		field = strings.Trim(field, " ,;()[]{}")
		if strings.TrimPrefix(field, "v") == baseline {
			return true
		}
	}
	return false
}

func (b *backend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now()
	}
	return time.Now()
}

func writeDockerSandboxEnvFile(env map[string]string) (string, func(), error) {
	file, err := os.CreateTemp("", "crabbox-docker-sandbox-env-*.env")
	if err != nil {
		return "", nil, fmt.Errorf("create docker-sandbox env file: %w", err)
	}
	localPath := file.Name()
	cleanup := func() { _ = os.Remove(localPath) }
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			cleanup()
		}
	}()
	body, err := formatDockerSandboxEnvFile(env)
	if err != nil {
		return "", nil, err
	}
	if _, err := file.WriteString(body); err != nil {
		return "", nil, fmt.Errorf("write docker-sandbox env file: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", nil, fmt.Errorf("close docker-sandbox env file: %w", err)
	}
	keep = true
	return localPath, cleanup, nil
}

func formatDockerSandboxEnvFile(env map[string]string) (string, error) {
	keys := make([]string, 0, len(env))
	for key := range env {
		if !validDockerSandboxEnvName(key) {
			return "", exit(2, "docker-sandbox env name %q is not a valid shell environment name", key)
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, key := range keys {
		value := env[key]
		if strings.ContainsAny(value, "\x00\r\n") {
			return "", exit(2, "docker-sandbox env value for %s cannot contain NUL or newlines when forwarded through sbx --env-file", key)
		}
		fmt.Fprintf(&b, "%s=%s\n", key, value)
	}
	return b.String(), nil
}

func validDockerSandboxEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}
