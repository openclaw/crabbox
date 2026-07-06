package exedev

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

type exeDevLeaseBackend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

func NewExeDevLeaseBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = providerName
	applyExeDevDefaults(&cfg)
	return &exeDevLeaseBackend{spec: spec, cfg: cfg, rt: rt}
}

func (b *exeDevLeaseBackend) Spec() ProviderSpec { return b.spec }

func (b *exeDevLeaseBackend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	leaseID := newLeaseID()
	servers, err := b.listServers(ctx, true)
	if err != nil {
		return LeaseTarget{}, err
	}
	slug, err := allocateDirectLeaseSlug(leaseID, req.RequestedSlug, servers)
	if err != nil {
		return LeaseTarget{}, err
	}
	cfg := b.configForRun()
	name := leaseProviderName(leaseID, slug)
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s name=%s image=%s cpus=%d memory=%s disk=%s keep=%v\n", providerName, leaseID, slug, name, exeDevImage(cfg), cfg.ExeDev.CPUs, cfg.ExeDev.Memory, cfg.ExeDev.Disk, req.Keep)
	vm, err := b.createVM(ctx, cfg, name, leaseID, slug)
	if err != nil {
		return LeaseTarget{}, err
	}
	lease, err := b.prepareLease(ctx, cfg, vm, leaseID, slug, req.Keep, true)
	if err != nil {
		if !req.Keep {
			err = b.rollbackCreatedVM(name, err)
		}
		return LeaseTarget{}, err
	}
	claim, err := claimLeaseTargetForRepoConfigIfUnchanged(leaseID, slug, cfg, lease.Server, lease.SSH, req.Repo.Root, cfg.IdleTimeout, req.Reclaim, LeaseClaim{}, false)
	if err != nil {
		if !req.Keep {
			err = b.rollbackCreatedVM(name, err)
		}
		return LeaseTarget{}, err
	}
	setServerLeaseClaimSnapshot(&lease.Server, claim, true)
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s name=%s state=ready\n", leaseID, name)
	return lease, nil
}

func (b *exeDevLeaseBackend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	cfg := b.configForRun()
	vm, leaseID, slug, err := b.resolveVM(ctx, req.ID)
	if err != nil {
		return LeaseTarget{}, err
	}
	if req.ReleaseOnly {
		server := exeDevServer(vm, leaseID, slug, cfg, true)
		claim, err := b.claimForVMRelease(vm, leaseID, slug)
		if err != nil {
			return LeaseTarget{}, err
		}
		setServerLeaseClaimSnapshot(&server, claim, true)
		return LeaseTarget{Server: server, LeaseID: leaseID}, nil
	}
	lease, err := b.prepareLease(ctx, cfg, vm, leaseID, slug, true, false)
	if err != nil {
		return LeaseTarget{}, err
	}
	if req.Repo.Root != "" {
		claim, err := b.claimResolvedVM(lease, vm, leaseID, slug, req)
		if err != nil {
			return LeaseTarget{}, err
		}
		setServerLeaseClaimSnapshot(&lease.Server, claim, true)
	} else if claim, exists, err := readLeaseClaimWithPresence(leaseID); err != nil {
		return LeaseTarget{}, err
	} else {
		if exists {
			if err := b.validateVMClaimBinding(vm, claim, leaseID, slug); err != nil {
				return LeaseTarget{}, err
			}
		} else if !req.IsReadOnlyStatus() {
			return LeaseTarget{}, exit(2, "provider=%s lease %s has no exact local claim; use a repository-scoped reuse with --reclaim before operating on it", providerName, leaseID)
		}
		setServerLeaseClaimSnapshot(&lease.Server, claim, exists)
	}
	return lease, nil
}

func (b *exeDevLeaseBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	return b.listServers(ctx, req.All)
}

func (b *exeDevLeaseBackend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	servers, err := b.List(ctx, ListRequest{})
	if err != nil {
		return DoctorResult{}, err
	}
	return cliDoctorResult(providerName, len(servers), "unchecked"), nil
}

func (b *exeDevLeaseBackend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	claim, exists, snapshotSet := serverLeaseClaimSnapshot(req.Lease.Server)
	if !snapshotSet || !exists {
		return exit(2, "provider=%s release requires an exact local claim snapshot", providerName)
	}
	if err := b.validateReleaseTarget(req.Lease, claim); err != nil {
		return err
	}
	return removeLeaseClaimIfUnchangedAfter(claim.LeaseID, claim, func() error {
		vm, err := b.findVMByExactName(ctx, claim.CloudID)
		if err != nil {
			return err
		}
		if err := b.validateVMClaimBinding(vm, claim, claim.LeaseID, claim.Slug); err != nil {
			return err
		}
		return b.deleteVM(ctx, claim.CloudID)
	})
}

func (b *exeDevLeaseBackend) Touch(_ context.Context, req TouchRequest) (Server, error) {
	server := req.Lease.Server
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	server.Labels = touchDirectLeaseLabels(server.Labels, b.configForRun(), req.State, time.Now().UTC())
	return server, nil
}

func (b *exeDevLeaseBackend) configForRun() Config {
	cfg := b.cfg
	applyExeDevDefaults(&cfg)
	return cfg
}

func applyExeDevDefaults(cfg *Config) {
	cfg.Provider = providerName
	if cfg.TargetOS == "" {
		cfg.TargetOS = targetLinux
	}
	cfg.SSHPort = "22"
	cfg.SSHFallbackPorts = nil
	if cfg.ExeDev.ControlHost == "" {
		cfg.ExeDev.ControlHost = "exe.dev"
	}
	if cfg.ExeDev.CPUs <= 0 {
		cfg.ExeDev.CPUs = 2
	}
	if cfg.ExeDev.Memory == "" {
		cfg.ExeDev.Memory = "4GB"
	}
	if cfg.ExeDev.Disk == "" {
		cfg.ExeDev.Disk = "10GB"
	}
	if cfg.ExeDev.WorkRoot == "" {
		if !isDefaultWorkRoot(cfg.WorkRoot) {
			cfg.ExeDev.WorkRoot = cfg.WorkRoot
		} else {
			cfg.ExeDev.WorkRoot = "/tmp/crabbox"
		}
	}
	if cfg.ExeDev.User != "" {
		cfg.SSHUser = cfg.ExeDev.User
	} else if cfg.SSHUser == "" || cfg.SSHUser == "crabbox" {
		cfg.SSHUser = blank(os.Getenv("USER"), "root")
	}
	if cfg.ExeDev.WorkRoot != "" {
		cfg.WorkRoot = cfg.ExeDev.WorkRoot
	}
	cfg.ServerType = exeDevImage(*cfg)
}

func (b *exeDevLeaseBackend) createVM(ctx context.Context, cfg Config, name, leaseID, slug string) (exeDevVM, error) {
	args := []string{"new", "--name", name, "--json", "--tag", "crabbox", "--tag", "crabbox-lease-" + leaseID, "--tag", "crabbox-slug-" + slug}
	if cfg.ExeDev.NoEmail {
		args = append(args, "--no-email")
	}
	if image := strings.TrimSpace(cfg.ExeDev.Image); image != "" {
		args = append(args, "--image", image)
	}
	if cfg.ExeDev.CPUs > 0 {
		args = append(args, "--cpu", strconv.Itoa(cfg.ExeDev.CPUs))
	}
	if memory := strings.TrimSpace(cfg.ExeDev.Memory); memory != "" {
		args = append(args, "--memory", memory)
	}
	if disk := strings.TrimSpace(cfg.ExeDev.Disk); disk != "" {
		args = append(args, "--disk", disk)
	}
	if command := strings.TrimSpace(cfg.ExeDev.Command); command != "" {
		args = append(args, "--command", command)
	}
	out, err := b.controlOutput(ctx, args)
	if err != nil {
		return exeDevVM{}, err
	}
	vm, err := parseExeDevVM(out)
	if err == nil && vm.Name() != "" {
		return vm, nil
	}
	vm, _, _, err = b.resolveVM(ctx, name)
	return vm, err
}

func (b *exeDevLeaseBackend) deleteVM(ctx context.Context, name string) error {
	result, err := b.control(ctx, []string{"rm", name, "--json"}, io.Discard, b.rt.Stderr)
	if err != nil {
		return exit(commandExitCode(result), "exe.dev rm %s failed: %v", name, err)
	}
	return nil
}

func (b *exeDevLeaseBackend) claimResolvedVM(lease LeaseTarget, vm exeDevVM, leaseID, slug string, req ResolveRequest) (LeaseClaim, error) {
	if err := b.validateResolvedLeaseTarget(lease, vm, leaseID, slug); err != nil {
		return LeaseClaim{}, err
	}
	previous, exists, err := readLeaseClaimWithPresence(leaseID)
	if err != nil {
		return LeaseClaim{}, err
	}
	if !exists && !req.Reclaim {
		return LeaseClaim{}, exit(2, "exe.dev VM %s is not locally claimed; inspect it, then reuse with --reclaim before operating on it", vm.Name())
	}
	if exists {
		if previous.Provider != "" && previous.Provider != providerName {
			return LeaseClaim{}, exit(2, "lease %s is already claimed by provider=%s", leaseID, previous.Provider)
		}
		if previous.Provider == "" && !req.Reclaim {
			return LeaseClaim{}, exit(2, "lease %s has a legacy providerless claim; reuse with --reclaim to bind provider=%s", leaseID, providerName)
		}
		if previous.CloudID != "" && previous.CloudID != vm.Name() {
			return LeaseClaim{}, exit(2, "lease %s is already bound to exe.dev VM %s, refusing retarget to %s", leaseID, previous.CloudID, vm.Name())
		}
		if err := b.validateExistingClaimRoute(previous); err != nil {
			return LeaseClaim{}, err
		}
	}
	cfg := b.configForRun()
	claim, err := claimLeaseTargetForRepoConfigIfUnchanged(leaseID, slug, cfg, lease.Server, lease.SSH, req.Repo.Root, cfg.IdleTimeout, req.Reclaim, previous, exists)
	if err != nil {
		return LeaseClaim{}, err
	}
	return claim, nil
}

func (b *exeDevLeaseBackend) validateResolvedLeaseTarget(lease LeaseTarget, vm exeDevVM, leaseID, slug string) error {
	if err := validateExeDevVMOwnership(vm, leaseID, slug, "reuse"); err != nil {
		return err
	}
	wantScope, err := exeDevControlScope(b.configForRun())
	if err != nil {
		return err
	}
	wantHost := vm.SSHAddress().Host
	if lease.LeaseID != leaseID || lease.Server.Provider != providerName || lease.Server.CloudID != vm.Name() || lease.Server.Name != vm.Name() || lease.SSH.Host != wantHost {
		return exit(2, "exe.dev VM %s resolved to inconsistent provider metadata", vm.Name())
	}
	if lease.Server.Labels["provider"] != providerName || lease.Server.Labels["lease"] != leaseID || normalizeLeaseSlug(lease.Server.Labels["slug"]) != normalizeLeaseSlug(slug) || lease.Server.Labels["name"] != vm.Name() || lease.Server.Labels[exeDevControlScopeLabel] != wantScope {
		return exit(2, "exe.dev VM %s resolved to incomplete claim metadata", vm.Name())
	}
	return nil
}

func (b *exeDevLeaseBackend) claimForVMRelease(vm exeDevVM, leaseID, slug string) (LeaseClaim, error) {
	claim, exists, err := readLeaseClaimWithPresence(leaseID)
	if err != nil {
		return LeaseClaim{}, err
	}
	if !exists {
		return LeaseClaim{}, exit(2, "exe.dev VM %s has no exact local claim; refusing deletion", vm.Name())
	}
	if err := b.validateVMClaimBinding(vm, claim, leaseID, slug); err != nil {
		return LeaseClaim{}, err
	}
	return claim, nil
}

func (b *exeDevLeaseBackend) validateExistingClaimRoute(claim LeaseClaim) error {
	want, err := exeDevControlScope(b.configForRun())
	if err != nil {
		return err
	}
	if got := strings.TrimSpace(claim.Labels[exeDevControlScopeLabel]); got != "" && got != want {
		return exit(2, "lease %s is bound to a different exe.dev control route", claim.LeaseID)
	}
	return nil
}

func (b *exeDevLeaseBackend) validateVMClaimBinding(vm exeDevVM, claim LeaseClaim, leaseID, slug string) error {
	if err := validateExeDevVMOwnership(vm, leaseID, slug, "deletion"); err != nil {
		return err
	}
	slug = normalizeLeaseSlug(slug)
	if claim.LeaseID != leaseID || claim.Provider != providerName || normalizeLeaseSlug(claim.Slug) != slug || claim.CloudID != vm.Name() {
		return exit(2, "exe.dev VM %s is not bound to an exact provider/resource claim", vm.Name())
	}
	wantScope, err := exeDevControlScope(b.configForRun())
	if err != nil {
		return err
	}
	if claim.Labels["provider"] != providerName || claim.Labels["lease"] != leaseID || normalizeLeaseSlug(claim.Labels["slug"]) != slug || claim.Labels["name"] != vm.Name() || claim.Labels[exeDevControlScopeLabel] != wantScope {
		return exit(2, "exe.dev VM %s claim metadata does not attest the current provider binding", vm.Name())
	}
	return nil
}

func validateExeDevVMOwnership(vm exeDevVM, leaseID, slug, operation string) error {
	remoteLeaseID, remoteSlug, owned, err := exeDevOwnershipIdentity(vm)
	if err != nil {
		return err
	}
	if !owned {
		return exit(2, "exe.dev VM %s has no complete Crabbox ownership tags; refusing %s", vm.Name(), operation)
	}
	slug = normalizeLeaseSlug(slug)
	if remoteLeaseID != leaseID || remoteSlug != slug {
		return exit(2, "exe.dev VM %s ownership tags do not match lease=%s slug=%s", vm.Name(), leaseID, slug)
	}
	wantName := leaseProviderName(leaseID, slug)
	if vm.Name() != wantName {
		return exit(2, "exe.dev VM %s does not match the claimed provider name %s", vm.Name(), wantName)
	}
	return nil
}

func (b *exeDevLeaseBackend) validateReleaseTarget(lease LeaseTarget, claim LeaseClaim) error {
	if lease.LeaseID != claim.LeaseID || lease.Server.Provider != providerName || lease.Server.CloudID != claim.CloudID || lease.Server.Name != claim.CloudID {
		return exit(2, "provider=%s release target does not match its claim snapshot", providerName)
	}
	if lease.Server.Labels["lease"] != claim.LeaseID || normalizeLeaseSlug(lease.Server.Labels["slug"]) != normalizeLeaseSlug(claim.Slug) {
		return exit(2, "provider=%s release labels do not match their claim snapshot", providerName)
	}
	return b.validateExistingClaimRoute(claim)
}

func (b *exeDevLeaseBackend) prepareLease(ctx context.Context, cfg Config, vm exeDevVM, leaseID, slug string, keep, wait bool) (LeaseTarget, error) {
	server := exeDevServer(vm, leaseID, slug, cfg, keep)
	target := exeDevSSHTarget(cfg, vm)
	if wait {
		if err := waitForSSHReady(ctx, &target, b.rt.Stderr, "exe.dev vm ssh", bootstrapWaitTimeout(cfg)); err != nil {
			return LeaseTarget{}, err
		}
		server.Status = "ready"
		server.Labels["state"] = "ready"
	}
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *exeDevLeaseBackend) resolveVM(ctx context.Context, identifier string) (exeDevVM, string, string, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return exeDevVM{}, "", "", exit(2, "provider=%s requires --id <vm-name-or-slug>", providerName)
	}
	if claim, ok, err := resolveLeaseClaimForProvider(identifier, providerName); err != nil {
		return exeDevVM{}, "", "", err
	} else if ok {
		slug := blank(claim.Slug, newLeaseSlug(claim.LeaseID))
		name := leaseProviderName(claim.LeaseID, slug)
		vm, err := b.findVM(ctx, name)
		return vm, claim.LeaseID, slug, err
	}
	if strings.HasPrefix(identifier, "cbx_") {
		vm, ok, err := b.findVMByLeaseID(ctx, identifier)
		if err != nil {
			return exeDevVM{}, "", "", err
		}
		if ok {
			leaseID, slug, err := b.leaseIdentityForVM(vm)
			if err != nil {
				return exeDevVM{}, "", "", err
			}
			return vm, leaseID, slug, nil
		}
		slug := newLeaseSlug(identifier)
		vm, err = b.findVM(ctx, leaseProviderName(identifier, slug))
		return vm, identifier, slug, err
	}
	vm, err := b.findVM(ctx, identifier)
	if err != nil {
		return exeDevVM{}, "", "", err
	}
	leaseID, slug, err := b.leaseIdentityForVM(vm)
	if err != nil {
		return exeDevVM{}, "", "", err
	}
	return vm, leaseID, slug, nil
}

func (b *exeDevLeaseBackend) findVM(ctx context.Context, identifier string) (exeDevVM, error) {
	vms, err := b.listVMs(ctx, true)
	if err != nil {
		return exeDevVM{}, err
	}
	id := normalizeLeaseSlug(identifier)
	for _, vm := range vms {
		if vm.Name() == identifier || normalizeLeaseSlug(vm.Name()) == id || vm.SSHHost() == identifier {
			return vm, nil
		}
	}
	return exeDevVM{}, exit(4, "exe.dev VM not found: %s", identifier)
}

func (b *exeDevLeaseBackend) findVMByExactName(ctx context.Context, name string) (exeDevVM, error) {
	vms, err := b.listVMs(ctx, true)
	if err != nil {
		return exeDevVM{}, err
	}
	for _, vm := range vms {
		if vm.Name() == name {
			return vm, nil
		}
	}
	return exeDevVM{}, exit(4, "exe.dev VM not found: %s", name)
}

func (b *exeDevLeaseBackend) findVMByLeaseID(ctx context.Context, leaseID string) (exeDevVM, bool, error) {
	vms, err := b.listVMs(ctx, true)
	if err != nil {
		return exeDevVM{}, false, err
	}
	for _, vm := range vms {
		taggedLeaseID, _, owned, err := exeDevOwnershipIdentity(vm)
		if err != nil {
			return exeDevVM{}, false, err
		}
		if owned && taggedLeaseID == leaseID {
			return vm, true, nil
		}
	}
	return exeDevVM{}, false, nil
}

func (b *exeDevLeaseBackend) listServers(ctx context.Context, all bool) ([]LeaseView, error) {
	vms, err := b.listVMs(ctx, all)
	if err != nil {
		return nil, err
	}
	cfg := b.configForRun()
	servers := make([]Server, 0, len(vms))
	for _, vm := range vms {
		if !all {
			_, _, owned, err := exeDevOwnershipIdentity(vm)
			if err != nil {
				return nil, err
			}
			if !owned {
				continue
			}
		}
		leaseID, slug, err := b.leaseIdentityForVM(vm)
		if err != nil {
			return nil, err
		}
		servers = append(servers, exeDevServer(vm, leaseID, slug, cfg, true))
	}
	return servers, nil
}

func (b *exeDevLeaseBackend) listVMs(ctx context.Context, _ bool) ([]exeDevVM, error) {
	args := []string{"ls", "--l", "--json"}
	out, err := b.controlOutput(ctx, args)
	if err != nil {
		return nil, err
	}
	var res exeDevListResponse
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		return nil, exit(5, "exe.dev ls returned invalid JSON: %v", err)
	}
	return res.VMs, nil
}

func (b *exeDevLeaseBackend) controlOutput(ctx context.Context, args []string) (string, error) {
	result, err := b.control(ctx, args, nil, b.rt.Stderr)
	if err != nil {
		if msg := exeDevErrorMessage(result.Stdout); msg != "" {
			return "", exit(commandExitCode(result), "exe.dev %s failed: %s", strings.Join(args, " "), msg)
		}
		return "", exit(commandExitCode(result), "exe.dev %s failed: %v", strings.Join(args, " "), err)
	}
	if msg := exeDevErrorMessage(result.Stdout); msg != "" {
		return "", exit(5, "exe.dev %s failed: %s", strings.Join(args, " "), msg)
	}
	return result.Stdout, nil
}

func (b *exeDevLeaseBackend) control(ctx context.Context, args []string, stdout, stderr io.Writer) (LocalCommandResult, error) {
	dest, port, err := exeDevControlDestination(b.configForRun().ExeDev.ControlHost)
	if err != nil {
		return LocalCommandResult{}, err
	}
	sshArgs := []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new", "-o", "ConnectTimeout=10"}
	if port != "" {
		sshArgs = append(sshArgs, "-p", port)
	}
	sshArgs = append(sshArgs, dest)
	sshArgs = append(sshArgs, shellQuoteArgs(args))
	return b.rt.Exec.Run(ctx, LocalCommandRequest{Name: "ssh", Args: sshArgs, Stdout: stdout, Stderr: stderr})
}

func (b *exeDevLeaseBackend) rollbackCreatedVM(name string, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := b.deleteVM(cleanupCtx, name); err != nil {
		return exit(exitCodeForError(cause), "%v; exe.dev cleanup failed for VM %s; manual cleanup: crabbox stop --provider exe-dev --id %s: %v", cause, name, name, err)
	}
	return cause
}

func exitCodeForError(err error) int {
	var exitErr ExitError
	if errors.As(err, &exitErr) && exitErr.Code != 0 {
		return exitErr.Code
	}
	return 1
}

func exeDevControlDestination(value string) (string, string, error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return "", "", exit(2, "provider=%s requires exe.dev control host", providerName)
	}
	if strings.Contains(raw, "://") || strings.HasPrefix(raw, "-") || strings.ContainsAny(raw, "/?#") || containsSpaceOrControl(raw) {
		return "", "", exit(2, "invalid exe.dev control host: %q", value)
	}
	user := ""
	hostPort := raw
	if strings.Count(raw, "@") > 1 {
		return "", "", exit(2, "invalid exe.dev control host: %q", value)
	}
	if before, after, ok := strings.Cut(raw, "@"); ok {
		if before == "" || strings.HasPrefix(before, "-") || strings.ContainsAny(before, ":@") {
			return "", "", exit(2, "invalid exe.dev control host: %q", value)
		}
		user = before
		hostPort = after
	}
	host := hostPort
	port := ""
	if strings.HasPrefix(hostPort, "[") {
		end := strings.Index(hostPort, "]")
		if end < 0 {
			return "", "", exit(2, "invalid exe.dev control host: %q", value)
		}
		host = hostPort[1:end]
		rest := hostPort[end+1:]
		if rest != "" {
			if !strings.HasPrefix(rest, ":") || rest == ":" {
				return "", "", exit(2, "invalid exe.dev control host: %q", value)
			}
			port = strings.TrimPrefix(rest, ":")
		}
		if !validExeDevControlIPHost(host) {
			return "", "", exit(2, "invalid exe.dev control host: %q", value)
		}
	} else if strings.Count(hostPort, ":") == 1 {
		before, after, _ := strings.Cut(hostPort, ":")
		host, port = before, after
	} else if strings.Contains(hostPort, ":") {
		if !validExeDevControlIPHost(hostPort) {
			return "", "", exit(2, "invalid exe.dev control host: %q", value)
		}
	}
	if host == "" || strings.HasPrefix(host, "-") || strings.ContainsAny(host, "/?#@") || containsSpaceOrControl(host) {
		return "", "", exit(2, "invalid exe.dev control host: %q", value)
	}
	if strings.Contains(host, "%") && !validExeDevControlIPHost(host) {
		return "", "", exit(2, "invalid exe.dev control host: %q", value)
	}
	if port != "" {
		p, err := strconv.Atoi(port)
		if err != nil || p <= 0 || p > 65535 {
			return "", "", exit(2, "invalid exe.dev control host port: %q", value)
		}
	}
	dest := host
	if user != "" {
		dest = user + "@" + host
	}
	return dest, port, nil
}

func validExeDevControlIPHost(host string) bool {
	if net.ParseIP(host) != nil {
		return true
	}
	addr, zone, ok := strings.Cut(host, "%")
	if !ok || addr == "" || zone == "" || net.ParseIP(addr) == nil || !strings.Contains(addr, ":") {
		return false
	}
	if strings.HasPrefix(zone, "-") || strings.ContainsAny(zone, "/%?#@[]:") || containsSpaceOrControl(zone) {
		return false
	}
	return true
}

func containsSpaceOrControl(value string) bool {
	for _, r := range value {
		if r <= 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func commandExitCode(result LocalCommandResult) int {
	if result.ExitCode != 0 {
		return result.ExitCode
	}
	return 1
}

const exeDevControlScopeLabel = "exe_dev_control_scope"

func exeDevControlScope(cfg Config) (string, error) {
	destination, port, err := exeDevControlDestination(cfg.ExeDev.ControlHost)
	if err != nil {
		return "", err
	}
	return "ssh:" + destination + "|port:" + blank(port, "default"), nil
}

func exeDevServer(vm exeDevVM, leaseID, slug string, cfg Config, keep bool) Server {
	labels := directLeaseLabels(cfg, leaseID, slug, providerName, "", keep, time.Now().UTC())
	labels["name"] = vm.Name()
	labels["state"] = blank(vm.Status, "unknown")
	labels["work_root"] = cfg.WorkRoot
	if scope, err := exeDevControlScope(cfg); err == nil {
		labels[exeDevControlScopeLabel] = scope
	}
	if vm.Region != "" {
		labels["region"] = vm.Region
	}
	if vm.RegionDisplay != "" {
		labels["region_display"] = vm.RegionDisplay
	}
	if vm.HTTPSURL != "" {
		labels["https_url"] = vm.HTTPSURL
	}
	server := Server{
		CloudID:  vm.Name(),
		Provider: providerName,
		Name:     vm.Name(),
		Status:   labels["state"],
		Labels:   labels,
	}
	server.PublicNet.IPv4.IP = vm.SSHHost()
	server.ServerType.Name = exeDevImage(cfg)
	return server
}

func exeDevSSHTarget(cfg Config, vm exeDevVM) SSHTarget {
	address := vm.SSHAddress()
	target := sshTargetFromConfig(cfg, address.Host)
	target.Key = ""
	if address.User != "" {
		target.User = address.User
	}
	if address.Port != "" {
		target.Port = address.Port
	}
	target.TargetOS = targetLinux
	target.NetworkKind = networkPublic
	target.ReadyCheck = "command -v git >/dev/null && command -v rsync >/dev/null && command -v tar >/dev/null"
	return target
}

func shellQuoteArgs(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, shellQuote(arg))
	}
	return strings.Join(quoted, " ")
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	safe := true
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("_@%+=:,./-", r) {
			continue
		}
		safe = false
		break
	}
	if safe {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func (b *exeDevLeaseBackend) leaseIdentityForVM(vm exeDevVM) (string, string, error) {
	if leaseID, slug, owned, err := exeDevOwnershipIdentity(vm); err != nil {
		return "", "", err
	} else if owned {
		return leaseID, slug, nil
	}
	if slug := inferExeDevSlugFromName(vm.Name()); slug != "" {
		if claim, ok, err := resolveLeaseClaimForProvider(slug, providerName); err != nil {
			return "", "", err
		} else if ok {
			claimSlug := blank(claim.Slug, newLeaseSlug(claim.LeaseID))
			if leaseProviderName(claim.LeaseID, claimSlug) == vm.Name() {
				return claim.LeaseID, claimSlug, nil
			}
		}
	}
	slug := normalizeLeaseSlug(vm.Name())
	leaseID := "exe_" + slug
	if strings.HasPrefix(slug, "crabbox-") {
		leaseID = "exe_" + strings.TrimPrefix(slug, "crabbox-")
	}
	return leaseID, slug, nil
}

func exeDevOwnershipIdentity(vm exeDevVM) (string, string, bool, error) {
	leaseIDs := map[string]struct{}{}
	slugs := map[string]struct{}{}
	baseTag := false
	for _, tag := range vm.Tags {
		tag = strings.TrimSpace(tag)
		if tag == "crabbox" {
			baseTag = true
		}
		if strings.HasPrefix(tag, "crabbox-lease-") {
			leaseID := strings.TrimSpace(strings.TrimPrefix(tag, "crabbox-lease-"))
			if leaseID != "" {
				leaseIDs[leaseID] = struct{}{}
			}
		}
		if strings.HasPrefix(tag, "crabbox-slug-") {
			slug := normalizeLeaseSlug(strings.TrimPrefix(tag, "crabbox-slug-"))
			if slug != "" {
				slugs[slug] = struct{}{}
			}
		}
	}
	if len(leaseIDs) > 1 || len(slugs) > 1 {
		return "", "", false, exit(2, "exe.dev VM %s has conflicting Crabbox ownership tags", vm.Name())
	}
	if !baseTag || len(leaseIDs) != 1 || len(slugs) != 1 {
		return "", "", false, nil
	}
	leaseID := ""
	for value := range leaseIDs {
		leaseID = value
	}
	if !isCanonicalLeaseID(leaseID) {
		return "", "", false, exit(2, "exe.dev VM %s has an invalid Crabbox lease tag", vm.Name())
	}
	slug := ""
	for value := range slugs {
		slug = value
	}
	return leaseID, slug, true, nil
}

func inferExeDevSlugFromName(name string) string {
	const prefix = "crabbox-"
	if !strings.HasPrefix(name, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(name, prefix)
	idx := strings.LastIndex(rest, "-")
	if idx <= 0 || idx == len(rest)-1 {
		return ""
	}
	hash := rest[idx+1:]
	if len(hash) != 8 || !isLowerHex(hash) {
		return ""
	}
	return normalizeLeaseSlug(rest[:idx])
}

func isLowerHex(value string) bool {
	for _, r := range value {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			continue
		}
		return false
	}
	return value != ""
}

func exeDevImage(cfg Config) string {
	return blank(strings.TrimSpace(cfg.ExeDev.Image), "default")
}
