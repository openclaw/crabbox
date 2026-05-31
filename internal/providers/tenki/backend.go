package tenki

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type tenkiFlagValues struct {
	CLIPath   *string
	Endpoint  *string
	Gateway   *string
	Workspace *string
	Project   *string
	Image     *string
	Snapshot  *string
	WorkRoot  *string
	CPUs      *int
	MemoryMB  *int
	DiskGB    *int
}

func RegisterTenkiProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return tenkiFlagValues{
		CLIPath:   fs.String("tenki-cli", defaults.Tenki.CLIPath, "Tenki CLI path"),
		Endpoint:  fs.String("tenki-endpoint", defaults.Tenki.Endpoint, "Tenki sandbox API endpoint"),
		Gateway:   fs.String("tenki-gateway", defaults.Tenki.Gateway, "Tenki sandbox SSH gateway WebSocket URL"),
		Workspace: fs.String("tenki-workspace", defaults.Tenki.Workspace, "Tenki workspace ID"),
		Project:   fs.String("tenki-project", defaults.Tenki.Project, "Tenki project ID"),
		Image:     fs.String("tenki-image", defaults.Tenki.Image, "Tenki sandbox registry image ref"),
		Snapshot:  fs.String("tenki-snapshot", defaults.Tenki.Snapshot, "Tenki sandbox snapshot ID"),
		WorkRoot:  fs.String("tenki-work-root", defaults.Tenki.WorkRoot, "Tenki remote work root"),
		CPUs:      fs.Int("tenki-cpus", defaults.Tenki.CPUs, "Tenki sandbox CPU cores"),
		MemoryMB:  fs.Int("tenki-memory-mb", defaults.Tenki.MemoryMB, "Tenki sandbox memory in MB"),
		DiskGB:    fs.Int("tenki-disk-gb", defaults.Tenki.DiskGB, "Tenki sandbox root disk size in GB"),
	}
}

func ApplyTenkiProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if cfg.Provider == tenkiProvider {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=tenki; use --tenki-cpus/--tenki-memory-mb/--tenki-disk-gb")
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=tenki; use --tenki-image or --tenki-snapshot")
		}
		if cfg.TargetOS != "" && cfg.TargetOS != targetLinux {
			return exit(2, "provider=tenki supports target=linux only")
		}
	}
	v, ok := values.(tenkiFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "tenki-cli") {
		cfg.Tenki.CLIPath = *v.CLIPath
	}
	if flagWasSet(fs, "tenki-endpoint") {
		cfg.Tenki.Endpoint = *v.Endpoint
	}
	if flagWasSet(fs, "tenki-gateway") {
		cfg.Tenki.Gateway = *v.Gateway
	}
	if flagWasSet(fs, "tenki-workspace") {
		cfg.Tenki.Workspace = *v.Workspace
	}
	if flagWasSet(fs, "tenki-project") {
		cfg.Tenki.Project = *v.Project
	}
	if flagWasSet(fs, "tenki-image") {
		cfg.Tenki.Image = *v.Image
	}
	if flagWasSet(fs, "tenki-snapshot") {
		cfg.Tenki.Snapshot = *v.Snapshot
	}
	if flagWasSet(fs, "tenki-work-root") {
		cfg.Tenki.WorkRoot = *v.WorkRoot
	}
	if flagWasSet(fs, "tenki-cpus") {
		cfg.Tenki.CPUs = *v.CPUs
	}
	if flagWasSet(fs, "tenki-memory-mb") {
		cfg.Tenki.MemoryMB = *v.MemoryMB
	}
	if flagWasSet(fs, "tenki-disk-gb") {
		cfg.Tenki.DiskGB = *v.DiskGB
	}
	if cfg.Provider == tenkiProvider {
		return validateTenkiOptions(*cfg)
	}
	return nil
}

func NewTenkiBackend(spec ProviderSpec, cfg Config, rt Runtime) (Backend, error) {
	if err := validateTenkiOptions(cfg); err != nil {
		return nil, err
	}
	cfg.Provider = tenkiProvider
	cfg.TargetOS = targetLinux
	cfg.SSHUser = "tenki"
	cfg.SSHPort = "22"
	cfg.SSHFallbackPorts = nil
	cfg.Network = networkPublic
	cfg.WorkRoot = tenkiWorkRoot(cfg)
	return &tenkiBackend{spec: spec, cfg: cfg, rt: rt}, nil
}

func validateTenkiOptions(cfg Config) error {
	if cfg.Tailscale.Enabled {
		return exit(2, "--tailscale is not supported for provider=tenki; Tenki owns sandbox networking")
	}
	if strings.TrimSpace(cfg.Tenki.Image) != "" && strings.TrimSpace(cfg.Tenki.Snapshot) != "" {
		return exit(2, "provider=tenki accepts only one of tenki.image or tenki.snapshot")
	}
	if err := cleanTenkiWorkRoot(tenkiWorkRoot(cfg)); err != nil {
		return err
	}
	return nil
}

type tenkiBackend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

func (b *tenkiBackend) Spec() ProviderSpec { return b.spec }

func (b *tenkiBackend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	cfg := b.configForRun()
	leaseID := newLeaseID()
	slug, err := allocateClaimLeaseSlug(leaseID, req.RequestedSlug)
	if err != nil {
		return LeaseTarget{}, err
	}
	name := leaseProviderName(leaseID, slug)

	fmt.Fprintf(b.rt.Stderr, "provisioning provider=tenki lease=%s slug=%s session=%s keep=%v\n", leaseID, slug, name, req.Keep)
	session, err := b.createSession(ctx, cfg, name, leaseID, slug, req.Keep)
	if err != nil {
		return LeaseTarget{}, err
	}
	lease, err := b.prepareLease(ctx, cfg, session, leaseID, slug, req.Keep, true)
	if err != nil {
		if !req.Keep {
			_ = b.terminateSession(context.Background(), session.ID)
		}
		return LeaseTarget{}, err
	}
	if err := claimLeaseForRepoProvider(leaseID, slug, tenkiProvider, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
		if !req.Keep {
			_ = b.terminateSession(context.Background(), session.ID)
		}
		return LeaseTarget{}, err
	}
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s tenki_session=%s state=ready\n", leaseID, session.ID)
	return lease, nil
}

func (b *tenkiBackend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	cfg := b.configForRun()
	session, leaseID, slug, err := b.resolveSession(ctx, req.ID, req.Reclaim)
	if err != nil {
		return LeaseTarget{}, err
	}
	if req.ReleaseOnly {
		return LeaseTarget{Server: b.sessionToServer(cfg, session, leaseID, slug, true), LeaseID: leaseID}, nil
	}
	lease, err := b.prepareLease(ctx, cfg, session, leaseID, slug, true, true)
	if err != nil {
		return LeaseTarget{}, err
	}
	if req.Repo.Root != "" {
		if err := claimLeaseForRepoProvider(leaseID, slug, tenkiProvider, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
			return LeaseTarget{}, err
		}
	}
	return lease, nil
}

func (b *tenkiBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	cfg := b.configForRun()
	sessions, err := b.listSessions(ctx, req.All)
	if err != nil {
		return nil, err
	}
	out := make([]Server, 0, len(sessions))
	for _, session := range sessions {
		if !isCrabboxTenkiSession(session) {
			continue
		}
		leaseID, slug := tenkiLeaseMetadata(session)
		out = append(out, b.sessionToServer(cfg, session, leaseID, slug, session.Sticky))
	}
	return out, nil
}

func (b *tenkiBackend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	if _, err := b.runTenki(ctx, []string{"--version"}, nil, nil); err != nil {
		return DoctorResult{}, exit(2, "provider=tenki requires the tenki CLI on PATH and authenticated: %v", err)
	}
	servers, err := b.List(ctx, ListRequest{})
	if err != nil {
		return DoctorResult{}, err
	}
	return inventoryDoctorResult(tenkiProvider, len(servers)), nil
}

func (b *tenkiBackend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	sessionID := strings.TrimSpace(req.Lease.Server.CloudID)
	if sessionID == "" && req.Lease.Server.Labels != nil {
		sessionID = strings.TrimSpace(req.Lease.Server.Labels["tenki_session_id"])
	}
	if sessionID == "" {
		session, _, _, err := b.resolveSession(ctx, req.Lease.LeaseID, true)
		if err != nil {
			return err
		}
		sessionID = session.ID
	}
	if err := b.terminateSession(ctx, sessionID); err != nil {
		return err
	}
	removeLeaseClaim(req.Lease.LeaseID)
	fmt.Fprintf(b.rt.Stderr, "released lease=%s tenki_session=%s\n", req.Lease.LeaseID, sessionID)
	return nil
}

func (b *tenkiBackend) Touch(_ context.Context, req TouchRequest) (Server, error) {
	server := req.Lease.Server
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	server.Labels = touchDirectLeaseLabels(server.Labels, b.cfg, req.State, time.Now().UTC())
	return server, nil
}

func (b *tenkiBackend) configForRun() Config {
	cfg := b.cfg
	cfg.Provider = tenkiProvider
	cfg.TargetOS = targetLinux
	cfg.SSHUser = "tenki"
	cfg.SSHPort = "22"
	cfg.SSHFallbackPorts = nil
	cfg.Network = networkPublic
	cfg.WorkRoot = tenkiWorkRoot(cfg)
	return cfg
}

func (b *tenkiBackend) createSession(ctx context.Context, cfg Config, name, leaseID, slug string, keep bool) (tenkiSession, error) {
	args := b.sandboxArgs("create")
	args = b.appendScopeArgs(args)
	args = append(args,
		"--no-wait",
		"--name", name,
		"--metadata", tenkiMetadataProvider+"="+tenkiProvider,
		"--metadata", tenkiMetadataLease+"="+leaseID,
		"--metadata", tenkiMetadataSlug+"="+slug,
		"--tags", "crabbox,crabbox-provider-tenki",
	)
	if keep {
		args = append(args, "--sticky")
	}
	if cfg.TTL > 0 {
		args = append(args, "--max-duration", cfg.TTL.String())
	}
	if cfg.IdleTimeout > 0 {
		args = append(args, "--idle-timeout", cfg.IdleTimeout.String())
	}
	if cfg.Tenki.CPUs > 0 {
		args = append(args, "--cpu", strconv.Itoa(cfg.Tenki.CPUs))
	}
	if cfg.Tenki.MemoryMB > 0 {
		args = append(args, "--memory-mb", strconv.Itoa(cfg.Tenki.MemoryMB))
	}
	if cfg.Tenki.DiskGB > 0 {
		args = append(args, "--disk-size-gb", strconv.Itoa(cfg.Tenki.DiskGB))
	}
	if cfg.Tenki.Image != "" {
		args = append(args, "--image", cfg.Tenki.Image)
	}
	if cfg.Tenki.Snapshot != "" {
		args = append(args, "--snapshot", cfg.Tenki.Snapshot)
	}
	result, err := b.runTenki(ctx, args, nil, b.rt.Stderr)
	if err != nil {
		return tenkiSession{}, ExitError{Code: result.ExitCode, Message: fmt.Sprintf("tenki sandbox create failed: %v", err)}
	}
	sessionID := parseTenkiCreateSessionID(result.Stdout)
	if sessionID == "" {
		return tenkiSession{}, exit(5, "tenki sandbox create did not return a session id in expected `id: <session-id>` output")
	}
	return b.getSession(ctx, sessionID)
}

func (b *tenkiBackend) prepareLease(ctx context.Context, cfg Config, session tenkiSession, leaseID, slug string, keep bool, waitSSH bool) (LeaseTarget, error) {
	session, err := b.ensureSessionReadyForSSH(ctx, cfg, session)
	if err != nil {
		return LeaseTarget{}, err
	}
	keyPath, certPath, err := b.waitForTenkiSSHMaterial(ctx, session.ID, bootstrapWaitTimeout(cfg))
	if err != nil {
		return LeaseTarget{}, err
	}
	target := b.sshTarget(session.ID, keyPath, certPath)
	target.ReadyCheck = "command -v git >/dev/null && command -v rsync >/dev/null && command -v tar >/dev/null && command -v python3 >/dev/null"
	server := b.sessionToServer(cfg, session, leaseID, slug, keep)
	if waitSSH {
		if err := waitForSSHReadyFunc(ctx, &target, b.rt.Stderr, "tenki sandbox ssh", bootstrapWaitTimeout(cfg)); err != nil {
			return LeaseTarget{}, err
		}
	}
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *tenkiBackend) getSession(ctx context.Context, sessionID string) (tenkiSession, error) {
	args := append(b.sandboxArgs("get"), "--json", sessionID)
	result, err := b.runTenki(ctx, args, nil, nil)
	if err != nil {
		return tenkiSession{}, ExitError{Code: result.ExitCode, Message: fmt.Sprintf("tenki sandbox get failed: %v", err)}
	}
	var session tenkiSession
	if err := json.Unmarshal([]byte(result.Stdout), &session); err != nil {
		return tenkiSession{}, fmt.Errorf("parse tenki sandbox get JSON: %w", err)
	}
	return session, nil
}

func (b *tenkiBackend) ensureSessionReadyForSSH(ctx context.Context, cfg Config, session tenkiSession) (tenkiSession, error) {
	state := tenkiNormalizedState(session.State)
	switch state {
	case "", "ready", "running", "creating":
		return session, nil
	case "paused":
		if err := b.resumeSession(ctx, session.ID); err != nil {
			return tenkiSession{}, err
		}
		return b.waitForSessionReady(ctx, session.ID, bootstrapWaitTimeout(cfg))
	case "pausing":
		session, err := b.waitForSessionPausedOrReady(ctx, session.ID, bootstrapWaitTimeout(cfg))
		if err != nil {
			return tenkiSession{}, err
		}
		if tenkiSessionReady(session) {
			return session, nil
		}
		if err := b.resumeSession(ctx, session.ID); err != nil {
			return tenkiSession{}, err
		}
		return b.waitForSessionReady(ctx, session.ID, bootstrapWaitTimeout(cfg))
	case "resuming":
		return b.waitForSessionReady(ctx, session.ID, bootstrapWaitTimeout(cfg))
	case "terminating", "terminated":
		return tenkiSession{}, exit(4, "tenki session %s is %s", session.ID, state)
	default:
		return session, nil
	}
}

func (b *tenkiBackend) resumeSession(ctx context.Context, sessionID string) error {
	args := b.sandboxArgs("resume")
	args = append(args, "--session", sessionID)
	result, err := b.runTenki(ctx, args, nil, b.rt.Stderr)
	if err != nil {
		return ExitError{Code: result.ExitCode, Message: fmt.Sprintf("tenki sandbox resume failed: %v%s", err, tenkiCommandOutputDetail(result))}
	}
	return nil
}

func (b *tenkiBackend) waitForSessionPausedOrReady(ctx context.Context, sessionID string, timeout time.Duration) (tenkiSession, error) {
	return b.waitForSessionState(ctx, sessionID, timeout, func(session tenkiSession) (bool, error) {
		switch tenkiNormalizedState(session.State) {
		case "ready", "running", "paused":
			return true, nil
		case "terminating", "terminated":
			return false, exit(4, "tenki session %s is %s while waiting to resume", sessionID, tenkiNormalizedState(session.State))
		default:
			return false, nil
		}
	})
}

func (b *tenkiBackend) waitForSessionReady(ctx context.Context, sessionID string, timeout time.Duration) (tenkiSession, error) {
	return b.waitForSessionState(ctx, sessionID, timeout, func(session tenkiSession) (bool, error) {
		switch tenkiNormalizedState(session.State) {
		case "ready", "running":
			return true, nil
		case "paused":
			if msg := strings.TrimSpace(session.LastResumeError); msg != "" {
				return false, exit(5, "tenki session %s failed to resume: %s", sessionID, msg)
			}
		case "terminating", "terminated":
			return false, exit(4, "tenki session %s is %s while waiting for resume", sessionID, tenkiNormalizedState(session.State))
		}
		return false, nil
	})
}

func (b *tenkiBackend) waitForSessionState(ctx context.Context, sessionID string, timeout time.Duration, done func(tenkiSession) (bool, error)) (tenkiSession, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	deadline := time.Now().Add(timeout)
	var last tenkiSession
	var lastErr error
	for {
		session, err := b.getSession(ctx, sessionID)
		if err == nil {
			last = session
			ok, stateErr := done(session)
			if stateErr != nil {
				return tenkiSession{}, stateErr
			}
			if ok {
				return session, nil
			}
			lastErr = nil
		} else {
			lastErr = err
		}

		if ctx.Err() != nil {
			if lastErr != nil {
				return tenkiSession{}, exit(5, "timed out waiting for Tenki session %s to become ready: %v", sessionID, lastErr)
			}
			return tenkiSession{}, exit(5, "timed out waiting for Tenki session %s to become ready; last state=%s", sessionID, last.State)
		}
		fmt.Fprintf(b.rt.Stderr, "waiting for tenki session=%s state=%s remaining=%s\n", sessionID, blank(last.State, "unknown"), time.Until(deadline).Round(time.Second))
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return tenkiSession{}, exit(5, "timed out waiting for Tenki session %s to become ready: %v", sessionID, lastErr)
			}
			return tenkiSession{}, exit(5, "timed out waiting for Tenki session %s to become ready; last state=%s", sessionID, last.State)
		case <-time.After(5 * time.Second):
		}
	}
}

func (b *tenkiBackend) listSessions(ctx context.Context, all bool) ([]tenkiSession, error) {
	args := b.sandboxArgs("list")
	args = b.appendScopeArgs(args)
	args = append(args, "--json", "--tags", "crabbox,crabbox-provider-tenki")
	if all {
		args = append(args, "--all")
	}
	result, err := b.runTenki(ctx, args, nil, nil)
	if err != nil {
		return nil, ExitError{Code: result.ExitCode, Message: fmt.Sprintf("tenki sandbox list failed: %v", err)}
	}
	var sessions []tenkiSession
	if err := json.Unmarshal([]byte(result.Stdout), &sessions); err != nil {
		return nil, fmt.Errorf("parse tenki sandbox list JSON: %w", err)
	}
	return sessions, nil
}

func (b *tenkiBackend) resolveSession(ctx context.Context, identifier string, reclaim bool) (tenkiSession, string, string, error) {
	if strings.TrimSpace(identifier) == "" {
		return tenkiSession{}, "", "", exit(2, "provider=tenki requires a Crabbox lease id, slug, or Tenki sandbox session id")
	}
	if claim, ok, err := resolveLeaseClaim(identifier); err != nil {
		return tenkiSession{}, "", "", err
	} else if ok {
		if claim.Provider != "" && claim.Provider != tenkiProvider {
			return tenkiSession{}, "", "", exit(4, "lease %q is claimed for provider=%s, not tenki", identifier, claim.Provider)
		}
		session, err := b.findSessionByLease(ctx, claim.LeaseID)
		if err != nil {
			return tenkiSession{}, "", "", err
		}
		return session, claim.LeaseID, claim.Slug, nil
	}
	if strings.HasPrefix(identifier, "cbx_") {
		session, err := b.findSessionByLease(ctx, identifier)
		if err != nil {
			return tenkiSession{}, "", "", err
		}
		_, slug := tenkiLeaseMetadata(session)
		return session, identifier, slug, nil
	}
	if session, err := b.getSession(ctx, identifier); err == nil {
		if !isCrabboxTenkiSession(session) && !reclaim {
			return tenkiSession{}, "", "", exit(4, "tenki session %q is not Crabbox-managed; use --reclaim to adopt it", identifier)
		}
		leaseID, slug := tenkiLeaseMetadata(session)
		if leaseID == "" {
			leaseID = "tenki_" + normalizeLeaseSlug(session.ID)
		}
		if slug == "" {
			slug = normalizeLeaseSlug(blank(session.Name, session.ID))
		}
		return session, leaseID, slug, nil
	}
	sessions, err := b.listSessions(ctx, false)
	if err != nil {
		return tenkiSession{}, "", "", err
	}
	for _, session := range sessions {
		leaseID, slug := tenkiLeaseMetadata(session)
		if identifier == slug || identifier == session.Name {
			return session, leaseID, slug, nil
		}
	}
	return tenkiSession{}, "", "", exit(4, "tenki lease or session %q was not found", identifier)
}

func (b *tenkiBackend) findSessionByLease(ctx context.Context, leaseID string) (tenkiSession, error) {
	sessions, err := b.listSessions(ctx, false)
	if err != nil {
		return tenkiSession{}, err
	}
	for _, session := range sessions {
		if got, _ := tenkiLeaseMetadata(session); got == leaseID {
			return session, nil
		}
	}
	return tenkiSession{}, exit(4, "tenki lease %q was not found", leaseID)
}

func (b *tenkiBackend) terminateSession(ctx context.Context, sessionID string) error {
	args := b.sandboxArgs("terminate")
	args = b.appendScopeArgs(args)
	args = append(args, sessionID)
	result, err := b.runTenki(ctx, args, nil, b.rt.Stderr)
	if err != nil {
		combined := strings.ToLower(result.Stdout + "\n" + result.Stderr)
		if strings.Contains(combined, "not found") || strings.Contains(combined, "no sandbox") {
			return nil
		}
		return ExitError{Code: result.ExitCode, Message: fmt.Sprintf("tenki sandbox terminate failed: %v", err)}
	}
	return nil
}

func (b *tenkiBackend) waitForTenkiSSHMaterial(ctx context.Context, sessionID string, timeout time.Duration) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		args := b.sandboxArgs("ssh")
		args = append(args,
			"--session", sessionID,
			"--user", "tenki",
			"--batch-mode",
			"--connect-timeout", "10s",
		)
		if b.cfg.Tenki.Gateway != "" {
			args = append(args, "--gateway", b.cfg.Tenki.Gateway)
		}
		args = append(args, "--", "true")

		result, err := b.runTenki(ctx, args, nil, nil)
		if err == nil {
			keyPath, certPath, materialErr := tenkiSSHMaterialPaths(sessionID)
			if materialErr == nil && fileExists(keyPath) && fileExists(certPath) {
				return keyPath, certPath, nil
			}
			lastErr = materialErr
			if lastErr == nil {
				lastErr = fmt.Errorf("tenki ssh material missing key=%s cert=%s", keyPath, certPath)
			}
		} else {
			lastErr = ExitError{Code: result.ExitCode, Message: fmt.Sprintf("tenki sandbox ssh readiness failed: %v%s", err, tenkiCommandOutputDetail(result))}
		}

		if ctx.Err() != nil {
			return "", "", exit(5, "timed out waiting for Tenki SSH cert for session %s: %v", sessionID, lastErr)
		}
		fmt.Fprintf(b.rt.Stderr, "waiting for tenki ssh cert session=%s remaining=%s last=%v\n", sessionID, time.Until(deadline).Round(time.Second), lastErr)
		select {
		case <-ctx.Done():
			return "", "", exit(5, "timed out waiting for Tenki SSH cert for session %s: %v", sessionID, lastErr)
		case <-time.After(5 * time.Second):
		}
	}
}

func tenkiCommandOutputDetail(result LocalCommandResult) string {
	output := strings.TrimSpace(result.Stderr)
	if output == "" {
		output = strings.TrimSpace(result.Stdout)
	}
	if output == "" {
		return ""
	}
	return ": " + output
}

func tenkiSSHMaterialPaths(sessionID string) (string, string, error) {
	// Matches the Tenki CLI managed SSH state layout. A future Tenki CLI
	// machine-readable ssh-info command should replace this filesystem lookup.
	sshDir, err := tenkiSSHStateDir("ssh")
	if err != nil {
		return "", "", err
	}
	keyPath := filepath.Join(sshDir, "id_ed25519")
	certDir, err := tenkiSSHCertDir(sessionID)
	if err != nil {
		return keyPath, "", err
	}
	certPath, err := newestTenkiSSHCert(certDir)
	if err != nil {
		return keyPath, "", err
	}
	return keyPath, certPath, nil
}

func tenkiSSHCertDir(sessionID string) (string, error) {
	sid := strings.TrimSpace(sessionID)
	if sid == "" {
		return "", fmt.Errorf("session id is required for Tenki SSH cert cache")
	}
	return tenkiSSHStateDir(filepath.Join("ssh-certs", sid))
}

func newestTenkiSSHCert(certDir string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(certDir, "*-cert.pub"))
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no Tenki SSH cert found in %s", certDir)
	}
	newest := ""
	var newestMod time.Time
	for _, match := range matches {
		info, err := os.Stat(match)
		if err != nil {
			continue
		}
		if newest == "" || info.ModTime().After(newestMod) {
			newest = match
			newestMod = info.ModTime()
		}
	}
	if newest == "" {
		return "", fmt.Errorf("no readable Tenki SSH cert found in %s", certDir)
	}
	return newest, nil
}

func tenkiSSHStateDir(elem ...string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	parts := append([]string{home, ".config", "tenki"}, elem...)
	return filepath.Join(parts...), nil
}

func (b *tenkiBackend) sshTarget(sessionID, keyPath, certPath string) SSHTarget {
	return SSHTarget{
		User:                   "tenki",
		Host:                   "sandbox",
		Key:                    keyPath,
		CertificateFile:        certPath,
		Port:                   "22",
		TargetOS:               targetLinux,
		DisableHostKeyChecking: true,
		NetworkKind:            networkPublic,
		SSHConfigProxy:         true,
		ProxyCommand:           b.proxyCommand(sessionID),
	}
}

func (b *tenkiBackend) sessionToServer(cfg Config, session tenkiSession, leaseID, slug string, keep bool) Server {
	labels := directLeaseLabels(cfg, leaseID, slug, tenkiProvider, "", keep, time.Now().UTC())
	labels["tenki_session_id"] = session.ID
	labels["name"] = session.Name
	labels["state"] = tenkiState(session.State)
	labels["work_root"] = cfg.WorkRoot
	if session.ProjectID != "" {
		labels["project_id"] = session.ProjectID
	}
	server := Server{
		CloudID:  session.ID,
		Provider: tenkiProvider,
		Name:     blank(session.Name, session.ID),
		Status:   labels["state"],
		Labels:   labels,
	}
	server.ServerType.Name = tenkiServerType(cfg, session)
	return server
}

func (b *tenkiBackend) sandboxArgs(command string) []string {
	args := []string{"sandbox", command}
	if b.cfg.Tenki.Endpoint != "" {
		args = append(args, "--endpoint", b.cfg.Tenki.Endpoint)
	}
	return args
}

func (b *tenkiBackend) appendScopeArgs(args []string) []string {
	if b.cfg.Tenki.Workspace != "" {
		args = append(args, "--workspace", b.cfg.Tenki.Workspace)
	}
	if b.cfg.Tenki.Project != "" {
		args = append(args, "--project", b.cfg.Tenki.Project)
	}
	return args
}

func (b *tenkiBackend) proxyCommand(sessionID string) string {
	args := []string{tenkiCLIPath(b.cfg), "sandbox", "ssh-proxy", "--session", sessionID}
	if b.cfg.Tenki.Endpoint != "" {
		args = append(args, "--endpoint", b.cfg.Tenki.Endpoint)
	}
	if b.cfg.Tenki.Gateway != "" {
		args = append(args, "--gateway", b.cfg.Tenki.Gateway)
	}
	return shellJoin(args...)
}

func (b *tenkiBackend) runTenki(ctx context.Context, args []string, stdout, stderr io.Writer) (LocalCommandResult, error) {
	return b.rt.Exec.Run(ctx, LocalCommandRequest{Name: tenkiCLIPath(b.cfg), Args: args, Stdout: stdout, Stderr: stderr})
}

type tenkiSession struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	State            string            `json:"state"`
	ProjectID        string            `json:"project_id"`
	CPUCores         int               `json:"cpu_cores"`
	MemoryMB         int               `json:"memory_mb"`
	DiskSizeGB       int               `json:"disk_size_gb"`
	Sticky           bool              `json:"sticky"`
	SourceImageRef   string            `json:"source_image_ref"`
	SourceSnapshotID string            `json:"source_snapshot_id"`
	LastResumeError  string            `json:"last_resume_error"`
	Metadata         map[string]string `json:"metadata"`
	Tags             []string          `json:"tags"`
}

const (
	tenkiMetadataProvider = "crabbox_provider"
	tenkiMetadataLease    = "crabbox_lease_id"
	tenkiMetadataSlug     = "crabbox_slug"
)

func tenkiLeaseMetadata(session tenkiSession) (string, string) {
	leaseID := ""
	slug := ""
	if session.Metadata != nil {
		leaseID = strings.TrimSpace(session.Metadata[tenkiMetadataLease])
		slug = strings.TrimSpace(session.Metadata[tenkiMetadataSlug])
	}
	if slug == "" {
		slug = normalizeLeaseSlug(strings.TrimPrefix(session.Name, "crabbox-"))
	}
	return leaseID, slug
}

func isCrabboxTenkiSession(session tenkiSession) bool {
	if session.Metadata != nil && session.Metadata[tenkiMetadataProvider] == tenkiProvider {
		return true
	}
	for _, tag := range session.Tags {
		if tag == "crabbox-provider-tenki" {
			return true
		}
	}
	return false
}

func tenkiState(state string) string {
	state = tenkiNormalizedState(state)
	if state == "" {
		return "unknown"
	}
	switch state {
	case "running", "ready":
		return "ready"
	default:
		return state
	}
}

func tenkiNormalizedState(state string) string {
	return strings.ToLower(strings.TrimSpace(state))
}

func tenkiSessionReady(session tenkiSession) bool {
	switch tenkiNormalizedState(session.State) {
	case "ready", "running":
		return true
	default:
		return false
	}
}

func tenkiServerType(cfg Config, session tenkiSession) string {
	if session.SourceImageRef != "" {
		return session.SourceImageRef
	}
	if session.SourceSnapshotID != "" {
		return "snapshot"
	}
	if cfg.Tenki.Image != "" {
		return cfg.Tenki.Image
	}
	if cfg.Tenki.Snapshot != "" {
		return "snapshot"
	}
	return "sandbox"
}

func tenkiWorkRoot(cfg Config) string {
	return blank(strings.TrimSpace(cfg.Tenki.WorkRoot), "/home/tenki/crabbox")
}

func tenkiCLIPath(cfg Config) string {
	return blank(strings.TrimSpace(cfg.Tenki.CLIPath), "tenki")
}

func cleanTenkiWorkRoot(workRoot string) error {
	clean := path.Clean(strings.TrimSpace(workRoot))
	if clean == "" || !strings.HasPrefix(clean, "/") {
		return exit(2, "tenki.workRoot %q must resolve to an absolute path", workRoot)
	}
	switch clean {
	case "/", "/bin", "/dev", "/etc", "/home", "/home/tenki", "/lib", "/lib64", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var":
		return exit(2, "tenki.workRoot %q is too broad; choose a dedicated subdirectory", clean)
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
var tenkiIDLineRE = regexp.MustCompile(`(?m)^\s*id\s*:\s*(\S+)`)

func parseTenkiCreateSessionID(output string) string {
	clean := ansiRE.ReplaceAllString(output, "")
	match := tenkiIDLineRE.FindStringSubmatch(clean)
	if len(match) < 2 {
		return ""
	}
	return strings.TrimSpace(match[1])
}

func shellJoin(args ...string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, shellQuote(arg))
	}
	return strings.Join(quoted, " ")
}

func shellQuote(arg string) string {
	if arg == "" {
		return "''"
	}
	if strings.IndexFunc(arg, func(r rune) bool {
		return !(r == '_' || r == '-' || r == '.' || r == '/' || r == ':' || r == '=' ||
			(r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z'))
	}) == -1 {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
}

var waitForSSHReadyFunc = waitForSSHReady
