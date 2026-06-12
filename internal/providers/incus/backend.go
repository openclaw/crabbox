package incus

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/lxc/incus/v7/shared/api"
	core "github.com/openclaw/crabbox/internal/cli"
)

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
type SSHTarget = core.SSHTarget

type backend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

var waitForSSHReady = core.WaitForSSHReady

type retainedAcquireError struct {
	err     error
	cleanup func()
}

func (e *retainedAcquireError) Error() string { return e.err.Error() }

func (e *retainedAcquireError) Unwrap() error { return e.err }

func newBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	applyDefaults(&cfg)
	return &backend{spec: spec, cfg: cfg, rt: rt}
}

func applyDefaults(cfg *Config) {
	cfg.Provider = providerName
	base := core.BaseConfig()
	if cfg.TargetOS == "" {
		cfg.TargetOS = core.TargetLinux
	}
	if cfg.TargetOS == core.TargetLinux {
		cfg.WindowsMode = ""
	}
	if cfg.Incus.User != "" && (cfg.SSHUser == "" || cfg.SSHUser == base.SSHUser || cfg.Incus.User != base.Incus.User) {
		cfg.SSHUser = cfg.Incus.User
	}
	if cfg.Incus.WorkRoot != "" && (core.IsDefaultWorkRoot(cfg.WorkRoot) || cfg.Incus.WorkRoot != base.Incus.WorkRoot) {
		cfg.WorkRoot = cfg.Incus.WorkRoot
	}
	currentSSHPort := strings.TrimSpace(cfg.SSHPort)
	defaultSSHPort := core.Blank(strings.TrimSpace(cfg.Incus.ProxyListenPort), core.Blank(strings.TrimSpace(cfg.Incus.LaunchPort), "22"))
	baseSSHPort := core.Blank(strings.TrimSpace(base.Incus.ProxyListenPort), core.Blank(strings.TrimSpace(base.Incus.LaunchPort), "22"))
	if currentSSHPort == "" || currentSSHPort == strings.TrimSpace(base.SSHPort) || currentSSHPort == baseSSHPort {
		cfg.SSHPort = defaultSSHPort
	} else {
		cfg.SSHPort = currentSSHPort
	}
	cfg.SSHFallbackPorts = nil
	if cfg.ServerType == "" {
		cfg.ServerType = core.IncusServerTypeForConfig(*cfg)
	}
}

func (b *backend) Spec() ProviderSpec { return b.spec }

func (b *backend) RebindResolvedLeaseTarget(target *LeaseTarget, leaseID string) error {
	core.UseStoredTestboxKey(&target.SSH, leaseID)
	return nil
}

func (b *backend) configForRun() Config {
	cfg := b.cfg
	applyDefaults(&cfg)
	return cfg
}

func (b *backend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	var lastErr error
	attempts := core.AcquireAttempts(req.Keep)
	for attempt := 1; attempt <= attempts; attempt++ {
		lease, err := b.acquireOnce(ctx, req)
		if err == nil {
			return lease, nil
		}
		lastErr = err
		if attempt == attempts || !core.IsBootstrapWaitError(err) {
			return LeaseTarget{}, err
		}
		var retained *retainedAcquireError
		if errors.As(err, &retained) && retained.cleanup != nil {
			retained.cleanup()
		}
		fmt.Fprintf(b.rt.Stderr, "warning: bootstrap failed; retrying with fresh lease: %v\n", err)
	}
	return LeaseTarget{}, lastErr
}

func (b *backend) acquireOnce(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	cfg := b.configForRun()
	client, err := newClient(cfg)
	if err != nil {
		return LeaseTarget{}, err
	}
	leaseID := core.NewLeaseID()
	instances, err := client.ListInstances()
	if err != nil {
		return LeaseTarget{}, err
	}
	servers := make([]core.Server, 0, len(instances))
	for _, inst := range instances {
		servers = append(servers, serverFromInstance(inst, nil, cfg))
	}
	slug, err := core.AllocateDirectLeaseSlug(leaseID, req.RequestedSlug, servers)
	if err != nil {
		return LeaseTarget{}, err
	}
	keyPath, publicKey, err := core.EnsureTestboxKeyForConfig(cfg, leaseID)
	if err != nil {
		return LeaseTarget{}, err
	}
	cleanupKey := true
	defer func() {
		if cleanupKey {
			core.RemoveStoredTestboxKey(leaseID)
		}
	}()
	cfg.SSHKey = keyPath
	cfg.ProviderKey = core.ProviderKeyForLease(leaseID)
	name := core.LeaseProviderName(leaseID, slug)
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", req.Keep, time.Now().UTC())
	labels["instance"] = name
	labels["image"] = cfg.Incus.Image
	labels["ssh_user"] = cfg.SSHUser
	labels["ssh_port"] = cfg.SSHPort
	labels["work_root"] = cfg.WorkRoot
	labels["release"] = incusReleaseAction(cfg)
	if port := strings.TrimSpace(cfg.Incus.ProxyListenPort); port != "" {
		labels["proxy_port"] = port
		if host := sshHostForConfig(cfg); host != "" {
			labels["proxy_host"] = host
		}
	}
	createReq := api.InstancesPost{
		Name: name,
		Type: api.InstanceType(normalizeInstanceType(cfg.Incus.InstanceType)),
		InstancePut: api.InstancePut{
			Config:   instanceConfigForCreate(cfg, labels, publicKey),
			Profiles: profilesForConfig(cfg),
			Devices:  devicesForCreate(cfg),
		},
		Source: imageSourceForConfig(cfg),
	}
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s type=%s image=%s keep=%v\n", providerName, leaseID, slug, cfg.Incus.InstanceType, cfg.Incus.Image, req.Keep)
	if err := client.CreateInstance(createReq); err != nil {
		return LeaseTarget{}, err
	}
	if req.Keep {
		cleanupKey = false
	}
	cleanupInstance := func() {
		if inst, _, err := client.GetInstance(name); err == nil && inst.IsActive() {
			_ = client.SetInstanceState(name, api.InstanceStatePut{Action: "stop", Force: true, Timeout: durationSecondsCeil(cfg.Incus.StartTimeout)}, "")
		}
		_ = client.DeleteInstance(name)
	}
	retainedCleanup := func() {
		cleanupInstance()
		core.RemoveStoredTestboxKey(leaseID)
	}
	inst, etag, err := client.GetInstance(name)
	if err != nil {
		if !req.Keep {
			cleanupInstance()
		} else {
			err = &retainedAcquireError{err: err, cleanup: retainedCleanup}
		}
		return LeaseTarget{}, err
	}
	if !inst.IsActive() {
		if err := client.SetInstanceState(name, api.InstanceStatePut{Action: "start", Timeout: durationSecondsCeil(cfg.Incus.StartTimeout)}, etag); err != nil {
			if !req.Keep {
				cleanupInstance()
			} else {
				err = &retainedAcquireError{err: err, cleanup: retainedCleanup}
			}
			return LeaseTarget{}, err
		}
	}
	inst, _, err = b.waitForAddress(ctx, client, name)
	if err != nil {
		if !req.Keep {
			cleanupInstance()
		} else {
			err = &retainedAcquireError{err: err, cleanup: retainedCleanup}
		}
		return LeaseTarget{}, err
	}
	server := serverFromInstance(*inst, nil, cfg)
	target := core.SSHTargetFromConfig(cfg, sshTargetHost(server, cfg))
	if err := waitForSSHReady(ctx, &target, b.rt.Stderr, "bootstrap", core.BootstrapWaitTimeout(cfg)); err != nil {
		if !req.Keep {
			cleanupInstance()
		} else {
			err = &retainedAcquireError{err: err, cleanup: retainedCleanup}
		}
		return LeaseTarget{}, err
	}
	server.Labels = core.TouchDirectLeaseLabels(server.Labels, cfg, "ready", time.Now().UTC())
	if err := setInstanceLabels(ctx, client, name, server.Labels); err != nil {
		fmt.Fprintf(b.rt.Stderr, "warning: set incus labels: %v\n", err)
	}
	if err := core.ClaimLeaseForRepoProviderScopePond(leaseID, slug, providerName, instanceScope(name), cfg.Pond, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
		if !req.Keep {
			cleanupInstance()
		} else {
			err = &retainedAcquireError{err: err, cleanup: retainedCleanup}
		}
		return LeaseTarget{}, err
	}
	if err := core.UpdateLeaseClaimEndpoint(leaseID, server, target); err != nil {
		if !req.Keep {
			cleanupInstance()
		} else {
			err = &retainedAcquireError{err: err, cleanup: retainedCleanup}
		}
		return LeaseTarget{}, err
	}
	cleanupKey = false
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *backend) Resolve(ctx context.Context, req ResolveRequest) (lease LeaseTarget, err error) {
	cfg := b.configForRun()
	client, err := newClient(cfg)
	if err != nil {
		return LeaseTarget{}, err
	}
	inst, server, leaseID, err := b.resolveInstance(ctx, client, req.ID)
	if err != nil {
		return LeaseTarget{}, err
	}
	if req.ReleaseOnly {
		return LeaseTarget{Server: server, LeaseID: leaseID}, nil
	}
	var previousClaim, preflightClaim core.LeaseClaim
	var previousClaimExists, rollbackClaim bool
	defer func() {
		if err == nil || !rollbackClaim {
			return
		}
		if restoreErr := core.RestoreLeaseClaimIfUnchanged(leaseID, preflightClaim, previousClaim, previousClaimExists); restoreErr != nil {
			fmt.Fprintf(b.rt.Stderr, "warning: restore Incus lease claim %s after resolve failure: %v\n", leaseID, restoreErr)
		}
	}()
	if req.Repo.Root != "" && leaseID != "" {
		previousClaim, previousClaimExists, err = core.ReadLeaseClaimWithPresence(leaseID)
		if err != nil {
			return LeaseTarget{}, err
		}
		preflightClaim, err = core.ClaimLeaseForRepoProviderScopePondIfUnchanged(leaseID, server.Labels["slug"], providerName, instanceScope(inst.Name), cfg.Pond, req.Repo.Root, cfg.IdleTimeout, req.Reclaim, previousClaim, previousClaimExists)
		if err != nil {
			return LeaseTarget{}, err
		}
		rollbackClaim = true
	}
	if req.StatusOnly {
		state, _, err := client.GetInstanceState(inst.Name)
		if err != nil {
			return LeaseTarget{}, err
		}
		server = serverFromInstance(inst, state, cfg)
		if liveState := strings.ToLower(strings.TrimSpace(state.Status)); liveState != "" {
			server.Status = liveState
			labelState := strings.ToLower(strings.TrimSpace(server.Labels["state"]))
			if liveState != "running" || (labelState != "ready" && labelState != "running") {
				server.Labels["state"] = liveState
			}
		}
	} else {
		if !inst.IsActive() {
			if err := client.SetInstanceState(inst.Name, api.InstanceStatePut{Action: "start", Timeout: durationSecondsCeil(cfg.Incus.StartTimeout)}, ""); err != nil {
				return LeaseTarget{}, err
			}
		}
		instWithAddress, _, err := b.waitForAddress(ctx, client, inst.Name)
		if err != nil {
			return LeaseTarget{}, err
		}
		inst = *instWithAddress
		server = serverFromInstance(inst, nil, cfg)
	}
	target := core.SSHTargetFromConfig(cfg, sshTargetHost(server, cfg))
	if labelUser := strings.TrimSpace(server.Labels["ssh_user"]); labelUser != "" {
		target.User = labelUser
	}
	if labelPort := strings.TrimSpace(server.Labels["ssh_port"]); labelPort != "" {
		target.Port = labelPort
	}
	if leaseID != "" {
		if keyPath, keyErr := core.TestboxKeyPath(leaseID); keyErr == nil {
			if _, statErr := os.Stat(keyPath); statErr == nil {
				target.Key = keyPath
			}
		}
	}
	if !req.StatusOnly {
		if err := waitForSSHReady(ctx, &target, b.rt.Stderr, "reuse", core.BootstrapWaitTimeout(cfg)); err != nil {
			return LeaseTarget{}, err
		}
		server.Status = "ready"
		server.Labels = core.TouchDirectLeaseLabels(server.Labels, cfg, "ready", time.Now().UTC())
		if err := setInstanceLabels(ctx, client, inst.Name, server.Labels); err != nil {
			fmt.Fprintf(b.rt.Stderr, "warning: set incus labels: %v\n", err)
		}
	}
	if req.Repo.Root != "" && leaseID != "" {
		if _, err = core.UpdateLeaseClaimEndpointIfUnchanged(leaseID, preflightClaim, server, target); err != nil {
			return LeaseTarget{}, err
		}
		rollbackClaim = false
	} else if !req.StatusOnly && leaseID != "" {
		if err := core.UpdateLeaseClaimEndpoint(leaseID, server, target); err != nil {
			return LeaseTarget{}, err
		}
	}
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *backend) List(ctx context.Context, _ ListRequest) ([]LeaseView, error) {
	cfg := b.configForRun()
	client, err := newClient(cfg)
	if err != nil {
		return nil, err
	}
	instances, err := client.ListInstances()
	if err != nil {
		return nil, err
	}
	views := make([]LeaseView, 0, len(instances))
	for _, inst := range instances {
		if !isCrabboxInstance(inst) {
			continue
		}
		views = append(views, serverFromInstance(inst, nil, cfg))
	}
	return views, nil
}

func (b *backend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	cfg := b.configForRun()
	info, err := doctorConnectionInfoForConfig(cfg)
	if err != nil {
		return core.DoctorResult{}, err
	}
	views, err := b.List(ctx, ListRequest{})
	if err != nil {
		return core.DoctorResult{}, err
	}
	fields := []string{
		"cli=ready",
		"inventory=ready",
		"api=list",
		"mutation=false",
		fmt.Sprintf("leases=%d", len(views)),
		"runtime=go_client",
		"mode=" + info.Mode,
		"protocol=" + info.Protocol,
		"endpoint=" + core.Blank(info.Endpoint, "-"),
		"project=" + info.Project,
		"auth=" + info.Auth,
	}
	if info.Mode == "socket" {
		fields = append(fields, "control_plane=local")
	} else {
		fields = append(fields, "control_plane=remote")
	}
	if info.Remote != "" {
		fields = append(fields, "remote="+info.Remote)
	}
	return core.DoctorResult{Provider: providerName, Message: strings.Join(fields, " ")}, nil
}

func (b *backend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	cfg := b.configForRun()
	client, err := newClient(cfg)
	if err != nil {
		return err
	}
	inst, _, leaseID, err := b.resolveInstance(ctx, client, req.Lease.LeaseID)
	if err != nil && strings.TrimSpace(req.Lease.Server.CloudID) != "" {
		inst, _, leaseID, err = b.resolveInstance(ctx, client, req.Lease.Server.CloudID)
	}
	if err != nil {
		return err
	}
	claim, claimOK, err := core.ResolveLeaseClaimForProvider(leaseID, providerName)
	if err != nil {
		return err
	}
	deleteInstance := incusDeleteOnRelease(req.Lease, cfg)
	if deleteInstance {
		if inst.IsActive() {
			if err := client.SetInstanceState(inst.Name, api.InstanceStatePut{Action: "stop", Force: req.Force, Timeout: durationSecondsCeil(cfg.Incus.StartTimeout)}, ""); err != nil {
				return err
			}
		}
		if err := client.DeleteInstance(inst.Name); err != nil {
			return err
		}
	} else {
		labels := labelsFromInstance(inst)
		labels["state"] = "stopped"
		labels["release"] = "stop"
		delete(labels, "host")
		stopAndPersist := func() error {
			if inst.IsActive() {
				if err := client.SetInstanceState(inst.Name, api.InstanceStatePut{Action: "stop", Force: true, Timeout: durationSecondsCeil(cfg.Incus.StartTimeout)}, ""); err != nil {
					return err
				}
			}
			return setInstanceLabels(ctx, client, inst.Name, labels)
		}
		if leaseID != "" && claimOK {
			server := core.Server{CloudID: inst.Name, Provider: providerName, Name: inst.Name, Status: "stopped", Labels: labels}
			if _, err := core.UpdateLeaseClaimEndpointIfUnchangedAfter(leaseID, claim, server, core.SSHTarget{}, stopAndPersist); err != nil {
				return err
			}
		} else if err := stopAndPersist(); err != nil {
			return err
		}
	}
	if leaseID != "" && deleteInstance {
		core.RemoveLeaseClaim(leaseID)
		core.RemoveStoredTestboxKey(leaseID)
	}
	return nil
}

func (b *backend) ReleaseLeaseMessage(lease LeaseTarget) string {
	instance := core.Blank(core.Blank(lease.Server.CloudID, lease.Server.Name), "-")
	if incusDeleteOnRelease(lease, b.configForRun()) {
		return fmt.Sprintf("deleted lease=%s instance=%s", lease.LeaseID, instance)
	}
	return fmt.Sprintf("stopped lease=%s instance=%s retained=true", lease.LeaseID, instance)
}

func (b *backend) RetainLeaseClaimAfterRelease(lease LeaseTarget) bool {
	return !incusDeleteOnRelease(lease, b.configForRun())
}

func incusReleaseAction(cfg Config) string {
	if cfg.Incus.DeleteOnRelease {
		return "delete"
	}
	return "stop"
}

func incusDeleteOnRelease(lease LeaseTarget, cfg Config) bool {
	if core.DeleteOnReleaseExplicit(cfg, providerName) {
		return cfg.Incus.DeleteOnRelease
	}
	if lease.Server.Labels != nil {
		switch strings.ToLower(strings.TrimSpace(lease.Server.Labels["release"])) {
		case "delete":
			return true
		case "stop":
			return false
		}
	}
	return cfg.Incus.DeleteOnRelease
}

func (b *backend) Touch(ctx context.Context, req TouchRequest) (core.Server, error) {
	cfg := b.configForRun()
	client, err := newClient(cfg)
	if err != nil {
		return core.Server{}, err
	}
	server := req.Lease.Server
	server.Labels = core.TouchDirectLeaseLabels(server.Labels, cfg, req.State, time.Now().UTC())
	name := strings.TrimSpace(core.Blank(server.CloudID, server.Labels["instance"]))
	if name == "" {
		return core.Server{}, core.Exit(2, "provider=%s touch requires an Incus instance name", providerName)
	}
	if err := setInstanceLabels(ctx, client, name, server.Labels); err != nil {
		return core.Server{}, err
	}
	return server, nil
}

func (b *backend) Cleanup(ctx context.Context, req CleanupRequest) error {
	cfg := b.configForRun()
	client, err := newClient(cfg)
	if err != nil {
		return err
	}
	instances, err := client.ListInstances()
	if err != nil {
		return err
	}
	for _, inst := range instances {
		if !isCrabboxInstance(inst) {
			continue
		}
		server := serverFromInstance(inst, nil, cfg)
		shouldDelete, reason := core.ShouldCleanupServer(server, time.Now().UTC())
		if !shouldDelete {
			fmt.Fprintf(b.rt.Stderr, "skip instance name=%s reason=%s\n", inst.Name, reason)
			continue
		}
		fmt.Fprintf(b.rt.Stdout, "remove instance name=%s lease=%s reason=%s\n", inst.Name, core.Blank(server.Labels["lease"], "-"), reason)
		if req.DryRun {
			continue
		}
		if inst.IsActive() {
			if err := client.SetInstanceState(inst.Name, api.InstanceStatePut{Action: "stop", Force: true, Timeout: durationSecondsCeil(cfg.Incus.StartTimeout)}, ""); err != nil {
				return err
			}
		}
		if err := client.DeleteInstance(inst.Name); err != nil {
			return err
		}
		if leaseID := strings.TrimSpace(server.Labels["lease"]); leaseID != "" {
			core.RemoveLeaseClaim(leaseID)
			core.RemoveStoredTestboxKey(leaseID)
		}
	}
	return nil
}

func (b *backend) waitForAddress(ctx context.Context, client instanceClient, name string) (*api.Instance, string, error) {
	cfg := b.configForRun()
	timeout := cfg.Incus.StartTimeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	for {
		inst, etag, err := client.GetInstance(name)
		if err != nil {
			return nil, "", err
		}
		state, _, stateErr := client.GetInstanceState(name)
		if stateErr != nil {
			return nil, "", stateErr
		}
		if inst.Config == nil {
			inst.Config = map[string]string{}
		}
		delete(inst.Config, labelKey("host"))
		if host := instanceHost(*inst, state, cfg); host != "" {
			inst.Config[labelKey("host")] = host
			return inst, etag, nil
		}
		if time.Now().After(deadline) {
			return nil, "", core.Exit(5, "timed out waiting for Incus address for %s", name)
		}
		select {
		case <-ctx.Done():
			return nil, "", context.Cause(ctx)
		case <-time.After(2 * time.Second):
		}
	}
}

func (b *backend) resolveInstance(ctx context.Context, client instanceClient, identifier string) (api.Instance, core.Server, string, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return api.Instance{}, core.Server{}, "", core.Exit(2, "provider=%s requires --id <lease-id-or-slug-or-instance>", providerName)
	}
	instances, err := client.ListInstances()
	if err != nil {
		return api.Instance{}, core.Server{}, "", err
	}
	servers := make([]core.Server, 0, len(instances))
	byName := make(map[string]api.Instance, len(instances))
	for _, inst := range instances {
		if !isCrabboxInstance(inst) {
			continue
		}
		server := serverFromInstance(inst, nil, b.configForRun())
		servers = append(servers, server)
		byName[inst.Name] = inst
	}
	server, leaseID, err := core.FindServerByAlias(servers, identifier)
	if err != nil {
		return api.Instance{}, core.Server{}, "", err
	}
	if server.Name != "" {
		inst, ok := byName[server.Name]
		if !ok {
			return api.Instance{}, core.Server{}, "", core.Exit(4, "lease/server not found: %s", identifier)
		}
		return inst, server, leaseID, nil
	}
	inst, _, err := client.GetInstance(identifier)
	if err != nil {
		return api.Instance{}, core.Server{}, "", core.Exit(4, "lease/server not found: %s", identifier)
	}
	if !isCrabboxInstance(*inst) {
		return api.Instance{}, core.Server{}, "", core.Exit(4, "lease/server not found: %s (instance exists but is not Crabbox-managed)", identifier)
	}
	server = serverFromInstance(*inst, nil, b.configForRun())
	return *inst, server, strings.TrimSpace(server.Labels["lease"]), nil
}

func instanceConfigForCreate(cfg Config, labels map[string]string, publicKey string) map[string]string {
	bootstrapCfg := cfg
	if strings.TrimSpace(cfg.Incus.ProxyListenPort) != "" {
		bootstrapCfg.SSHPort = core.Blank(strings.TrimSpace(cfg.Incus.LaunchPort), "22")
		bootstrapCfg.SSHFallbackPorts = nil
	}
	config := map[string]string{
		"cloud-init.user-data": core.CloudInitUserData(bootstrapCfg, publicKey),
	}
	for key, value := range labels {
		config[labelKey(key)] = value
	}
	return config
}

func profilesForConfig(cfg Config) []string {
	profile := strings.TrimSpace(cfg.Incus.Profile)
	if profile == "" {
		return nil
	}
	return []string{profile}
}

func devicesForCreate(cfg Config) api.DevicesMap {
	port := strings.TrimSpace(cfg.Incus.ProxyListenPort)
	if port == "" {
		return nil
	}
	deviceName := strings.TrimSpace(cfg.Incus.ProxyDevice)
	if deviceName == "" {
		deviceName = "crabbox-ssh"
	}
	host := strings.TrimSpace(cfg.Incus.ProxyListenHost)
	if host == "" {
		host = "127.0.0.1"
	}
	guestPort := core.Blank(strings.TrimSpace(cfg.Incus.LaunchPort), "22")
	device := map[string]string{
		"type":    "proxy",
		"listen":  fmt.Sprintf("tcp:%s:%s", host, port),
		"connect": fmt.Sprintf("tcp:0.0.0.0:%s", guestPort),
		"bind":    "host",
	}
	return api.DevicesMap{
		deviceName: device,
	}
}

func setInstanceLabels(ctx context.Context, client instanceClient, name string, labels map[string]string) error {
	inst, etag, err := client.GetInstance(name)
	if err != nil {
		return err
	}
	put := inst.Writable()
	if put.Config == nil {
		put.Config = map[string]string{}
	}
	for key := range put.Config {
		if strings.HasPrefix(key, labelPrefix) {
			delete(put.Config, key)
		}
	}
	for key, value := range labels {
		put.Config[labelKey(key)] = value
	}
	return client.UpdateInstance(name, put, etag)
}

func serverFromInstance(inst api.Instance, state *api.InstanceState, cfg Config) core.Server {
	server := core.Server{
		CloudID:  inst.Name,
		Provider: providerName,
		Name:     inst.Name,
		Status:   strings.ToLower(strings.TrimSpace(inst.Status)),
		Labels:   labelsFromInstance(inst),
	}
	server.ServerType.Name = core.Blank(server.Labels["server_type"], core.IncusServerTypeForConfig(cfg))
	server.PublicNet.IPv4.IP = instanceHost(inst, state, cfg)
	return server
}

func instanceHost(inst api.Instance, state *api.InstanceState, cfg Config) string {
	if strings.TrimSpace(cfg.Incus.ProxyListenPort) != "" {
		if host := sshHostForConfig(cfg); host != "" {
			return host
		}
	}
	if host := persistedProxyHost(labelsFromInstance(inst)); host != "" {
		return host
	}
	return bestAddress(inst, state)
}

func sshTargetHost(server core.Server, cfg Config) string {
	if strings.TrimSpace(cfg.Incus.ProxyListenPort) != "" {
		if host := sshHostForConfig(cfg); host != "" {
			return host
		}
	}
	if host := persistedProxyHost(server.Labels); host != "" {
		return host
	}
	return server.PublicNet.IPv4.IP
}

func persistedProxyHost(labels map[string]string) string {
	if strings.TrimSpace(labels["proxy_port"]) == "" {
		return ""
	}
	return strings.TrimSpace(labels["proxy_host"])
}

func bestAddress(inst api.Instance, state *api.InstanceState) string {
	if host := bestStateAddress(state); host != "" {
		return host
	}
	if host := labelValue(inst, "host"); host != "" {
		return host
	}
	return bestInstanceAddress(inst)
}

func bestStateAddress(state *api.InstanceState) string {
	if state == nil || len(state.Network) == 0 {
		return ""
	}
	names := make([]string, 0, len(state.Network))
	for name := range state.Network {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		network := state.Network[name]
		for _, address := range network.Addresses {
			if address.Family == "inet" && address.Scope == "global" && address.Address != "" {
				return address.Address
			}
		}
	}
	return ""
}

func bestInstanceAddress(inst api.Instance) string {
	for _, device := range inst.ExpandedDevices {
		if strings.EqualFold(device["type"], "nic") {
			if addr := strings.TrimSpace(device["ipv4.address"]); addr != "" {
				return addr
			}
		}
	}
	return ""
}

func labelsFromInstance(inst api.Instance) map[string]string {
	labels := map[string]string{}
	for key, value := range inst.Config {
		if strings.HasPrefix(key, labelPrefix) {
			labels[strings.TrimPrefix(key, labelPrefix)] = value
		}
	}
	if labels["instance"] == "" {
		labels["instance"] = inst.Name
	}
	return labels
}

func isCrabboxInstance(inst api.Instance) bool {
	return strings.EqualFold(strings.TrimSpace(labelValue(inst, "crabbox")), "true")
}

func labelValue(inst api.Instance, key string) string {
	return strings.TrimSpace(inst.Config[labelKey(key)])
}

const labelPrefix = "user.crabbox."

func labelKey(key string) string {
	return labelPrefix + key
}

func instanceScope(name string) string {
	return "instance:" + strings.TrimSpace(name)
}

func normalizeInstanceType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "container", "containers":
		return "container"
	case "vm", "virtual-machine", "virtual_machine":
		return "virtual-machine"
	default:
		return ""
	}
}

func durationSecondsCeil(duration time.Duration) int {
	if duration <= 0 {
		return 0
	}
	seconds := duration / time.Second
	if duration%time.Second != 0 {
		seconds++
	}
	return int(seconds)
}
