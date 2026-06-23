package upstashbox

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
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
	return &backend{spec: spec, cfg: cfg, rt: rt}
}

func (b *backend) Spec() ProviderSpec { return b.spec }

func (b *backend) Warmup(ctx context.Context, req WarmupRequest) error {
	if req.ActionsRunner {
		return exit(2, "--actions-runner is not supported for provider=%s", providerName)
	}
	started := b.now()
	client, err := newAPI(b.cfg, b.rt)
	if err != nil {
		return err
	}
	leaseID, box, slug, err := b.createBox(ctx, client, req.Repo, req.Keep, req.Reclaim, req.RequestedSlug)
	if err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s box=%s name=%s\n", leaseID, slug, providerName, box.ID, box.Name)
	if !req.Keep {
		fmt.Fprintf(b.rt.Stderr, "warning: upstash-box warmup keeps the box until explicit stop\n")
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

func (b *backend) Run(ctx context.Context, req RunRequest) (result RunResult, retErr error) {
	workdir, err := cleanWorkdir(workdir(b.cfg))
	if err != nil {
		return RunResult{}, err
	}
	folder, err := workspaceFolder(workdir)
	if err != nil {
		return RunResult{}, err
	}
	started := b.now()
	client, err := newAPI(b.cfg, b.rt)
	if err != nil {
		return RunResult{}, err
	}
	leaseID, boxID, slug := "", "", ""
	acquired := false
	if req.ID == "" {
		var box boxData
		leaseID, box, slug, err = b.createBox(ctx, client, req.Repo, req.Keep, req.Reclaim, req.RequestedSlug)
		if err != nil {
			return RunResult{}, err
		}
		boxID = box.ID
		fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=%s box=%s name=%s\n", leaseID, slug, providerName, box.ID, box.Name)
		acquired = true
	} else {
		leaseID, boxID, slug, err = b.resolveBoxID(ctx, client, req.ID, req.Repo.Root, req.Reclaim)
		if err != nil {
			return RunResult{}, err
		}
	}
	shouldStop := acquired && !req.Keep
	cleanedUp := false
	session := &RunSessionHandle{
		Provider:       providerName,
		LeaseID:        leaseID,
		Slug:           slug,
		Reused:         !acquired,
		Kept:           !shouldStop,
		CleanupCommand: upstashBoxCleanupCommand(leaseID),
	}
	finishResult := func(result RunResult) RunResult {
		if result.Provider == "" {
			result.Provider = providerName
		}
		if result.LeaseID == "" {
			result.LeaseID = leaseID
		}
		if result.Slug == "" {
			result.Slug = slug
		}
		result.Session = session
		result.Session.Kept = !cleanedUp && !shouldStop
		return result
	}
	defer func() {
		result = finishResult(result)
	}()
	cleanupBox := func() error {
		if !shouldStop {
			return nil
		}
		cleanupCtx, cancel := upstashBoxCleanupContext()
		defer cancel()
		if err := client.DeleteBoxes(cleanupCtx, []string{boxID}); err != nil {
			shouldStop = false
			return err
		}
		removeLeaseClaim(leaseID)
		cleanedUp = true
		shouldStop = false
		return nil
	}
	if shouldStop {
		defer func() {
			if err := cleanupBox(); err != nil {
				fmt.Fprintf(b.rt.Stderr, "warning: upstash-box delete failed for %s: %v\n", boxID, err)
			}
		}()
	}

	syncDuration := time.Duration(0)
	syncPhases := []timingPhase{{Name: "sync", Skipped: true, Reason: "--no-sync"}}
	if !req.NoSync {
		syncPhases, syncDuration, err = b.syncWorkspace(ctx, client, boxID, req, workdir, folder)
		if err != nil {
			return RunResult{Total: b.now().Sub(started), SyncDelegated: true}, err
		}
		fmt.Fprintf(b.rt.Stderr, "sync complete in %s\n", syncDuration.Round(time.Millisecond))
	} else if err := b.prepareWorkspace(ctx, client, boxID, folder, false); err != nil {
		return RunResult{}, err
	}
	if req.SyncOnly {
		result := RunResult{Total: b.now().Sub(started), SyncDelegated: true}
		fmt.Fprintf(b.rt.Stdout, "synced %s\n", workdir)
		if req.TimingJSON {
			err := writeTimingJSON(b.rt.Stderr, timingReportWithRunResult(timingReport{
				Provider:      providerName,
				LeaseID:       leaseID,
				Slug:          slug,
				SyncDelegated: true,
				SyncMs:        syncDuration.Milliseconds(),
				SyncPhases:    syncPhases,
				SyncSkipped:   req.NoSync,
				TotalMs:       result.Total.Milliseconds(),
				ExitCode:      0,
				Label:         strings.TrimSpace(req.Label),
			}, result, nil))
			return result, err
		}
		return result, nil
	}

	command, err := buildCommand(req.Command, req.ShellMode)
	if err != nil {
		return RunResult{}, err
	}
	if req.EnvSummary {
		printEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, req.Env)
	}
	envPath := ""
	if len(req.Env) > 0 {
		envPath = workspacePath(".crabbox-env-" + leaseID + ".sh")
		if err := client.WriteFile(ctx, boxID, envPath, shellEnvProfile(req.Env)); err != nil {
			return RunResult{}, err
		}
		command = ". " + shellQuote(envPath) + " && " + command
	}
	commandStarted := b.now()
	exitCode, commandErr := client.ExecStream(ctx, boxID, command, folder, b.rt.Stdout)
	commandDuration := b.now().Sub(commandStarted)
	envCleanupErr := error(nil)
	if envPath != "" {
		envCleanupErr = b.cleanupEnvFile(client, boxID, envPath)
	}
	finalExitCode := exitCode
	if commandErr != nil {
		finalExitCode = 1
	} else if exitCode == 0 && envCleanupErr != nil {
		finalExitCode = 5
	}
	result = RunResult{
		ExitCode:      finalExitCode,
		Command:       commandDuration,
		Total:         b.now().Sub(started),
		SyncDelegated: true,
		Provider:      providerName,
		LeaseID:       leaseID,
		Slug:          slug,
		CommandText:   strings.Join(req.Command, " "),
	}
	if req.NoSync {
		fmt.Fprintf(b.rt.Stderr, "upstash-box run summary sync_skipped=true command=%s total=%s exit=%d\n", result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), finalExitCode)
	} else {
		fmt.Fprintf(b.rt.Stderr, "upstash-box run summary sync=%s command=%s total=%s exit=%d\n", syncDuration.Round(time.Millisecond), result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), finalExitCode)
	}
	if req.TimingJSON {
		timingErr := commandErr
		if timingErr == nil {
			timingErr = envCleanupErr
		}
		report := timingReportWithRunResult(timingReport{
			Provider:      providerName,
			LeaseID:       leaseID,
			Slug:          slug,
			SyncDelegated: true,
			SyncMs:        syncDuration.Milliseconds(),
			SyncPhases:    syncPhases,
			SyncSkipped:   req.NoSync,
			CommandMs:     commandDuration.Milliseconds(),
			TotalMs:       result.Total.Milliseconds(),
			ExitCode:      finalExitCode,
			Label:         strings.TrimSpace(req.Label),
		}, result, timingErr)
		if commandErr == nil && envCleanupErr != nil {
			report = timingReportWithProviderError(report)
		}
		if err := writeTimingJSON(b.rt.Stderr, report); err != nil {
			return result, err
		}
	}
	if commandErr != nil {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		if envCleanupErr != nil {
			return result, ExitError{Code: 1, Message: fmt.Sprintf("upstash-box run failed: %v; %v", commandErr, envCleanupErr)}
		}
		return result, ExitError{Code: 1, Message: fmt.Sprintf("upstash-box run failed: %v", commandErr)}
	}
	if exitCode != 0 {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		if envCleanupErr != nil {
			return result, ExitError{Code: exitCode, Message: fmt.Sprintf("upstash-box run exited %d; %v", exitCode, envCleanupErr)}
		}
		return result, ExitError{Code: exitCode, Message: fmt.Sprintf("upstash-box run exited %d", exitCode)}
	}
	if envCleanupErr != nil {
		return result, ExitError{Code: 5, Message: envCleanupErr.Error()}
	}
	return result, nil
}

func (b *backend) cleanupEnvFile(client api, boxID, envPath string) error {
	if err := cleanupRemoteFile(client, boxID, envPath); err != nil {
		return fmt.Errorf("upstash-box env cleanup failed for %s: %w", boxID, err)
	}
	return nil
}

func cleanupRemoteFile(client api, boxID, remotePath string) error {
	cleanupCtx, cancel := upstashBoxCleanupContext()
	defer cancel()
	result, err := client.Exec(cleanupCtx, boxID, "rm -f "+shellQuote(remotePath), "")
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return commandExitError("upstash-box exec rm -f "+remotePath, result)
	}
	return nil
}

func (b *backend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	_ = req
	client, err := newAPI(b.cfg, b.rt)
	if err != nil {
		return nil, err
	}
	boxes, err := client.ListBoxes(ctx)
	if err != nil {
		return nil, err
	}
	servers := make([]Server, 0, len(boxes))
	for _, box := range boxes {
		if isCrabboxBox(box) {
			servers = append(servers, boxToServer(b.cfg, box))
		}
	}
	return servers, nil
}

func (b *backend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	servers, err := b.List(ctx, ListRequest{})
	if err != nil {
		return DoctorResult{}, err
	}
	return inventoryDoctorResult(providerName, len(servers)), nil
}

func (b *backend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	client, err := newAPI(b.cfg, b.rt)
	if err != nil {
		return StatusView{}, err
	}
	leaseID, boxID, slug, err := b.resolveBoxID(ctx, client, req.ID, "", false)
	if err != nil {
		return StatusView{}, err
	}
	deadline := b.now().Add(req.WaitTimeout)
	if req.WaitTimeout <= 0 {
		deadline = b.now().Add(5 * time.Minute)
	}
	for {
		box, err := client.GetBox(ctx, boxID)
		if err != nil {
			return StatusView{}, err
		}
		server := boxToServer(b.cfg, box)
		view := StatusView{
			ID:         leaseID,
			Slug:       blank(slug, server.Labels["slug"]),
			Provider:   providerName,
			TargetOS:   targetLinux,
			State:      box.Status,
			ServerID:   box.ID,
			ServerType: server.ServerType.Name,
			Network:    networkPublic,
			Ready:      statusReady(box.Status),
			Labels:     server.Labels,
		}
		if !req.Wait || view.Ready {
			return view, nil
		}
		if b.now().After(deadline) {
			return StatusView{}, exit(5, "timed out waiting for upstash-box %s to become ready", boxID)
		}
		select {
		case <-ctx.Done():
			return StatusView{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (b *backend) Stop(ctx context.Context, req StopRequest) error {
	client, err := newAPI(b.cfg, b.rt)
	if err != nil {
		return err
	}
	leaseID, boxID, _, err := b.resolveBoxID(ctx, client, req.ID, "", false)
	if err != nil {
		return err
	}
	if err := client.DeleteBoxes(ctx, []string{boxID}); err != nil {
		return err
	}
	removeLeaseClaim(leaseID)
	fmt.Fprintf(b.rt.Stderr, "released lease=%s box=%s\n", leaseID, boxID)
	return nil
}

func (b *backend) createBox(ctx context.Context, client api, repo Repo, keep, reclaim bool, requestedSlug string) (string, boxData, string, error) {
	leaseID := newLeaseID()
	slug, err := allocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return "", boxData{}, "", err
	}
	name := upstashBoxName(leaseID, slug)
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s name=%s runtime=%s size=%s keep_alive=%t\n", providerName, leaseID, slug, name, runtimeName(b.cfg), sizeName(b.cfg), b.cfg.UpstashBox.KeepAlive)
	box, err := client.CreateBox(ctx, createRequest{
		Name:      name,
		Runtime:   runtimeName(b.cfg),
		Size:      sizeName(b.cfg),
		KeepAlive: b.cfg.UpstashBox.KeepAlive,
	})
	if err != nil {
		return "", boxData{}, "", err
	}
	if err := claimLeaseForRepoProvider(leaseID, slug, providerName, repo.Root, b.cfg.IdleTimeout, reclaim); err != nil {
		cleanupCtx, cancel := upstashBoxCleanupContext()
		cleanupErr := client.DeleteBoxes(cleanupCtx, []string{box.ID})
		cancel()
		if cleanupErr != nil {
			return "", boxData{}, "", fmt.Errorf("%w; cleanup failed for upstash-box %s: %v", err, box.ID, cleanupErr)
		}
		return "", boxData{}, "", err
	}
	return leaseID, box, slug, nil
}

func (b *backend) resolveBoxID(ctx context.Context, client api, id, repoRoot string, reclaim bool) (string, string, string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", "", "", exit(2, "provider=%s requires a Crabbox lease id, slug, or Upstash Box id", providerName)
	}
	if claim, ok, err := resolveLeaseClaim(id); err != nil {
		return "", "", "", err
	} else if ok && claim.Provider == providerName {
		if repoRoot != "" {
			if err := claimLeaseForRepoProvider(claim.LeaseID, claim.Slug, providerName, repoRoot, time.Duration(claim.IdleTimeoutSeconds)*time.Second, reclaim); err != nil {
				return "", "", "", err
			}
		}
		box, err := resolveBoxByLease(ctx, client, claim.LeaseID)
		if err != nil {
			return "", "", "", err
		}
		return claim.LeaseID, box.ID, claim.Slug, nil
	}
	if strings.HasPrefix(id, "cbx_") {
		box, err := resolveBoxByLease(ctx, client, id)
		if err != nil {
			return "", "", "", err
		}
		return id, box.ID, boxSlug(id, box), nil
	}
	if box, err := client.GetBox(ctx, id); err == nil && isCrabboxBox(box) {
		leaseID := boxLeaseID(box)
		return leaseID, box.ID, boxSlug(leaseID, box), nil
	} else if err != nil && !isNotFound(err) {
		return "", "", "", err
	}
	box, err := resolveBoxBySlug(ctx, client, id)
	if err != nil {
		return "", "", "", err
	}
	leaseID := boxLeaseID(box)
	return leaseID, box.ID, boxSlug(leaseID, box), nil
}

func resolveBoxByLease(ctx context.Context, client api, leaseID string) (boxData, error) {
	boxes, err := client.ListBoxes(ctx)
	if err != nil {
		return boxData{}, err
	}
	for _, box := range boxes {
		if isCrabboxBox(box) && boxLeaseID(box) == leaseID {
			return box, nil
		}
	}
	return boxData{}, exit(4, "upstash-box lease %q was not found", leaseID)
}

func resolveBoxBySlug(ctx context.Context, client api, slug string) (boxData, error) {
	boxes, err := client.ListBoxes(ctx)
	if err != nil {
		return boxData{}, err
	}
	for _, box := range boxes {
		if isCrabboxBox(box) && boxSlug(boxLeaseID(box), box) == slug {
			return box, nil
		}
	}
	return boxData{}, exit(4, "upstash-box %q was not found", slug)
}

func (b *backend) now() time.Time {
	return now(b.rt)
}

func boxToServer(cfg Config, box boxData) Server {
	leaseID := boxLeaseID(box)
	labels := directLeaseLabels(cfg, leaseID, boxSlug(leaseID, box), providerName, "", box.KeepAlive, time.Now().UTC())
	labels["box_id"] = box.ID
	labels["box_name"] = box.Name
	labels["runtime"] = blank(box.Runtime, runtimeName(cfg))
	labels["size"] = blank(box.Size, sizeName(cfg))
	labels["state"] = box.Status
	server := Server{
		Provider: providerName,
		CloudID:  box.ID,
		Name:     blank(box.Name, box.ID),
		Status:   box.Status,
		Labels:   labels,
	}
	server.ServerType.Name = blank(box.Size, sizeName(cfg))
	server.PublicNet.IPv4.IP = boxBaseHost(cfg)
	return server
}

func boxBaseHost(cfg Config) string {
	raw := blank(strings.TrimSpace(cfg.UpstashBox.BaseURL), "https://us-east-1.box.upstash.com")
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return raw
	}
	return parsed.Host
}

var boxNamePattern = regexp.MustCompile(`^crabbox-(.+)-([0-9a-f]{12})$`)

func isCrabboxBox(box boxData) bool {
	return boxNamePattern.MatchString(strings.TrimSpace(box.Name))
}

func boxLeaseID(box boxData) string {
	if match := boxNamePattern.FindStringSubmatch(strings.TrimSpace(box.Name)); len(match) == 3 {
		return "cbx_" + match[2]
	}
	return "upstash_" + box.ID
}

func boxSlug(leaseID string, box boxData) string {
	if match := boxNamePattern.FindStringSubmatch(strings.TrimSpace(box.Name)); len(match) == 3 {
		return match[1]
	}
	return newLeaseSlug(leaseID)
}

func statusReady(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "running", "ready", "idle", "paused":
		return true
	default:
		return false
	}
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "404") || strings.Contains(msg, "not found")
}

func runtimeName(cfg Config) string {
	return blank(strings.TrimSpace(cfg.UpstashBox.Runtime), "node")
}

func upstashBoxName(leaseID, slug string) string {
	slug = strings.Trim(strings.ToLower(strings.TrimSpace(slug)), "-")
	if slug == "" {
		slug = newLeaseSlug(leaseID)
	}
	return "crabbox-" + slug + "-" + strings.TrimPrefix(leaseID, "cbx_")
}

func sizeName(cfg Config) string {
	return blank(strings.TrimSpace(cfg.UpstashBox.Size), "small")
}

func workdir(cfg Config) string {
	return blank(strings.TrimSpace(cfg.UpstashBox.Workdir), "/workspace/home/crabbox")
}

func cleanWorkdir(workdir string) (string, error) {
	trimmed := strings.TrimSpace(workdir)
	if trimmed == "" {
		return "", exit(2, "upstash-box workdir is empty")
	}
	clean := path.Clean(trimmed)
	if !strings.HasPrefix(clean, "/") {
		return "", exit(2, "upstash-box workdir %q must resolve to an absolute path", workdir)
	}
	switch clean {
	case "/", "/bin", "/dev", "/etc", "/home", "/lib", "/lib64", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var", "/workspace", "/workspace/home":
		return "", exit(2, "upstash-box workdir %q is too broad; choose a dedicated subdirectory", clean)
	}
	return clean, nil
}

func buildCommand(command []string, shellMode bool) (string, error) {
	if len(command) == 0 {
		return "", errors.New("missing command")
	}
	var script string
	if shellMode {
		script = strings.Join(command, " ")
	} else if shouldUseShell(command) || leadingEnvAssignment(command) {
		script = shellScriptFromArgv(command)
	} else {
		script = "exec " + strings.Join(shellWords(command), " ")
	}
	return script, nil
}

const workspaceRoot = "/workspace/home"

func workspaceFolder(workdir string) (string, error) {
	clean, err := cleanWorkdir(workdir)
	if err != nil {
		return "", err
	}
	prefix := workspaceRoot + "/"
	if !strings.HasPrefix(clean, prefix) {
		return "", exit(2, "upstash-box workdir %q must be under %s", clean, workspaceRoot)
	}
	return strings.TrimPrefix(clean, prefix), nil
}

func workspacePath(name string) string {
	return path.Join(workspaceRoot, name)
}

func shellEnvProfile(env map[string]string) string {
	var b strings.Builder
	keys := make([]string, 0, len(env))
	for key := range env {
		if !validEnvName(key) {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	b.WriteString("set -a\n")
	for _, key := range keys {
		b.WriteString(key)
		b.WriteString("=")
		b.WriteString(shellQuote(env[key]))
		b.WriteByte('\n')
	}
	b.WriteString("set +a\n")
	return b.String()
}

func validEnvName(name string) bool {
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
