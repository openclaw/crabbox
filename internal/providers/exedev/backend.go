package exedev

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
			_ = b.deleteVM(context.Background(), name)
		}
		return LeaseTarget{}, err
	}
	if err := claimLeaseForRepoProvider(leaseID, slug, providerName, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
		if !req.Keep {
			_ = b.deleteVM(context.Background(), name)
		}
		return LeaseTarget{}, err
	}
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
		return LeaseTarget{Server: exeDevServer(vm, leaseID, slug, cfg, true), LeaseID: leaseID}, nil
	}
	lease, err := b.prepareLease(ctx, cfg, vm, leaseID, slug, true, false)
	if err != nil {
		return LeaseTarget{}, err
	}
	if req.Repo.Root != "" {
		if err := claimLeaseForRepoProvider(leaseID, slug, providerName, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
			return LeaseTarget{}, err
		}
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
	name := strings.TrimSpace(req.Lease.Server.Name)
	if name == "" {
		vm, _, _, err := b.resolveVM(ctx, req.Lease.LeaseID)
		if err != nil {
			return err
		}
		name = vm.Name()
	}
	if name == "" {
		return exit(2, "provider=%s release requires a VM name", providerName)
	}
	if err := b.deleteVM(ctx, name); err != nil {
		return err
	}
	removeLeaseClaim(req.Lease.LeaseID)
	return nil
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

func (b *exeDevLeaseBackend) findVMByLeaseID(ctx context.Context, leaseID string) (exeDevVM, bool, error) {
	vms, err := b.listVMs(ctx, true)
	if err != nil {
		return exeDevVM{}, false, err
	}
	for _, vm := range vms {
		if taggedLeaseID, _ := exeDevTaggedIdentity(vm); taggedLeaseID == leaseID {
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
		if !all && !strings.HasPrefix(vm.Name(), "crabbox-") {
			continue
		}
		leaseID, slug, err := b.leaseIdentityForVM(vm)
		if err != nil {
			return nil, err
		}
		servers = append(servers, exeDevServer(vm, leaseID, slug, cfg, true))
	}
	return servers, nil
}

func (b *exeDevLeaseBackend) listVMs(ctx context.Context, all bool) ([]exeDevVM, error) {
	args := []string{"ls", "--l", "--json"}
	if all {
		args = append(args, "--a")
	}
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
	sshArgs := []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new", "-o", "ConnectTimeout=10", strings.TrimSpace(b.configForRun().ExeDev.ControlHost)}
	sshArgs = append(sshArgs, shellQuoteArgs(args))
	return b.rt.Exec.Run(ctx, LocalCommandRequest{Name: "ssh", Args: sshArgs, Stdout: stdout, Stderr: stderr})
}

func commandExitCode(result LocalCommandResult) int {
	if result.ExitCode != 0 {
		return result.ExitCode
	}
	return 1
}

func exeDevServer(vm exeDevVM, leaseID, slug string, cfg Config, keep bool) Server {
	labels := directLeaseLabels(cfg, leaseID, slug, providerName, "", keep, time.Now().UTC())
	labels["name"] = vm.Name()
	labels["state"] = blank(vm.Status, "unknown")
	labels["work_root"] = cfg.WorkRoot
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
	if leaseID, slug := exeDevTaggedIdentity(vm); leaseID != "" {
		return leaseID, blank(slug, newLeaseSlug(leaseID)), nil
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

func exeDevTaggedIdentity(vm exeDevVM) (string, string) {
	leaseID := ""
	slug := ""
	for _, tag := range vm.Tags {
		tag = strings.TrimSpace(tag)
		if strings.HasPrefix(tag, "crabbox-lease-") {
			leaseID = strings.TrimPrefix(tag, "crabbox-lease-")
		}
		if strings.HasPrefix(tag, "crabbox-slug-") {
			slug = normalizeLeaseSlug(strings.TrimPrefix(tag, "crabbox-slug-"))
		}
	}
	return leaseID, slug
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
