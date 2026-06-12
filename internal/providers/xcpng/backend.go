package xcpng

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

type leaseBackend struct{ shared.DirectSSHBackend }

type lifecycleClient interface {
	Close(context.Context) error
	DoctorInventory(context.Context, xcpNgConfig) ([]Server, error)
	ListCrabboxServers(context.Context) ([]Server, error)
	ResolveTemplate(context.Context, xcpNgConfig) (xapiRef, error)
	ResolveSR(context.Context, xcpNgConfig) (xapiRef, error)
	ResolveNetwork(context.Context, xcpNgConfig) (xapiRef, error)
	ResolveHost(context.Context, xcpNgConfig) (xapiRef, error)
	ResolveISOMedia(context.Context, xcpNgConfig, string) (xcpNgISOMediaRef, error)
	CloneVM(context.Context, xcpNgCloneRequest) (xapiVM, error)
	CreateFreshVM(context.Context, xcpNgFreshVMRequest) (xcpNgFreshVMResult, error)
	SetVMBootOrder(context.Context, xapiRef, string) error
	ImportISO(context.Context, xcpNgImportISORequest) (xcpNgConfigDrive, error)
	AttachDisk(context.Context, xcpNgDiskAttachRequest) (xcpNgConfigDrive, error)
	AttachConfigDrive(context.Context, xcpNgConfigDriveRequest) (xcpNgConfigDrive, error)
	AttachISO(context.Context, xcpNgISOAttachRequest) (xcpNgConfigDrive, error)
	StartVM(context.Context, xapiRef) error
	GuestIPv4(context.Context, xapiRef) (string, error)
	GuestIPv4ForID(context.Context, string) (string, error)
	GetServer(context.Context, string) (Server, error)
	SetLabels(context.Context, string, map[string]string) error
	DeleteServer(context.Context, string) error
	DeleteFreshServer(context.Context, string, string) error
	DeleteConfigDrive(context.Context, xcpNgConfigDrive) error
}

type guestIPv4Discoverer interface {
	DiscoverGuestIPv4(context.Context, xapiRef) (string, error)
}

type xcpNgConfig struct {
	APIURL       string
	Username     string
	Password     string
	Template     string
	TemplateUUID string
	SR           string
	SRUUID       string
	Network      string
	NetworkUUID  string
	Host         string
	User         string
	WorkRoot     string
	InsecureTLS  bool
}

type xcpNgCloneRequest struct {
	Config      Config
	TemplateRef xapiRef
	SRRef       xapiRef
	NetworkRef  xapiRef
	HostRef     xapiRef
	LeaseID     string
	Slug        string
	PublicKey   string
	Keep        bool
	Labels      map[string]string
}

type xcpNgConfigDriveRequest struct {
	VMRef   xapiRef
	SRRef   xapiRef
	LeaseID string
	Slug    string
	Payload xcpNgCloudInitPayload
	Labels  map[string]string
}

type xcpNgConfigDrive struct {
	VDIRef     string
	VBDRef     string
	Name       string
	Labels     map[string]string
	DestroyVDI bool
}

func NewLeaseBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) core.Backend {
	cfg.Provider = "xcp-ng"
	if cfg.XCPNg.User != "" {
		cfg.SSHUser = cfg.XCPNg.User
	}
	if cfg.XCPNg.WorkRoot != "" {
		cfg.WorkRoot = cfg.XCPNg.WorkRoot
	}
	if cfg.ServerType == "" {
		cfg.ServerType = xcpNgServerTypeForConfig(cfg)
	}
	return &leaseBackend{DirectSSHBackend: shared.DirectSSHBackend{SpecValue: spec, Cfg: cfg, RT: rt}}
}

func (b *leaseBackend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	return shared.AcquireAttemptsRetry(b.RT, req.Keep, func() (LeaseTarget, error) {
		return b.acquireOnce(ctx, req.Keep, req.RequestedSlug)
	})
}

func (b *leaseBackend) acquireOnce(ctx context.Context, keep bool, requestedSlug string) (lease LeaseTarget, err error) {
	if err := validateXCPNgProvisioningConfig(xcpNgProviderConfig(b.Cfg)); err != nil {
		return LeaseTarget{}, err
	}
	client, err := newLifecycleClient(ctx, b.Cfg)
	if err != nil {
		return LeaseTarget{}, err
	}
	defer closeClient(ctx, client, b.RT.Stderr)

	leaseID := newLeaseID()
	servers, err := client.ListCrabboxServers(ctx)
	if err != nil {
		return LeaseTarget{}, err
	}
	slug, err := allocateDirectLeaseSlug(leaseID, requestedSlug, servers)
	if err != nil {
		return LeaseTarget{}, err
	}
	cfg := b.Cfg
	keyPath, publicKey, err := ensureTestboxKeyForConfig(cfg, leaseID)
	if err != nil {
		return LeaseTarget{}, err
	}
	retainKey := false
	defer func() {
		if !retainKey {
			removeStoredTestboxKey(leaseID)
		}
	}()
	cfg.SSHKey = keyPath
	cfg.ProviderKey = providerKeyForLease(leaseID)
	cfg.ServerType = xcpNgServerTypeForConfig(cfg)
	now := currentTime(b.RT).UTC()
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, "xcp-ng", "", keep, now)
	labels["work_root"] = firstNonBlank(cfg.XCPNg.WorkRoot, cfg.WorkRoot)

	resolved, err := b.resolvePlacement(ctx, client)
	if err != nil {
		return LeaseTarget{}, err
	}
	fmt.Fprintf(b.RT.Stderr, "provisioning provider=xcp-ng lease=%s slug=%s template=%s keep=%v\n",
		leaseID, slug, resolved.templateRef.value(), keep)
	server, configDrive, vmRef, err := b.createAndBoot(ctx, client, cfg, resolved, leaseID, slug, publicKey, keep, labels)
	if err != nil {
		retainKey = server.CloudID != ""
		if retainKey {
			return LeaseTarget{}, fmt.Errorf("%w; retained xcp-ng VM requires manual cleanup: %s", err, server.CloudID)
		}
		return LeaseTarget{}, err
	}
	ip, err := b.waitForGuestIPv4(ctx, client, vmRef, bootstrapWaitTimeout(cfg))
	if err != nil {
		vmRetained, cleanupErr := b.cleanupFailedLease(ctx, client, server.CloudID, configDrive)
		retainKey = vmRetained
		if vmRetained {
			return LeaseTarget{}, fmt.Errorf("xcp-ng VM retained after guest IP failure; manual cleanup required for %s: %v; cleanup: %v", server.CloudID, err, cleanupErr)
		}
		return LeaseTarget{}, errors.Join(err, cleanupErr)
	}
	server.PublicNet.IPv4.IP = ip
	target := sshTargetFromConfig(cfg, ip)
	if err := waitForSSHReady(ctx, &target, b.RT.Stderr, "bootstrap", bootstrapWaitTimeout(cfg)); err != nil {
		vmRetained, cleanupErr := b.cleanupFailedLease(ctx, client, server.CloudID, configDrive)
		retainKey = vmRetained
		if vmRetained {
			return LeaseTarget{}, fmt.Errorf("xcp-ng VM retained after SSH bootstrap failure; manual cleanup required for %s: %v; cleanup: %v", server.CloudID, err, cleanupErr)
		}
		return LeaseTarget{}, errors.Join(err, cleanupErr)
	}
	server.Labels = core.TouchDirectLeaseLabels(server.Labels, cfg, "ready", currentTime(b.RT).UTC())
	if err := client.SetLabels(ctx, server.CloudID, server.Labels); err != nil {
		fmt.Fprintf(b.RT.Stderr, "warning: set xcp-ng labels: %v\n", err)
	}
	fmt.Fprintf(b.RT.Stderr, "provisioned lease=%s server=%s ip=%s\n", leaseID, server.DisplayID(), ip)
	retainKey = true
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

type xcpNgPlacement struct {
	templateRef xapiRef
	srRef       xapiRef
	networkRef  xapiRef
	hostRef     xapiRef
}

func (b *leaseBackend) resolvePlacement(ctx context.Context, client lifecycleClient) (xcpNgPlacement, error) {
	cfg := xcpNgProviderConfig(b.Cfg)
	if err := validateXCPNgProvisioningConfig(cfg); err != nil {
		return xcpNgPlacement{}, err
	}
	templateRef, err := client.ResolveTemplate(ctx, cfg)
	if err != nil {
		return xcpNgPlacement{}, err
	}
	srRef, err := client.ResolveSR(ctx, cfg)
	if err != nil {
		return xcpNgPlacement{}, err
	}
	networkRef, err := client.ResolveNetwork(ctx, cfg)
	if err != nil {
		return xcpNgPlacement{}, err
	}
	hostRef, err := client.ResolveHost(ctx, cfg)
	if err != nil {
		return xcpNgPlacement{}, err
	}
	return xcpNgPlacement{templateRef: templateRef, srRef: srRef, networkRef: networkRef, hostRef: hostRef}, nil
}

func (b *leaseBackend) createAndBoot(ctx context.Context, client lifecycleClient, cfg Config, placement xcpNgPlacement, leaseID, slug, publicKey string, keep bool, labels map[string]string) (Server, xcpNgConfigDrive, xapiRef, error) {
	vm, err := client.CloneVM(ctx, xcpNgCloneRequest{
		Config:      cfg,
		TemplateRef: placement.templateRef,
		SRRef:       placement.srRef,
		NetworkRef:  placement.networkRef,
		HostRef:     placement.hostRef,
		LeaseID:     leaseID,
		Slug:        slug,
		PublicKey:   publicKey,
		Keep:        keep,
		Labels:      labels,
	})
	if err != nil {
		if vm.Ref != "" {
			return xcpNgVMToServer(vm, labels, ""), xcpNgConfigDrive{}, xapiRef(vm.Ref), err
		}
		return Server{}, xcpNgConfigDrive{}, "", err
	}
	server := xcpNgVMToServer(vm, labels, "")
	payload, err := newCloudInitPayload(cfg, leaseID, slug, publicKey)
	if err != nil {
		vmRetained, cleanupErr := b.cleanupFailedLease(ctx, client, server.CloudID, xcpNgConfigDrive{})
		if vmRetained {
			return server, xcpNgConfigDrive{}, xapiRef(vm.Ref), errors.Join(err, cleanupErr)
		}
		return Server{}, xcpNgConfigDrive{}, "", errors.Join(err, cleanupErr)
	}
	configDrive, err := client.AttachConfigDrive(ctx, xcpNgConfigDriveRequest{VMRef: xapiRef(vm.Ref), SRRef: placement.srRef, LeaseID: leaseID, Slug: slug, Payload: payload, Labels: labels})
	if err != nil {
		vmRetained, cleanupErr := b.cleanupFailedLease(ctx, client, server.CloudID, xcpNgConfigDrive{})
		if vmRetained {
			return server, xcpNgConfigDrive{}, xapiRef(vm.Ref), errors.Join(err, cleanupErr)
		}
		return Server{}, xcpNgConfigDrive{}, "", errors.Join(err, cleanupErr)
	}
	if err := client.StartVM(ctx, xapiRef(vm.Ref)); err != nil {
		vmRetained, cleanupErr := b.cleanupFailedLease(ctx, client, server.CloudID, configDrive)
		if vmRetained {
			return server, configDrive, xapiRef(vm.Ref), errors.Join(err, cleanupErr)
		}
		return Server{}, xcpNgConfigDrive{}, "", errors.Join(err, cleanupErr)
	}
	return server, configDrive, xapiRef(vm.Ref), nil
}

func (b *leaseBackend) waitForGuestIPv4(ctx context.Context, client lifecycleClient, vmRef xapiRef, timeout time.Duration) (string, error) {
	deadline := currentTime(b.RT).Add(timeout)
	var lastErr error
	nextDiscover := time.Time{}
	for {
		ip, err := client.GuestIPv4(ctx, vmRef)
		if err == nil && ip != "" {
			return ip, nil
		}
		lastErr = err
		if discoverer, ok := client.(guestIPv4Discoverer); ok && currentTime(b.RT).After(nextDiscover) {
			nextDiscover = currentTime(b.RT).Add(guestIPDiscoverInterval)
			discovered, discoverErr := discoverer.DiscoverGuestIPv4(ctx, vmRef)
			if discoverErr == nil && discovered != "" {
				return discovered, nil
			}
			var configErr guestProbeConfigError
			if errors.As(discoverErr, &configErr) {
				return "", discoverErr
			}
			if lastErr == nil && discoverErr != nil {
				lastErr = discoverErr
			}
		}
		if currentTime(b.RT).After(deadline) {
			if lastErr != nil {
				return "", exit(5, "timed out waiting for XCP-ng guest IPv4: %v", lastErr)
			}
			return "", exit(5, "timed out waiting for XCP-ng guest IPv4")
		}
		select {
		case <-ctx.Done():
			return "", context.Cause(ctx)
		case <-time.After(guestIPPollInterval):
		}
	}
}

func (b *leaseBackend) cleanupFailedLease(ctx context.Context, client lifecycleClient, vmID string, drive xcpNgConfigDrive) (bool, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), xcpNgRollbackCleanupTimeout)
	defer cancel()
	var cleanupErr error
	if drive.VBDRef != "" || drive.VDIRef != "" {
		if err := client.DeleteConfigDrive(ctx, drive); err != nil {
			fmt.Fprintf(b.RT.Stderr, "warning: cleanup xcp-ng config-drive: %v\n", err)
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("cleanup xcp-ng config-drive: %w", err))
		}
	}
	if vmID != "" {
		if err := client.DeleteServer(ctx, vmID); err != nil {
			fmt.Fprintf(b.RT.Stderr, "warning: cleanup xcp-ng vm: %v\n", err)
			return true, errors.Join(cleanupErr, fmt.Errorf("cleanup xcp-ng vm: %w", err))
		}
	}
	return false, cleanupErr
}

func (b *leaseBackend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	client, err := newLifecycleClient(ctx, b.Cfg)
	if err != nil {
		return LeaseTarget{}, err
	}
	defer closeClient(ctx, client, b.RT.Stderr)
	if req.ID != "" {
		server, err := client.GetServer(ctx, req.ID)
		if err == nil {
			if !isCrabboxLease(server) {
				return LeaseTarget{}, exit(4, "lease/server not found: %s (VM exists but is not Crabbox-managed)", req.ID)
			}
			if req.StatusOnly && !req.ReadyProbe {
				return b.targetForServer(b.resolveStatusServer(ctx, client, server)), nil
			}
			server, err = b.ensureServerIP(ctx, client, server, req.ReleaseOnly)
			if err != nil {
				return LeaseTarget{}, err
			}
			return b.targetForServer(server), nil
		}
		if !isNotFound(err) {
			return LeaseTarget{}, err
		}
	}
	servers, err := client.ListCrabboxServers(ctx)
	if err != nil {
		return LeaseTarget{}, err
	}
	if server, leaseID, err := findServerByAlias(servers, req.ID); err != nil {
		return LeaseTarget{}, err
	} else if leaseID != "" {
		if refreshed, err := client.GetServer(ctx, server.CloudID); err == nil {
			server = refreshed
		} else if !req.ReleaseOnly {
			return LeaseTarget{}, err
		}
		if req.StatusOnly && !req.ReadyProbe {
			target := b.targetForServer(b.resolveStatusServer(ctx, client, server))
			target.LeaseID = leaseID
			return target, nil
		}
		server, err = b.ensureServerIP(ctx, client, server, req.ReleaseOnly)
		if err != nil {
			return LeaseTarget{}, err
		}
		target := b.targetForServer(server)
		target.LeaseID = leaseID
		return target, nil
	}
	return LeaseTarget{}, exit(4, "lease/server not found: %s", req.ID)
}

func (b *leaseBackend) resolveStatusServer(ctx context.Context, client lifecycleClient, server Server) Server {
	server = reconcileXCPNgServerState(server)
	if !strings.EqualFold(strings.TrimSpace(server.Status), "running") || firstNonBlank(server.PublicNet.IPv4.IP, server.PrivateNet.IPv4.IP) != "" {
		return server
	}
	probeCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	ip, err := client.GuestIPv4ForID(probeCtx, server.CloudID)
	if err != nil || ip == "" {
		if discoverer, ok := client.(guestIPv4Discoverer); ok {
			ip, _ = discoverer.DiscoverGuestIPv4(probeCtx, xapiRef(server.CloudID))
		}
	}
	if ip != "" {
		server.PublicNet.IPv4.IP = ip
		server.PrivateNet.IPv4.IP = ip
	}
	return server
}

func reconcileXCPNgServerState(server Server) Server {
	liveState := strings.ToLower(strings.TrimSpace(server.Status))
	if liveState == "" {
		return server
	}
	server.Status = liveState
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	labelState := strings.ToLower(strings.TrimSpace(server.Labels["state"]))
	if liveState != "running" || labelState == "" {
		server.Labels["state"] = liveState
	}
	return server
}

func (b *leaseBackend) ensureServerIP(ctx context.Context, client lifecycleClient, server Server, releaseOnly bool) (Server, error) {
	if firstNonBlank(server.PublicNet.IPv4.IP, server.PrivateNet.IPv4.IP) != "" || releaseOnly {
		return server, nil
	}
	ip, guestErr := client.GuestIPv4ForID(ctx, server.CloudID)
	if ip == "" {
		if discoverer, ok := client.(guestIPv4Discoverer); ok {
			discovered, discoverErr := discoverer.DiscoverGuestIPv4(ctx, xapiRef(server.CloudID))
			if discoverErr == nil && discovered != "" {
				ip = discovered
			} else if discoverErr != nil {
				if guestErr == nil {
					guestErr = errors.New("no guest ipv4 address reported by XCP-ng guest metrics")
				}
				return Server{}, errors.Join(guestErr, discoverErr)
			}
		}
	}
	if ip == "" {
		if guestErr != nil {
			return Server{}, guestErr
		}
		return Server{}, errors.New("no guest ipv4 address reported by XCP-ng guest metrics")
	}
	server.PublicNet.IPv4.IP = ip
	server.PrivateNet.IPv4.IP = ip
	return server, nil
}

func (b *leaseBackend) targetForServer(server Server) LeaseTarget {
	cfg := b.Cfg
	if storedTarget := strings.TrimSpace(server.Labels["target"]); storedTarget != "" {
		cfg.TargetOS = storedTarget
		if !strings.EqualFold(storedTarget, "windows") {
			cfg.WindowsMode = ""
		}
	}
	if storedWindowsMode := strings.TrimSpace(server.Labels["windows_mode"]); storedWindowsMode != "" {
		cfg.WindowsMode = storedWindowsMode
	}
	if storedWorkRoot := strings.TrimSpace(server.Labels["work_root"]); storedWorkRoot != "" {
		cfg.WorkRoot = storedWorkRoot
	}
	target := sshTargetFromConfig(cfg, firstNonBlank(server.PublicNet.IPv4.IP, server.PrivateNet.IPv4.IP))
	leaseID := core.Blank(server.Labels["lease"], server.CloudID)
	useStoredTestboxKey(&target, leaseID)
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}
}

func (b *leaseBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	_ = req
	client, err := newLifecycleClient(ctx, b.Cfg)
	if err != nil {
		return nil, err
	}
	defer closeClient(ctx, client, b.RT.Stderr)
	return client.ListCrabboxServers(ctx)
}

func (b *leaseBackend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	client, err := newLifecycleClient(ctx, b.Cfg)
	if err != nil {
		return err
	}
	defer closeClient(ctx, client, b.RT.Stderr)
	server := req.Lease.Server
	if !isCrabboxLease(server) {
		return exit(4, "refusing to release non-Crabbox xcp-ng VM: %s", server.DisplayID())
	}
	if err := client.DeleteServer(ctx, server.CloudID); err != nil {
		return err
	}
	removeLeaseClaim(req.Lease.LeaseID)
	removeStoredTestboxKey(req.Lease.LeaseID)
	return nil
}

func (b *leaseBackend) Touch(ctx context.Context, req TouchRequest) (Server, error) {
	client, err := newLifecycleClient(ctx, b.Cfg)
	if err != nil {
		return Server{}, err
	}
	defer closeClient(ctx, client, b.RT.Stderr)
	server := req.Lease.Server
	if !isCrabboxLease(server) {
		return Server{}, exit(4, "refusing to touch non-Crabbox xcp-ng VM: %s", server.DisplayID())
	}
	server.Labels = core.TouchDirectLeaseLabels(server.Labels, b.Cfg, req.State, currentTime(b.RT).UTC())
	if err := client.SetLabels(ctx, server.CloudID, server.Labels); err != nil {
		return Server{}, err
	}
	return server, nil
}

func (b *leaseBackend) Cleanup(ctx context.Context, req CleanupRequest) error {
	servers, err := b.List(ctx, ListRequest{Options: req.Options})
	if err != nil {
		return err
	}
	client, err := newLifecycleClient(ctx, b.Cfg)
	if err != nil {
		return err
	}
	defer closeClient(ctx, client, b.RT.Stderr)
	for _, server := range servers {
		if !isCrabboxLease(server) {
			fmt.Fprintf(b.RT.Stderr, "skip server id=%s name=%s reason=not-crabbox-managed\n", server.DisplayID(), server.Name)
			continue
		}
		shouldDelete, reason := core.ShouldCleanupServer(server, currentTime(b.RT).UTC())
		if !shouldDelete {
			fmt.Fprintf(b.RT.Stderr, "skip server id=%s name=%s reason=%s\n", server.DisplayID(), server.Name, reason)
			continue
		}
		fmt.Fprintf(b.RT.Stderr, "delete server id=%s name=%s\n", server.DisplayID(), server.Name)
		if req.DryRun {
			continue
		}
		if err := client.DeleteServer(ctx, server.CloudID); err != nil {
			return err
		}
		leaseID := strings.TrimSpace(server.Labels["lease"])
		if leaseID != "" {
			removeLeaseClaim(leaseID)
			removeStoredTestboxKey(leaseID)
		}
	}
	return nil
}

func (b *leaseBackend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	cfg := xcpNgProviderConfig(b.Cfg)
	if err := validateXCPNgConfig(cfg); err != nil {
		return core.DoctorResult{
			Provider: "xcp-ng",
			Message:  "auth=configuration-incomplete control_plane=unchecked inventory=unchecked mutation=false runtime=unchecked",
			Status:   "failed",
			Checks: []core.DoctorCheck{{
				Status:  "failed",
				Check:   "configuration",
				Message: err.Error(),
				Details: map[string]string{
					"mutation": "false",
				},
			}},
		}, nil
	}
	if err := validateXCPNgProvisioningConfig(cfg); err != nil {
		return core.DoctorResult{
			Provider: "xcp-ng",
			Message:  "auth=configuration-incomplete control_plane=unchecked inventory=unchecked mutation=false runtime=unchecked",
			Status:   "failed",
			Checks: []core.DoctorCheck{{
				Status:  "failed",
				Check:   "configuration",
				Message: err.Error(),
				Details: map[string]string{
					"mutation": "false",
				},
			}},
		}, nil
	}
	client, err := newLifecycleClient(ctx, b.Cfg)
	if err != nil {
		return core.DoctorResult{}, err
	}
	defer closeClient(ctx, client, b.RT.Stderr)
	if err := b.resolveDoctorPlacement(ctx, client, cfg); err != nil {
		return core.DoctorResult{
			Provider: "xcp-ng",
			Message:  "auth=ready control_plane=ready placement=failed inventory=unchecked mutation=false runtime=unchecked",
			Status:   "failed",
			Checks: []core.DoctorCheck{
				{Status: "ok", Check: "auth", Message: "XAPI session established", Details: map[string]string{"mutation": "false"}},
				{Status: "failed", Check: "placement", Message: err.Error(), Details: map[string]string{"mutation": "false"}},
			},
		}, nil
	}
	servers, err := client.DoctorInventory(ctx, cfg)
	if err != nil {
		return core.DoctorResult{}, err
	}
	return core.DoctorResult{
		Provider: "xcp-ng",
		Message:  fmt.Sprintf("auth=ready control_plane=ready placement=ready inventory=ready api=list mutation=false leases=%d runtime=unchecked", len(servers)),
		Status:   "ok",
		Checks: []core.DoctorCheck{
			{Status: "ok", Check: "auth", Message: "XAPI session established", Details: map[string]string{"mutation": "false"}},
			{Status: "ok", Check: "placement", Message: "configured placement resources resolved", Details: map[string]string{"mutation": "false"}},
			{Status: "ok", Check: "inventory", Message: fmt.Sprintf("listed %d Crabbox-managed leases", len(servers)), Details: map[string]string{"mutation": "false"}},
		},
	}, nil
}

func (b *leaseBackend) resolveDoctorPlacement(ctx context.Context, client lifecycleClient, cfg xcpNgConfig) error {
	if _, err := client.ResolveTemplate(ctx, cfg); err != nil {
		return err
	}
	if _, err := client.ResolveSR(ctx, cfg); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.Network) != "" || strings.TrimSpace(cfg.NetworkUUID) != "" {
		if _, err := client.ResolveNetwork(ctx, cfg); err != nil {
			return err
		}
	}
	if strings.TrimSpace(cfg.Host) != "" {
		if _, err := client.ResolveHost(ctx, cfg); err != nil {
			return err
		}
	}
	return nil
}

func xcpNgServerTypeForConfig(cfg core.Config) string {
	if value := strings.TrimSpace(cfg.XCPNg.TemplateUUID); value != "" {
		return "template-" + value
	}
	if value := strings.TrimSpace(cfg.XCPNg.Template); value != "" {
		return "template-" + core.NormalizeLeaseSlug(value)
	}
	return "template"
}

func xcpNgProviderConfig(cfg Config) xcpNgConfig {
	return xcpNgConfig{
		APIURL:       cfg.XCPNg.APIURL,
		Username:     cfg.XCPNg.Username,
		Password:     cfg.XCPNg.Password,
		Template:     cfg.XCPNg.Template,
		TemplateUUID: cfg.XCPNg.TemplateUUID,
		SR:           cfg.XCPNg.SR,
		SRUUID:       cfg.XCPNg.SRUUID,
		Network:      cfg.XCPNg.Network,
		NetworkUUID:  cfg.XCPNg.NetworkUUID,
		Host:         cfg.XCPNg.Host,
		User:         cfg.XCPNg.User,
		WorkRoot:     cfg.XCPNg.WorkRoot,
		InsecureTLS:  cfg.XCPNg.InsecureTLS,
	}
}

func validateXCPNgConfig(cfg xcpNgConfig) error {
	var missing []string
	if strings.TrimSpace(cfg.APIURL) == "" {
		missing = append(missing, "xcpNg.apiUrl or CRABBOX_XCP_NG_API_URL")
	}
	if strings.TrimSpace(cfg.Username) == "" {
		missing = append(missing, "xcpNg.username or CRABBOX_XCP_NG_USERNAME")
	}
	if strings.TrimSpace(cfg.Password) == "" {
		missing = append(missing, "xcpNg.password or CRABBOX_XCP_NG_PASSWORD")
	}
	if len(missing) > 0 {
		return exit(3, "xcp-ng configuration is incomplete: missing %s", strings.Join(missing, ", "))
	}
	return nil
}

func validateXCPNgProvisioningConfig(cfg xcpNgConfig) error {
	var missing []string
	if strings.TrimSpace(cfg.Template) == "" && strings.TrimSpace(cfg.TemplateUUID) == "" {
		missing = append(missing, "xcpNg.template/xcpNg.templateUuid or CRABBOX_XCP_NG_TEMPLATE/CRABBOX_XCP_NG_TEMPLATE_UUID")
	}
	if strings.TrimSpace(cfg.SR) == "" && strings.TrimSpace(cfg.SRUUID) == "" {
		missing = append(missing, "xcpNg.sr/xcpNg.srUuid or CRABBOX_XCP_NG_SR/CRABBOX_XCP_NG_SR_UUID")
	}
	if len(missing) > 0 {
		return exit(3, "xcp-ng configuration is incomplete: missing %s", strings.Join(missing, ", "))
	}
	return nil
}

func xcpNgVMToServer(vm xapiVM, labels map[string]string, ip string) Server {
	if labels == nil {
		labels = vm.Labels
	}
	if labels == nil {
		labels = map[string]string{}
	}
	server := Server{
		Provider: "xcp-ng",
		CloudID:  firstNonBlank(vm.UUID, vm.Ref),
		Name:     vm.Name,
		Status:   vm.PowerState,
		Labels:   labels,
	}
	server.PublicNet.IPv4.IP = ip
	server.PrivateNet.IPv4.IP = ip
	server.ServerType.Name = core.Blank(labels["server_type"], "template")
	return server
}

func isCrabboxLease(server Server) bool {
	if server.Labels == nil {
		return false
	}
	if server.Labels["crabbox"] != "true" || server.Labels["created_by"] != "crabbox" {
		return false
	}
	if server.Labels["provider"] != "xcp-ng" {
		return false
	}
	return strings.TrimSpace(server.Labels["lease"]) != ""
}

func isCrabboxVMDisk(labels map[string]string, leaseID string) bool {
	if labels == nil {
		return false
	}
	return labels["crabbox"] == "true" &&
		labels["created_by"] == "crabbox" &&
		labels["provider"] == "xcp-ng" &&
		labels["lease"] == leaseID &&
		labels["resource"] == "vm-disk"
}

func closeClient(ctx context.Context, client lifecycleClient, stderr io.Writer) {
	closeCtx, cancel := xcpNgRollbackContext(ctx)
	defer cancel()
	if err := client.Close(closeCtx); err != nil {
		fmt.Fprintf(stderr, "warning: close xcp-ng session: %v\n", err)
	}
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func currentTime(rt Runtime) time.Time {
	if rt.Clock != nil {
		return rt.Clock.Now()
	}
	return time.Now()
}

var newLifecycleClient = func(ctx context.Context, cfg Config) (lifecycleClient, error) {
	return newXAPIClient(ctx, cfg)
}

var newLeaseID = func() string { return core.NewLeaseID() }
var allocateDirectLeaseSlug = func(id, requested string, servers []Server) (string, error) {
	return core.AllocateDirectLeaseSlug(id, requested, servers)
}
var ensureTestboxKeyForConfig = func(cfg Config, leaseID string) (string, string, error) {
	return core.EnsureTestboxKeyForConfig(cfg, leaseID)
}
var providerKeyForLease = func(leaseID string) string { return core.ProviderKeyForLease(leaseID) }
var sshTargetFromConfig = func(cfg Config, host string) SSHTarget { return core.SSHTargetFromConfig(cfg, host) }
var waitForSSHReady = func(ctx context.Context, target *SSHTarget, stderr io.Writer, phase string, timeout time.Duration) error {
	return core.WaitForSSHReady(ctx, target, stderr, phase, timeout)
}
var bootstrapWaitTimeout = func(cfg Config) time.Duration { return core.BootstrapWaitTimeout(cfg) }
var guestIPPollInterval = 5 * time.Second
var guestIPDiscoverInterval = 15 * time.Second
var xcpNgRollbackCleanupTimeout = 11 * time.Minute
var xcpNgPartialRollbackTimeout = 30 * time.Second
var findServerByAlias = func(servers []Server, id string) (Server, string, error) {
	return core.FindServerByAlias(servers, id)
}
var removeLeaseClaim = func(leaseID string) { core.RemoveLeaseClaim(leaseID) }
var removeStoredTestboxKey = func(leaseID string) { core.RemoveStoredTestboxKey(leaseID) }
var exit = func(code int, format string, args ...any) core.ExitError { return core.Exit(code, format, args...) }

func useStoredTestboxKey(target *SSHTarget, leaseID string) {
	if keyPath, err := core.TestboxKeyPath(leaseID); err == nil {
		if _, statErr := os.Stat(keyPath); statErr == nil {
			target.Key = keyPath
		}
	}
}
