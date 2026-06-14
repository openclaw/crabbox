package firecracker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/containernetworking/cni/libcni"
	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type Config = core.Config
type Runtime = core.Runtime
type ProviderSpec = core.ProviderSpec
type Backend = core.Backend
type DoctorRequest = core.DoctorRequest
type DoctorResult = core.DoctorResult
type DoctorCheck = core.DoctorCheck
type AcquireRequest = core.AcquireRequest
type ResolveRequest = core.ResolveRequest
type ListRequest = core.ListRequest
type LeaseView = core.LeaseView
type ReleaseLeaseRequest = core.ReleaseLeaseRequest
type TouchRequest = core.TouchRequest
type CleanupRequest = core.CleanupRequest
type LeaseTarget = core.LeaseTarget
type LeaseClaim = core.LeaseClaim
type Server = core.Server
type SSHTarget = core.SSHTarget

const (
	providerName           = "firecracker"
	firecrackerNetworkCNI  = "cni"
	firecrackerSSHPort     = "22"
	firecrackerServerClass = "microvm"
)

var (
	firecrackerHostGOOS = runtime.GOOS
	firecrackerLookPath = exec.LookPath
	firecrackerStat     = os.Stat
	firecrackerOpenKVM  = openKVMDevice
	firecrackerLoadCNI  = libcni.LoadConfList
	ensureTestboxKey    = core.EnsureTestboxKeyForConfig
	removeTestboxKey    = core.RemoveStoredTestboxKey
)

type backend struct {
	spec           ProviderSpec
	cfg            Config
	rt             Runtime
	stateRoot      func() (string, error)
	machines       machineFactory
	processes      processManager
	waitForSSH     func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error
	cleanupNetwork func(context.Context, leaseStateRecord) error
}

func newBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	applyDefaults(&cfg)
	b := &backend{
		spec:           spec,
		cfg:            cfg,
		rt:             rt,
		machines:       sdkMachineFactory{LogWriter: rt.Stderr},
		processes:      localProcessManager{},
		waitForSSH:     core.WaitForSSHReady,
		cleanupNetwork: cleanupFirecrackerNetwork,
	}
	b.stateRoot = b.firecrackerStateRoot
	return b
}

func applyDefaults(cfg *Config) {
	if cfg == nil {
		return
	}
	cfg.Provider = providerName
	base := core.BaseConfig()
	if cfg.TargetOS == "" {
		cfg.TargetOS = core.TargetLinux
	}
	if cfg.TargetOS == core.TargetLinux {
		cfg.WindowsMode = ""
	}
	if user := strings.TrimSpace(cfg.Firecracker.User); user != "" &&
		(cfg.SSHUser == "" || cfg.SSHUser == base.SSHUser || user != strings.TrimSpace(base.Firecracker.User)) {
		cfg.SSHUser = user
	}
	if workRoot := strings.TrimSpace(cfg.Firecracker.WorkRoot); workRoot != "" &&
		(core.IsDefaultWorkRoot(cfg.WorkRoot) || workRoot != strings.TrimSpace(base.Firecracker.WorkRoot)) {
		cfg.WorkRoot = workRoot
	}
	currentSSHPort := strings.TrimSpace(cfg.SSHPort)
	if currentSSHPort == "" || currentSSHPort == strings.TrimSpace(base.SSHPort) {
		cfg.SSHPort = firecrackerSSHPort
	} else {
		cfg.SSHPort = currentSSHPort
	}
	cfg.SSHFallbackPorts = nil
	if !cfg.ServerTypeExplicit && strings.TrimSpace(cfg.ServerType) == "" {
		cfg.ServerType = firecrackerServerTypeForConfig(*cfg)
	}
}

func firecrackerServerTypeForConfig(_ Config) string {
	return firecrackerServerClass
}

func normalizeFirecrackerNetwork(value string) string {
	mode := strings.ToLower(strings.TrimSpace(value))
	if mode == "" {
		return firecrackerNetworkCNI
	}
	return mode
}

func validateConfig(cfg Config) error {
	applyDefaults(&cfg)
	if cfg.TargetOS != "" && cfg.TargetOS != core.TargetLinux {
		return core.Exit(2, "provider=firecracker supports target=linux only")
	}
	if cfg.Tailscale.Enabled || cfg.Network == core.NetworkTailscale {
		return core.Exit(2, "provider=firecracker does not support tailscale-managed networking")
	}
	if strings.TrimSpace(cfg.Firecracker.User) == "" {
		return core.Exit(2, "provider=firecracker requires firecracker.user")
	}
	workRoot := strings.TrimSpace(cfg.Firecracker.WorkRoot)
	if workRoot == "" || !strings.HasPrefix(workRoot, "/") {
		return core.Exit(2, "provider=firecracker requires firecracker.workRoot to be an absolute POSIX path")
	}
	if cfg.Firecracker.CPUs <= 0 {
		return core.Exit(2, "provider=firecracker requires firecracker.cpus > 0")
	}
	if cfg.Firecracker.MemoryMiB <= 0 {
		return core.Exit(2, "provider=firecracker requires firecracker.memoryMiB > 0")
	}
	if cfg.Firecracker.DiskMiB <= 0 {
		return core.Exit(2, "provider=firecracker requires firecracker.diskMiB > 0")
	}
	if mode := normalizeFirecrackerNetwork(cfg.Firecracker.Network); mode != firecrackerNetworkCNI {
		return core.Exit(2, "provider=firecracker supports firecracker.network=%s only", firecrackerNetworkCNI)
	}
	return nil
}

func (b *backend) Spec() ProviderSpec { return b.spec }

func (b *backend) RebindResolvedLeaseTarget(target *LeaseTarget, leaseID string) error {
	core.UseStoredTestboxKey(&target.SSH, leaseID)
	return nil
}

func (b *backend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	return shared.AcquireAttemptsRetry(b.rt, req.Keep, func() (LeaseTarget, error) {
		return b.acquireOnce(ctx, req)
	})
}

func (b *backend) acquireOnce(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	cfg := b.configForRun()
	if err := requireLifecycleHost(); err != nil {
		return LeaseTarget{}, err
	}
	if jailer := strings.TrimSpace(cfg.Firecracker.Jailer); jailer != "" {
		return LeaseTarget{}, exit(2, "provider=firecracker does not support firecracker.jailer yet")
	}

	servers, err := b.listServers(cfg)
	if err != nil {
		return LeaseTarget{}, err
	}
	leaseID := core.NewLeaseID()
	slug, err := core.AllocateDirectLeaseSlug(leaseID, req.RequestedSlug, servers)
	if err != nil {
		return LeaseTarget{}, err
	}
	name := core.LeaseProviderName(leaseID, slug)
	now := b.currentTime().UTC()
	paths, err := b.ensureLeaseDir(leaseID)
	if err != nil {
		return LeaseTarget{}, err
	}

	keyPath, publicKey, err := ensureTestboxKey(cfg, leaseID)
	if err != nil {
		return LeaseTarget{}, err
	}
	cleanupKey := true
	defer func() {
		if cleanupKey {
			removeTestboxKey(leaseID)
		}
	}()
	cfg.SSHKey = keyPath
	payload, err := buildCloudInitPayload(cfg, leaseID, slug, publicKey)
	if err != nil {
		_ = b.removeStateDir(leaseStateRecord{LeaseID: leaseID})
		return LeaseTarget{}, err
	}

	if err := prepareWritableRootFS(cfg.Firecracker.RootFS, paths.RootFS, cfg.Firecracker.DiskMiB); err != nil {
		_ = b.removeStateDir(leaseStateRecord{LeaseID: leaseID})
		return LeaseTarget{}, err
	}
	if err := writeCloudInitDrive(paths.CloudInit, payload); err != nil {
		_ = b.removeStateDir(leaseStateRecord{LeaseID: leaseID})
		return LeaseTarget{}, err
	}

	labels := core.TouchDirectLeaseLabels(core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", req.Keep, now), cfg, "provisioning", now)
	labels["instance"] = name
	labels["vmid"] = name
	labels["ssh_user"] = cfg.SSHUser
	labels["ssh_port"] = cfg.SSHPort
	labels["work_root"] = cfg.WorkRoot
	labels["network"] = firecrackerNetworkCNI
	labels["cni_network"] = cfg.Firecracker.CNINetwork

	record := leaseStateRecord{
		LeaseID:         leaseID,
		Slug:            slug,
		Name:            name,
		VMID:            name,
		StateDir:        paths.Dir,
		SocketPath:      paths.Socket,
		LogPath:         paths.Log,
		RootFSPath:      paths.RootFS,
		CloudInitPath:   paths.CloudInit,
		NetNSPath:       paths.NetNS,
		CNICacheDir:     paths.CNICache,
		SSHUser:         cfg.SSHUser,
		SSHPort:         cfg.SSHPort,
		BinaryPath:      cfg.Firecracker.Binary,
		KernelPath:      cfg.Firecracker.Kernel,
		SourceRootFS:    cfg.Firecracker.RootFS,
		CNINetwork:      cfg.Firecracker.CNINetwork,
		CNIConfDir:      cfg.Firecracker.CNIConfDir,
		CNIBinDir:       cfg.Firecracker.CNIBinDir,
		DeleteOnRelease: cfg.Firecracker.DeleteOnRelease,
		Labels:          cloneLabels(labels),
		CreatedAt:       now.Format(time.RFC3339Nano),
		UpdatedAt:       now.Format(time.RFC3339Nano),
	}
	if err := b.writeStateRecord(record); err != nil {
		_ = b.removeStateDir(record)
		return LeaseTarget{}, err
	}

	vm, err := b.machines.New(ctx, machineLaunchConfig{
		BinaryPath:    cfg.Firecracker.Binary,
		SocketPath:    record.SocketPath,
		LogPath:       record.LogPath,
		KernelPath:    cfg.Firecracker.Kernel,
		KernelArgs:    firecrackerDefaultKernelArgs,
		RootFSPath:    record.RootFSPath,
		CloudInitPath: record.CloudInitPath,
		VMID:          record.VMID,
		NetNSPath:     record.NetNSPath,
		CNINetwork:    record.CNINetwork,
		CNIConfDir:    record.CNIConfDir,
		CNIBinDir:     record.CNIBinDir,
		CNICacheDir:   record.CNICacheDir,
		CPUs:          cfg.Firecracker.CPUs,
		MemoryMiB:     cfg.Firecracker.MemoryMiB,
	})
	if err != nil {
		_ = b.removeStateDir(record)
		return LeaseTarget{}, err
	}
	rollback := func(cause error) error {
		return b.rollbackAcquire(record, vm, cause)
	}

	startCtx := ctx
	cancelStart := func() {}
	if cfg.Firecracker.LaunchTimeout > 0 {
		startCtx, cancelStart = context.WithTimeout(ctx, cfg.Firecracker.LaunchTimeout)
	}
	defer cancelStart()
	if err := vm.Start(startCtx); err != nil {
		return LeaseTarget{}, rollback(err)
	}

	identity, err := b.processes.Capture(vm.PID())
	if err != nil {
		return LeaseTarget{}, rollback(err)
	}
	record.PID = identity.PID
	record.ProcessStarted = identity.Started
	record.BootID = identity.BootID
	record.Labels = core.TouchDirectLeaseLabels(record.Labels, cfg, "running", b.currentTime().UTC())
	record.UpdatedAt = b.currentTime().UTC().Format(time.RFC3339Nano)
	if err := b.writeStateRecord(record); err != nil {
		return LeaseTarget{}, rollback(err)
	}

	guestIP := vm.GuestIP()
	if strings.TrimSpace(guestIP) == "" {
		return LeaseTarget{}, rollback(exit(5, "firecracker lease %s did not report a guest IP from CNI", leaseID))
	}
	record.GuestIP = guestIP
	target, err := b.targetFromRecord(cfg, record)
	if err != nil {
		return LeaseTarget{}, rollback(err)
	}
	if err := b.waitForSSH(ctx, &target, b.rt.Stderr, "bootstrap", cfg.Firecracker.LaunchTimeout); err != nil {
		return LeaseTarget{}, rollback(err)
	}

	record.Labels = core.TouchDirectLeaseLabels(record.Labels, cfg, "ready", b.currentTime().UTC())
	record.UpdatedAt = b.currentTime().UTC().Format(time.RFC3339Nano)
	if err := b.writeStateRecord(record); err != nil {
		return LeaseTarget{}, rollback(err)
	}
	server := b.serverFromRecord(cfg, record, true)
	if err := b.claimLeaseTarget(cfg, leaseID, slug, req.Repo.Root, req.Reclaim, server, target); err != nil {
		return LeaseTarget{}, rollback(err)
	}

	cleanupKey = false
	fmt.Fprintf(b.rt.Stderr, "provisioned provider=%s lease=%s vmid=%s ip=%s\n", providerName, leaseID, name, guestIP)
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *backend) Resolve(_ context.Context, req ResolveRequest) (LeaseTarget, error) {
	cfg := b.configForRun()
	record, found, err := b.recordByIdentifier(cfg, req.ID)
	if err != nil {
		return LeaseTarget{}, err
	}
	if !found {
		claim, ok, err := core.ResolveLeaseClaimForProvider(req.ID, providerName)
		if err != nil {
			return LeaseTarget{}, err
		}
		if !ok {
			return LeaseTarget{}, exit(4, "lease/server not found: %s", req.ID)
		}
		if req.ReleaseOnly {
			return LeaseTarget{Server: serverFromClaim(cfg, claim), LeaseID: claim.LeaseID}, nil
		}
		return LeaseTarget{}, exit(4, "firecracker lease %q has a stale local claim but no local state; run `crabbox cleanup --provider firecracker`", req.ID)
	}

	running := b.processes.Matches(record.processIdentity())
	server := b.serverFromRecord(cfg, record, running)
	if req.ReleaseOnly {
		return LeaseTarget{Server: server, LeaseID: record.LeaseID}, nil
	}
	if !running {
		if req.StatusOnly {
			return LeaseTarget{Server: server, LeaseID: record.LeaseID}, nil
		}
		return LeaseTarget{}, exit(5, "firecracker lease %s is not running; use `crabbox stop --provider firecracker %s` or `crabbox cleanup --provider firecracker`", blankIfEmpty(record.Name), req.ID)
	}
	if strings.TrimSpace(record.GuestIP) == "" && req.StatusOnly {
		return LeaseTarget{Server: server, LeaseID: record.LeaseID}, nil
	}
	target, err := b.targetFromRecord(cfg, record)
	if err != nil {
		return LeaseTarget{}, err
	}
	lease := LeaseTarget{Server: server, SSH: target, LeaseID: record.LeaseID}
	if req.Repo.Root != "" {
		if err := b.claimLeaseTarget(cfg, record.LeaseID, record.Slug, req.Repo.Root, req.Reclaim, server, target); err != nil {
			return LeaseTarget{}, err
		}
	}
	return lease, nil
}

func (b *backend) List(_ context.Context, _ ListRequest) ([]LeaseView, error) {
	cfg := b.configForRun()
	records, err := b.listStateRecords()
	if err != nil {
		return nil, err
	}
	views := make([]LeaseView, 0, len(records))
	for _, record := range records {
		views = append(views, b.serverFromRecord(cfg, record, b.processes.Matches(record.processIdentity())))
	}
	return views, nil
}

func (b *backend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	cfg := b.configForRun()
	record, found, err := b.releaseRecordForLease(cfg, req.Lease)
	if err != nil {
		return err
	}
	leaseID := firstNonBlank(req.Lease.LeaseID, req.Lease.Server.Labels["lease"])
	if !found {
		if strings.TrimSpace(leaseID) != "" {
			core.RemoveLeaseClaim(leaseID)
			removeTestboxKey(leaseID)
			return nil
		}
		return exit(2, "provider=%s release requires a lease id or firecracker instance name", providerName)
	}
	if err := b.releaseStateRecord(ctx, cfg, record, record.DeleteOnRelease); err != nil {
		return err
	}
	core.RemoveLeaseClaim(record.LeaseID)
	removeTestboxKey(record.LeaseID)
	return nil
}

func (b *backend) Cleanup(ctx context.Context, req CleanupRequest) error {
	cfg := b.configForRun()
	records, err := b.listStateRecords()
	if err != nil {
		return err
	}
	claims, err := firecrackerClaims()
	if err != nil {
		return err
	}
	stateLeaseIDs := make(map[string]struct{}, len(records))
	for _, record := range records {
		stateLeaseIDs[record.LeaseID] = struct{}{}
		server := b.serverFromRecord(cfg, record, b.processes.Matches(record.processIdentity()))
		shouldDelete, reason := b.shouldCleanupRecord(server, record)
		if !shouldDelete {
			fmt.Fprintf(b.rt.Stderr, "skip firecracker lease=%s vmid=%s reason=%s\n", record.LeaseID, blankIfEmpty(record.VMID), reason)
			continue
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would remove firecracker lease=%s vmid=%s reason=%s\n", record.LeaseID, blankIfEmpty(record.VMID), reason)
			continue
		}
		fmt.Fprintf(b.rt.Stdout, "remove firecracker lease=%s vmid=%s reason=%s\n", record.LeaseID, blankIfEmpty(record.VMID), reason)
		if err := b.releaseStateRecord(ctx, cfg, record, true); err != nil {
			return err
		}
		core.RemoveLeaseClaim(record.LeaseID)
		removeTestboxKey(record.LeaseID)
	}
	for leaseID, claim := range claims {
		if _, ok := stateLeaseIDs[leaseID]; ok {
			continue
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would remove firecracker claim lease=%s slug=%s reason=missing_state\n", leaseID, blankIfEmpty(claim.Slug))
			continue
		}
		fmt.Fprintf(b.rt.Stdout, "remove firecracker claim lease=%s slug=%s reason=missing_state\n", leaseID, blankIfEmpty(claim.Slug))
		core.RemoveLeaseClaim(leaseID)
		removeTestboxKey(leaseID)
	}
	return nil
}

func (b *backend) Touch(_ context.Context, req TouchRequest) (Server, error) {
	cfg := b.configForRun()
	server := req.Lease.Server
	if server.Provider == "" {
		server.Provider = providerName
	}
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	state := strings.TrimSpace(req.State)
	if state == "" {
		state = "touched"
	}
	server.Status = state
	server.Labels = core.TouchDirectLeaseLabels(server.Labels, cfg, state, b.currentTime().UTC())

	leaseID := firstNonBlank(req.Lease.LeaseID, server.Labels["lease"])
	if strings.TrimSpace(leaseID) == "" {
		return server, nil
	}

	record, err := b.readStateRecord(leaseID)
	if err == nil {
		record.Labels = cloneLabels(server.Labels)
		record.UpdatedAt = b.currentTime().UTC().Format(time.RFC3339Nano)
		if writeErr := b.writeStateRecord(record); writeErr != nil {
			fmt.Fprintf(b.rt.Stderr, "warning: touch firecracker state=%s lease=%s: %v\n", state, leaseID, writeErr)
		}
	}
	if claim, ok, readErr := core.ReadLeaseClaimWithPresence(leaseID); readErr == nil && ok {
		if _, updateErr := core.UpdateLeaseClaimLabelsIfUnchanged(leaseID, claim, server.Labels); updateErr != nil {
			fmt.Fprintf(b.rt.Stderr, "warning: touch firecracker claim=%s state=%s: %v\n", leaseID, state, updateErr)
		}
	}
	return server, nil
}

func (b *backend) Doctor(_ context.Context, _ DoctorRequest) (DoctorResult, error) {
	cfg := b.cfg
	applyDefaults(&cfg)
	checks := []DoctorCheck{
		doctorHostCheck(),
		doctorKVMCheck(),
		doctorExecutableCheck("binary", "firecracker.binary", cfg.Firecracker.Binary),
		doctorJailerCheck(cfg.Firecracker.Jailer),
		doctorFileCheck("kernel", "firecracker.kernel", cfg.Firecracker.Kernel),
		doctorFileCheck("rootfs", "firecracker.rootfs", cfg.Firecracker.RootFS),
		doctorNetworkCheck(cfg),
	}
	return DoctorResult{
		Provider: providerName,
		Status:   aggregateDoctorStatus(checks),
		Message:  summarizeDoctorChecks(checks),
		Checks:   checks,
	}, nil
}

func (b *backend) configForRun() Config {
	cfg := b.cfg
	applyDefaults(&cfg)
	return cfg
}

func (b *backend) currentTime() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now()
	}
	return time.Now()
}

func (b *backend) claimLeaseTarget(cfg Config, leaseID, slug, repoRoot string, reclaim bool, server Server, target SSHTarget) error {
	if strings.TrimSpace(repoRoot) == "" {
		return core.ClaimLeaseTargetForConfig(leaseID, slug, cfg, server, target, cfg.IdleTimeout)
	}
	return core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, target, repoRoot, cfg.IdleTimeout, reclaim)
}

func (b *backend) listServers(cfg Config) ([]Server, error) {
	records, err := b.listStateRecords()
	if err != nil {
		return nil, err
	}
	servers := make([]Server, 0, len(records))
	for _, record := range records {
		servers = append(servers, b.serverFromRecord(cfg, record, b.processes.Matches(record.processIdentity())))
	}
	return servers, nil
}

func (b *backend) recordByIdentifier(cfg Config, identifier string) (leaseStateRecord, bool, error) {
	records, err := b.listStateRecords()
	if err != nil {
		return leaseStateRecord{}, false, err
	}
	servers := make([]Server, 0, len(records))
	byLeaseID := make(map[string]leaseStateRecord, len(records))
	byName := make(map[string]leaseStateRecord, len(records))
	for _, record := range records {
		servers = append(servers, b.serverFromRecord(cfg, record, b.processes.Matches(record.processIdentity())))
		byLeaseID[record.LeaseID] = record
		byName[record.Name] = record
	}
	server, leaseID, err := core.FindServerByAlias(servers, identifier)
	if err != nil {
		return leaseStateRecord{}, false, err
	}
	if strings.TrimSpace(leaseID) != "" {
		record, ok := byLeaseID[leaseID]
		return record, ok, nil
	}
	if strings.TrimSpace(server.Name) != "" {
		record, ok := byName[server.Name]
		return record, ok, nil
	}
	return leaseStateRecord{}, false, nil
}

func (b *backend) targetFromRecord(cfg Config, record leaseStateRecord) (SSHTarget, error) {
	if strings.TrimSpace(record.GuestIP) == "" {
		return SSHTarget{}, exit(5, "firecracker lease %s has no guest IP", record.LeaseID)
	}
	cfg.SSHUser = firstNonBlank(record.SSHUser, cfg.SSHUser)
	cfg.SSHPort = firstNonBlank(record.SSHPort, cfg.SSHPort)
	target := core.SSHTargetFromConfig(cfg, record.GuestIP)
	core.UseStoredTestboxKey(&target, record.LeaseID)
	return target, nil
}

func (b *backend) serverFromRecord(cfg Config, record leaseStateRecord, running bool) Server {
	labels := cloneLabels(record.Labels)
	status := strings.TrimSpace(labels["state"])
	if status == "" {
		status = "ready"
	}
	if !running && status != "released" && status != "failed" && status != "expired" {
		status = "stopped"
		labels["state"] = status
	}
	server := Server{
		CloudID:  firstNonBlank(record.VMID, record.Name, record.LeaseID),
		Provider: providerName,
		Name:     firstNonBlank(record.Name, record.VMID, record.LeaseID),
		Status:   status,
		Labels:   labels,
	}
	server.PublicNet.IPv4.IP = record.GuestIP
	server.ServerType.Name = firstNonBlank(labels["server_type"], firecrackerServerTypeForConfig(cfg))
	return server
}

func serverFromClaim(cfg Config, claim LeaseClaim) Server {
	labels := cloneLabels(claim.Labels)
	name := firstNonBlank(labels["instance"], core.LeaseProviderName(claim.LeaseID, claim.Slug))
	server := Server{
		CloudID:  name,
		Provider: providerName,
		Name:     name,
		Status:   firstNonBlank(labels["state"], "unknown"),
		Labels:   labels,
	}
	server.PublicNet.IPv4.IP = claim.SSHHost
	server.ServerType.Name = firstNonBlank(labels["server_type"], firecrackerServerTypeForConfig(cfg))
	return server
}

func (b *backend) rollbackAcquire(record leaseStateRecord, vm machine, cause error) error {
	var cleanupErr error
	if vm != nil {
		if err := vm.StopVMM(); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("stop firecracker lease %s: %w", record.LeaseID, err))
		}
	}
	if record.PID > 0 && cleanupErr != nil {
		cleanupErr = errors.Join(cleanupErr, b.stopRecordedProcess(record))
	}
	cleanupErr = errors.Join(cleanupErr, b.cleanupNetwork(context.Background(), record))
	cleanupErr = errors.Join(cleanupErr, b.removeStateDir(record))
	core.RemoveLeaseClaim(record.LeaseID)
	return errors.Join(cause, cleanupErr)
}

func (b *backend) stopRecordedProcess(record leaseStateRecord) error {
	identity := record.processIdentity()
	if !b.processes.Matches(identity) {
		return nil
	}
	if err := b.processes.Signal(identity, syscall.SIGTERM); err != nil {
		return fmt.Errorf("stop firecracker process %d: %w", identity.PID, err)
	}
	if err := waitForProcessExit(b.processes, identity, firecrackerStopTimeout); err != nil {
		if killErr := b.processes.Signal(identity, syscall.SIGKILL); killErr != nil {
			return errors.Join(err, fmt.Errorf("force kill firecracker process %d: %w", identity.PID, killErr))
		}
		if killWaitErr := waitForProcessExit(b.processes, identity, firecrackerKillTimeout); killWaitErr != nil {
			return errors.Join(err, killWaitErr)
		}
	}
	return nil
}

func (b *backend) releaseStateRecord(ctx context.Context, cfg Config, record leaseStateRecord, deleteArtifacts bool) error {
	if err := b.stopRecordedProcess(record); err != nil {
		return err
	}
	if err := b.cleanupNetwork(ctx, record); err != nil {
		return err
	}
	if deleteArtifacts {
		return b.removeStateDir(record)
	}
	record.PID = 0
	record.ProcessStarted = ""
	record.BootID = ""
	record.Labels = core.TouchDirectLeaseLabels(record.Labels, cfg, "released", b.currentTime().UTC())
	record.UpdatedAt = b.currentTime().UTC().Format(time.RFC3339Nano)
	if err := b.writeStateRecord(record); err != nil {
		return err
	}
	if err := removeIfExists(record.SocketPath); err != nil {
		return err
	}
	return nil
}

func (b *backend) releaseRecordForLease(cfg Config, lease LeaseTarget) (leaseStateRecord, bool, error) {
	identifier := firstNonBlank(lease.LeaseID, lease.Server.Labels["lease"], lease.Server.Name, lease.Server.CloudID)
	if strings.TrimSpace(identifier) == "" {
		return leaseStateRecord{}, false, nil
	}
	return b.recordByIdentifier(cfg, identifier)
}

func (b *backend) shouldCleanupRecord(server Server, record leaseStateRecord) (bool, string) {
	if strings.EqualFold(server.Labels["keep"], "true") {
		return false, "keep=true"
	}
	if !b.processes.Matches(record.processIdentity()) {
		return true, "process_exited"
	}
	return core.ShouldCleanupServer(server, b.currentTime().UTC())
}

func firecrackerClaims() (map[string]LeaseClaim, error) {
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return nil, err
	}
	out := make(map[string]LeaseClaim)
	for _, claim := range claims {
		if claim.Provider != providerName {
			continue
		}
		out[claim.LeaseID] = claim
	}
	return out, nil
}

func (record leaseStateRecord) processIdentity() processIdentity {
	return processIdentity{PID: record.PID, Started: record.ProcessStarted, BootID: record.BootID}
}

func requireLifecycleHost() error {
	if firecrackerHostGOOS != "linux" {
		return exit(2, "provider=firecracker requires a Linux KVM host, got host=%s", firecrackerHostGOOS)
	}
	return nil
}

func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func cloneLabels(labels map[string]string) map[string]string {
	if labels == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(labels))
	for key, value := range labels {
		out[key] = value
	}
	return out
}

func removeIfExists(path string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return exit(2, "remove firecracker artifact %s: %v", path, err)
	}
	return nil
}

func doctorHostCheck() DoctorCheck {
	details := map[string]string{
		"os":       firecrackerHostGOOS,
		"mutation": "false",
	}
	if firecrackerHostGOOS != "linux" {
		details["class"] = "environment_blocked"
		return DoctorCheck{
			Status:  "failed",
			Check:   "host",
			Message: fmt.Sprintf("host=%s requires a Linux KVM host", firecrackerHostGOOS),
			Details: details,
		}
	}
	return DoctorCheck{
		Status:  "ok",
		Check:   "host",
		Message: "host=linux mutation=false",
		Details: details,
	}
}

func doctorKVMCheck() DoctorCheck {
	details := map[string]string{
		"path":     "/dev/kvm",
		"mutation": "false",
	}
	if firecrackerHostGOOS != "linux" {
		details["reason"] = "unsupported_host"
		return DoctorCheck{
			Status:  "skip",
			Check:   "kvm",
			Message: "/dev/kvm check skipped on non-Linux host",
			Details: details,
		}
	}
	info, err := firecrackerStat("/dev/kvm")
	if err != nil {
		details["class"] = "environment_blocked"
		return DoctorCheck{
			Status:  "failed",
			Check:   "kvm",
			Message: fmt.Sprintf("/dev/kvm unavailable: %v", err),
			Details: details,
		}
	}
	if info.IsDir() {
		details["class"] = "environment_blocked"
		return DoctorCheck{
			Status:  "failed",
			Check:   "kvm",
			Message: "/dev/kvm must be a device file, not a directory",
			Details: details,
		}
	}
	if err := firecrackerOpenKVM(); err != nil {
		details["class"] = "environment_blocked"
		return DoctorCheck{
			Status:  "failed",
			Check:   "kvm",
			Message: fmt.Sprintf("/dev/kvm not accessible: %v", err),
			Details: details,
		}
	}
	return DoctorCheck{
		Status:  "ok",
		Check:   "kvm",
		Message: "kvm=/dev/kvm mutation=false",
		Details: details,
	}
}

func doctorExecutableCheck(check, field, configured string) DoctorCheck {
	value := strings.TrimSpace(configured)
	details := map[string]string{
		"configured": value,
		"field":      field,
		"mutation":   "false",
	}
	if value == "" {
		details["class"] = "configuration_incomplete"
		return DoctorCheck{
			Status:  "failed",
			Check:   check,
			Message: fmt.Sprintf("%s is required", field),
			Details: details,
		}
	}
	resolved, err := firecrackerLookPath(value)
	if err != nil {
		details["class"] = "environment_blocked"
		return DoctorCheck{
			Status:  "failed",
			Check:   check,
			Message: fmt.Sprintf("%s unavailable: %v", field, err),
			Details: details,
		}
	}
	details["path"] = resolved
	return DoctorCheck{
		Status:  "ok",
		Check:   check,
		Message: fmt.Sprintf("%s=%s mutation=false", check, resolved),
		Details: details,
	}
}

func doctorJailerCheck(configured string) DoctorCheck {
	value := strings.TrimSpace(configured)
	details := map[string]string{
		"configured": value,
		"mutation":   "false",
	}
	if value == "" {
		return DoctorCheck{
			Status:  "skip",
			Check:   "jailer",
			Message: "jailer=disabled",
			Details: details,
		}
	}
	details["class"] = "configuration_incomplete"
	return DoctorCheck{
		Status:  "failed",
		Check:   "jailer",
		Message: "firecracker.jailer is configured, but jailer launch is not supported yet",
		Details: details,
	}
}

func doctorFileCheck(check, field, configured string) DoctorCheck {
	value := strings.TrimSpace(configured)
	details := map[string]string{
		"path":     value,
		"field":    field,
		"mutation": "false",
	}
	if value == "" {
		details["class"] = "configuration_incomplete"
		return DoctorCheck{
			Status:  "failed",
			Check:   check,
			Message: fmt.Sprintf("%s is required", field),
			Details: details,
		}
	}
	info, err := firecrackerStat(value)
	if err != nil {
		details["class"] = "environment_blocked"
		return DoctorCheck{
			Status:  "failed",
			Check:   check,
			Message: fmt.Sprintf("%s unavailable: %v", field, err),
			Details: details,
		}
	}
	if info.IsDir() {
		details["class"] = "configuration_incomplete"
		return DoctorCheck{
			Status:  "failed",
			Check:   check,
			Message: fmt.Sprintf("%s must point to a file, got directory %s", field, value),
			Details: details,
		}
	}
	return DoctorCheck{
		Status:  "ok",
		Check:   check,
		Message: fmt.Sprintf("%s=%s mutation=false", check, value),
		Details: details,
	}
}

func doctorNetworkCheck(cfg Config) DoctorCheck {
	mode := normalizeFirecrackerNetwork(cfg.Firecracker.Network)
	details := map[string]string{
		"mode":       mode,
		"cniNetwork": strings.TrimSpace(cfg.Firecracker.CNINetwork),
		"cniConfDir": strings.TrimSpace(cfg.Firecracker.CNIConfDir),
		"cniBinDir":  strings.TrimSpace(cfg.Firecracker.CNIBinDir),
		"mutation":   "false",
	}
	if mode != firecrackerNetworkCNI {
		details["class"] = "configuration_incomplete"
		return DoctorCheck{
			Status:  "failed",
			Check:   "network",
			Message: fmt.Sprintf("firecracker.network=%s is unsupported; only %s is supported", blankIfEmpty(mode), firecrackerNetworkCNI),
			Details: details,
		}
	}
	problems := make([]string, 0, 3)
	class := "configuration_incomplete"
	if details["cniNetwork"] == "" {
		problems = append(problems, "firecracker.cniNetwork is required")
	}
	if err := doctorRequireDir(details["cniConfDir"]); err != nil {
		class = "environment_blocked"
		problems = append(problems, fmt.Sprintf("firecracker.cniConfDir %v", err))
	}
	if err := doctorRequireDir(details["cniBinDir"]); err != nil {
		class = "environment_blocked"
		problems = append(problems, fmt.Sprintf("firecracker.cniBinDir %v", err))
	}
	if len(problems) == 0 {
		if _, err := firecrackerLoadCNI(details["cniConfDir"], details["cniNetwork"]); err != nil {
			class = "configuration_incomplete"
			problems = append(problems, fmt.Sprintf("firecracker.cniNetwork %q unavailable in %s: %v", details["cniNetwork"], details["cniConfDir"], err))
		}
	}
	if len(problems) > 0 {
		details["class"] = class
		return DoctorCheck{
			Status:  "failed",
			Check:   "network",
			Message: fmt.Sprintf("network=%s %s", mode, strings.Join(problems, "; ")),
			Details: details,
		}
	}
	return DoctorCheck{
		Status:  "ok",
		Check:   "network",
		Message: fmt.Sprintf("network=%s cni_network=%s mutation=false", mode, details["cniNetwork"]),
		Details: details,
	}
}

func doctorRequireDir(path string) error {
	value := strings.TrimSpace(path)
	if value == "" {
		return fmt.Errorf("is required")
	}
	info, err := firecrackerStat(value)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("must be a directory")
	}
	return nil
}

func openKVMDevice() error {
	file, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return err
	}
	return file.Close()
}

func aggregateDoctorStatus(checks []DoctorCheck) string {
	for _, check := range checks {
		if strings.EqualFold(strings.TrimSpace(check.Status), "failed") || strings.EqualFold(strings.TrimSpace(check.Status), "missing") {
			return "failed"
		}
	}
	for _, check := range checks {
		if strings.EqualFold(strings.TrimSpace(check.Status), "warning") {
			return "warning"
		}
	}
	return "ok"
}

func summarizeDoctorChecks(checks []DoctorCheck) string {
	fields := make([]string, 0, len(checks)+1)
	for _, check := range checks {
		fields = append(fields, fmt.Sprintf("%s=%s", check.Check, strings.TrimSpace(check.Status)))
	}
	fields = append(fields, "mutation=false")
	return strings.Join(fields, " ")
}

func blankIfEmpty(value string) string {
	if strings.TrimSpace(value) == "" {
		return "<empty>"
	}
	return value
}
