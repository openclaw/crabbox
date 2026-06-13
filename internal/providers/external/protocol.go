package external

import (
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const protocolVersion = 1

type protocolRequest struct {
	ProtocolVersion int                               `json:"protocolVersion"`
	Operation       string                            `json:"operation"`
	Config          map[string]any                    `json:"config,omitempty"`
	Desired         *desiredLease                     `json:"desired,omitempty"`
	Lease           *protocolLease                    `json:"lease,omitempty"`
	Expected        *protocolExpectedProviderIdentity `json:"expected,omitempty"`
	ID              string                            `json:"id,omitempty"`
	State           string                            `json:"state,omitempty"`
	Keep            bool                              `json:"keep,omitempty"`
	Reclaim         bool                              `json:"reclaim,omitempty"`
	ReleaseOnly     bool                              `json:"releaseOnly,omitempty"`
	Force           bool                              `json:"force,omitempty"`
	All             bool                              `json:"all,omitempty"`
	Refresh         bool                              `json:"refresh,omitempty"`
	DryRun          bool                              `json:"dryRun,omitempty"`
	Repo            *protocolRepo                     `json:"repo,omitempty"`
}

type protocolExpectedProviderIdentity struct {
	LeaseID        string `json:"leaseId,omitempty"`
	AttemptLeaseID string `json:"attemptLeaseId,omitempty"`
	Slug           string `json:"slug,omitempty"`
	CloudID        string `json:"cloudId,omitempty"`
}

type desiredLease struct {
	LeaseID string `json:"leaseId"`
	Slug    string `json:"slug"`
	Name    string `json:"name"`
}

type protocolRepo struct {
	Root      string `json:"root,omitempty"`
	Name      string `json:"name,omitempty"`
	RemoteURL string `json:"remoteUrl,omitempty"`
	Head      string `json:"head,omitempty"`
	BaseRef   string `json:"baseRef,omitempty"`
}

type protocolResponse struct {
	ProtocolVersion      int             `json:"protocolVersion,omitempty"`
	Lease                *protocolLease  `json:"lease,omitempty"`
	Leases               []protocolLease `json:"leases,omitempty"`
	Message              string          `json:"message,omitempty"`
	Error                string          `json:"error,omitempty"`
	SynthesizedIdentity  bool            `json:"-"`
	RawLifecycleIdentity bool            `json:"-"`
}

type protocolLease struct {
	LeaseID    string            `json:"leaseId,omitempty"`
	Slug       string            `json:"slug,omitempty"`
	Name       string            `json:"name,omitempty"`
	CloudID    string            `json:"cloudId,omitempty"`
	Status     string            `json:"status,omitempty"`
	ServerType string            `json:"serverType,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	SSH        *protocolSSH      `json:"ssh,omitempty"`
}

type protocolSSH struct {
	User            string   `json:"user,omitempty"`
	Host            string   `json:"host,omitempty"`
	Key             string   `json:"key,omitempty"`
	Port            string   `json:"port,omitempty"`
	FallbackPorts   []string `json:"fallbackPorts,omitempty"`
	ReadyCheck      string   `json:"readyCheck,omitempty"`
	AuthSecret      bool     `json:"authSecret,omitempty"`
	NoControlMaster bool     `json:"noControlMaster,omitempty"`
	SSHConfigProxy  bool     `json:"sshConfigProxy,omitempty"`
	ProxyCommand    string   `json:"proxyCommand,omitempty"`
}

func repoForProtocol(repo core.Repo) *protocolRepo {
	if repo == (core.Repo{}) {
		return nil
	}
	return &protocolRepo{
		Root:      repo.Root,
		Name:      repo.Name,
		RemoteURL: repo.RemoteURL,
		Head:      repo.Head,
		BaseRef:   repo.BaseRef,
	}
}

func leaseForProtocol(lease core.LeaseTarget) *protocolLease {
	server := lease.Server
	result := &protocolLease{
		LeaseID:    lease.LeaseID,
		Slug:       server.Labels["slug"],
		Name:       server.Name,
		CloudID:    server.CloudID,
		Status:     server.Status,
		ServerType: server.ServerType.Name,
		Labels:     server.Labels,
	}
	if lease.SSH.Host != "" {
		result.SSH = &protocolSSH{
			User:            lease.SSH.User,
			Host:            lease.SSH.Host,
			Key:             lease.SSH.Key,
			Port:            lease.SSH.Port,
			FallbackPorts:   lease.SSH.FallbackPorts,
			ReadyCheck:      lease.SSH.ReadyCheck,
			AuthSecret:      lease.SSH.AuthSecret,
			NoControlMaster: lease.SSH.NoControlMaster,
			SSHConfigProxy:  lease.SSH.SSHConfigProxy,
			ProxyCommand:    lease.SSH.ProxyCommand,
		}
	}
	return result
}

func (p protocolLease) target(cfg core.Config, keep bool) core.LeaseTarget {
	leaseID := p.LeaseID
	slug := core.NormalizeLeaseSlug(p.Slug)
	if slug == "" {
		slug = core.NormalizeLeaseSlug(p.Labels["slug"])
	}
	labels := make(map[string]string, len(p.Labels)+8)
	for key, value := range p.Labels {
		labels[key] = value
	}
	defaults := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", keep, time.Now().UTC())
	for key, value := range defaults {
		if labels[key] == "" {
			labels[key] = value
		}
	}
	status := core.Blank(p.Status, "ready")
	labels["name"] = p.Name
	labels["state"] = core.Blank(labels["state"], status)
	server := core.Server{
		CloudID:  core.Blank(p.CloudID, p.Name),
		Provider: providerName,
		Name:     p.Name,
		Status:   status,
		Labels:   labels,
	}
	server.ServerType.Name = core.Blank(p.ServerType, "external")
	target := core.SSHTarget{TargetOS: core.TargetLinux, NetworkKind: core.NetworkPublic}
	if p.SSH != nil {
		target.User = p.SSH.User
		target.Host = p.SSH.Host
		target.Key = p.SSH.Key
		target.Port = core.Blank(p.SSH.Port, "22")
		target.FallbackPorts = p.SSH.FallbackPorts
		target.ReadyCheck = core.Blank(p.SSH.ReadyCheck, externalDefaultReadyCheck)
		target.AuthSecret = p.SSH.AuthSecret
		target.NoControlMaster = p.SSH.NoControlMaster
		target.ProxyCommand = p.SSH.ProxyCommand
		target.SSHConfigProxy = p.SSH.SSHConfigProxy || strings.TrimSpace(target.ProxyCommand) != ""
		server.PublicNet.IPv4.IP = target.Host
	}
	return core.LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}
}

const externalDefaultReadyCheck = "command -v bash >/dev/null && command -v python3 >/dev/null && command -v git >/dev/null && command -v rsync >/dev/null && command -v tar >/dev/null"
