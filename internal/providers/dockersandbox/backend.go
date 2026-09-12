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
	"github.com/openclaw/crabbox/internal/providers/shared"
)

var randomBytes = rand.Read
var statusPollInterval = 2 * time.Second

const dockerSandboxCleanupTimeout = 30 * time.Second

type backend struct {
	spec core.ProviderSpec
	cfg  core.Config
	rt   core.Runtime
}

func NewBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) core.Backend {
	cfg.Provider = providerName
	return &backend{spec: spec, cfg: cfg, rt: rt}
}

func (b *backend) Spec() core.ProviderSpec { return b.spec }

func (b *backend) Warmup(ctx context.Context, req core.WarmupRequest) error {
	if req.ActionsRunner {
		return core.Exit(2, "--actions-runner is not supported for provider=%s", providerName)
	}
	started := core.ClockNow(b.rt.Clock)
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
	total := core.ClockNow(b.rt.Clock).Sub(started)
	return shared.CompleteWarmup(b.rt, req.TimingJSON, shared.WarmupCompletion{
		Provider: providerName,
		LeaseID:  leaseID,
		Slug:     slug,
		Total:    total,
	})
}

func (b *backend) Run(ctx context.Context, req core.RunRequest) (result core.RunResult, retErr error) {
	if err := rejectRunOptions(b.spec, req); err != nil {
		return core.RunResult{}, err
	}
	workdir, err := dockerSandboxWorkdir(b.cfg, req.Repo.Root)
	if err != nil {
		return core.RunResult{}, err
	}
	started := core.ClockNow(b.rt.Clock)
	cli, err := newSBXCLI(b.cfg, b.rt)
	if err != nil {
		return core.RunResult{}, err
	}
	leaseID, sandboxName, slug := "", "", ""
	acquired := req.ID == ""
	if acquired {
		leaseID, sandboxName, slug, err = b.createSandbox(ctx, cli, req.Repo, req.Reclaim, req.RequestedSlug)
		if err != nil {
			return core.RunResult{}, err
		}
		fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=%s sandbox=%s name=%s\n", leaseID, slug, providerName, sandboxName, sandboxName)
	} else {
		leaseID, sandboxName, slug, err = resolveLeaseID(req.ID, req.Repo.Root, req.Reclaim, b.cfg.IdleTimeout)
		if err != nil {
			return core.RunResult{}, err
		}
	}
	result = core.RunResult{
		Provider: providerName, LeaseID: leaseID, Slug: slug, SyncDelegated: true,
		Session: &core.RunSessionHandle{
			Provider: providerName, LeaseID: leaseID, Slug: slug, Reused: !acquired, Kept: true,
			CleanupCommand: fmt.Sprintf("crabbox stop --provider %s %s", providerName, slug),
		},
	}
	shouldStop := acquired && !req.Keep
	var cleanupClaim core.LeaseClaim
	commandRan := false
	defer func() {
		result, retErr = shared.PinDelegatedRunFailure(result, retErr)
		if retErr != nil {
			core.HandleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		}
		// Preserve clone commits based on command success, before reporting can fail.
		if commandRan && retErr == nil && acquired && b.cfg.DockerSandbox.Clone && !req.Keep {
			shouldStop = false
			fmt.Fprintf(b.rt.Stderr, "docker-sandbox clone run kept sandbox to preserve unfetched commits; cleanup manually with: %s\n", result.Session.CleanupCommand)
		}
		if shouldStop {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), dockerSandboxCleanupTimeout)
			removeErr := core.RemoveLeaseClaimIfUnchangedAfter(leaseID, cleanupClaim, func() error {
				return cli.remove(cleanupCtx, sandboxName)
			})
			cancel()
			if removeErr != nil {
				result, retErr = shared.AppendDelegatedRunFailure(result, retErr, fmt.Errorf("docker-sandbox cleanup failed for %s: %w", sandboxName, removeErr), 1)
			} else {
				result.Session.Kept = false
			}
		}
		result.Total = core.ClockNow(b.rt.Clock).Sub(started)
		result = core.FinalizeRunResult(result, retErr)
		if commandRan {
			fmt.Fprintf(b.rt.Stderr, "docker-sandbox run summary sync_delegated=true command=%s total=%s exit=%d\n", result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), result.ExitCode)
		}
		if req.TimingJSON {
			timingErr := core.WriteTimingJSON(b.rt.Stderr, core.TimingReportWithRunResult(core.TimingReport{
				Provider: providerName, LeaseID: leaseID, Slug: slug, SyncDelegated: true,
				SyncPhases:  []core.TimingPhase{{Name: "sync", Skipped: true, Reason: "provider-delegated workspace"}},
				SyncSkipped: true, CommandMs: result.Command.Milliseconds(), TotalMs: result.Total.Milliseconds(),
				ExitCode: result.ExitCode, Label: strings.TrimSpace(req.Label),
			}, result, retErr))
			result, retErr = shared.AppendDelegatedRunFailure(result, retErr, timingErr, 1)
		}
	}()
	if shouldStop {
		cleanupClaim, err = dockerSandboxClaimForDeletion(leaseID)
		if err != nil {
			shouldStop = false
			return result, err
		}
	}
	intent, err := core.ParseCommandIntent(req.Command, req.ShellMode, req.CommandLiteralArgs)
	if err != nil {
		return result, err
	}
	command := intent.Argv("sh", "-lc")
	if req.EnvSummary || strings.TrimSpace(os.Getenv("CRABBOX_ENV_ALLOW")) != "" {
		core.PrintEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, req.Env)
	}
	envFile := ""
	if len(req.Env) > 0 {
		var cleanup func()
		envFile, cleanup, err = writeDockerSandboxEnvFile(req.Env)
		if err != nil {
			return result, err
		}
		defer cleanup()
	}
	fmt.Fprintf(b.rt.Stderr, "provider=%s lease=%s sandbox=%s workdir=%s sync_delegated=true\n", providerName, leaseID, sandboxName, workdir)
	commandStart := core.ClockNow(b.rt.Clock)
	req.Observation.Phase(core.RunPhaseCommand)
	stdout, stderr := req.Observation.CommandWriters(b.rt.Stdout, b.rt.Stderr, core.RunOutputProvider)
	exitCode, runErr := cli.execStream(ctx, sandboxName, workdir, envFile, command, stdout, stderr)
	result.Command = core.ClockNow(b.rt.Clock).Sub(commandStart)
	result.CommandText = strings.Join(req.Command, " ")
	commandRan = true
	outcome := shared.FinalizeDelegatedCommandOutcome(exitCode, runErr)
	result.ExitCode, result.Status, result.ErrorKind = outcome.ExitCode, outcome.Status, outcome.ErrorKind
	if runErr != nil {
		return result, shared.ExitErrorWithCause(result.ExitCode, fmt.Sprintf("docker-sandbox run failed: %v", shared.RedactErrorSecrets(runErr.Error())), runErr)
	}
	if result.ExitCode != 0 {
		return result, core.Exit(result.ExitCode, "docker-sandbox run exited %d", result.ExitCode)
	}
	return result, nil
}

func (b *backend) List(ctx context.Context, _ core.ListRequest) ([]core.LeaseView, error) {
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
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return nil, err
	}
	var servers []core.Server
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

func (b *backend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	cli, err := newSBXCLI(b.cfg, b.rt)
	if err != nil {
		return core.DoctorResult{}, err
	}
	result := core.DoctorResult{Provider: providerName}
	version, versionErr := cli.version(ctx)
	result.Checks = append(result.Checks, doctorCheck("sbx_version", versionErr, map[string]string{"version": version}))
	if versionErr == nil {
		result.Checks = append(result.Checks, dockerSandboxCompatibilityCheck(version))
	}
	records, listErr := cli.list(ctx)
	result.Checks = append(result.Checks, doctorCheck("sbx_inventory", listErr, map[string]string{"leases": fmt.Sprint(len(records))}))
	if _, diagnoseErr := cli.diagnose(ctx); diagnoseErr == nil {
		result.Checks = append(result.Checks, core.DoctorCheck{Status: "ok", Check: "sbx_diagnose", Message: "diagnose completed", Details: map[string]string{"mutation": "false"}})
	} else {
		result.Checks = append(result.Checks, core.DoctorCheck{Status: "warn", Check: "sbx_diagnose", Message: diagnoseErr.Error(), Details: map[string]string{"mutation": "false", "optional": "true"}})
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

func (b *backend) Status(ctx context.Context, req core.StatusRequest) (core.StatusView, error) {
	cli, err := newSBXCLI(b.cfg, b.rt)
	if err != nil {
		return core.StatusView{}, err
	}
	leaseID, sandboxName, slug, err := resolveLeaseID(req.ID, "", false, 0)
	if err != nil {
		return core.StatusView{}, err
	}
	deadline := core.ClockNow(b.rt.Clock).Add(req.WaitTimeout)
	if req.WaitTimeout <= 0 {
		deadline = core.ClockNow(b.rt.Clock).Add(5 * time.Minute)
	}
	for {
		records, err := cli.list(ctx)
		if err != nil {
			return core.StatusView{}, err
		}
		record, ok := findRecord(records, sandboxName)
		if !ok {
			return core.StatusView{}, core.Exit(4, "docker-sandbox sandbox %q is not present in sbx inventory", sandboxName)
		}
		view := statusFromRecord(leaseID, slug, record)
		if !req.Wait || view.Ready || dockerSandboxTerminalState(view.State) {
			return view, nil
		}
		remaining := deadline.Sub(core.ClockNow(b.rt.Clock))
		if remaining <= 0 {
			return core.StatusView{}, core.Exit(5, "timed out waiting for docker-sandbox sandbox %s to become ready", sandboxName)
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
			return core.StatusView{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (b *backend) Stop(ctx context.Context, req core.StopRequest) error {
	cli, err := newSBXCLI(b.cfg, b.rt)
	if err != nil {
		return err
	}
	leaseID, sandboxName, _, err := resolveLeaseID(req.ID, "", false, 0)
	if err != nil {
		return err
	}
	claim, err := dockerSandboxClaimForDeletion(leaseID)
	if err != nil {
		return err
	}
	if err := core.RemoveLeaseClaimIfUnchangedAfter(leaseID, claim, func() error {
		if err := cli.remove(ctx, sandboxName); err != nil && !dockerSandboxRemoveNotFoundError(err) {
			return err
		}
		return nil
	}); err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stderr, "released lease=%s sandbox=%s\n", leaseID, sandboxName)
	return nil
}

func dockerSandboxClaimForDeletion(leaseID string) (core.LeaseClaim, error) {
	claim, ok, err := core.ResolveLeaseClaimForProvider(leaseID, providerName)
	if err != nil {
		return core.LeaseClaim{}, err
	}
	if !ok || claim.LeaseID != leaseID {
		return core.LeaseClaim{}, core.Exit(4, "docker-sandbox sandbox %q is not claimed by Crabbox", leaseID)
	}
	return claim, nil
}

func (b *backend) Ports(ctx context.Context, req core.PortsRequest) (string, error) {
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

func (b *backend) Copy(ctx context.Context, req core.CopyRequest) error {
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
		return "", "", core.Exit(2, "copy requires one side to use SANDBOX:PATH")
	}
	if srcSandbox != "" && dstSandbox != "" {
		return "", "", core.Exit(2, "copy does not support sandbox-to-sandbox transfers")
	}
	if srcSandbox != "" {
		if !strings.EqualFold(srcSandbox, "SANDBOX") {
			return "", "", core.Exit(2, "copy source must use SANDBOX:PATH")
		}
		src = sandboxName + ":" + srcPath
	}
	if dstSandbox != "" {
		if !strings.EqualFold(dstSandbox, "SANDBOX") {
			return "", "", core.Exit(2, "copy destination must use SANDBOX:PATH")
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

func (b *backend) createSandbox(ctx context.Context, cli *sbxCLI, repo core.Repo, reclaim bool, requestedSlug string) (string, string, string, error) {
	if err := validateCreateRepo(b.cfg, repo); err != nil {
		return "", "", "", err
	}
	sandboxName := newSandboxName(repo)
	if err := cli.create(ctx, sandboxName, repo); err != nil {
		return "", "", "", err
	}
	leaseID := leasePrefix + sandboxName
	slug, err := core.AllocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return "", "", "", b.removeCreatedSandboxAfterClaimFailure(cli, sandboxName, err)
	}
	if err := core.ClaimLeaseForRepoProviderPond(leaseID, slug, providerName, b.cfg.Pond, repo.Root, b.cfg.IdleTimeout, reclaim); err != nil {
		return "", "", "", b.removeCreatedSandboxAfterClaimFailure(cli, sandboxName, err)
	}
	return leaseID, sandboxName, slug, nil
}

func (b *backend) removeCreatedSandboxAfterClaimFailure(cli *sbxCLI, sandboxName string, primaryErr error) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), dockerSandboxCleanupTimeout)
	defer cancel()
	if cleanupErr := cli.remove(cleanupCtx, sandboxName); cleanupErr != nil {
		leakErr := fmt.Errorf("cleanup docker-sandbox sandbox %s after claim setup failure: %w; run `sbx rm --force %s` to retry cleanup", sandboxName, cleanupErr, sandboxName)
		fmt.Fprintf(b.rt.Stderr, "warning: %v\n", leakErr)
		return errors.Join(primaryErr, leakErr)
	}
	return primaryErr
}

func resolveLeaseID(id, repoRoot string, reclaim bool, idleTimeout time.Duration) (string, string, string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", "", "", core.Exit(2, "provider=docker-sandbox requires a Crabbox-created sandbox slug or lease id")
	}
	probes := []string{id}
	if !strings.HasPrefix(id, leasePrefix) {
		probes = append(probes, leasePrefix+id)
	}
	for _, probe := range probes {
		claim, ok, err := core.ResolveLeaseClaimForProvider(probe, providerName)
		if err != nil {
			return "", "", "", err
		}
		if !ok {
			continue
		}
		if repoRoot != "" {
			if err := core.ClaimLeaseForRepoProviderPond(claim.LeaseID, claim.Slug, providerName, claim.Pond, repoRoot, timeoutOrDefault(idleTimeout, time.Duration(claim.IdleTimeoutSeconds)*time.Second), reclaim); err != nil {
				return "", "", "", err
			}
		}
		slug := claim.Slug
		if strings.TrimSpace(slug) == "" {
			slug = core.NewLeaseSlug(claim.LeaseID)
		}
		return claim.LeaseID, sandboxNameFromLeaseID(claim.LeaseID), slug, nil
	}
	return "", "", "", core.Exit(4, "docker-sandbox sandbox %q is not claimed by Crabbox; use a Crabbox slug or %s<sandbox-name>", id, leasePrefix)
}

func rejectRunOptions(spec core.ProviderSpec, req core.RunRequest) error {
	if err := core.RejectDelegatedSyncOptionsForSpec(spec, req); err != nil {
		return err
	}
	if req.Options.Desktop || req.Options.Browser || req.Options.Code {
		return core.Exit(2, "provider=%s does not support desktop, browser, or code-server options in v1", providerName)
	}
	if req.Options.Tailscale.Enabled {
		return core.Exit(2, "provider=%s is delegated-run only and does not support Tailscale options", providerName)
	}
	if !req.ApplyLocalPatch && strings.TrimSpace(req.Repo.Root) == "" {
		return core.Exit(2, "provider=%s requires a local workspace", providerName)
	}
	return nil
}

func validateCreateRepo(cfg core.Config, repo core.Repo) error {
	if strings.TrimSpace(repo.Root) == "" {
		return core.Exit(2, "provider=%s requires a local workspace", providerName)
	}
	if cfg.DockerSandbox.Clone {
		cmd := exec.Command("git", "rev-parse", "--git-dir", "--git-common-dir", "--is-inside-work-tree")
		cmd.Dir = repo.Root
		output, err := cmd.CombinedOutput()
		if err != nil {
			return core.Exit(2, "docker-sandbox --clone requires a normal Git repository workspace: %v: %s", err, strings.TrimSpace(string(output)))
		}
		lines := strings.Split(strings.TrimSpace(string(output)), "\n")
		if len(lines) != 3 || strings.TrimSpace(lines[2]) != "true" {
			return core.Exit(2, "docker-sandbox --clone requires a normal Git repository workspace: git rev-parse output was %q", strings.TrimSpace(string(output)))
		}
		resolveGitPath := func(value string) string {
			value = strings.TrimSpace(value)
			if filepath.IsAbs(value) {
				return filepath.Clean(value)
			}
			return filepath.Clean(filepath.Join(repo.Root, value))
		}
		if resolveGitPath(lines[0]) != resolveGitPath(lines[1]) {
			return core.Exit(2, "docker-sandbox --clone requires a normal Git repository workspace: linked Git worktrees are not supported")
		}
	}
	return nil
}

func dockerSandboxAgent(cfg core.Config) string {
	agent := strings.TrimSpace(cfg.DockerSandbox.Agent)
	if agent == "" {
		return defaultAgent
	}
	return agent
}

func dockerSandboxWorkdir(cfg core.Config, repoRoot string) (string, error) {
	workdir := strings.TrimSpace(cfg.DockerSandbox.Workdir)
	if workdir == "" {
		workdir = repoRoot
	}
	if workdir == "" {
		workdir = defaultWorkdir
	}
	clean := path.Clean(workdir)
	if !strings.HasPrefix(clean, "/") {
		return "", core.Exit(2, "docker-sandbox workdir %q must be an absolute path", workdir)
	}
	return clean, nil
}

func sandboxNameFromLeaseID(leaseID string) string {
	return strings.TrimPrefix(leaseID, leasePrefix)
}

func newSandboxName(repo core.Repo) string {
	base := core.NormalizeLeaseSlug(repo.Name)
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

func serverFromClaimRecord(claim core.LeaseClaim, record sandboxRecord) core.Server {
	state := core.Blank(record.State, "unknown")
	labels := map[string]string{
		"provider":  providerName,
		"lease":     claim.LeaseID,
		"slug":      claim.Slug,
		"target":    targetLinux,
		"state":     state,
		"sandbox":   record.Name,
		"agent":     core.Blank(record.Agent, defaultAgent),
		"workspace": record.Workspace,
	}
	server := core.Server{
		Provider: providerName,
		CloudID:  record.Name,
		Name:     record.Name,
		Status:   state,
		Labels:   labels,
	}
	server.ServerType.Name = providerName
	return server
}

func statusFromRecord(leaseID, slug string, record sandboxRecord) core.StatusView {
	state := core.Blank(record.State, "unknown")
	return core.StatusView{
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
			"agent":     core.Blank(record.Agent, defaultAgent),
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

func doctorCheck(name string, err error, details map[string]string) core.DoctorCheck {
	if details == nil {
		details = map[string]string{}
	}
	details["mutation"] = "false"
	if err != nil {
		return core.DoctorCheck{Status: "error", Check: name, Message: err.Error(), Details: details}
	}
	return core.DoctorCheck{Status: "ok", Check: name, Message: "ready", Details: details}
}

func dockerSandboxCompatibilityCheck(version string) core.DoctorCheck {
	details := map[string]string{
		"mutation": "false",
		"baseline": baselineSBX,
		"version":  version,
	}
	if sbxVersionMatchesBaseline(version) {
		return core.DoctorCheck{
			Status:  "ok",
			Check:   "sbx_compatibility",
			Message: fmt.Sprintf("matches documented compatibility baseline %s", baselineSBX),
			Details: details,
		}
	}
	return core.DoctorCheck{
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
		if !core.ValidShellEnvName(key) {
			return "", core.Exit(2, "docker-sandbox env name %q is not a valid shell environment name", key)
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, key := range keys {
		value := env[key]
		if strings.ContainsAny(value, "\x00\r\n") {
			return "", core.Exit(2, "docker-sandbox env value for %s cannot contain NUL or newlines when forwarded through sbx --env-file", key)
		}
		fmt.Fprintf(&b, "%s=%s\n", key, value)
	}
	return b.String(), nil
}
