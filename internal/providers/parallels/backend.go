package parallels

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type Config = core.Config
type Runtime = core.Runtime
type ProviderSpec = core.ProviderSpec
type Backend = core.Backend
type AcquireRequest = core.AcquireRequest
type ResolveRequest = core.ResolveRequest
type ListRequest = core.ListRequest
type LeaseView = core.LeaseView
type ReleaseLeaseRequest = core.ReleaseLeaseRequest
type TouchRequest = core.TouchRequest
type CleanupRequest = core.CleanupRequest
type LeaseTarget = core.LeaseTarget
type Server = core.Server
type SSHTarget = core.SSHTarget

type leaseBackend struct {
	shared.DirectSSHBackend
}

func NewBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = "parallels"
	if cfg.Parallels.User != "" {
		cfg.SSHUser = cfg.Parallels.User
	}
	if cfg.Parallels.WorkRoot != "" {
		cfg.WorkRoot = cfg.Parallels.WorkRoot
	}
	if cfg.TargetOS == core.TargetMacOS && cfg.SSHUser == core.BaseConfig().SSHUser {
		cfg.SSHUser = os.Getenv("USER")
	}
	if cfg.SSHPort == "" {
		cfg.SSHPort = "22"
	}
	return &leaseBackend{DirectSSHBackend: shared.DirectSSHBackend{SpecValue: spec, Cfg: cfg, RT: rt, StoredLeaseKeys: true}}
}

func (b *leaseBackend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	return shared.AcquireAttemptsRetry(b.RT, req.Keep, func() (LeaseTarget, error) {
		return b.acquireOnce(ctx, req.Keep, req.RequestedSlug)
	})
}

func (b *leaseBackend) acquireOnce(ctx context.Context, keep bool, requestedSlug string) (LeaseTarget, error) {
	cfg := b.Cfg
	source := strings.TrimSpace(firstNonEmpty(cfg.Parallels.SourceID, cfg.Parallels.Source))
	if source == "" {
		return LeaseTarget{}, core.Exit(2, "provider=parallels requires --parallels-source, --parallels-template, or parallels.source")
	}
	selected, err := core.SelectParallelsFleetConfig(ctx, cfg, b.RT.Exec, source)
	if err != nil {
		return LeaseTarget{}, err
	}
	cfg = selected
	client := core.NewParallelsClient(cfg, b.RT.Exec)
	servers, err := client.ListCrabboxServers(ctx)
	if err != nil {
		return LeaseTarget{}, err
	}
	leaseID := core.NewLeaseID()
	slug, err := core.AllocateDirectLeaseSlug(leaseID, requestedSlug, servers)
	if err != nil {
		return LeaseTarget{}, err
	}
	keyPath, publicKey, err := core.EnsureTestboxKeyForConfig(cfg, leaseID)
	if err != nil {
		return LeaseTarget{}, err
	}
	keepKey := false
	defer func() {
		if !keepKey {
			core.RemoveStoredTestboxKey(leaseID)
		}
	}()
	cleanupVM := func(id string) {
		if err := client.Delete(context.Background(), id); err != nil {
			keepKey = true
		}
	}
	cfg.SSHKey = keyPath
	cfg.ProviderKey = core.ProviderKeyForLease(leaseID)
	snapshotID := strings.TrimSpace(firstNonEmpty(cfg.Parallels.SourceSnapshotID, cfg.Parallels.SourceSnapshot))
	if snapshotID != "" && cfg.Parallels.SourceSnapshotID == "" {
		resolved, err := client.SnapshotID(ctx, source, snapshotID)
		if err != nil {
			return LeaseTarget{}, err
		}
		snapshotID = resolved
	}
	fmt.Fprintf(b.RT.Stderr, "provisioning provider=parallels lease=%s slug=%s host=%s source=%s snapshot=%s clone_mode=%s keep=%v\n",
		leaseID, slug, blank(cfg.Parallels.SelectedHost, "local"), source, blank(snapshotID, "-"), blank(cfg.Parallels.CloneMode, "linked"), keep)
	server, err := client.Clone(ctx, source, snapshotID, leaseID, slug, keep)
	if err != nil {
		return LeaseTarget{}, err
	}
	if err := client.Start(ctx, server.CloudID); err != nil {
		cleanupVM(server.CloudID)
		return LeaseTarget{}, err
	}
	vm, err := client.WaitForIP(ctx, server.CloudID, cfg.Parallels.StartupTimeout)
	if err != nil {
		cleanupVM(server.CloudID)
		return LeaseTarget{}, err
	}
	if err := client.WaitForGuestExec(ctx, server.CloudID, cfg, cfg.Parallels.StartupTimeout); err != nil {
		cleanupVM(server.CloudID)
		return LeaseTarget{}, err
	}
	if err := client.InstallSSHKey(ctx, server.CloudID, cfg, publicKey); err != nil {
		cleanupVM(server.CloudID)
		return LeaseTarget{}, err
	}
	if err := client.EnsureGuestReady(ctx, server.CloudID, cfg); err != nil {
		cleanupVM(server.CloudID)
		return LeaseTarget{}, err
	}
	server.PublicNet.IPv4.IP = vm.IP
	target := core.SSHTargetFromConfig(cfg, vm.IP)
	if cfg.TargetOS == core.TargetWindows && cfg.WindowsMode == core.WindowsModeNormal {
		target.ReadyCheck = core.PowershellCommand(`$PSVersionTable.PSVersion | Out-Null`)
	}
	if cfg.Parallels.Host != "" {
		target.ProxyCommand = parallelsProxyCommand(cfg, vm.IP)
		target.SSHConfigProxy = true
	}
	if err := core.WaitForSSHReady(ctx, &target, b.RT.Stderr, "bootstrap", core.BootstrapWaitTimeout(cfg)); err != nil {
		cleanupVM(server.CloudID)
		return LeaseTarget{}, err
	}
	server.Status = "ready"
	server.Labels = core.TouchDirectLeaseLabels(server.Labels, cfg, "ready", time.Now().UTC())
	fmt.Fprintf(b.RT.Stderr, "provisioned lease=%s vm=%s ip=%s\n", leaseID, server.DisplayID(), vm.IP)
	keepKey = true
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *leaseBackend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	id := strings.TrimSpace(req.ID)
	if id == "" {
		return LeaseTarget{}, core.Exit(2, "parallels resolve requires lease id or slug")
	}
	if claim, ok, err := core.ResolveLeaseClaimForProvider(id, "parallels"); err != nil {
		return LeaseTarget{}, err
	} else if ok {
		id = claim.LeaseID
	}
	var hostErrs []error
	for _, candidate := range core.ParallelsCandidateConfigs(b.Cfg) {
		client := core.NewParallelsClient(candidate, b.RT.Exec)
		vms, err := client.ListVMs(ctx)
		if err != nil {
			hostErrs = append(hostErrs, parallelsHostError(candidate, "list vms", err))
			continue
		}
		for _, vm := range vms {
			leaseID, slug := parallelsLeaseFromVMName(vm.Name)
			labels := core.ParallelsLabelsFromName(vm.Name)
			if labels["lease"] == "" {
				labels = core.DirectLeaseLabels(candidate, firstNonEmpty(leaseID, vm.ID), slug, "parallels", "", true, time.Now().UTC())
			}
			labels["host"] = blank(candidate.Parallels.SelectedHost, "local")
			server := core.Server{CloudID: vm.ID, Provider: "parallels", Name: vm.Name, Status: strings.ToLower(vm.State), Labels: labels}
			server.PublicNet.IPv4.IP = vm.IP
			server.ServerType.Name = core.ServerTypeForProviderClass("parallels", candidate.Class)
			normalizedID := strings.ReplaceAll(id, "_", "-")
			if vm.ID == id || vm.Name == id || leaseID == id || strings.ReplaceAll(leaseID, "_", "-") == normalizedID || core.NormalizeLeaseSlug(slug) == core.NormalizeLeaseSlug(id) {
				if vm.IP == "" && strings.EqualFold(vm.State, "running") {
					vm, _ = client.WaitForIP(ctx, vm.ID, 30*time.Second)
				}
				if strings.TrimSpace(candidate.SSHUser) == core.BaseConfig().SSHUser {
					var user string
					var err error
					if candidate.TargetOS == core.TargetWindows && candidate.WindowsMode == core.WindowsModeNormal {
						user, err = client.WindowsGuestText(ctx, vm.ID, `C:\ProgramData\crabbox\windows.username`)
					} else {
						user, err = client.POSIXGuestText(ctx, vm.ID, `/var/lib/crabbox/ssh.username`)
					}
					if err == nil && strings.TrimSpace(user) != "" {
						candidate.SSHUser = strings.TrimSpace(user)
					}
				}
				target := core.SSHTargetFromConfig(candidate, vm.IP)
				useStoredTestboxKey(&target, leaseID)
				if candidate.Parallels.Host != "" {
					target.ProxyCommand = parallelsProxyCommand(candidate, vm.IP)
					target.SSHConfigProxy = true
				}
				return LeaseTarget{Server: server, SSH: target, LeaseID: firstNonEmpty(leaseID, vm.ID)}, nil
			}
		}
	}
	if len(hostErrs) > 0 {
		return LeaseTarget{}, fmt.Errorf("parallels fleet inventory incomplete while resolving %s: %w", req.ID, errors.Join(hostErrs...))
	}
	return LeaseTarget{}, core.Exit(4, "parallels lease not found: %s", req.ID)
}

func (b *leaseBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	_ = req
	var out []LeaseView
	var hostErrs []error
	for _, cfg := range core.ParallelsCandidateConfigs(b.Cfg) {
		leases, err := core.NewParallelsClient(cfg, b.RT.Exec).ListCrabboxServers(ctx)
		if err != nil {
			hostErrs = append(hostErrs, parallelsHostError(cfg, "list leases", err))
			continue
		}
		for i := range leases {
			if leases[i].Labels == nil {
				leases[i].Labels = map[string]string{}
			}
			leases[i].Labels["host"] = blank(cfg.Parallels.SelectedHost, "local")
		}
		out = append(out, leases...)
	}
	if len(hostErrs) > 0 {
		return nil, fmt.Errorf("parallels fleet inventory incomplete: %w", errors.Join(hostErrs...))
	}
	return out, nil
}

func parallelsHostError(cfg Config, action string, err error) error {
	return fmt.Errorf("host %s %s: %w", blank(cfg.Parallels.SelectedHost, "local"), action, err)
}

func (b *leaseBackend) Doctor(ctx context.Context, req core.DoctorRequest) (core.DoctorResult, error) {
	servers, err := b.List(ctx, ListRequest{})
	if err != nil {
		return core.DoctorResult{}, err
	}
	runtime := "unchecked"
	cfg := b.Cfg
	if req.ProbeSSH && cfg.Parallels.Source != "" {
		selected, err := core.SelectParallelsFleetConfig(ctx, cfg, b.RT.Exec, cfg.Parallels.Source)
		if err != nil {
			return core.DoctorResult{}, err
		}
		client := core.NewParallelsClient(selected, b.RT.Exec)
		vm, err := client.GetVM(ctx, selected.Parallels.Source)
		if err != nil {
			return core.DoctorResult{}, err
		}
		if vm.IP != "" {
			target := core.SSHTargetFromConfig(selected, vm.IP)
			if selected.Parallels.Host != "" {
				target.ProxyCommand = parallelsProxyCommand(selected, vm.IP)
				target.SSHConfigProxy = true
			}
			if err := core.WaitForSSHReady(ctx, &target, io.Discard, "doctor", 10*time.Second); err != nil {
				return core.DoctorResult{}, err
			}
			runtime = "ssh_reachable"
		}
	}
	return core.DoctorResult{
		Provider: "parallels",
		Message:  fmt.Sprintf("cli=ready control_plane=ready inventory=ready api=list mutation=false leases=%d runtime=%s hosts=%d template=%s", len(servers), runtime, len(b.Cfg.Parallels.Hosts), blank(b.Cfg.Parallels.Template, "-")),
	}, nil
}

func (b *leaseBackend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	if req.Lease.Server.Name != "" && !strings.HasPrefix(req.Lease.Server.Name, "crabbox-") {
		return core.Exit(2, "refusing to release non-Crabbox Parallels VM %q", req.Lease.Server.Name)
	}
	id := firstNonEmpty(req.Lease.Server.CloudID, req.Lease.LeaseID)
	cfg := b.configForLease(ctx, req.Lease)
	if err := core.NewParallelsClient(cfg, b.RT.Exec).Delete(ctx, id); err != nil {
		return err
	}
	core.RemoveLeaseClaim(req.Lease.LeaseID)
	core.RemoveStoredTestboxKey(req.Lease.LeaseID)
	return nil
}

func (b *leaseBackend) Touch(ctx context.Context, req TouchRequest) (Server, error) {
	server := req.Lease.Server
	server.Labels = core.TouchDirectLeaseLabels(server.Labels, b.Cfg, req.State, time.Now().UTC())
	core.NewParallelsClient(b.configForLease(ctx, req.Lease), b.RT.Exec).SetLeaseLabels(firstNonEmpty(req.Lease.LeaseID, server.Labels["lease"]), server.Labels)
	return server, nil
}

func (b *leaseBackend) Cleanup(ctx context.Context, req CleanupRequest) error {
	servers, err := b.List(ctx, ListRequest{Options: req.Options})
	if err != nil {
		return err
	}
	for _, server := range servers {
		shouldDelete, reason := core.ShouldCleanupServer(server, time.Now().UTC())
		if !shouldDelete {
			fmt.Fprintf(b.RT.Stderr, "skip vm id=%s name=%s reason=%s\n", server.DisplayID(), server.Name, reason)
			continue
		}
		fmt.Fprintf(b.RT.Stderr, "delete vm id=%s name=%s\n", server.DisplayID(), server.Name)
		if req.DryRun {
			continue
		}
		client := core.NewParallelsClient(b.configForLease(ctx, LeaseTarget{
			Server:  server,
			LeaseID: server.Labels["lease"],
		}), b.RT.Exec)
		if err := client.Delete(ctx, server.CloudID); err != nil {
			return err
		}
		leaseID := server.Labels["lease"]
		core.RemoveLeaseClaim(leaseID)
		core.RemoveStoredTestboxKey(leaseID)
	}
	return nil
}

func parallelsProxyCommand(cfg Config, guestIP string) string {
	host := cfg.Parallels.Host
	if cfg.Parallels.HostUser != "" {
		host = cfg.Parallels.HostUser + "@" + host
	}
	args := []string{"ssh", "-W", guestIP + ":%p"}
	if cfg.Parallels.HostKey != "" {
		args = append([]string{"ssh", "-i", cfg.Parallels.HostKey, "-o", "IdentitiesOnly=yes", "-W", guestIP + ":%p"}, host)
	} else {
		args = append(args, host)
	}
	return strings.Join(core.ShellWords(args), " ")
}

func (b *leaseBackend) configForLease(ctx context.Context, lease LeaseTarget) Config {
	host := strings.TrimSpace(lease.Server.Labels["host"])
	if host != "" {
		for _, candidate := range core.ParallelsCandidateConfigs(b.Cfg) {
			if blank(candidate.Parallels.SelectedHost, "local") == host {
				return candidate
			}
		}
	}
	id := firstNonEmpty(lease.Server.CloudID, lease.LeaseID)
	if id != "" {
		for _, candidate := range core.ParallelsCandidateConfigs(b.Cfg) {
			client := core.NewParallelsClient(candidate, b.RT.Exec)
			if _, err := client.GetVM(ctx, id); err == nil {
				return candidate
			}
		}
	}
	return b.Cfg
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func blank(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func useStoredTestboxKey(target *SSHTarget, leaseID string) {
	if keyPath, err := core.TestboxKeyPath(leaseID); err == nil {
		if _, statErr := os.Stat(keyPath); statErr == nil {
			target.Key = keyPath
		}
	}
}

func parallelsLeaseFromVMName(name string) (string, string) {
	rest := strings.TrimPrefix(name, "crabbox-")
	if rest == name {
		return "", ""
	}
	parts := strings.SplitN(rest, "-", 3)
	if len(parts) < 2 || parts[0] != "cbx" {
		return "", core.NormalizeLeaseSlug(rest)
	}
	leaseID := "cbx_" + parts[1]
	slug := ""
	if len(parts) == 3 {
		slug = core.NormalizeLeaseSlug(parts[2])
	}
	return leaseID, slug
}
