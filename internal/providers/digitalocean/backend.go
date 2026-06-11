package digitalocean

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type digitalOceanAPI interface {
	ListCrabboxDroplets(context.Context) ([]droplet, error)
	GetDroplet(context.Context, int64) (droplet, error)
	CreateDroplet(context.Context, core.Config, string, string, string, bool, time.Time) (droplet, error)
	DeleteDroplet(context.Context, int64) error
	DeleteSSHKeyByName(context.Context, string) error
	ReplaceDropletTags(context.Context, int64, []string, []string) error
}

type digitalOceanLeaseBackend struct {
	shared.DirectSSHBackend
	clientFactory func(core.Runtime) (digitalOceanAPI, error)
	waitSSH       func(context.Context, *core.SSHTarget, string, time.Duration) error
}

var claimLeaseTargetForRepoConfig = core.ClaimLeaseTargetForRepoConfig

func NewDigitalOceanLeaseBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) core.Backend {
	cfg.Provider = providerName
	applyDigitalOceanDefaults(&cfg)
	b := &digitalOceanLeaseBackend{DirectSSHBackend: shared.DirectSSHBackend{SpecValue: spec, Cfg: cfg, RT: rt}}
	b.clientFactory = func(rt core.Runtime) (digitalOceanAPI, error) { return newDigitalOceanClient(rt) }
	b.waitSSH = func(ctx context.Context, target *core.SSHTarget, phase string, timeout time.Duration) error {
		return core.WaitForSSHReady(ctx, target, b.RT.Stderr, phase, timeout)
	}
	b.Delete = b.deleteServer
	return b
}

func (b *digitalOceanLeaseBackend) Acquire(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	return shared.AcquireAttemptsRetry(b.RT, req.Keep, func() (core.LeaseTarget, error) {
		return b.acquireOnce(ctx, req)
	})
}

func (b *digitalOceanLeaseBackend) acquireOnce(ctx context.Context, req core.AcquireRequest) (target core.LeaseTarget, err error) {
	cfg := b.Cfg
	if cfg.TargetOS != "" && cfg.TargetOS != core.TargetLinux {
		return core.LeaseTarget{}, core.Exit(2, "provider=digitalocean only supports target=linux")
	}
	if cfg.Tailscale.Enabled && cfg.Tailscale.AuthKey == "" {
		return core.LeaseTarget{}, core.Exit(2, "direct --tailscale requires %s to contain a Tailscale auth key", cfg.Tailscale.AuthKeyEnv)
	}
	client, err := b.clientFactory(b.RT)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	leaseID := core.NewLeaseID()
	droplets, err := client.ListCrabboxDroplets(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	servers := make([]core.Server, 0, len(droplets))
	for _, item := range droplets {
		servers = append(servers, serverFromDroplet(item, cfg))
	}
	slug, err := core.AllocateDirectLeaseSlug(leaseID, req.RequestedSlug, servers)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	keyPath, publicKey, err := core.EnsureTestboxKeyForConfig(cfg, leaseID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	cfg.SSHKey = keyPath
	cfg.ProviderKey = providerKeyForLease(leaseID)
	if cfg.ServerType == "" {
		cfg.ServerType = digitalOceanServerTypeForClass(cfg.Class)
	}
	if cfg.Tailscale.Enabled && cfg.Tailscale.Hostname == "" {
		cfg.Tailscale.Hostname = core.RenderTailscaleHostname(cfg.Tailscale.HostnameTemplate, leaseID, slug, cfg.Provider)
	}
	now := b.now()
	fmt.Fprintf(b.RT.Stderr, "provisioning provider=digitalocean lease=%s slug=%s type=%s region=%s image=%s keep=%v\n", leaseID, slug, cfg.ServerType, digitalOceanRegion(cfg), digitalOceanImage(cfg), req.Keep)
	created, err := client.CreateDroplet(ctx, cfg, publicKey, leaseID, slug, req.Keep, now)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	committed := false
	defer func() {
		if err == nil || committed {
			return
		}
		if cleanupErr := rollbackDigitalOceanAcquire(client, created.ID, providerKeyForLease(leaseID)); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("digitalocean cleanup failed: %w", cleanupErr))
		}
	}()
	created, err = b.waitForDropletIP(ctx, client, created.ID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	server := serverFromDroplet(created, cfg)
	ssh := core.SSHTargetFromConfig(cfg, server.PublicNet.IPv4.IP)
	if err := b.waitSSH(ctx, &ssh, "digitalocean bootstrap", core.BootstrapWaitTimeout(cfg)); err != nil {
		return core.LeaseTarget{}, err
	}
	readyTags := leaseTags(cfg, leaseID, slug, "ready", req.Keep, b.now())
	if err := client.ReplaceDropletTags(ctx, created.ID, created.Tags, readyTags); err != nil {
		return core.LeaseTarget{}, err
	} else {
		server.Labels = labelsFromTags(readyTags)
	}
	server.Status = "ready"
	if err := claimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, ssh, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
		return core.LeaseTarget{}, err
	}
	committed = true
	fmt.Fprintf(b.RT.Stderr, "provisioned lease=%s droplet=%s type=%s\n", leaseID, server.DisplayID(), cfg.ServerType)
	return core.LeaseTarget{Server: server, SSH: ssh, LeaseID: leaseID}, nil
}

func (b *digitalOceanLeaseBackend) Resolve(ctx context.Context, req core.ResolveRequest) (core.LeaseTarget, error) {
	client, err := b.clientFactory(b.RT)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	droplets, err := client.ListCrabboxDroplets(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	servers := make([]core.Server, 0, len(droplets))
	byID := map[int64]droplet{}
	for _, item := range droplets {
		server := serverFromDroplet(item, b.Cfg)
		servers = append(servers, server)
		byID[server.ID] = item
	}
	server, leaseID, err := core.FindServerByAlias(servers, req.ID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if leaseID != "" {
		return b.targetFromDroplet(byID[server.ID], req)
	}
	if id, ok := parseDropletID(req.ID); ok {
		item, err := client.GetDroplet(ctx, id)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		return b.targetFromDroplet(item, req)
	}
	return core.LeaseTarget{}, core.Exit(4, "lease/droplet not found: %s", req.ID)
}

func (b *digitalOceanLeaseBackend) targetFromDroplet(item droplet, req core.ResolveRequest) (core.LeaseTarget, error) {
	if err := validateDropletLabels(labelsFromTags(item.Tags)); err != nil {
		return core.LeaseTarget{}, err
	}
	server := serverFromDroplet(item, b.Cfg)
	leaseID := server.Labels["lease"]
	if req.ReleaseOnly {
		return core.LeaseTarget{Server: server, LeaseID: leaseID}, nil
	}
	ssh := core.SSHTargetFromConfig(b.Cfg, server.PublicNet.IPv4.IP)
	if keyPath, err := core.TestboxKeyPath(leaseID); err == nil {
		if _, statErr := os.Stat(keyPath); statErr == nil {
			ssh.Key = keyPath
		}
	}
	if req.Repo.Root != "" {
		if err := claimLeaseTargetForRepoConfig(leaseID, server.Labels["slug"], b.Cfg, server, ssh, req.Repo.Root, b.Cfg.IdleTimeout, req.Reclaim); err != nil {
			return core.LeaseTarget{}, err
		}
	}
	return core.LeaseTarget{Server: server, SSH: ssh, LeaseID: leaseID}, nil
}

func (b *digitalOceanLeaseBackend) List(ctx context.Context, req core.ListRequest) ([]core.LeaseView, error) {
	_ = req
	client, err := b.clientFactory(b.RT)
	if err != nil {
		return nil, err
	}
	droplets, err := client.ListCrabboxDroplets(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]core.LeaseView, 0, len(droplets))
	for _, item := range droplets {
		out = append(out, serverFromDroplet(item, b.Cfg))
	}
	return out, nil
}

func (b *digitalOceanLeaseBackend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	leases, err := b.List(ctx, core.ListRequest{})
	if err != nil {
		return core.DoctorResult{}, err
	}
	result := core.InventoryDoctorResult(providerName, len(leases))
	result.Message += fmt.Sprintf(" default_type=%s region=%s image=%s", b.Cfg.ServerType, digitalOceanRegion(b.Cfg), digitalOceanImage(b.Cfg))
	return result, nil
}

func (b *digitalOceanLeaseBackend) ReleaseLease(ctx context.Context, req core.ReleaseLeaseRequest) error {
	if err := b.deleteServer(ctx, b.Cfg, req.Lease.Server); err != nil {
		return err
	}
	core.RemoveLeaseClaim(req.Lease.LeaseID)
	return nil
}

func (b *digitalOceanLeaseBackend) ReleaseLeaseMessage(lease core.LeaseTarget) string {
	return fmt.Sprintf("deleted lease=%s droplet=%s name=%s", lease.LeaseID, lease.Server.DisplayID(), lease.Server.Name)
}

func (b *digitalOceanLeaseBackend) Touch(ctx context.Context, req core.TouchRequest) (core.Server, error) {
	server := req.Lease.Server
	if err := validateDropletLabels(server.Labels); err != nil {
		return core.Server{}, err
	}
	client, err := b.clientFactory(b.RT)
	if err != nil {
		return core.Server{}, err
	}
	cfg := b.Cfg
	labels := server.Labels
	if req.IdleTimeout > 0 {
		cfg.IdleTimeout = req.IdleTimeout
		labels = make(map[string]string, len(server.Labels))
		for key, value := range server.Labels {
			labels[key] = value
		}
		delete(labels, "idle_timeout")
		delete(labels, "idle_timeout_secs")
	}
	labels = core.TouchDirectLeaseLabels(labels, cfg, req.State, b.now())
	item, err := client.GetDroplet(ctx, server.ID)
	if err != nil {
		return core.Server{}, err
	}
	if err := client.ReplaceDropletTags(ctx, server.ID, item.Tags, tagsFromLabels(labels)); err != nil {
		return core.Server{}, err
	}
	server.Labels = labels
	return server, nil
}

func (b *digitalOceanLeaseBackend) Cleanup(ctx context.Context, req core.CleanupRequest) error {
	servers, err := b.List(ctx, core.ListRequest{Options: req.Options})
	if err != nil {
		return err
	}
	return b.CleanupServers(ctx, req, servers)
}

func (b *digitalOceanLeaseBackend) deleteServer(ctx context.Context, cfg core.Config, server core.Server) error {
	if err := validateDropletLabels(server.Labels); err != nil {
		return err
	}
	client, err := b.clientFactory(b.RT)
	if err != nil {
		return err
	}
	if err := client.DeleteDroplet(ctx, server.ID); err != nil {
		return err
	}
	if keyName := core.ServerProviderKey(server); core.ValidCrabboxProviderKey(keyName) {
		return client.DeleteSSHKeyByName(ctx, keyName)
	}
	return nil
}

func (b *digitalOceanLeaseBackend) waitForDropletIP(ctx context.Context, client digitalOceanAPI, id int64) (droplet, error) {
	deadline := time.Now().Add(5 * time.Minute)
	for {
		item, err := client.GetDroplet(ctx, id)
		if err != nil {
			return droplet{}, err
		}
		if publicIPv4(item) != "" {
			return item, nil
		}
		if time.Now().After(deadline) {
			return droplet{}, core.Exit(5, "timed out waiting for DigitalOcean Droplet IP")
		}
		time.Sleep(3 * time.Second)
	}
}

func (b *digitalOceanLeaseBackend) now() time.Time {
	if b.RT.Clock != nil {
		return b.RT.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func rollbackDigitalOceanAcquire(client digitalOceanAPI, dropletID int64, keyName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var errs []error
	if dropletID != 0 {
		if err := client.DeleteDroplet(ctx, dropletID); err != nil {
			errs = append(errs, err)
		}
	}
	if keyName != "" {
		if err := client.DeleteSSHKeyByName(ctx, keyName); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func serverFromDroplet(item droplet, cfg core.Config) core.Server {
	labels := labelsFromTags(item.Tags)
	if labels["provider_key"] == "" && labels["lease"] != "" {
		labels["provider_key"] = providerKeyForLease(labels["lease"])
	}
	server := core.Server{
		CloudID:  strconv.FormatInt(item.ID, 10),
		Provider: providerName,
		ID:       item.ID,
		Name:     item.Name,
		Status:   normalizeDropletStatus(item.Status),
		Labels:   labels,
	}
	server.PublicNet.IPv4.IP = publicIPv4(item)
	server.ServerType.Name = firstNonBlank(item.Size.Slug, cfg.ServerType)
	return server
}

func publicIPv4(item droplet) string {
	for _, net := range item.Networks.V4 {
		if net.Type == "public" && net.IPAddress != "" {
			return net.IPAddress
		}
	}
	for _, net := range item.Networks.V4 {
		if net.IPAddress != "" {
			return net.IPAddress
		}
	}
	return ""
}

func normalizeDropletStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "active":
		return "ready"
	case "":
		return "unknown"
	default:
		return status
	}
}

func parseDropletID(id string) (int64, bool) {
	if strings.TrimSpace(id) == "" || strings.HasPrefix(id, "cbx_") {
		return 0, false
	}
	parsed, err := strconv.ParseInt(id, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, false
	}
	return parsed, true
}

func applyDigitalOceanDefaults(cfg *core.Config) {
	cfg.Provider = providerName
	if cfg.TargetOS == "" {
		cfg.TargetOS = core.TargetLinux
	}
	if cfg.DigitalOcean.Region == "" {
		cfg.DigitalOcean.Region = "nyc3"
	}
	if cfg.DigitalOcean.Image == "" {
		cfg.DigitalOcean.Image = "ubuntu-24-04-x64"
	}
	if cfg.ServerType == "" {
		cfg.ServerType = digitalOceanServerTypeForClass(cfg.Class)
	}
	if cfg.SSHUser == "" || cfg.SSHUser == "crabbox" {
		cfg.SSHUser = "root"
	}
	if cfg.SSHPort == "" {
		cfg.SSHPort = "22"
	}
	cfg.SSHFallbackPorts = nil
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
