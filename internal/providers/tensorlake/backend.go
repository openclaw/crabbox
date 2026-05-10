package tensorlake

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

func NewTensorlakeBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = providerName
	return &tensorlakeBackend{spec: spec, cfg: cfg, rt: rt}
}

type tensorlakeBackend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

func (b *tensorlakeBackend) Spec() ProviderSpec { return b.spec }

func (b *tensorlakeBackend) Warmup(ctx context.Context, req WarmupRequest) error {
	started := b.now()
	cli, err := newTensorlakeCLI(b.cfg, b.rt)
	if err != nil {
		return err
	}
	leaseID, name, slug, err := b.createSandbox(ctx, cli, req.Repo, req.Reclaim)
	if err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s sandbox=%s\n", leaseID, slug, providerName, name)
	if !req.Keep {
		fmt.Fprintf(b.rt.Stderr, "warning: tensorlake warmup keeps the sandbox until explicit stop\n")
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

func (b *tensorlakeBackend) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if err := rejectSyncOptions(req); err != nil {
		return RunResult{}, err
	}
	started := b.now()
	cli, err := newTensorlakeCLI(b.cfg, b.rt)
	if err != nil {
		return RunResult{}, err
	}
	leaseID, name := "", ""
	acquired := false
	if req.ID == "" {
		var slug string
		leaseID, name, slug, err = b.createSandbox(ctx, cli, req.Repo, req.Reclaim)
		if err != nil {
			return RunResult{}, err
		}
		fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=%s sandbox=%s\n", leaseID, slug, providerName, name)
		acquired = true
	} else {
		leaseID, name, err = resolveLeaseID(req.ID, req.Repo.Root, req.Reclaim, b.cfg.IdleTimeout)
		if err != nil {
			return RunResult{}, err
		}
	}
	if acquired && !req.Keep {
		defer func() {
			if termErr := cli.terminate(context.Background(), name); termErr != nil {
				fmt.Fprintf(b.rt.Stderr, "warning: tensorlake terminate failed for %s: %v\n", name, termErr)
				return
			}
			removeLeaseClaim(leaseID)
		}()
	}
	fmt.Fprintf(b.rt.Stderr, "provider=%s lease=%s sandbox=%s\n", providerName, leaseID, name)
	command, err := buildCommand(req.Command, req.ShellMode)
	if err != nil {
		return RunResult{}, err
	}
	commandStart := b.now()
	exitCode, runErr := cli.execStream(ctx, name, "", command, b.rt.Stdout, b.rt.Stderr)
	commandDuration := b.now().Sub(commandStart)
	result := RunResult{
		ExitCode:      exitCode,
		Command:       commandDuration,
		Total:         b.now().Sub(started),
		SyncDelegated: true,
	}
	fmt.Fprintf(b.rt.Stderr, "tensorlake run summary command=%s total=%s exit=%d\n",
		result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), exitCode)
	if req.TimingJSON {
		if err := writeTimingJSON(b.rt.Stderr, timingReport{
			Provider:    providerName,
			LeaseID:     leaseID,
			SyncSkipped: true,
			CommandMs:   result.Command.Milliseconds(),
			TotalMs:     result.Total.Milliseconds(),
			ExitCode:    exitCode,
		}); err != nil {
			return result, err
		}
	}
	if runErr != nil {
		return result, ExitError{Code: 1, Message: fmt.Sprintf("tensorlake run failed: %v", runErr)}
	}
	if exitCode != 0 {
		return result, ExitError{Code: exitCode, Message: fmt.Sprintf("tensorlake run exited %d", exitCode)}
	}
	return result, nil
}

func (b *tensorlakeBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	_ = req
	cli, err := newTensorlakeCLI(b.cfg, b.rt)
	if err != nil {
		return nil, err
	}
	ids, err := cli.listIDs(ctx)
	if err != nil {
		return nil, err
	}
	servers := make([]Server, 0, len(ids))
	for _, id := range ids {
		// `sbx ls -q` returns IDs only; we don't know which are crabbox-owned
		// without describing each. Cross-reference with local claims instead.
		claim, ok, err := resolveLeaseClaim(id)
		if err != nil {
			continue
		}
		if !ok || claim.Provider != providerName {
			// Try matching by lease prefix on the canonical name as well.
			if leaseClaim, leaseOK, _ := resolveLeaseClaim(leasePrefix + id); leaseOK && leaseClaim.Provider == providerName {
				claim = leaseClaim
				ok = true
			}
		}
		if !ok || claim.Provider != providerName {
			continue
		}
		servers = append(servers, Server{
			Provider: providerName,
			CloudID:  id,
			Name:     strings.TrimPrefix(claim.LeaseID, leasePrefix),
			Status:   "",
			Labels: map[string]string{
				"provider": providerName,
				"lease":    claim.LeaseID,
				"slug":     claim.Slug,
				"target":   targetLinux,
			},
		})
	}
	return servers, nil
}

func (b *tensorlakeBackend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	cli, err := newTensorlakeCLI(b.cfg, b.rt)
	if err != nil {
		return StatusView{}, err
	}
	leaseID, name, err := resolveLeaseID(req.ID, "", false, 0)
	if err != nil {
		return StatusView{}, err
	}
	deadline := b.now().Add(req.WaitTimeout)
	if req.WaitTimeout <= 0 {
		deadline = b.now().Add(5 * time.Minute)
	}
	for {
		_, describeErr := cli.describe(ctx, name)
		view := StatusView{
			ID:       leaseID,
			Slug:     newLeaseSlug(leaseID),
			Provider: providerName,
			TargetOS: targetLinux,
			ServerID: name,
			Network:  NetworkPublic,
			Ready:    describeErr == nil,
			Labels: map[string]string{
				"provider": providerName,
				"lease":    leaseID,
			},
		}
		if describeErr == nil {
			view.State = statusViewReady
		}
		if !req.Wait || view.Ready {
			return view, nil
		}
		if b.now().After(deadline) {
			return StatusView{}, exit(5, "timed out waiting for tensorlake sandbox %s to become ready", name)
		}
		select {
		case <-ctx.Done():
			return StatusView{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (b *tensorlakeBackend) Stop(ctx context.Context, req StopRequest) error {
	cli, err := newTensorlakeCLI(b.cfg, b.rt)
	if err != nil {
		return err
	}
	leaseID, name, err := resolveLeaseID(req.ID, "", false, 0)
	if err != nil {
		return err
	}
	if err := cli.terminate(ctx, name); err != nil {
		return err
	}
	removeLeaseClaim(leaseID)
	fmt.Fprintf(b.rt.Stderr, "released lease=%s sandbox=%s\n", leaseID, name)
	return nil
}

func (b *tensorlakeBackend) createSandbox(ctx context.Context, cli *tensorlakeCLI, repo Repo, reclaim bool) (string, string, string, error) {
	name := newSandboxName(repo)
	if err := cli.createSandbox(ctx, name); err != nil {
		return "", "", "", err
	}
	leaseID := leasePrefix + name
	slug := newLeaseSlug(leaseID)
	if err := claimLeaseForRepoProvider(leaseID, slug, providerName, repo.Root, b.cfg.IdleTimeout, reclaim); err != nil {
		_ = cli.terminate(context.Background(), name)
		return "", "", "", err
	}
	return leaseID, name, slug, nil
}

func resolveLeaseID(id, repoRoot string, reclaim bool, idleTimeout time.Duration) (string, string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", "", exit(2, "provider=tensorlake requires a Crabbox-created sandbox name, lease id, or slug")
	}
	if strings.HasPrefix(id, leasePrefix) {
		name := strings.TrimPrefix(id, leasePrefix)
		if !isCrabboxSandboxName(name) {
			return "", "", exit(4, "tensorlake lease %q is not a Crabbox-owned sandbox", id)
		}
		return id, name, nil
	}
	if claim, ok, err := resolveLeaseClaim(id); err != nil {
		return "", "", err
	} else if ok && claim.Provider == providerName {
		if repoRoot != "" {
			if err := claimLeaseForRepoProvider(claim.LeaseID, claim.Slug, providerName, repoRoot,
				timeoutOrDefault(idleTimeout, time.Duration(claim.IdleTimeoutSeconds)*time.Second), reclaim); err != nil {
				return "", "", err
			}
		}
		return claim.LeaseID, strings.TrimPrefix(claim.LeaseID, leasePrefix), nil
	}
	if !isCrabboxSandboxName(id) {
		return "", "", exit(4, "tensorlake sandbox %q is not claimed by Crabbox; pass a Crabbox slug or %s<crabbox-sandbox-name>", id, leasePrefix)
	}
	return leasePrefix + id, id, nil
}

func timeoutOrDefault(primary, fallback time.Duration) time.Duration {
	if primary > 0 {
		return primary
	}
	return fallback
}

func newSandboxName(repo Repo) string {
	base := normalizeLeaseSlug(repo.Name)
	if base == "" {
		base = "crabbox"
	}
	base = strings.TrimPrefix(base, strings.TrimSuffix(namePrefix, "-")+"-")
	return namePrefix + base + "-" + randomSuffix()
}

func isCrabboxSandboxName(name string) bool {
	return strings.HasPrefix(normalizeLeaseSlug(name), namePrefix)
}

func randomSuffix() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())[:6]
	}
	return hex.EncodeToString(b[:])
}

func buildCommand(command []string, shellMode bool) ([]string, error) {
	if len(command) == 0 {
		return nil, errors.New("missing command")
	}
	if shellMode {
		return []string{"bash", "-lc", strings.Join(command, " ")}, nil
	}
	return command, nil
}

func (b *tensorlakeBackend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now()
	}
	return time.Now()
}
