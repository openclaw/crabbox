package utm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type backend struct {
	cfg      core.Config
	rt       core.Runtime
	hostOS   string
	hostArch string
}

func (b *backend) Spec() core.ProviderSpec { return (Provider{}).Spec() }

var waitForSSHReady = core.WaitForSSHReady

func (b *backend) Acquire(ctx context.Context, req core.AcquireRequest) (lease core.LeaseTarget, err error) {
	if req.RequestedLeaseID != "" {
		return lease, fmt.Errorf("provider=utm does not support caller-supplied lease IDs")
	}
	if b.cfg.UTM.Acquisition == "ipsw" {
		return b.acquireMacOS(ctx, req)
	}
	if _, err := b.storageRoot(); err != nil {
		return lease, err
	}
	if !validUUID(b.cfg.UTM.Template) {
		return lease, fmt.Errorf("utm.template must be the UUID of a stopped prepared Linux QEMU template")
	}
	if b.cfg.UTM.SSHKey == "" {
		return lease, fmt.Errorf("utm.sshKey (or --ssh-key) must name the private key already authorized in the prepared template")
	}
	if info, e := os.Stat(b.cfg.UTM.SSHKey); e != nil || !info.Mode().IsRegular() {
		return lease, fmt.Errorf("UTM template SSH private key is unavailable: %s", b.cfg.UTM.SSHKey)
	}
	unlock, err := b.lock(ctx)
	if err != nil {
		return lease, err
	}
	defer unlock()
	vms, err := b.inventory(ctx)
	if err != nil {
		return lease, err
	}
	template, ok := findVM(vms, b.cfg.UTM.Template)
	if !ok {
		return lease, fmt.Errorf("UTM template UUID %s not found", b.cfg.UTM.Template)
	}
	if err = validateTemplate(template); err != nil {
		return lease, err
	}
	arch := "arm64"
	if template.Configuration.Architecture == "x86_64" {
		arch = "amd64"
	}
	if b.cfg.Architecture != "" && b.cfg.Architecture != arch {
		return lease, fmt.Errorf("UTM template architecture %s does not satisfy --arch %s", arch, b.cfg.Architecture)
	}
	records, err := b.records()
	if err != nil {
		return lease, err
	}
	servers := make([]core.Server, 0, len(records))
	for _, r := range records {
		servers = append(servers, b.project(r, vms))
	}
	id := core.NewLeaseID()
	slug, err := core.AllocateDirectLeaseSlug(id, req.RequestedSlug, servers)
	if err != nil {
		return lease, err
	}
	token, err := randomToken()
	if err != nil {
		return lease, err
	}
	mac, port, err := allocateNetwork(records, vms)
	if err != nil {
		return lease, err
	}
	r := record{Version: 1, LeaseID: id, Slug: slug, AppPath: b.cfg.UTM.AppPath, SourceID: template.ID, OwnedName: "crabbox-utm-" + token, Token: token, Architecture: template.Configuration.Architecture, MAC: mac, Port: port, User: b.cfg.UTM.User, SSHKey: b.cfg.UTM.SSHKey, WorkRoot: b.cfg.UTM.WorkRoot, State: "cloning", Keep: req.Keep, Created: core.ClockNow(b.rt.Clock).UTC()}
	r.Labels = core.DirectLeaseLabels(b.cfg, id, slug, "utm", "", req.Keep, r.Created)
	r.Labels["work_root"] = r.WorkRoot
	r.Labels["ssh_user"] = r.User
	r.Labels["ssh_port"] = strconv.Itoa(port)
	r.Labels["utm_source"] = r.SourceID
	r.Labels["utm_mac"] = r.MAC
	r.Labels["architecture"] = arch
	for _, v := range vms {
		r.Before = append(r.Before, v.ID)
		if v.Name == r.OwnedName {
			return lease, fmt.Errorf("UTM generated clone name already exists")
		}
	}
	if err = saveRecord(r); err != nil {
		return lease, err
	}
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=utm lease=%s slug=%s source=%s backend=qemu ssh=127.0.0.1:%d\n", id, slug, template.ID, port)
	journalRetired := false
	defer func() {
		if err != nil && !journalRetired {
			r.State = "recovery"
			r.Labels["recovery"] = "acquisition incomplete"
			r.Labels["state"] = "recovery"
			err = errors.Join(err, saveRecord(r))
			fmt.Fprintf(b.rt.Stderr, "UTM acquisition incomplete; ownership journal retained lease=%s vm=%s; retry disposition with crabbox stop --provider utm %s\n", id, core.Blank(r.VMID, "unresolved"), id)
		}
	}()
	// Apple events can outlive a canceled utmctl client. Finish a bounded clone and
	// identity readback independently of caller cancellation, then honor it before boot.
	cloneCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
	cloneErr := b.control(cloneCtx, "clone", template.ID, r.OwnedName)
	after, inventoryErr := b.inventory(cloneCtx)
	if inventoryErr == nil {
		inventoryErr = b.reconcile(cloneCtx, &r, after)
	}
	cancel()
	if cloneErr != nil || inventoryErr != nil {
		return lease, errors.Join(cloneErr, inventoryErr)
	}
	lease = b.target(r, after)
	if req.OnAcquired != nil {
		if err = req.OnAcquired(lease); err != nil {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
			cleanupErr := b.releaseRecord(cleanupCtx, &r, "", nil)
			cleanupCancel()
			journalRetired = cleanupErr == nil
			return lease, errors.Join(err, cleanupErr)
		}
	}
	if err = ctx.Err(); err != nil {
		return lease, err
	}
	if err = b.configureClone(ctx, &r); err != nil {
		return lease, err
	}
	after, err = b.inventory(ctx)
	if err != nil {
		return lease, err
	}
	v, ok := findVM(after, r.VMID)
	if !ok {
		return lease, fmt.Errorf("UTM clone vanished before boot")
	}
	if err = validateClone(v, r); err != nil {
		return lease, err
	}
	r.State = "configured"
	r.Configured = true
	if err = saveRecord(r); err != nil {
		return lease, err
	}
	if err = b.control(ctx, "start", r.VMID); err != nil {
		return lease, err
	}
	// Retain the host allocation fence through start/readiness. Another Crabbox
	// allocation cannot reuse this port; UTM's actual mapping is read back first.
	lease = b.target(r, after)
	startup, cancelStartup := context.WithTimeout(ctx, b.cfg.UTM.StartupTimeout)
	defer cancelStartup()
	if err = waitForSSHReady(startup, &lease.SSH, b.rt.Stderr, "UTM authenticated guest identity", b.cfg.UTM.StartupTimeout); err != nil {
		return lease, fmt.Errorf("UTM guest SSH readiness failed; ensure template SSH key/user, DHCP, Linux tools and port 22 service are prepared: %w", err)
	}
	r.State = "ready"
	r.Labels = core.TouchDirectLeaseLabels(r.Labels, b.cfg, "ready", core.ClockNow(b.rt.Clock).UTC())
	if err = saveRecord(r); err != nil {
		return lease, err
	}
	lease = b.target(r, after)
	lease.Server.Status = "ready"
	claim, err := b.claimTarget(lease, slug, req.Repo.Root, req.Reclaim, core.LeaseClaim{}, false)
	if err != nil {
		return lease, err
	}
	core.SetServerLeaseClaimSnapshot(&lease.Server, claim, true)
	return lease, nil
}
func (b *backend) project(r record, vms []vm) core.Server {
	labels := shared.CloneLabels(r.Labels)
	status := r.State
	name := r.OwnedName
	if v, ok := findVM(vms, r.VMID); ok {
		name = v.Name
		if r.State == "ready" {
			status = v.Status
			if status == "started" {
				status = "ready"
			}
		}
	} else if r.VMID != "" {
		status = "missing"
	}
	if claim, exists, err := core.ReadLeaseClaimWithPresence(r.LeaseID); err == nil && exists && claim.Provider == "utm" && claim.CloudID == r.VMID {
		labels = shared.CloneLabels(claim.Labels)
	}
	if r.isMacOS() {
		labels["utm_installation"] = fmt.Sprint(r.InstallComplete)
		labels["utm_first_boot_submitted"] = fmt.Sprint(r.FirstBootSubmitted)
		labels["utm_account_verified"] = fmt.Sprint(r.AccountComplete)
		labels["utm_ssh_verified"] = fmt.Sprint(r.SSHReady)
	}
	s := core.Server{Provider: "utm", CloudID: r.VMID, Name: name, Status: status, Labels: labels}
	s.PublicNet.IPv4.IP = "127.0.0.1"
	if r.isMacOS() {
		s.PublicNet.IPv4.IP = r.Address
	}
	return s
}
func (b *backend) target(r record, vms []vm) core.LeaseTarget {
	target := core.SSHTargetFromConfig(b.cfg, "127.0.0.1")
	target.User = r.User
	target.Key = r.SSHKey
	target.Port = strconv.Itoa(r.Port)
	target.FallbackPorts = nil
	target.TargetOS = core.TargetLinux
	target.NoControlMaster = true
	machine := "aarch64"
	if r.Architecture == "x86_64" {
		machine = "x86_64"
	}
	// A clone retains template host keys, so authentication alone cannot establish
	// which guest owns a forwarded port. Verify its newly assigned NIC identity.
	target.ReadyCheck = fmt.Sprintf(`test "$(uname -s)" = Linux && test "$(uname -m)" = %s && command -v bash >/dev/null && command -v rsync >/dev/null && command -v tar >/dev/null && (found=0; for f in /sys/class/net/*/address; do if test "$(cat "$f")" = %s; then found=1; fi; done; test "$found" = 1)`, core.ShellQuote(machine), core.ShellQuote(r.MAC))
	if r.isMacOS() {
		dir, _ := macOSKeyDir(r)
		target = core.SSHTarget{Host: r.Address, User: r.User, Key: r.SSHKey, Port: "22", TargetOS: core.TargetMacOS,
			SSHHostKey: r.SSHHostKey, HostKeyAlias: "crabbox-utm-" + r.LeaseID, KnownHostsFile: filepath.Join(dir, "known_hosts"),
			AuthoritativeKnownHosts: true, NoControlMaster: true, ReadyCheck: macOSReadyCheck(r)}
		if r.SSHHostKey != "" {
			target.PrepareConnection = func(context.Context) error { return b.pinMacOSHostKey(r) }
		}

	}
	return core.LeaseTarget{Server: b.project(r, vms), SSH: target, LeaseID: r.LeaseID}
}
func (b *backend) Resolve(ctx context.Context, req core.ResolveRequest) (core.LeaseTarget, error) {
	unlock, err := b.lock(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	defer unlock()
	r, err := b.lookup(strings.TrimSpace(req.ID))
	if err != nil {
		return core.LeaseTarget{}, err
	}
	vms, err := b.inventory(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if r.VMID == "" && !req.NoLocalStateMutations {
		if err = b.reconcile(ctx, &r, vms); err != nil {
			return core.LeaseTarget{}, err
		}
	}
	lease := b.target(r, vms)
	if err = core.ValidateLeaseTargetProviderIdentity(lease, req.ExpectedProviderIdentity); err != nil {
		return lease, err
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(r.LeaseID)
	if err != nil {
		return lease, err
	}
	core.SetServerLeaseClaimSnapshot(&lease.Server, claim, exists)
	if req.ReleaseOnly || req.StatusOnly || req.NoLocalStateMutations {
		return lease, nil
	}
	if r.State != "ready" {
		return lease, fmt.Errorf("UTM lease %s acquisition is incomplete (state=%s); release it and acquire again", r.LeaseID, r.State)
	}
	if !exists || claim.Provider != "utm" || claim.CloudID != r.VMID || claim.ProviderScope != (Provider{}).ClaimScope(b.cfg) {
		return lease, fmt.Errorf("UTM retained lease requires its current matching claim; release it and acquire again")
	}
	if req.Repo.Root != "" {
		if err = core.CheckLeaseClaimRepositoryOwner(r.LeaseID, claim, req.Repo.Root, req.Reclaim); err != nil {
			return lease, err
		}
	}
	v, ok := findVM(vms, r.VMID)
	if !ok {
		return lease, fmt.Errorf("UTM owned clone %s is absent", r.VMID)
	}
	if err = validateClone(v, r); err != nil {
		return lease, err
	}
	if v.Status != "started" {
		return lease, fmt.Errorf("UTM owned clone %s is not running (status=%s)", r.VMID, v.Status)
	}
	if err = shared.AuthorizeClaimActivity(claim); err != nil {
		return lease, err
	}
	// Readiness executes guest commands and may repair a local host-key pin.
	// Keep the resolved claim unchanged through all continuing-access effects.
	err = core.WithLeaseClaimUnchangedContext(ctx, r.LeaseID, claim, func() error {
		if r.isMacOS() {
			if err := b.validateOwnedMacOS(ctx, r, vms, false); err != nil {
				return err
			}
			if !r.SSHReady {
				return fmt.Errorf("UTM macOS continuing access is incomplete; release and acquire again")
			}
			if err := b.refreshMacOSAddress(ctx, &r); err != nil {
				return err
			}
			lease = b.target(r, vms)
		}
		return waitForSSHReady(ctx, &lease.SSH, b.rt.Stderr, "UTM retained guest identity", b.cfg.UTM.StartupTimeout)
	})
	if err != nil {
		return lease, err
	}
	updated, err := b.claimTarget(lease, r.Slug, req.Repo.Root, req.Reclaim, claim, exists)
	if err != nil {
		return lease, err
	}
	core.SetServerLeaseClaimSnapshot(&lease.Server, updated, true)
	return lease, nil
}
func (b *backend) List(ctx context.Context, _ core.ListRequest) ([]core.LeaseView, error) {
	records, err := b.records()
	if err != nil {
		return nil, err
	}
	vms, err := b.inventory(ctx)
	if err != nil {
		return nil, err
	}
	out := []core.LeaseView{}
	for _, r := range records {
		out = append(out, b.project(r, vms))
	}
	return out, nil
}
func (b *backend) Touch(ctx context.Context, req core.TouchRequest) (core.Server, error) {
	unlock, err := b.lock(ctx)
	if err != nil {
		return core.Server{}, err
	}
	defer unlock()
	r, err := b.lookup(req.Lease.LeaseID)
	if err != nil {
		return core.Server{}, err
	}
	updated, err := shared.CommitClaimTouch(ctx, req, shared.ClaimTouchPolicy{Provider: "utm", Authorize: func(_ context.Context, lease core.LeaseTarget, claim core.LeaseClaim) error {
		if r.State != "ready" || lease.Server.CloudID != r.VMID || claim.CloudID != r.VMID || claim.Provider != "utm" || claim.LeaseID != r.LeaseID || claim.ProviderScope != (Provider{}).ClaimScope(b.cfg) {
			return fmt.Errorf("UTM touch ownership mismatch or incomplete acquisition")
		}
		return shared.AuthorizeClaimActivity(claim)
	}, Prepare: func(claim core.LeaseClaim) (map[string]string, time.Time) {
		now := core.ClockNow(b.rt.Clock).UTC()
		return core.TouchDirectLeaseLabelsWithIdleTimeoutOverride(shared.ClaimLifecycleLabels(claim), b.cfg, req.State, now, req.IdleTimeoutOverride), now
	}})
	if err != nil {
		return core.Server{}, err
	}
	r.Labels = shared.CloneLabels(updated.Labels)
	if err = saveRecord(r); err != nil {
		return core.Server{}, err
	}
	s := req.Lease.Server
	s.Labels = r.Labels
	core.SetServerLeaseClaimSnapshot(&s, updated, true)
	return s, nil
}

// Warmup without repository context still needs an exact local endpoint claim.
func (b *backend) claimTarget(lease core.LeaseTarget, slug, repoRoot string, reclaim bool, expected core.LeaseClaim, exists bool) (core.LeaseClaim, error) {
	if repoRoot == "" {
		return core.ClaimLeaseTargetForConfigScopeIfUnchanged(lease.LeaseID, slug, b.cfg, (Provider{}).ClaimScope(b.cfg), lease.Server, lease.SSH, b.cfg.IdleTimeout, expected, exists)
	}
	return core.ClaimLeaseTargetForRepoConfigScopeIfUnchangedDurable(lease.LeaseID, slug, b.cfg, (Provider{}).ClaimScope(b.cfg), lease.Server, lease.SSH, repoRoot, b.cfg.IdleTimeout, reclaim, expected, exists)
}
