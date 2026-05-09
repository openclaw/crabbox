package semaphore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	core "github.com/openclaw/crabbox/internal/cli"
)

type semaphoreBackend struct {
	spec   core.ProviderSpec
	cfg    core.Config
	rt     core.Runtime
	client *apiClient
}

func newBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if cfg.Semaphore.Host == "" || cfg.Semaphore.Token == "" {
		return nil, core.Exit(2, "semaphore provider requires semaphore.host in config, environment, or --semaphore-host and semaphore.token in config or environment")
	}
	cfg.Provider = providerName
	client := newAPIClient(cfg.Semaphore.Host, cfg.Semaphore.Token, rt)
	return &semaphoreBackend{spec: spec, cfg: cfg, rt: rt, client: client}, nil
}

func (b *semaphoreBackend) Spec() core.ProviderSpec { return b.spec }

// Acquire creates a Semaphore job and returns SSH connection info.
// Crabbox handles all sync and command execution from here.
func (b *semaphoreBackend) Acquire(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	project := b.cfg.Semaphore.Project
	if project == "" {
		return core.LeaseTarget{}, core.Exit(2, "semaphore.project is required")
	}

	machine := withDefault(b.cfg.Semaphore.Machine, "f1-standard-2")
	osImage := withDefault(b.cfg.Semaphore.OSImage, "ubuntu2204")
	timeout, err := idleTimeout(b.cfg)
	if err != nil {
		return core.LeaseTarget{}, core.Exit(2, "%v", err)
	}

	fmt.Fprintf(b.rt.Stderr, "provisioning provider=semaphore project=%s machine=%s os=%s\n", project, machine, osImage)

	// 1. Create standalone job
	jobID, err := b.client.CreateJob(ctx, project, machine, osImage, timeout)
	if err != nil {
		return core.LeaseTarget{}, err
	}

	// Best-effort cleanup if anything fails after job creation
	cleanup := func() {
		fmt.Fprintf(b.rt.Stderr, "cleaning up job %s after failed acquisition\n", jobID)
		_ = b.client.StopJob(context.Background(), jobID)
	}

	leaseID := "sem_" + jobID
	slug := core.NewLeaseSlug(leaseID)
	fmt.Fprintf(b.rt.Stderr, "created job=%s lease=%s slug=%s\n", jobID, leaseID, slug)

	// 2. Poll until RUNNING
	fmt.Fprintf(b.rt.Stderr, "waiting for job to start ")
	ip, sshPort, err := b.client.WaitForRunning(ctx, jobID, func() {
		fmt.Fprintf(b.rt.Stderr, ".")
	})
	fmt.Fprintln(b.rt.Stderr)
	if err != nil {
		cleanup()
		return core.LeaseTarget{}, err
	}

	// 3. Get SSH key and write to file (crabbox expects a file path)
	sshKey, err := b.client.GetSSHKey(ctx, jobID)
	if err != nil {
		cleanup()
		return core.LeaseTarget{}, err
	}

	keyPath, err := storeSSHKey(leaseID, sshKey)
	if err != nil {
		cleanup()
		return core.LeaseTarget{}, fmt.Errorf("store SSH key: %w", err)
	}

	target := core.SSHTarget{
		User:       "semaphore",
		Host:       ip,
		Key:        keyPath,
		Port:       fmt.Sprintf("%d", sshPort),
		TargetOS:   core.TargetLinux,
		ReadyCheck: "true", // Semaphore job is ready once SSH is reachable
	}

	server := core.Server{
		CloudID:  jobID,
		Provider: providerName,
		Name:     "sem-testbox-" + slug,
		Status:   "running",
		Labels: map[string]string{
			"lease":    leaseID,
			"slug":     slug,
			"provider": providerName,
			"project":  project,
			"machine":  machine,
			"os_image": osImage,
		},
	}
	server.ServerType.Name = machine
	server.PublicNet.IPv4.IP = ip

	return core.LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

// Resolve looks up an existing Semaphore job by ID or slug.
func (b *semaphoreBackend) Resolve(ctx context.Context, req core.ResolveRequest) (core.LeaseTarget, error) {
	id := req.ID

	// Try direct lease ID (sem_UUID or UUID)
	if isLeaseID(id) {
		return b.resolveByJobID(ctx, stripLeasePrefix(id))
	}

	// Resolve slug → lease ID via claim file
	if claim, found, err := core.ResolveLeaseClaim(id); err == nil && found && claim.Provider == providerName {
		return b.resolveByJobID(ctx, stripLeasePrefix(claim.LeaseID))
	}

	return core.LeaseTarget{}, core.Exit(4, "semaphore lease not found for %q — use the full lease ID (sem_UUID) or a slug from a recent warmup", id)
}

func isLeaseID(id string) bool {
	if len(id) > 4 && id[:4] == "sem_" {
		return true
	}
	// UUID format: 8-4-4-4-12 hex
	stripped := stripLeasePrefix(id)
	return len(stripped) == 36 && stripped[8] == '-' && stripped[13] == '-'
}

func (b *semaphoreBackend) resolveByJobID(ctx context.Context, jobID string) (core.LeaseTarget, error) {
	status, err := b.client.GetJobStatus(ctx, jobID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if !isCrabboxJobName(status.Name) {
		return core.LeaseTarget{}, core.Exit(4, "semaphore job %s is not Crabbox-managed", jobID)
	}
	if status.State != "RUNNING" {
		return core.LeaseTarget{}, core.Exit(4, "semaphore job %s is not running (state: %s)", jobID, status.State)
	}

	sshKey, err := b.client.GetSSHKey(ctx, jobID)
	if err != nil {
		return core.LeaseTarget{}, err
	}

	leaseID := "sem_" + jobID
	keyPath, err := storeSSHKey(leaseID, sshKey)
	if err != nil {
		return core.LeaseTarget{}, fmt.Errorf("store SSH key: %w", err)
	}

	slug := semaphoreClaimSlug(leaseID)

	target := core.SSHTarget{
		User:       "semaphore",
		Host:       status.IP,
		Key:        keyPath,
		Port:       fmt.Sprintf("%d", status.SSHPort),
		TargetOS:   core.TargetLinux,
		ReadyCheck: "true",
	}
	server := core.Server{
		CloudID:  jobID,
		Provider: providerName,
		Name:     semaphoreListName("sem-testbox", slug),
		Status:   "running",
		Labels: map[string]string{
			"lease":    leaseID,
			"slug":     slug,
			"provider": providerName,
		},
	}
	server.PublicNet.IPv4.IP = status.IP
	server.ServerType.Name = withDefault(b.cfg.Semaphore.Machine, "f1-standard-2")

	return core.LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

// List returns running Semaphore testbox jobs.
func (b *semaphoreBackend) List(ctx context.Context, req core.ListRequest) ([]core.Server, error) {
	jobs, err := b.client.ListRunningJobs(ctx)
	if err != nil {
		return nil, err
	}

	var servers []core.Server
	for _, j := range jobs {
		if !isCrabboxJobName(j.Name) {
			continue
		}
		leaseID := "sem_" + j.ID
		slug := semaphoreClaimSlug(leaseID)
		s := core.Server{
			CloudID:  j.ID,
			Provider: providerName,
			Name:     semaphoreListName(j.Name, slug),
			Status:   j.State,
			Labels: map[string]string{
				"lease":    leaseID,
				"provider": providerName,
			},
		}
		if slug != "" {
			s.Labels["slug"] = slug
		}
		servers = append(servers, s)
	}
	return servers, nil
}

// ReleaseLease stops the Semaphore job.
func (b *semaphoreBackend) ReleaseLease(ctx context.Context, req core.ReleaseLeaseRequest) error {
	jobID := stripLeasePrefix(req.Lease.LeaseID)
	if err := b.client.StopJob(ctx, jobID); err != nil {
		return err
	}
	core.RemoveLeaseClaim(req.Lease.LeaseID)
	core.RemoveStoredTestboxKey(req.Lease.LeaseID)
	return nil
}

// Touch is a no-op for Semaphore — the keepalive script handles idle timeout.
func (b *semaphoreBackend) Touch(ctx context.Context, req core.TouchRequest) (core.Server, error) {
	return req.Lease.Server, nil
}

func storeSSHKey(leaseID, keyContent string) (string, error) {
	path, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(keyContent), 0600); err != nil {
		return "", err
	}
	return path, nil
}

func stripLeasePrefix(leaseID string) string {
	if len(leaseID) > 4 && leaseID[:4] == "sem_" {
		return leaseID[4:]
	}
	return leaseID
}

func semaphoreClaimSlug(leaseID string) string {
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil || claim.Provider != providerName {
		return ""
	}
	return claim.Slug
}

func semaphoreListName(jobName, slug string) string {
	if slug == "" {
		return jobName
	}
	return "sem-testbox-" + slug
}
