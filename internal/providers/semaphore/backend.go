// Install: copy to internal/providers/semaphore/backend.go
package semaphore

import (
	"context"
	"fmt"

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
		return nil, core.Exit(2, "semaphore provider requires semaphore.host and semaphore.token in config or --semaphore-host/--semaphore-token flags")
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
	timeout := idleTimeout(b.cfg)

	fmt.Fprintf(b.rt.Stderr, "provisioning provider=semaphore project=%s machine=%s os=%s\n", project, machine, osImage)

	// 1. Create standalone job
	jobID, err := b.client.CreateJob(ctx, project, machine, osImage, timeout)
	if err != nil {
		return core.LeaseTarget{}, err
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
		return core.LeaseTarget{}, err
	}

	// 3. Get SSH key
	sshKey, err := b.client.GetSSHKey(ctx, jobID)
	if err != nil {
		return core.LeaseTarget{}, err
	}

	target := core.SSHTarget{
		User:     "semaphore",
		Host:     ip,
		Key:      sshKey,
		Port:     fmt.Sprintf("%d", sshPort),
		TargetOS: core.TargetLinux,
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

// Resolve looks up an existing Semaphore job by ID.
func (b *semaphoreBackend) Resolve(ctx context.Context, req core.ResolveRequest) (core.LeaseTarget, error) {
	jobID := stripLeasePrefix(req.ID)

	state, ip, sshPort, err := b.client.GetJobStatus(ctx, jobID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if state != "RUNNING" {
		return core.LeaseTarget{}, core.Exit(4, "semaphore job %s is not running (state: %s)", jobID, state)
	}

	sshKey, err := b.client.GetSSHKey(ctx, jobID)
	if err != nil {
		return core.LeaseTarget{}, err
	}

	leaseID := "sem_" + jobID
	target := core.SSHTarget{
		User:     "semaphore",
		Host:     ip,
		Key:      sshKey,
		Port:     fmt.Sprintf("%d", sshPort),
		TargetOS: core.TargetLinux,
	}
	server := core.Server{
		CloudID:  jobID,
		Provider: providerName,
		Name:     "sem-testbox",
		Status:   "running",
	}
	server.PublicNet.IPv4.IP = ip
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
		s := core.Server{
			CloudID:  j.ID,
			Provider: providerName,
			Name:     j.Name,
			Status:   j.State,
			Labels: map[string]string{
				"lease":    "sem_" + j.ID,
				"provider": providerName,
			},
		}
		servers = append(servers, s)
	}
	return servers, nil
}

// ReleaseLease stops the Semaphore job.
func (b *semaphoreBackend) ReleaseLease(ctx context.Context, req core.ReleaseLeaseRequest) error {
	jobID := stripLeasePrefix(req.Lease.LeaseID)
	return b.client.StopJob(ctx, jobID)
}

// Touch is a no-op for Semaphore — the keepalive script handles idle timeout.
func (b *semaphoreBackend) Touch(ctx context.Context, req core.TouchRequest) (core.Server, error) {
	return req.Lease.Server, nil
}

func stripLeasePrefix(leaseID string) string {
	if len(leaseID) > 4 && leaseID[:4] == "sem_" {
		return leaseID[4:]
	}
	return leaseID
}
