package islo

import (
	"context"
	"strings"
	"time"

	gosdk "github.com/islo-labs/go-sdk"
	core "github.com/openclaw/crabbox/internal/cli"
)

const isloSSHDomain = "islo"

var _ core.SSHLoginBackend = (*isloBackend)(nil)

func (b *isloBackend) Resolve(ctx context.Context, req core.ResolveRequest) (core.LeaseTarget, error) {
	client, err := newIsloClient(b.cfg, b.rt)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	leaseID, name, _, err := resolveIsloLeaseID(req.ID, req.Repo.Root, req.Reclaim)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	sandbox, err := b.resolveSSHReadySandbox(ctx, client, name, req)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	server := isloSandboxToServer(sandbox)
	applyIsloSSHLabels(&server, leaseID, b.cfg)
	target := b.sshTargetForSandbox(name)
	return core.LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *isloBackend) Touch(_ context.Context, req core.TouchRequest) (Server, error) {
	server := req.Lease.Server
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	server.Labels["state"] = blank(req.State, blank(server.Labels["state"], server.Status))
	return server, nil
}

func (b *isloBackend) resolveSSHReadySandbox(ctx context.Context, client isloAPI, name string, req core.ResolveRequest) (*gosdk.SandboxResponse, error) {
	sandbox, err := client.GetSandbox(ctx, name)
	if err != nil {
		return nil, isloError("get sandbox", err)
	}
	if sandbox == nil {
		return nil, exit(4, "islo sandbox %s not found", name)
	}
	if req.StatusOnly || isloStatusReady(sandbox.GetStatus()) {
		return sandbox, nil
	}
	if isloStatusTerminal(sandbox.GetStatus()) {
		return nil, exit(5, "islo sandbox %s entered terminal state=%s", name, sandbox.GetStatus())
	}
	if strings.EqualFold(strings.TrimSpace(sandbox.GetStatus()), "paused") {
		sandbox, err = resumeIsloSandbox(ctx, client, name)
		if err != nil {
			return nil, isloError("resume sandbox", err)
		}
		return sandbox, nil
	}
	return waitForIsloSandboxRunning(ctx, client, name, 2*time.Minute)
}

func waitForIsloSandboxRunning(ctx context.Context, client isloAPI, name string, timeout time.Duration) (*gosdk.SandboxResponse, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, exit(5, "timed out waiting for islo sandbox %s to become running", name)
		case <-ticker.C:
			sandbox, err := client.GetSandbox(ctx, name)
			if err != nil {
				return nil, isloError("get sandbox", err)
			}
			if sandbox == nil {
				return nil, exit(4, "islo sandbox %s not found", name)
			}
			if isloStatusReady(sandbox.GetStatus()) {
				return sandbox, nil
			}
			if isloStatusTerminal(sandbox.GetStatus()) {
				return nil, exit(5, "islo sandbox %s entered terminal state=%s", name, sandbox.GetStatus())
			}
		}
	}
}

func (b *isloBackend) sshTargetForSandbox(name string) core.SSHTarget {
	user := isloWorkloadUser
	if core.IsSSHUserExplicit(&b.cfg) && strings.TrimSpace(b.cfg.SSHUser) != "" {
		user = strings.TrimSpace(b.cfg.SSHUser)
	}
	port := "22"
	if core.IsSSHPortExplicit(&b.cfg) && strings.TrimSpace(b.cfg.SSHPort) != "" {
		port = strings.TrimSpace(b.cfg.SSHPort)
	}
	key := ""
	if core.IsSSHKeyExplicit(&b.cfg) {
		key = b.cfg.SSHKey
	}
	return core.SSHTarget{
		User:                   user,
		Host:                   isloSSHHost(name),
		Key:                    key,
		Port:                   port,
		FallbackPorts:          []string{},
		TargetOS:               targetLinux,
		SSHConfigProxy:         true,
		DisableHostKeyChecking: true,
	}
}

func isloSSHHost(name string) string {
	return strings.TrimSpace(name) + "." + isloSSHDomain
}

func applyIsloSSHLabels(server *Server, leaseID string, cfg Config) {
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	name := strings.TrimPrefix(leaseID, isloLeasePrefix)
	if server.Name != "" {
		name = server.Name
	}
	host := isloSSHHost(name)
	server.Labels["lease"] = leaseID
	server.Labels["ssh_host"] = host
	if workRoot, err := isloWorkspacePath(cfg); err == nil {
		server.Labels["work_root"] = workRoot
	}
	server.PublicNet.IPv4.IP = host
}
