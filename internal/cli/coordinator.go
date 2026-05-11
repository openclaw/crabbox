package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type CoordinatorClient struct {
	BaseURL string
	Token   string
	Access  AccessConfig
	Client  *http.Client
}

type CoordinatorHTTPError struct {
	Method     string
	Path       string
	StatusCode int
	Message    string
}

func (e CoordinatorHTTPError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("coordinator %s %s: http %d: %s", e.Method, e.Path, e.StatusCode, e.Message)
	}
	return fmt.Sprintf("coordinator %s %s: http %d", e.Method, e.Path, e.StatusCode)
}

type CoordinatorLease struct {
	ID                   string                `json:"id"`
	Slug                 string                `json:"slug,omitempty"`
	Provider             string                `json:"provider"`
	TargetOS             string                `json:"target,omitempty"`
	WindowsMode          string                `json:"windowsMode,omitempty"`
	Desktop              bool                  `json:"desktop,omitempty"`
	Browser              bool                  `json:"browser,omitempty"`
	Code                 bool                  `json:"code,omitempty"`
	Tailscale            *TailscaleMetadata    `json:"tailscale,omitempty"`
	Region               string                `json:"region,omitempty"`
	Owner                string                `json:"owner"`
	Org                  string                `json:"org"`
	Share                *CoordinatorShare     `json:"share,omitempty"`
	Profile              string                `json:"profile"`
	Class                string                `json:"class"`
	ServerType           string                `json:"serverType"`
	RequestedServerType  string                `json:"requestedServerType,omitempty"`
	Market               string                `json:"market,omitempty"`
	ProvisioningAttempts []ProvisioningAttempt `json:"provisioningAttempts,omitempty"`
	CapacityHints        []CapacityHint        `json:"capacityHints,omitempty"`
	ServerID             int64                 `json:"serverID"`
	CloudID              string                `json:"cloudID"`
	ServerName           string                `json:"serverName"`
	Host                 string                `json:"host"`
	SSHUser              string                `json:"sshUser"`
	SSHPort              string                `json:"sshPort"`
	SSHFallbackPorts     []string              `json:"sshFallbackPorts,omitempty"`
	WorkRoot             string                `json:"workRoot"`
	Keep                 bool                  `json:"keep"`
	State                string                `json:"state"`
	TTLSeconds           int                   `json:"ttlSeconds,omitempty"`
	IdleTimeoutSeconds   int                   `json:"idleTimeoutSeconds,omitempty"`
	CreatedAt            string                `json:"createdAt,omitempty"`
	UpdatedAt            string                `json:"updatedAt,omitempty"`
	LastTouchedAt        string                `json:"lastTouchedAt,omitempty"`
	ExpiresAt            string                `json:"expiresAt"`
	Telemetry            *LeaseTelemetry       `json:"telemetry,omitempty"`
	TelemetryHistory     []*LeaseTelemetry     `json:"telemetryHistory,omitempty"`
}

type CoordinatorShareRole string

const (
	CoordinatorShareUse    CoordinatorShareRole = "use"
	CoordinatorShareManage CoordinatorShareRole = "manage"
)

type CoordinatorShare struct {
	Users     map[string]CoordinatorShareRole `json:"users,omitempty"`
	Org       CoordinatorShareRole            `json:"org,omitempty"`
	UpdatedAt string                          `json:"updatedAt,omitempty"`
	UpdatedBy string                          `json:"updatedBy,omitempty"`
}

type ProvisioningAttempt struct {
	Region     string `json:"region,omitempty"`
	ServerType string `json:"serverType"`
	Market     string `json:"market,omitempty"`
	Category   string `json:"category,omitempty"`
	Message    string `json:"message"`
}

type CapacityHint struct {
	Code         string   `json:"code"`
	Message      string   `json:"message"`
	Action       string   `json:"action,omitempty"`
	Region       string   `json:"region,omitempty"`
	Market       string   `json:"market,omitempty"`
	Class        string   `json:"class,omitempty"`
	ServerType   string   `json:"serverType,omitempty"`
	RegionsTried []string `json:"regionsTried,omitempty"`
}

type CoordinatorMachine struct {
	ID         CoordinatorID     `json:"id"`
	Provider   string            `json:"provider"`
	CloudID    string            `json:"cloudID"`
	Name       string            `json:"name"`
	Status     string            `json:"status"`
	ServerType string            `json:"serverType"`
	Host       string            `json:"host"`
	Labels     map[string]string `json:"labels"`
}

type CoordinatorUsageResponse struct {
	Usage  CoordinatorUsageSummary `json:"usage"`
	Limits CoordinatorCostLimits   `json:"limits"`
}

type CoordinatorWhoami struct {
	Owner string `json:"owner"`
	Org   string `json:"org"`
	Auth  string `json:"auth"`
}

type CoordinatorProviderReadiness struct {
	Provider   string   `json:"provider"`
	Configured bool     `json:"configured"`
	Missing    []string `json:"missing,omitempty"`
	Message    string   `json:"message,omitempty"`
}

type CoordinatorImage struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	State      string `json:"state"`
	Region     string `json:"region,omitempty"`
	PromotedAt string `json:"promotedAt,omitempty"`
}

type CoordinatorGitHubLoginStart struct {
	LoginID   string `json:"loginID"`
	URL       string `json:"url"`
	ExpiresAt string `json:"expiresAt"`
}

type CoordinatorGitHubLoginPoll struct {
	Status   string `json:"status"`
	Token    string `json:"token,omitempty"`
	Owner    string `json:"owner,omitempty"`
	Org      string `json:"org,omitempty"`
	Login    string `json:"login,omitempty"`
	Provider string `json:"provider,omitempty"`
	Error    string `json:"error,omitempty"`
}

type CoordinatorWebVNCTicket struct {
	Ticket    string `json:"ticket"`
	LeaseID   string `json:"leaseID"`
	ExpiresAt string `json:"expiresAt"`
}

type CoordinatorWebVNCEvent struct {
	At     string `json:"at"`
	Event  string `json:"event"`
	Reason string `json:"reason,omitempty"`
}

type CoordinatorWebVNCStatus struct {
	LeaseID              string                   `json:"leaseID"`
	Slug                 string                   `json:"slug,omitempty"`
	BridgeConnected      bool                     `json:"bridgeConnected"`
	ViewerConnected      bool                     `json:"viewerConnected"`
	ViewerCount          int                      `json:"viewerCount,omitempty"`
	ObserverCount        int                      `json:"observerCount,omitempty"`
	AvailableViewerSlots int                      `json:"availableViewerSlots,omitempty"`
	ControllerLabel      string                   `json:"controllerLabel,omitempty"`
	Command              string                   `json:"command"`
	Message              string                   `json:"message,omitempty"`
	Events               []CoordinatorWebVNCEvent `json:"events,omitempty"`
}

type CoordinatorWebVNCReset struct {
	LeaseID            string                   `json:"leaseID"`
	Slug               string                   `json:"slug,omitempty"`
	BridgeWasConnected bool                     `json:"bridgeWasConnected"`
	ViewerWasConnected bool                     `json:"viewerWasConnected"`
	Command            string                   `json:"command"`
	Events             []CoordinatorWebVNCEvent `json:"events,omitempty"`
}

type CoordinatorEgressTicket struct {
	Ticket    string `json:"ticket"`
	LeaseID   string `json:"leaseID"`
	Role      string `json:"role"`
	SessionID string `json:"sessionID"`
	ExpiresAt string `json:"expiresAt"`
}

type CoordinatorEgressStatus struct {
	LeaseID         string   `json:"leaseID"`
	Slug            string   `json:"slug,omitempty"`
	SessionID       string   `json:"sessionID,omitempty"`
	Profile         string   `json:"profile,omitempty"`
	Allow           []string `json:"allow,omitempty"`
	HostConnected   bool     `json:"hostConnected"`
	ClientConnected bool     `json:"clientConnected"`
	CreatedAt       string   `json:"createdAt,omitempty"`
	UpdatedAt       string   `json:"updatedAt,omitempty"`
}

type CoordinatorRunsResponse struct {
	Runs []CoordinatorRun `json:"runs"`
}

type CoordinatorRunResponse struct {
	Run CoordinatorRun `json:"run"`
}

type CoordinatorRun struct {
	ID           string               `json:"id"`
	LeaseID      string               `json:"leaseID"`
	Slug         string               `json:"slug,omitempty"`
	Owner        string               `json:"owner"`
	Org          string               `json:"org"`
	Provider     string               `json:"provider"`
	TargetOS     string               `json:"target,omitempty"`
	WindowsMode  string               `json:"windowsMode,omitempty"`
	Class        string               `json:"class"`
	ServerType   string               `json:"serverType"`
	Command      []string             `json:"command"`
	State        string               `json:"state"`
	Phase        string               `json:"phase,omitempty"`
	ExitCode     *int                 `json:"exitCode,omitempty"`
	SyncMs       int64                `json:"syncMs,omitempty"`
	CommandMs    int64                `json:"commandMs,omitempty"`
	DurationMs   int64                `json:"durationMs,omitempty"`
	LogBytes     int64                `json:"logBytes"`
	LogTruncated bool                 `json:"logTruncated"`
	Results      *TestResultSummary   `json:"results,omitempty"`
	Telemetry    *RunTelemetrySummary `json:"telemetry,omitempty"`
	StartedAt    string               `json:"startedAt"`
	LastEventAt  string               `json:"lastEventAt,omitempty"`
	EventCount   int                  `json:"eventCount,omitempty"`
	EndedAt      string               `json:"endedAt,omitempty"`
}

type CoordinatorRunEventsResponse struct {
	Events []CoordinatorRunEvent `json:"events"`
}

type CoordinatorExternalRunner struct {
	ID                   string `json:"id"`
	Provider             string `json:"provider,omitempty"`
	Status               string `json:"status,omitempty"`
	Repo                 string `json:"repo,omitempty"`
	Workflow             string `json:"workflow,omitempty"`
	Job                  string `json:"job,omitempty"`
	Ref                  string `json:"ref,omitempty"`
	CreatedAt            string `json:"createdAt,omitempty"`
	Created              string `json:"created,omitempty"`
	ActionsRepo          string `json:"actionsRepo,omitempty"`
	ActionsRunID         string `json:"actionsRunID,omitempty"`
	ActionsRunURL        string `json:"actionsRunURL,omitempty"`
	ActionsRunStatus     string `json:"actionsRunStatus,omitempty"`
	ActionsRunConclusion string `json:"actionsRunConclusion,omitempty"`
	ActionsWorkflowName  string `json:"actionsWorkflowName,omitempty"`
	ActionsWorkflowURL   string `json:"actionsWorkflowURL,omitempty"`
}

type CoordinatorExternalRunnerSyncResponse struct {
	Runners []CoordinatorExternalRunner `json:"runners"`
	Stale   []CoordinatorExternalRunner `json:"stale"`
}

type CoordinatorRunEventResponse struct {
	Event CoordinatorRunEvent `json:"event"`
}

type CoordinatorRunEvent struct {
	RunID       string `json:"runID"`
	Seq         int    `json:"seq"`
	Type        string `json:"type"`
	Phase       string `json:"phase,omitempty"`
	Stream      string `json:"stream,omitempty"`
	Message     string `json:"message,omitempty"`
	Data        string `json:"data,omitempty"`
	LeaseID     string `json:"leaseID,omitempty"`
	Slug        string `json:"slug,omitempty"`
	Provider    string `json:"provider,omitempty"`
	TargetOS    string `json:"target,omitempty"`
	WindowsMode string `json:"windowsMode,omitempty"`
	Class       string `json:"class,omitempty"`
	ServerType  string `json:"serverType,omitempty"`
	ExitCode    *int   `json:"exitCode,omitempty"`
	CreatedAt   string `json:"createdAt"`
}

type CoordinatorRunEventInput struct {
	Type        string `json:"type,omitempty"`
	Phase       string `json:"phase,omitempty"`
	Stream      string `json:"stream,omitempty"`
	Message     string `json:"message,omitempty"`
	Data        string `json:"data,omitempty"`
	LeaseID     string `json:"leaseID,omitempty"`
	Slug        string `json:"slug,omitempty"`
	Provider    string `json:"provider,omitempty"`
	TargetOS    string `json:"target,omitempty"`
	WindowsMode string `json:"windowsMode,omitempty"`
	Class       string `json:"class,omitempty"`
	ServerType  string `json:"serverType,omitempty"`
	ExitCode    *int   `json:"exitCode,omitempty"`
}

type CoordinatorArtifactUploadRequest struct {
	Prefix string                           `json:"prefix,omitempty"`
	Files  []CoordinatorArtifactUploadInput `json:"files"`
}

type CoordinatorArtifactUploadInput struct {
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	ContentType string `json:"contentType,omitempty"`
	SHA256      string `json:"sha256,omitempty"`
}

type CoordinatorArtifactUploadResponse struct {
	Backend   string                           `json:"backend"`
	Bucket    string                           `json:"bucket"`
	Prefix    string                           `json:"prefix"`
	ExpiresAt string                           `json:"expiresAt"`
	Files     []CoordinatorArtifactUploadGrant `json:"files"`
}

type CoordinatorArtifactUploadGrant struct {
	Name   string `json:"name"`
	Key    string `json:"key"`
	Upload struct {
		Method    string            `json:"method"`
		URL       string            `json:"url"`
		Headers   map[string]string `json:"headers"`
		ExpiresAt string            `json:"expiresAt"`
	} `json:"upload"`
	URL string `json:"url"`
}

type TestResultSummary struct {
	Format      string        `json:"format"`
	Files       []string      `json:"files"`
	Suites      int           `json:"suites"`
	Tests       int           `json:"tests"`
	Failures    int           `json:"failures"`
	Errors      int           `json:"errors"`
	Skipped     int           `json:"skipped"`
	TimeSeconds float64       `json:"timeSeconds"`
	Failed      []TestFailure `json:"failed"`
}

type TestFailure struct {
	Suite     string `json:"suite"`
	Name      string `json:"name"`
	Classname string `json:"classname,omitempty"`
	File      string `json:"file,omitempty"`
	Message   string `json:"message,omitempty"`
	Type      string `json:"type,omitempty"`
	Kind      string `json:"kind"`
}

type CoordinatorUsageSummary struct {
	Month          string                  `json:"month"`
	Scope          string                  `json:"scope"`
	Owner          string                  `json:"owner,omitempty"`
	Org            string                  `json:"org,omitempty"`
	Leases         int                     `json:"leases"`
	ActiveLeases   int                     `json:"activeLeases"`
	RuntimeSeconds int64                   `json:"runtimeSeconds"`
	EstimatedUSD   float64                 `json:"estimatedUSD"`
	ReservedUSD    float64                 `json:"reservedUSD"`
	ByOwner        []CoordinatorUsageGroup `json:"byOwner"`
	ByOrg          []CoordinatorUsageGroup `json:"byOrg"`
	ByProvider     []CoordinatorUsageGroup `json:"byProvider"`
	ByServerType   []CoordinatorUsageGroup `json:"byServerType"`
}

type CoordinatorUsageGroup struct {
	Key            string  `json:"key"`
	Leases         int     `json:"leases"`
	ActiveLeases   int     `json:"activeLeases"`
	RuntimeSeconds int64   `json:"runtimeSeconds"`
	EstimatedUSD   float64 `json:"estimatedUSD"`
	ReservedUSD    float64 `json:"reservedUSD"`
}

type CoordinatorCostLimits struct {
	MaxActiveLeases         int     `json:"maxActiveLeases"`
	MaxActiveLeasesPerOwner int     `json:"maxActiveLeasesPerOwner"`
	MaxActiveLeasesPerOrg   int     `json:"maxActiveLeasesPerOrg"`
	MaxMonthlyUSD           float64 `json:"maxMonthlyUSD"`
	MaxMonthlyUSDPerOwner   float64 `json:"maxMonthlyUSDPerOwner"`
	MaxMonthlyUSDPerOrg     float64 `json:"maxMonthlyUSDPerOrg"`
}

type CoordinatorID string

func (id *CoordinatorID) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*id = CoordinatorID(s)
		return nil
	}
	var n int64
	if err := json.Unmarshal(data, &n); err == nil {
		*id = CoordinatorID(fmt.Sprint(n))
		return nil
	}
	return fmt.Errorf("invalid coordinator id: %s", string(data))
}

func newCoordinatorClient(cfg Config) (*CoordinatorClient, bool, error) {
	if cfg.Coordinator == "" {
		return nil, false, nil
	}
	base, err := url.Parse(cfg.Coordinator)
	if err != nil {
		return nil, true, exit(2, "invalid CRABBOX_COORDINATOR: %v", err)
	}
	if base.Scheme == "" || base.Host == "" {
		return nil, true, exit(2, "CRABBOX_COORDINATOR must be an absolute URL")
	}
	base.Path = strings.TrimRight(base.Path, "/")
	return &CoordinatorClient{
		BaseURL: strings.TrimRight(base.String(), "/"),
		Token:   cfg.CoordToken,
		Access:  cfg.Access,
		Client: &http.Client{
			Timeout: 5 * time.Minute,
			Transport: &http.Transport{
				Proxy: http.ProxyFromEnvironment,
				DialContext: (&net.Dialer{
					Timeout:   5 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 5 * time.Minute,
				IdleConnTimeout:       90 * time.Second,
			},
		},
	}, true, nil
}

func (c *CoordinatorClient) CreateLease(ctx context.Context, cfg Config, publicKey string, keep bool, leaseID, slug string) (CoordinatorLease, error) {
	var res struct {
		Lease CoordinatorLease `json:"lease"`
	}
	provider, err := ProviderFor(cfg.Provider)
	if err != nil {
		return CoordinatorLease{}, err
	}
	cfg.Provider = provider.Name()
	if slug == "" {
		slug = newLeaseSlug(leaseID)
	}
	capacity := map[string]any{}
	if cfg.Capacity.Market != "" && cfg.Capacity.Market != "spot" {
		capacity["market"] = cfg.Capacity.Market
	}
	if cfg.Capacity.Strategy != "" && cfg.Capacity.Strategy != "most-available" {
		capacity["strategy"] = cfg.Capacity.Strategy
	}
	if cfg.Capacity.Fallback != "" && cfg.Capacity.Fallback != "on-demand-after-120s" {
		capacity["fallback"] = cfg.Capacity.Fallback
	}
	if len(cfg.Capacity.Regions) > 0 {
		capacity["regions"] = cfg.Capacity.Regions
	}
	if len(cfg.Capacity.AvailabilityZones) > 0 {
		capacity["availabilityZones"] = cfg.Capacity.AvailabilityZones
	}
	if !cfg.Capacity.Hints {
		capacity["hints"] = false
	}
	req := map[string]any{
		"leaseID":                         leaseID,
		"slug":                            slug,
		"profile":                         cfg.Profile,
		"provider":                        cfg.Provider,
		"target":                          cfg.TargetOS,
		"windowsMode":                     cfg.WindowsMode,
		"desktop":                         cfg.Desktop,
		"browser":                         cfg.Browser,
		"code":                            cfg.Code,
		"tailscale":                       cfg.Tailscale.Enabled,
		"tailscaleTags":                   cfg.Tailscale.Tags,
		"tailscaleHostname":               cfg.Tailscale.Hostname,
		"tailscaleExitNode":               cfg.Tailscale.ExitNode,
		"tailscaleExitNodeAllowLanAccess": cfg.Tailscale.ExitNodeAllowLANAccess,
		"class":                           cfg.Class,
		"serverType":                      cfg.ServerType,
		"serverTypeExplicit":              cfg.ServerTypeExplicit,
		"location":                        cfg.Location,
		"image":                           cfg.Image,
		"awsRegion":                       cfg.AWSRegion,
		"awsAMI":                          cfg.AWSAMI,
		"awsSGID":                         cfg.AWSSGID,
		"awsSubnetID":                     cfg.AWSSubnetID,
		"awsProfile":                      cfg.AWSProfile,
		"awsRootGB":                       cfg.AWSRootGB,
		"awsSSHCIDRs":                     cfg.AWSSSHCIDRs,
		"awsMacHostID":                    cfg.AWSMacHostID,
		"azureLocation":                   cfg.AzureLocation,
		"azureImage":                      cfg.AzureImage,
		"sshUser":                         cfg.SSHUser,
		"sshPort":                         cfg.SSHPort,
		"sshFallbackPorts":                cfg.SSHFallbackPorts,
		"providerKey":                     cfg.ProviderKey,
		"workRoot":                        cfg.WorkRoot,
		"ttlSeconds":                      int(cfg.TTL.Seconds()),
		"idleTimeoutSeconds":              int(cfg.IdleTimeout.Seconds()),
		"keep":                            keep,
		"sshPublicKey":                    publicKey,
	}
	if len(capacity) > 0 {
		req["capacity"] = capacity
	}
	addCoordinatorGCPFields(req, cfg)
	err = c.do(ctx, http.MethodPost, "/v1/leases", req, &res)
	return res.Lease, err
}

func addCoordinatorGCPFields(req map[string]any, cfg Config) {
	if cfg.Provider != "gcp" {
		return
	}
	base := baseConfig()
	if cfg.GCPProject != "" && cfg.gcpProjectExplicit {
		req["gcpProject"] = cfg.GCPProject
	}
	if cfg.GCPZone != "" && (cfg.gcpZoneExplicit || cfg.GCPZone != base.GCPZone) {
		req["gcpZone"] = cfg.GCPZone
	}
	if cfg.GCPImage != "" && (cfg.gcpImageExplicit || cfg.GCPImage != base.GCPImage) {
		req["gcpImage"] = cfg.GCPImage
	}
	if cfg.GCPNetwork != "" && (cfg.gcpNetworkExplicit || cfg.GCPNetwork != base.GCPNetwork) {
		req["gcpNetwork"] = cfg.GCPNetwork
	}
	if cfg.GCPSubnet != "" {
		req["gcpSubnet"] = cfg.GCPSubnet
	}
	if len(cfg.GCPTags) > 0 && (cfg.gcpTagsExplicit || !stringSlicesEqual(cfg.GCPTags, base.GCPTags)) {
		req["gcpTags"] = cfg.GCPTags
	}
	if len(cfg.GCPSSHCIDRs) > 0 {
		req["gcpSSHCIDRs"] = cfg.GCPSSHCIDRs
	}
	if cfg.GCPRootGB > 0 && (cfg.gcpRootGBExplicit || cfg.GCPRootGB != base.GCPRootGB) {
		req["gcpRootGB"] = cfg.GCPRootGB
	}
	if cfg.GCPServiceAccount != "" {
		req["gcpServiceAccount"] = cfg.GCPServiceAccount
	}
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (c *CoordinatorClient) UpdateLeaseTailscale(ctx context.Context, id string, meta TailscaleMetadata) (CoordinatorLease, error) {
	var res struct {
		Lease CoordinatorLease `json:"lease"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/leases/"+url.PathEscape(id)+"/tailscale", meta, &res)
	return res.Lease, err
}

func (c *CoordinatorClient) GetLease(ctx context.Context, id string) (CoordinatorLease, error) {
	var res struct {
		Lease CoordinatorLease `json:"lease"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/leases/"+url.PathEscape(id), nil, &res)
	return res.Lease, err
}

func (c *CoordinatorClient) LeaseShare(ctx context.Context, id string) (CoordinatorShare, error) {
	var res struct {
		Share CoordinatorShare `json:"share"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/leases/"+url.PathEscape(id)+"/share", nil, &res)
	return res.Share, err
}

func (c *CoordinatorClient) UpdateLeaseShare(ctx context.Context, id string, share CoordinatorShare) (CoordinatorShare, error) {
	var res struct {
		Share CoordinatorShare `json:"share"`
	}
	err := c.do(ctx, http.MethodPut, "/v1/leases/"+url.PathEscape(id)+"/share", share, &res)
	return res.Share, err
}

func (c *CoordinatorClient) DeleteLeaseShare(ctx context.Context, id, user string, org bool) (CoordinatorShare, error) {
	var res struct {
		Share CoordinatorShare `json:"share"`
	}
	body := map[string]any{}
	if strings.TrimSpace(user) != "" {
		body["user"] = strings.TrimSpace(user)
	}
	if org {
		body["org"] = true
	}
	err := c.do(ctx, http.MethodDelete, "/v1/leases/"+url.PathEscape(id)+"/share", body, &res)
	return res.Share, err
}

func (c *CoordinatorClient) ReleaseLease(ctx context.Context, id string, deleteServer bool) (CoordinatorLease, error) {
	var res struct {
		Lease CoordinatorLease `json:"lease"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/leases/"+url.PathEscape(id)+"/release", map[string]any{"delete": deleteServer}, &res)
	return res.Lease, err
}

func (c *CoordinatorClient) TouchLease(ctx context.Context, id string) (CoordinatorLease, error) {
	return c.heartbeatLease(ctx, id, nil, nil)
}

func (c *CoordinatorClient) TouchLeaseWithTelemetry(ctx context.Context, id string, telemetry *LeaseTelemetry) (CoordinatorLease, error) {
	return c.heartbeatLease(ctx, id, nil, telemetry)
}

func (c *CoordinatorClient) UpdateLeaseIdleTimeout(ctx context.Context, id string, idleTimeout time.Duration) (CoordinatorLease, error) {
	return c.heartbeatLease(ctx, id, &idleTimeout, nil)
}

func (c *CoordinatorClient) UpdateLeaseIdleTimeoutWithTelemetry(ctx context.Context, id string, idleTimeout time.Duration, telemetry *LeaseTelemetry) (CoordinatorLease, error) {
	return c.heartbeatLease(ctx, id, &idleTimeout, telemetry)
}

func (c *CoordinatorClient) heartbeatLease(ctx context.Context, id string, idleTimeout *time.Duration, telemetry *LeaseTelemetry) (CoordinatorLease, error) {
	var res struct {
		Lease CoordinatorLease `json:"lease"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/leases/"+url.PathEscape(id)+"/heartbeat", heartbeatRequestBody(idleTimeout, telemetry), &res)
	return res.Lease, err
}

func heartbeatRequestBody(idleTimeout *time.Duration, telemetry *LeaseTelemetry) map[string]any {
	body := map[string]any{}
	if idleTimeout != nil && *idleTimeout > 0 {
		body["idleTimeoutSeconds"] = int(idleTimeout.Seconds())
	}
	if telemetry != nil {
		body["telemetry"] = telemetry
	}
	return body
}

func (c *CoordinatorClient) Pool(ctx context.Context, cfg Config) ([]CoordinatorMachine, error) {
	var res struct {
		Machines []CoordinatorMachine `json:"machines"`
	}
	path := "/v1/pool"
	if cfg.Provider != "" {
		path += "?provider=" + url.QueryEscape(cfg.Provider)
	}
	err := c.do(ctx, http.MethodGet, path, nil, &res)
	return res.Machines, err
}

func (c *CoordinatorClient) Leases(ctx context.Context, state string, limit int) ([]CoordinatorLease, error) {
	var res struct {
		Leases []CoordinatorLease `json:"leases"`
	}
	values := url.Values{}
	if state != "" {
		values.Set("state", state)
	}
	if limit > 0 {
		values.Set("limit", strconv.Itoa(limit))
	}
	path := "/v1/leases"
	if encoded := values.Encode(); encoded != "" {
		path += "?" + encoded
	}
	err := c.do(ctx, http.MethodGet, path, nil, &res)
	return res.Leases, err
}

func (c *CoordinatorClient) Usage(ctx context.Context, scope, owner, org, month string) (CoordinatorUsageResponse, error) {
	var res CoordinatorUsageResponse
	values := url.Values{}
	if scope != "" {
		values.Set("scope", scope)
	}
	if owner != "" {
		values.Set("owner", owner)
	}
	if org != "" {
		values.Set("org", org)
	}
	if month != "" {
		values.Set("month", month)
	}
	path := "/v1/usage"
	if encoded := values.Encode(); encoded != "" {
		path += "?" + encoded
	}
	err := c.do(ctx, http.MethodGet, path, nil, &res)
	return res, err
}

func (c *CoordinatorClient) Whoami(ctx context.Context) (CoordinatorWhoami, error) {
	var res CoordinatorWhoami
	err := c.do(ctx, http.MethodGet, "/v1/whoami", nil, &res)
	return res, err
}

func (c *CoordinatorClient) ProviderReadiness(ctx context.Context, provider string) (CoordinatorProviderReadiness, error) {
	var res CoordinatorProviderReadiness
	path := "/v1/providers/" + url.PathEscape(provider) + "/readiness"
	err := c.do(ctx, http.MethodGet, path, nil, &res)
	return res, err
}

func (c *CoordinatorClient) StartGitHubLogin(ctx context.Context, pollSecretHash, provider string) (CoordinatorGitHubLoginStart, error) {
	var res CoordinatorGitHubLoginStart
	body := map[string]any{"pollSecretHash": pollSecretHash}
	if provider != "" {
		body["provider"] = provider
	}
	err := c.do(ctx, http.MethodPost, "/v1/auth/github/start", body, &res)
	return res, err
}

func (c *CoordinatorClient) PollGitHubLogin(ctx context.Context, loginID, pollSecret string) (CoordinatorGitHubLoginPoll, error) {
	var res CoordinatorGitHubLoginPoll
	err := c.do(ctx, http.MethodPost, "/v1/auth/github/poll", map[string]any{
		"loginID":    loginID,
		"pollSecret": pollSecret,
	}, &res)
	return res, err
}

func (c *CoordinatorClient) CreateWebVNCTicket(ctx context.Context, leaseID string) (CoordinatorWebVNCTicket, error) {
	var res CoordinatorWebVNCTicket
	err := c.do(ctx, http.MethodPost, "/v1/leases/"+url.PathEscape(leaseID)+"/webvnc/ticket", map[string]any{}, &res)
	return res, err
}

func (c *CoordinatorClient) WebVNCStatus(ctx context.Context, leaseID string) (CoordinatorWebVNCStatus, error) {
	var res CoordinatorWebVNCStatus
	err := c.do(ctx, http.MethodGet, "/v1/leases/"+url.PathEscape(leaseID)+"/webvnc/status", nil, &res)
	return res, err
}

func (c *CoordinatorClient) ResetWebVNC(ctx context.Context, leaseID string) (CoordinatorWebVNCReset, error) {
	var res CoordinatorWebVNCReset
	err := c.do(ctx, http.MethodPost, "/v1/leases/"+url.PathEscape(leaseID)+"/webvnc/reset", map[string]any{}, &res)
	return res, err
}

func (c *CoordinatorClient) CreateEgressTicket(ctx context.Context, leaseID, role, sessionID, profile string, allow []string) (CoordinatorEgressTicket, error) {
	var res CoordinatorEgressTicket
	body := map[string]any{
		"role": role,
	}
	if strings.TrimSpace(sessionID) != "" {
		body["sessionID"] = strings.TrimSpace(sessionID)
	}
	if strings.TrimSpace(profile) != "" {
		body["profile"] = strings.TrimSpace(profile)
	}
	if len(allow) > 0 {
		body["allow"] = allow
	}
	err := c.do(ctx, http.MethodPost, "/v1/leases/"+url.PathEscape(leaseID)+"/egress/ticket", body, &res)
	return res, err
}

func (c *CoordinatorClient) EgressStatus(ctx context.Context, leaseID string) (CoordinatorEgressStatus, error) {
	var res CoordinatorEgressStatus
	err := c.do(ctx, http.MethodGet, "/v1/leases/"+url.PathEscape(leaseID)+"/egress/status", nil, &res)
	return res, err
}

func (c *CoordinatorClient) AdminLeases(ctx context.Context, state, owner, org string, limit int) ([]CoordinatorLease, error) {
	var res struct {
		Leases []CoordinatorLease `json:"leases"`
	}
	values := url.Values{}
	if state != "" {
		values.Set("state", state)
	}
	if owner != "" {
		values.Set("owner", owner)
	}
	if org != "" {
		values.Set("org", org)
	}
	if limit > 0 {
		values.Set("limit", strconv.Itoa(limit))
	}
	path := "/v1/admin/leases"
	if encoded := values.Encode(); encoded != "" {
		path += "?" + encoded
	}
	err := c.do(ctx, http.MethodGet, path, nil, &res)
	return res.Leases, err
}

func (c *CoordinatorClient) AdminReleaseLease(ctx context.Context, id string, deleteServer bool) (CoordinatorLease, error) {
	var res struct {
		Lease CoordinatorLease `json:"lease"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/admin/leases/"+url.PathEscape(id)+"/release", map[string]any{"delete": deleteServer}, &res)
	return res.Lease, err
}

func (c *CoordinatorClient) AdminDeleteLease(ctx context.Context, id string) (CoordinatorLease, error) {
	var res struct {
		Lease CoordinatorLease `json:"lease"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/admin/leases/"+url.PathEscape(id)+"/delete", map[string]any{}, &res)
	return res.Lease, err
}

func (c *CoordinatorClient) CreateImage(ctx context.Context, leaseID, name string, noReboot bool) (CoordinatorImage, error) {
	var res struct {
		Image CoordinatorImage `json:"image"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/images", map[string]any{
		"leaseID":  leaseID,
		"name":     name,
		"noReboot": noReboot,
	}, &res)
	return res.Image, err
}

func (c *CoordinatorClient) Image(ctx context.Context, imageID string) (CoordinatorImage, error) {
	var res struct {
		Image CoordinatorImage `json:"image"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/images/"+url.PathEscape(imageID), nil, &res)
	return res.Image, err
}

func (c *CoordinatorClient) PromoteImage(ctx context.Context, imageID string) (CoordinatorImage, error) {
	var res struct {
		Image CoordinatorImage `json:"image"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/images/"+url.PathEscape(imageID)+"/promote", map[string]any{}, &res)
	return res.Image, err
}

func (c *CoordinatorClient) CreateRun(ctx context.Context, leaseID string, cfg Config, command []string) (CoordinatorRun, error) {
	var res CoordinatorRunResponse
	err := c.do(ctx, http.MethodPost, "/v1/runs", map[string]any{
		"leaseID":     leaseID,
		"provider":    cfg.Provider,
		"target":      cfg.TargetOS,
		"windowsMode": cfg.WindowsMode,
		"class":       cfg.Class,
		"serverType":  cfg.ServerType,
		"command":     command,
	}, &res)
	return res.Run, err
}

func (c *CoordinatorClient) FinishRun(ctx context.Context, runID string, exitCode int, sync, command time.Duration, log string, truncated bool, results *TestResultSummary, telemetry *RunTelemetrySummary) (CoordinatorRun, error) {
	var res CoordinatorRunResponse
	logChunks := splitRunLogChunks(log)
	body := map[string]any{
		"exitCode":     exitCode,
		"syncMs":       sync.Milliseconds(),
		"commandMs":    command.Milliseconds(),
		"log":          runLogFallbackPreview(log, truncated),
		"logChunks":    logChunks,
		"logTruncated": truncated,
		"results":      results,
	}
	if telemetry != nil {
		body["telemetry"] = telemetry
	}
	err := c.do(ctx, http.MethodPost, "/v1/runs/"+url.PathEscape(runID)+"/finish", body, &res)
	return res.Run, err
}

func (c *CoordinatorClient) AppendRunTelemetry(ctx context.Context, runID string, telemetry *LeaseTelemetry) (CoordinatorRun, error) {
	var res CoordinatorRunResponse
	err := c.do(ctx, http.MethodPost, "/v1/runs/"+url.PathEscape(runID)+"/telemetry", map[string]any{
		"telemetry": telemetry,
	}, &res)
	return res.Run, err
}

func (c *CoordinatorClient) AppendRunEvent(ctx context.Context, runID string, input CoordinatorRunEventInput) (CoordinatorRunEvent, error) {
	var res CoordinatorRunEventResponse
	err := c.do(ctx, http.MethodPost, "/v1/runs/"+url.PathEscape(runID)+"/events", input, &res)
	return res.Event, err
}

func (c *CoordinatorClient) CreateArtifactUploads(ctx context.Context, input CoordinatorArtifactUploadRequest) (CoordinatorArtifactUploadResponse, error) {
	var res CoordinatorArtifactUploadResponse
	err := c.do(ctx, http.MethodPost, "/v1/artifacts/uploads", input, &res)
	return res, err
}

func (c *CoordinatorClient) SyncExternalRunners(ctx context.Context, provider string, runners []CoordinatorExternalRunner) (CoordinatorExternalRunnerSyncResponse, error) {
	var res CoordinatorExternalRunnerSyncResponse
	err := c.do(ctx, http.MethodPost, "/v1/runners/sync", map[string]any{
		"provider": provider,
		"runners":  runners,
	}, &res)
	return res, err
}

func (c *CoordinatorClient) RunEvents(ctx context.Context, runID string, after, limit int) ([]CoordinatorRunEvent, error) {
	var res CoordinatorRunEventsResponse
	values := url.Values{}
	if after > 0 {
		values.Set("after", strconv.Itoa(after))
	}
	if limit > 0 {
		values.Set("limit", strconv.Itoa(limit))
	}
	path := "/v1/runs/" + url.PathEscape(runID) + "/events"
	if encoded := values.Encode(); encoded != "" {
		path += "?" + encoded
	}
	err := c.do(ctx, http.MethodGet, path, nil, &res)
	return res.Events, err
}

func (c *CoordinatorClient) Runs(ctx context.Context, leaseID, owner, org, state string, limit int) ([]CoordinatorRun, error) {
	var res CoordinatorRunsResponse
	values := url.Values{}
	if leaseID != "" {
		values.Set("leaseID", leaseID)
	}
	if owner != "" {
		values.Set("owner", owner)
	}
	if org != "" {
		values.Set("org", org)
	}
	if state != "" {
		values.Set("state", state)
	}
	if limit > 0 {
		values.Set("limit", strconv.Itoa(limit))
	}
	path := "/v1/runs"
	if encoded := values.Encode(); encoded != "" {
		path += "?" + encoded
	}
	err := c.do(ctx, http.MethodGet, path, nil, &res)
	return res.Runs, err
}

func (c *CoordinatorClient) Run(ctx context.Context, runID string) (CoordinatorRun, error) {
	var res CoordinatorRunResponse
	err := c.do(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(runID), nil, &res)
	return res.Run, err
}

func (c *CoordinatorClient) RunLogs(ctx context.Context, runID string) (string, error) {
	var buf bytes.Buffer
	err := c.do(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(runID)+"/logs", nil, &buf)
	return buf.String(), err
}

func (c *CoordinatorClient) Health(ctx context.Context) error {
	var res map[string]any
	return c.do(ctx, http.MethodGet, "/v1/health", nil, &res)
}

func (c *CoordinatorClient) do(ctx context.Context, method, path string, body any, out any) error {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	err = c.doHTTP(ctx, method, path, data, body != nil, out)
	if err == nil || !isCoordinatorTransportError(err) {
		return err
	}
	if curlErr := c.doCurl(ctx, method, path, data, body != nil, out); curlErr == nil {
		return nil
	} else {
		return fmt.Errorf("%w; curl fallback failed: %v", err, curlErr)
	}
}

func (c *CoordinatorClient) doHTTP(ctx context.Context, method, path string, data []byte, hasBody bool, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}
	c.addRequestHeaders(req.Header)
	resp, err := c.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return decodeCoordinatorResponse(method, path, resp.StatusCode, resp.Body, out)
}

func (c *CoordinatorClient) addRequestHeaders(headers http.Header) {
	if c.Token != "" {
		headers.Set("Authorization", "Bearer "+c.Token)
	}
	c.addAccessHeaders(headers)
	if owner := localCoordinatorOwner(); owner != "" {
		headers.Set("X-Crabbox-Owner", owner)
	}
	if org := os.Getenv("CRABBOX_ORG"); org != "" {
		headers.Set("X-Crabbox-Org", org)
	}
}

func (c *CoordinatorClient) doCurl(ctx context.Context, method, path string, data []byte, hasBody bool, out any) error {
	config, cleanup, err := c.curlConfig(method, path, data, hasBody)
	if err != nil {
		return err
	}
	defer cleanup()

	cmd := exec.CommandContext(ctx, "curl", "--config", "-")
	cmd.Stdin = strings.NewReader(config)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return fmt.Errorf("%v: %s", err, msg)
		}
		return err
	}
	body, status, err := splitCurlResponse(stdout.Bytes())
	if err != nil {
		return err
	}
	return decodeCoordinatorResponse(method, path, status, bytes.NewReader(body), out)
}

func (c *CoordinatorClient) curlConfig(method, path string, data []byte, hasBody bool) (string, func(), error) {
	var bodyPath string
	cleanup := func() {}
	if hasBody {
		file, err := os.CreateTemp("", "crabbox-curl-body-*")
		if err != nil {
			return "", cleanup, err
		}
		bodyPath = file.Name()
		cleanup = func() { _ = os.Remove(bodyPath) }
		if _, err := file.Write(data); err != nil {
			_ = file.Close()
			cleanup()
			return "", func() {}, err
		}
		if err := file.Close(); err != nil {
			cleanup()
			return "", func() {}, err
		}
	}

	var cfg strings.Builder
	curlConfigValue(&cfg, "url", c.BaseURL+path)
	curlConfigValue(&cfg, "request", method)
	curlConfigValue(&cfg, "connect-timeout", "10")
	curlConfigValue(&cfg, "max-time", "300")
	curlConfigFlag(&cfg, "silent")
	curlConfigFlag(&cfg, "show-error")
	curlConfigFlag(&cfg, "location")
	curlConfigValue(&cfg, "output", "-")
	curlConfigValue(&cfg, "write-out", "\n%{http_code}")
	if hasBody {
		curlConfigValue(&cfg, "header", "Content-Type: application/json")
		curlConfigValue(&cfg, "data-binary", "@"+bodyPath)
	}
	if c.Token != "" {
		curlConfigValue(&cfg, "header", "Authorization: Bearer "+c.Token)
	}
	c.addCurlAccessHeaders(&cfg)
	if owner := localCoordinatorOwner(); owner != "" {
		curlConfigValue(&cfg, "header", "X-Crabbox-Owner: "+owner)
	}
	if org := os.Getenv("CRABBOX_ORG"); org != "" {
		curlConfigValue(&cfg, "header", "X-Crabbox-Org: "+org)
	}
	return cfg.String(), cleanup, nil
}

func (c *CoordinatorClient) addAccessHeaders(header http.Header) {
	if c.Access.ClientID != "" && c.Access.ClientSecret != "" {
		header.Set("CF-Access-Client-Id", c.Access.ClientID)
		header.Set("CF-Access-Client-Secret", c.Access.ClientSecret)
	}
	if c.Access.Token != "" {
		header.Set("cf-access-token", c.Access.Token)
	}
}

func (c *CoordinatorClient) addCurlAccessHeaders(cfg *strings.Builder) {
	if c.Access.ClientID != "" && c.Access.ClientSecret != "" {
		curlConfigValue(cfg, "header", "CF-Access-Client-Id: "+c.Access.ClientID)
		curlConfigValue(cfg, "header", "CF-Access-Client-Secret: "+c.Access.ClientSecret)
	}
	if c.Access.Token != "" {
		curlConfigValue(cfg, "header", "cf-access-token: "+c.Access.Token)
	}
}

func curlConfigValue(out *strings.Builder, key, value string) {
	fmt.Fprintf(out, "%s = %s\n", key, strconv.Quote(value))
}

func curlConfigFlag(out *strings.Builder, key string) {
	fmt.Fprintln(out, key)
}

func splitCurlResponse(data []byte) ([]byte, int, error) {
	idx := bytes.LastIndexByte(data, '\n')
	if idx < 0 || idx+1 >= len(data) {
		return nil, 0, fmt.Errorf("curl response missing status")
	}
	status, err := strconv.Atoi(strings.TrimSpace(string(data[idx+1:])))
	if err != nil {
		return nil, 0, fmt.Errorf("curl response invalid status: %w", err)
	}
	return data[:idx], status, nil
}

func decodeCoordinatorResponse(method, path string, statusCode int, body io.Reader, out any) error {
	if statusCode < 200 || statusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(body, 600))
		msg := strings.TrimSpace(string(data))
		return CoordinatorHTTPError{
			Method:     method,
			Path:       path,
			StatusCode: statusCode,
			Message:    msg,
		}
	}
	if out != nil {
		if buf, ok := out.(*bytes.Buffer); ok {
			_, err := io.Copy(buf, body)
			return err
		}
		if err := json.NewDecoder(body).Decode(out); err != nil {
			return err
		}
	}
	return nil
}

func isCoordinatorTransportError(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	var urlErr *url.Error
	return errors.As(err, &urlErr)
}

func localCoordinatorOwner() string {
	for _, key := range []string{"CRABBOX_OWNER", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	out, err := exec.Command("git", "config", "--get", "user.email").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func leaseToServerTarget(lease CoordinatorLease, cfg Config) (Server, SSHTarget, string) {
	server := Server{
		Provider: lease.Provider,
		CloudID:  lease.CloudID,
		ID:       lease.ServerID,
		Name:     lease.ServerName,
		Status:   lease.State,
		Labels: map[string]string{
			"lease":             lease.ID,
			"slug":              lease.Slug,
			"keep":              fmt.Sprint(lease.Keep),
			"target":            blank(lease.TargetOS, cfg.TargetOS),
			"windows_mode":      blank(lease.WindowsMode, cfg.WindowsMode),
			"desktop":           fmt.Sprint(lease.Desktop),
			"browser":           fmt.Sprint(lease.Browser),
			"code":              fmt.Sprint(lease.Code),
			"expires_at":        lease.ExpiresAt,
			"last_touched_at":   lease.LastTouchedAt,
			"idle_timeout_secs": fmt.Sprint(lease.IdleTimeoutSeconds),
		},
	}
	if lease.Tailscale != nil {
		applyTailscaleMetadataToServer(&server, *lease.Tailscale)
	}
	if server.Provider == "" {
		server.Provider = cfg.Provider
	}
	server.PublicNet.IPv4.IP = lease.Host
	server.ServerType.Name = lease.ServerType
	if lease.SSHFallbackPorts != nil {
		cfg.SSHFallbackPorts = lease.SSHFallbackPorts
	}
	if lease.TargetOS != "" {
		cfg.TargetOS = lease.TargetOS
	}
	if lease.WindowsMode != "" {
		cfg.WindowsMode = lease.WindowsMode
	}
	target := sshTargetForLease(cfg, lease.Host, lease.SSHUser, lease.SSHPort)
	useStoredTestboxKey(&target, lease.ID)
	return server, target, lease.ID
}

func (a App) touchCoordinatorLeaseBestEffort(ctx context.Context, cfg Config, leaseID string) {
	if leaseID == "" {
		return
	}
	coord, ok, err := newCoordinatorClient(cfg)
	if err != nil || !ok {
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := coord.TouchLease(callCtx, leaseID); err != nil {
		fmt.Fprintf(a.Stderr, "warning: touch failed for %s: %v\n", leaseID, err)
	}
}
