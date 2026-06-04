package tart

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type backend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

type tartInstance struct {
	Name   string `json:"Name"`
	State  string `json:"State"`
	Disk   int    `json:"Disk"`
	Size   string `json:"Size"`
	Source string `json:"Source"`
}

func newBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	applyDefaults(&cfg)
	return &backend{spec: spec, cfg: cfg, rt: rt}
}

func applyDefaults(cfg *Config) {
	cfg.Provider = providerName
	if cfg.TargetOS == "" {
		cfg.TargetOS = targetMacOS
	}
	cfg.WindowsMode = ""
	cfg.SSHFallbackPorts = []string{}
	if cfg.Tart.Image == "" {
		cfg.Tart.Image = "ghcr.io/cirruslabs/macos-sequoia-base:latest"
	}
	if cfg.Tart.User == "" {
		cfg.Tart.User = "admin"
	}
	if cfg.Tart.WorkRoot == "" {
		if !core.IsDefaultWorkRoot(cfg.WorkRoot) {
			cfg.Tart.WorkRoot = cfg.WorkRoot
		} else {
			cfg.Tart.WorkRoot = "/Users/admin/crabbox"
		}
	}
	if cfg.Tart.CPUs <= 0 {
		cfg.Tart.CPUs = 4
	}
	if cfg.Tart.Memory <= 0 {
		cfg.Tart.Memory = 8192
	}
	if cfg.Tart.Disk <= 0 {
		cfg.Tart.Disk = 50
	}
	cfg.SSHUser = cfg.Tart.User
	cfg.SSHPort = sshPort
	cfg.WorkRoot = cfg.Tart.WorkRoot
	cfg.ServerType = cfg.Tart.Image
}

func (b *backend) Spec() ProviderSpec { return b.spec }

func (b *backend) configForRun() Config {
	cfg := b.cfg
	applyDefaults(&cfg)
	return cfg
}

func (b *backend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	cfg := b.configForRun()
	leaseID := newLeaseID()
	instances, err := b.listInstances(ctx)
	if err != nil {
		return LeaseTarget{}, err
	}
	claims, err := providerClaims()
	if err != nil {
		return LeaseTarget{}, err
	}
	servers := make([]Server, 0, len(instances))
	for _, inst := range instances {
		servers = append(servers, b.serverFromInstance(inst, claims[inst.Name], cfg))
	}
	slug, err := allocateDirectLeaseSlug(leaseID, req.RequestedSlug, servers)
	if err != nil {
		return LeaseTarget{}, err
	}
	keyPath, publicKey, err := ensureTestboxKeyForConfig(cfg, leaseID)
	if err != nil {
		return LeaseTarget{}, err
	}
	cleanupKey := true
	defer func() {
		if cleanupKey {
			removeStoredTestboxKey(leaseID)
		}
	}()
	cfg.SSHKey = keyPath
	name := leaseProviderName(leaseID, slug)
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s image=%s cpus=%d memory=%dMB disk=%dGB keep=%v\n", providerName, leaseID, slug, cfg.Tart.Image, cfg.Tart.CPUs, cfg.Tart.Memory, cfg.Tart.Disk, req.Keep)

	if err := b.cloneVM(ctx, cfg, name); err != nil {
		_ = b.deleteVM(context.Background(), name)
		return LeaseTarget{}, err
	}
	if err := b.configureVM(ctx, cfg, name); err != nil {
		_ = b.deleteVM(context.Background(), name)
		return LeaseTarget{}, err
	}
	if err := b.startVM(ctx, cfg, name); err != nil {
		if !req.Keep {
			_ = b.deleteVM(context.Background(), name)
		}
		return LeaseTarget{}, err
	}
	ip, err := b.waitForIP(ctx, name)
	if err != nil {
		if !req.Keep {
			_ = b.stopVM(context.Background(), name)
			_ = b.deleteVM(context.Background(), name)
		}
		return LeaseTarget{}, err
	}
	if err := b.injectSSHKey(ctx, name, publicKey); err != nil {
		if !req.Keep {
			_ = b.stopVM(context.Background(), name)
			_ = b.deleteVM(context.Background(), name)
		}
		return LeaseTarget{}, err
	}

	labels := directLeaseLabels(cfg, leaseID, slug, providerName, "", req.Keep, time.Now().UTC())
	labels["instance"] = name
	labels["image"] = cfg.Tart.Image
	labels["ssh_user"] = cfg.Tart.User
	labels["ssh_port"] = sshPort
	labels["work_root"] = cfg.Tart.WorkRoot
	claim := core.LeaseClaim{LeaseID: leaseID, Slug: slug, Provider: providerName, ProviderScope: instanceScope(name), Labels: labels}

	inst := tartInstance{Name: name, State: "running"}
	lease, err := b.prepareLease(ctx, cfg, inst, ip, claim, true)
	if err != nil {
		if !req.Keep {
			_ = b.stopVM(context.Background(), name)
			_ = b.deleteVM(context.Background(), name)
		}
		return LeaseTarget{}, err
	}
	if err := claimLeaseForRepoProviderScopePond(leaseID, slug, providerName, instanceScope(name), cfg.Pond, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
		if !req.Keep {
			_ = b.stopVM(context.Background(), name)
			_ = b.deleteVM(context.Background(), name)
		}
		return LeaseTarget{}, err
	}
	if err := updateLeaseClaimEndpoint(leaseID, lease.Server, lease.SSH); err != nil {
		if !req.Keep {
			_ = b.stopVM(context.Background(), name)
			_ = b.deleteVM(context.Background(), name)
		}
		return LeaseTarget{}, err
	}
	cleanupKey = false
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s instance=%s state=ready\n", leaseID, name)
	return lease, nil
}

func (b *backend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	cfg := b.configForRun()
	inst, ip, claim, err := b.resolveInstance(ctx, req.ID)
	if err != nil {
		return LeaseTarget{}, err
	}
	if req.ReleaseOnly {
		return LeaseTarget{Server: b.serverFromInstance(inst, claim, cfg), LeaseID: claim.LeaseID}, nil
	}
	if claim.LeaseID == "" {
		return LeaseTarget{}, exit(4, "tart instance %q has no Crabbox lease claim; use `crabbox stop --provider tart %s` to delete it or warm a new lease", inst.Name, inst.Name)
	}
	lease, err := b.prepareLease(ctx, cfg, inst, ip, claim, false)
	if err != nil {
		return LeaseTarget{}, err
	}
	if req.Repo.Root != "" {
		if err := claimLeaseForRepoProviderScopePond(claim.LeaseID, claim.Slug, providerName, instanceScope(inst.Name), cfg.Pond, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
			return LeaseTarget{}, err
		}
	}
	return lease, nil
}

func (b *backend) List(ctx context.Context, _ ListRequest) ([]LeaseView, error) {
	cfg := b.configForRun()
	instances, err := b.listInstances(ctx)
	if err != nil {
		return nil, err
	}
	claims, err := providerClaims()
	if err != nil {
		return nil, err
	}
	views := make([]LeaseView, 0, len(instances))
	for _, inst := range instances {
		claim := claims[inst.Name]
		if claim.LeaseID == "" && !strings.HasPrefix(inst.Name, "crabbox-") {
			continue
		}
		views = append(views, b.serverFromInstance(inst, claim, cfg))
	}
	return views, nil
}

func (b *backend) Doctor(ctx context.Context, req DoctorRequest) (DoctorResult, error) {
	cfg := b.configForRun()
	version, err := b.tart(ctx, []string{"--version"}, nil, nil)
	if err != nil {
		return DoctorResult{}, commandError("tart --version", version, err)
	}
	instances, err := b.listInstances(ctx)
	if err != nil {
		return DoctorResult{}, err
	}
	probe := "unchecked"
	if req.ProbeSSH {
		probe = "requires_running_lease"
	}
	msg := fmt.Sprintf("cli=ready control_plane=local inventory=ready api=list mutation=false leases=%d runtime=%s image=%s ssh_probe=%s", len(instances), firstLine(version.Stdout+version.Stderr), cfg.Tart.Image, probe)
	return DoctorResult{Provider: providerName, Message: msg}, nil
}

func (b *backend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	lease := req.Lease
	if lease.LeaseID == "" {
		lease.LeaseID = strings.TrimSpace(lease.Server.Labels["lease"])
	}
	name := strings.TrimSpace(firstNonBlank(lease.Server.CloudID, lease.Server.Labels["instance"]))
	if name == "" && lease.LeaseID != "" {
		inst, _, claim, err := b.resolveInstance(ctx, lease.LeaseID)
		if err != nil {
			return err
		}
		name = inst.Name
		if lease.LeaseID == "" {
			lease.LeaseID = claim.LeaseID
		}
	}
	if name == "" {
		return exit(2, "provider=%s release requires a tart instance name", providerName)
	}
	_ = b.stopVM(ctx, name)
	if err := b.deleteVM(ctx, name); err != nil {
		return err
	}
	if lease.LeaseID != "" {
		removeLeaseClaim(lease.LeaseID)
		removeStoredTestboxKey(lease.LeaseID)
	}
	return nil
}

func (b *backend) ReleaseLeaseMessage(lease LeaseTarget) string {
	return fmt.Sprintf("released lease=%s instance=%s", lease.LeaseID, blank(firstNonBlank(lease.Server.CloudID, lease.Server.Labels["instance"]), "-"))
}

func (b *backend) Cleanup(ctx context.Context, req core.CleanupRequest) error {
	cfg := b.configForRun()
	instances, err := b.listInstances(ctx)
	if err != nil {
		return err
	}
	claims, err := providerClaims()
	if err != nil {
		return err
	}
	live := map[string]struct{}{}
	now := time.Now().UTC()
	removed := 0
	for _, inst := range instances {
		claim := claims[inst.Name]
		if claim.LeaseID != "" {
			live[claim.LeaseID] = struct{}{}
		}
		server := b.serverFromInstance(inst, claim, cfg)
		shouldDelete, reason := shouldCleanup(server, claim, claim.LeaseID != "", now)
		if !shouldDelete {
			fmt.Fprintf(b.rt.Stderr, "skip instance name=%s reason=%s\n", inst.Name, reason)
			continue
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would remove instance name=%s lease=%s reason=%s\n", inst.Name, blank(claim.LeaseID, "-"), reason)
			continue
		}
		fmt.Fprintf(b.rt.Stdout, "remove instance name=%s lease=%s reason=%s\n", inst.Name, blank(claim.LeaseID, "-"), reason)
		_ = b.stopVM(ctx, inst.Name)
		if err := b.deleteVM(ctx, inst.Name); err != nil {
			return err
		}
		if claim.LeaseID != "" {
			removeLeaseClaim(claim.LeaseID)
			removeStoredTestboxKey(claim.LeaseID)
		}
		removed++
	}
	claimsRemoved := 0
	for _, claim := range claims {
		if claim.LeaseID == "" {
			continue
		}
		if _, ok := live[claim.LeaseID]; ok {
			continue
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would remove claim lease=%s slug=%s reason=missing instance\n", claim.LeaseID, blank(claim.Slug, "-"))
			continue
		}
		fmt.Fprintf(b.rt.Stdout, "remove claim lease=%s slug=%s reason=missing instance\n", claim.LeaseID, blank(claim.Slug, "-"))
		removeLeaseClaim(claim.LeaseID)
		removeStoredTestboxKey(claim.LeaseID)
		claimsRemoved++
	}
	if !req.DryRun {
		fmt.Fprintf(b.rt.Stdout, "%s cleanup removed=%d claims_removed=%d checked=%d\n", providerName, removed, claimsRemoved, len(instances))
	}
	return nil
}

func (b *backend) Touch(_ context.Context, req TouchRequest) (Server, error) {
	server := req.Lease.Server
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	original := server.Labels
	server.Labels = touchDirectLeaseLabels(original, b.configForRun(), req.State, time.Now().UTC())
	for _, key := range []string{"image", "instance", "ssh_user", "ssh_port", "work_root"} {
		if value := strings.TrimSpace(original[key]); value != "" {
			server.Labels[key] = value
		}
	}
	return server, nil
}

// cloneVM clones the base image to create a new VM.
func (b *backend) cloneVM(ctx context.Context, cfg Config, name string) error {
	args := []string{"clone", cfg.Tart.Image, name}
	result, err := b.tart(ctx, args, nil, b.rt.Stderr)
	if err != nil {
		return commandError("tart clone", result, err)
	}
	return nil
}

// configureVM applies CPU, memory, and disk settings to the cloned VM before boot.
func (b *backend) configureVM(ctx context.Context, cfg Config, name string) error {
	if cfg.Tart.CPUs > 0 {
		if _, err := b.tart(ctx, []string{"set", name, "--cpu", strconv.Itoa(cfg.Tart.CPUs)}, nil, b.rt.Stderr); err != nil {
			return fmt.Errorf("tart set --cpu: %w", err)
		}
	}
	if cfg.Tart.Memory > 0 {
		if _, err := b.tart(ctx, []string{"set", name, "--memory", strconv.Itoa(cfg.Tart.Memory)}, nil, b.rt.Stderr); err != nil {
			return fmt.Errorf("tart set --memory: %w", err)
		}
	}
	if cfg.Tart.Disk > 0 {
		if _, err := b.tart(ctx, []string{"set", name, "--disk-size", strconv.Itoa(cfg.Tart.Disk)}, nil, b.rt.Stderr); err != nil {
			return fmt.Errorf("tart set --disk-size: %w", err)
		}
	}
	return nil
}

// startVM starts the VM headless in the background.
func (b *backend) startVM(ctx context.Context, cfg Config, name string) error {
	args := []string{"run", name, "--no-graphics"}
	cmd := exec.CommandContext(ctx, "tart", args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return exit(2, "tart run %s: %v", name, err)
	}
	// Release the process so it survives after we return.
	go func() { _ = cmd.Wait() }()
	return nil
}

// waitForIP polls `tart ip` until the VM has an IP address.
func (b *backend) waitForIP(ctx context.Context, name string) (string, error) {
	deadline := time.After(5 * time.Minute)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", exit(2, "tart ip %s: context cancelled", name)
		case <-deadline:
			return "", exit(5, "tart ip %s: timed out waiting for IP address", name)
		case <-ticker.C:
			result, err := b.tart(ctx, []string{"ip", name}, nil, nil)
			if err != nil {
				continue
			}
			ip := strings.TrimSpace(result.Stdout)
			if ip != "" && ip != "--" {
				return ip, nil
			}
		}
	}
}

// injectSSHKey injects the public key into the VM using the default
// password-based SSH credentials (Cirrus tart images use admin:admin).
func (b *backend) injectSSHKey(ctx context.Context, name string, publicKey string) error {
	result, err := b.tart(ctx, []string{"ip", name}, nil, nil)
	if err != nil {
		return commandError("tart ip", result, err)
	}
	ip := strings.TrimSpace(result.Stdout)
	if ip == "" {
		return exit(5, "tart instance %s has no IP address for SSH key injection", name)
	}

	injectScript := fmt.Sprintf(
		`mkdir -p ~/.ssh && chmod 700 ~/.ssh && echo '%s' >> ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys`,
		strings.TrimSpace(publicKey),
	)
	injectResult, err := b.tart(ctx, []string{"exec", name, "bash", "-c", injectScript}, nil, b.rt.Stderr)
	if err != nil {
		return commandError("ssh key injection", injectResult, err)
	}
	return nil
}

// stopVM stops a running VM.
func (b *backend) stopVM(ctx context.Context, name string) error {
	result, err := b.tart(ctx, []string{"stop", name}, nil, b.rt.Stderr)
	if err != nil {
		return commandError("tart stop", result, err)
	}
	return nil
}

// deleteVM deletes a VM.
func (b *backend) deleteVM(ctx context.Context, name string) error {
	result, err := b.tart(ctx, []string{"delete", name}, nil, b.rt.Stderr)
	if err != nil {
		return commandError("tart delete", result, err)
	}
	return nil
}

func (b *backend) listInstances(ctx context.Context) ([]tartInstance, error) {
	result, err := b.tart(ctx, []string{"list", "--format", "json"}, nil, nil)
	if err != nil {
		return nil, commandError("tart list", result, err)
	}
	var instances []tartInstance
	if err := json.Unmarshal([]byte(result.Stdout), &instances); err != nil {
		return nil, exit(2, "parse tart list: %v", err)
	}
	return instances, nil
}

func (b *backend) resolveInstance(ctx context.Context, identifier string) (tartInstance, string, core.LeaseClaim, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return tartInstance{}, "", core.LeaseClaim{}, exit(2, "provider=%s requires --id <lease-id-or-slug-or-instance>", providerName)
	}
	if claim, ok, err := resolveLeaseClaimForProvider(identifier, providerName); err != nil {
		return tartInstance{}, "", core.LeaseClaim{}, err
	} else if ok {
		name := instanceNameFromClaim(claim)
		if name == "" {
			return tartInstance{}, "", core.LeaseClaim{}, exit(4, "tart lease %s has no instance name in its claim", claim.LeaseID)
		}
		ip := b.getIP(ctx, name)
		return tartInstance{Name: name, State: "running"}, ip, claim, nil
	}
	instances, err := b.listInstances(ctx)
	if err != nil {
		return tartInstance{}, "", core.LeaseClaim{}, err
	}
	claims, err := providerClaims()
	if err != nil {
		return tartInstance{}, "", core.LeaseClaim{}, err
	}
	normalized := normalizeLeaseSlug(identifier)
	for _, inst := range instances {
		claim := claims[inst.Name]
		if inst.Name == identifier || claim.LeaseID == identifier || (normalized != "" && normalizeLeaseSlug(claim.Slug) == normalized) {
			ip := b.getIP(ctx, inst.Name)
			return inst, ip, claim, nil
		}
	}
	return tartInstance{}, "", core.LeaseClaim{}, exit(4, "tart lease not found: %s", identifier)
}

func (b *backend) getIP(ctx context.Context, name string) string {
	result, err := b.tart(ctx, []string{"ip", name}, nil, nil)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(result.Stdout)
}

func (b *backend) prepareLease(ctx context.Context, cfg Config, inst tartInstance, ip string, claim core.LeaseClaim, wait bool) (LeaseTarget, error) {
	server := b.serverFromInstance(inst, claim, cfg)
	if user := strings.TrimSpace(server.Labels["ssh_user"]); user != "" {
		cfg.Tart.User = user
		cfg.SSHUser = user
	}
	if root := strings.TrimSpace(server.Labels["work_root"]); root != "" {
		cfg.Tart.WorkRoot = root
		cfg.WorkRoot = root
	}
	if ip == "" {
		return LeaseTarget{}, exit(5, "tart instance %s has no IP address", inst.Name)
	}
	if claim.LeaseID != "" {
		keyPath, err := testboxKeyPath(claim.LeaseID)
		if err == nil {
			if _, statErr := os.Stat(keyPath); statErr == nil {
				cfg.SSHKey = keyPath
			}
		}
	}
	target := sshTargetFromConfig(cfg, ip)
	target.Port = sshPort
	target.FallbackPorts = []string{}
	target.ReadyCheck = "uname -s && test -d ~"
	if wait {
		if err := waitForSSHReady(ctx, &target, b.rt.Stderr, "tart ssh", bootstrapWaitTimeout(cfg)); err != nil {
			return LeaseTarget{}, err
		}
		server.Status = "ready"
		server.Labels["state"] = "ready"
	}
	return LeaseTarget{Server: server, SSH: target, LeaseID: claim.LeaseID}, nil
}

func (b *backend) serverFromInstance(inst tartInstance, claim core.LeaseClaim, cfg Config) Server {
	labels := map[string]string{}
	for key, value := range claim.Labels {
		labels[key] = value
	}
	if labels["crabbox"] == "" {
		labels["crabbox"] = "true"
	}
	if labels["provider"] == "" {
		labels["provider"] = providerName
	}
	if labels["instance"] == "" {
		labels["instance"] = inst.Name
	}
	if labels["lease"] == "" {
		labels["lease"] = claim.LeaseID
	}
	if labels["slug"] == "" {
		labels["slug"] = claim.Slug
	}
	if labels["state"] == "" {
		labels["state"] = tartState(inst.State)
	}
	if labels["server_type"] == "" {
		labels["server_type"] = firstNonBlank(inst.Source, cfg.Tart.Image)
	}
	if labels["image"] == "" {
		labels["image"] = cfg.Tart.Image
	}
	if labels["ssh_user"] == "" {
		labels["ssh_user"] = cfg.Tart.User
	}
	if labels["ssh_port"] == "" {
		labels["ssh_port"] = sshPort
	}
	if labels["work_root"] == "" {
		labels["work_root"] = cfg.Tart.WorkRoot
	}
	status := tartState(inst.State)
	if instanceRunning(inst.State) && labels["state"] == "ready" {
		status = "ready"
	}
	server := Server{
		CloudID:  inst.Name,
		Provider: providerName,
		Name:     inst.Name,
		Status:   status,
		Labels:   labels,
	}
	server.ServerType.Name = firstNonBlank(labels["server_type"], cfg.Tart.Image)
	return server
}

func providerClaims() (map[string]core.LeaseClaim, error) {
	claims, err := listLeaseClaims()
	if err != nil {
		return nil, err
	}
	out := map[string]core.LeaseClaim{}
	for _, claim := range claims {
		if claim.Provider != providerName {
			continue
		}
		name := instanceNameFromClaim(claim)
		if name == "" {
			continue
		}
		out[name] = claim
	}
	return out, nil
}

func instanceScope(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	return "instance:" + name
}

func instanceNameFromClaim(claim core.LeaseClaim) string {
	if name := strings.TrimSpace(claim.Labels["instance"]); name != "" {
		return name
	}
	return instanceNameFromScope(claim.ProviderScope)
}

func instanceNameFromScope(scope string) string {
	return strings.TrimPrefix(strings.TrimSpace(scope), "instance:")
}

func shouldCleanup(server Server, claim core.LeaseClaim, hasClaim bool, now time.Time) (bool, string) {
	if strings.EqualFold(server.Labels["keep"], "true") {
		return false, "keep=true"
	}
	if !instanceRunning(server.Status) && server.Status != "ready" {
		return true, "instance state=" + blank(server.Status, "unknown")
	}
	if hasClaim {
		lastUsed, err := time.Parse(time.RFC3339, strings.TrimSpace(claim.LastUsedAt))
		if err != nil || lastUsed.IsZero() {
			return false, "claim active"
		}
		idle := time.Duration(claim.IdleTimeoutSeconds) * time.Second
		if idle <= 0 {
			return false, "claim active"
		}
		if now.After(lastUsed.Add(idle).Add(12 * time.Hour)) {
			return true, "claim expired"
		}
		return false, "claim active"
	}
	return false, "missing claim"
}

func (b *backend) tart(ctx context.Context, args []string, stdout, stderr io.Writer) (LocalCommandResult, error) {
	return b.rt.Exec.Run(ctx, LocalCommandRequest{
		Name:   "tart",
		Args:   args,
		Stdout: stdout,
		Stderr: stderr,
	})
}

func instanceRunning(state string) bool {
	switch tartState(state) {
	case "running", "ready":
		return true
	default:
		return false
	}
}

func tartState(state string) string {
	return strings.ToLower(strings.TrimSpace(state))
}

func commandError(action string, result LocalCommandResult, err error) error {
	code := result.ExitCode
	if code == 0 {
		code = 1
	}
	detail := strings.TrimSpace(result.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(result.Stdout)
	}
	if detail != "" {
		return exit(code, "%s failed: %v: %s", action, err, detail)
	}
	return exit(code, "%s failed: %v", action, err)
}

func firstLine(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	if idx := strings.IndexByte(value, '\n'); idx >= 0 {
		value = value[:idx]
	}
	return strings.TrimSpace(value)
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

