package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Profile                       string
	Provider                      string
	providerExplicit              bool
	providerDefaultsApplied       string
	TargetOS                      string
	targetExplicit                bool
	targetFlagExplicit            bool
	inferredTargetProvider        string
	Architecture                  string
	architectureExplicit          bool
	OSImage                       string
	osImageExplicit               bool
	osImageProviderDefaults       string
	WindowsMode                   string
	explicitWindowsMode           string
	windowsModeFlagExplicit       bool
	Desktop                       bool
	DesktopEnv                    string
	Browser                       bool
	Code                          bool
	Network                       NetworkMode
	Class                         string
	classExplicitOrder            uint64
	explicitSelectionOrder        uint64
	Pond                          string
	ExposedPorts                  []string
	ServerType                    string
	ServerTypeExplicit            bool
	Coordinator                   string
	BrokerMode                    BrokerMode
	brokerProvider                string
	BrokerAutoWebVNC              bool
	CoordToken                    string
	CoordTokenCommand             []string
	CoordAdminToken               string
	credentialProvenance          credentialDestinationProvenance
	HostID                        string
	Access                        AccessConfig
	Location                      string
	locationExplicit              bool
	Image                         string
	imageExplicit                 bool
	AWSRegion                     string
	AWSAMI                        string
	AWSSnapshot                   string
	AWSSGID                       string
	AWSSubnetID                   string
	AWSProfile                    string
	AWSRootGB                     int32
	AWSSSHCIDRs                   []string
	AWSMacHostID                  string
	AzureSubscription             string
	AzureTenant                   string
	AzureClientID                 string
	AzureLocation                 string
	AzureBackend                  string
	AzureResourceGroup            string
	AzureImage                    string
	azureImageExplicit            bool
	AzureSnapshot                 string
	AzureOSDisk                   string
	AzureOSDiskExplicit           bool
	AzureVNet                     string
	AzureSubnet                   string
	AzureNSG                      string
	AzureSSHCIDRs                 []string
	AzureNetwork                  string
	AzureDynamicSessions          AzureDynamicSessionsConfig
	GCPProject                    string
	gcpProjectExplicit            bool
	GCPZone                       string
	gcpZoneExplicit               bool
	GCPImage                      string
	gcpImageExplicit              bool
	GCPMachineImage               string
	GCPSnapshot                   string
	GCPNetwork                    string
	gcpNetworkExplicit            bool
	GCPSubnet                     string
	GCPTags                       []string
	gcpTagsExplicit               bool
	GCPSSHCIDRs                   []string
	GCPRootGB                     int64
	gcpRootGBExplicit             bool
	GCPServiceAccount             string
	DigitalOcean                  DigitalOceanConfig
	digitalOceanImageExplicit     bool
	Linode                        LinodeConfig
	linodeImageExplicit           bool
	linodeTypeExplicit            bool
	OVH                           OVHConfig
	ovhImageExplicit              bool
	Scaleway                      ScalewayConfig
	scalewayRegionExplicit        bool
	scalewayZoneExplicit          bool
	scalewayImageExplicit         bool
	scalewayTypeExplicit          bool
	Incus                         IncusConfig
	Proxmox                       ProxmoxConfig
	XCPNg                         XCPNgConfig
	Parallels                     ParallelsConfig
	parallelsTemplateApplied      bool
	SSHUser                       string
	explicitSSHUser               string
	SSHKey                        string
	explicitSSHKey                string
	SSHPort                       string
	explicitSSHPort               string
	SSHFallbackPorts              []string
	sshFallbackPortsExplicit      bool
	explicitSSHFallbackPorts      []string
	ProviderKey                   string
	WorkRoot                      string
	explicitWorkRoot              string
	TTL                           time.Duration
	IdleTimeout                   time.Duration
	Sync                          SyncConfig
	Run                           RunConfig
	EnvAllow                      []string
	Capacity                      CapacityConfig
	Actions                       ActionsConfig
	Blacksmith                    BlacksmithConfig
	KubeVirt                      KubeVirtConfig
	AgentSandbox                  AgentSandboxConfig
	deleteOnReleaseExplicit       map[string]bool
	External                      ExternalConfig
	Namespace                     NamespaceConfig
	NamespaceInstance             NamespaceInstanceConfig
	Phala                         PhalaConfig
	phalaTypeExplicitOrder        uint64
	Morph                         MorphConfig
	Daytona                       DaytonaConfig
	E2B                           E2BConfig
	ExeDev                        ExeDevConfig
	Railway                       RailwayConfig
	Runpod                        RunpodConfig
	NvidiaBrev                    NvidiaBrevConfig
	nvidiaBrevWorkRootExplicit    bool
	Hostinger                     HostingerConfig
	hostingerUserExplicit         bool
	hostingerWorkRootExplicit     bool
	Wandb                         WandbConfig
	Islo                          IsloConfig
	isloImageExplicit             bool
	isloVCPUsExplicit             bool
	isloMemoryMBExplicit          bool
	isloDiskGBExplicit            bool
	Freestyle                     FreestyleConfig
	Tenki                         TenkiConfig
	Tensorlake                    TensorlakeConfig
	OpenComputer                  OpenComputerConfig
	CodeSandbox                   CodeSandboxConfig
	OpenSandbox                   OpenSandboxConfig
	VercelSandbox                 VercelSandboxConfig
	Superserve                    SuperserveConfig
	DockerSandbox                 DockerSandboxConfig
	AnthropicSRT                  AnthropicSRTConfig
	Modal                         ModalConfig
	UpstashBox                    UpstashBoxConfig
	Smolvm                        SmolvmConfig
	AsciiBox                      AsciiBoxConfig
	Cloudflare                    CloudflareConfig
	CloudflareDynamicWorkers      CloudflareDynamicWorkersConfig
	Semaphore                     SemaphoreConfig
	Sprites                       SpritesConfig
	LocalContainer                LocalContainerConfig
	localContainerRuntimeExplicit bool
	localContainerImageExplicit   bool
	AppleContainer                AppleContainerConfig
	appleContainerImageExplicit   bool
	AppleVZ                       AppleVZConfig
	appleVZImageExplicit          bool
	appleVZImageSHA256Explicit    bool
	appleVZCPUsExplicit           bool
	appleVZMemoryExplicit         bool
	appleVZDiskExplicit           bool
	MXC                           MXCConfig
	Multipass                     MultipassConfig
	multipassImageExplicit        bool
	Tart                          TartConfig
	tartImageExplicit             bool
	tartDiskExplicit              bool
	tartCPUsExplicit              bool
	tartMemoryExplicit            bool
	HyperV                        HyperVConfig
	WindowsSandbox                WindowsSandboxConfig
	Tailscale                     TailscaleConfig
	Static                        StaticConfig
	Results                       ResultsConfig
	Cache                         CacheConfig
	Profiles                      map[string]ProfileConfig
	Presets                       map[string]PresetConfig
	ProofTemplates                map[string]ProofTemplateConfig
	Jobs                          map[string]JobConfig
}

type SyncConfig struct {
	Excludes    []string
	Includes    []string
	Delete      bool
	Checksum    bool
	GitSeed     bool
	Fingerprint bool
	BaseRef     string
	Timeout     time.Duration
	WarnFiles   int
	WarnBytes   int64
	FailFiles   int
	FailBytes   int64
	AllowLarge  bool
}

type RunConfig struct {
	PreflightTools []string
}

type CapacityConfig struct {
	Market            string
	Strategy          string
	Fallback          string
	Regions           []string
	AvailabilityZones []string
	Hints             bool
}

type DigitalOceanConfig struct {
	Region   string
	Image    string
	VPCUUID  string
	SSHCIDRs []string
}

type LinodeConfig struct {
	Region     string
	Image      string
	Type       string
	FirewallID string
	SSHCIDRs   []string
}

// OVHConfig contains non-secret OVHcloud Public Cloud settings. OVH
// application credentials are intentionally read from environment variables by
// the provider client and are not persisted in Crabbox config.
type OVHConfig struct {
	Endpoint  string
	ProjectID string
	Region    string
	Image     string
	Flavor    string
}

// ScalewayConfig contains non-secret Scaleway Instances settings. Scaleway
// credentials are intentionally loaded by the provider client from the official
// SDK environment/config surfaces and are not persisted in Crabbox config.
type ScalewayConfig struct {
	Region         string
	Zone           string
	Image          string
	Type           string
	ProjectID      string
	OrganizationID string
	SecurityGroup  string
	SSHCIDRs       []string
}

type ActionsConfig struct {
	Repo          string
	Workflow      string
	Job           string
	Ref           string
	Fields        []string
	RunnerLabels  []string
	RunnerVersion string
	Ephemeral     bool
}

type BlacksmithConfig struct {
	Org         string
	Workflow    string
	Job         string
	Ref         string
	IdleTimeout time.Duration
	Debug       bool
}

type KubeVirtConfig struct {
	Kubectl         string
	Virtctl         string
	Kubeconfig      string
	Context         string
	Namespace       string
	Template        string
	SSHUser         string
	SSHKey          string
	SSHPublicKey    string
	SSHPort         string
	WorkRoot        string
	DeleteOnRelease bool
}

type AgentSandboxConfig struct {
	Kubectl             string
	Kubeconfig          string
	Context             string
	Namespace           string
	WarmPool            string
	Container           string
	Workdir             string
	SandboxReadyTimeout time.Duration
	PodReadyTimeout     time.Duration
	ExecTimeoutSecs     int
	DeleteOnRelease     bool
	ForgetMissing       bool
}

type ExternalConfig struct {
	Command       string
	Args          []string
	Config        map[string]any
	Capabilities  ExternalCapabilitiesConfig
	Lifecycle     ExternalLifecycleConfig
	Connection    ExternalConnectionConfig
	WorkRoot      string
	RoutingFile   string
	routingLoaded bool
}

type ExternalCapabilitiesConfig struct {
	IdempotentLeaseID bool `yaml:"idempotentLeaseId,omitempty" json:"idempotentLeaseId,omitempty"`
}

type ExternalLifecycleConfig struct {
	Doctor  ExternalLifecycleOperation `yaml:"doctor,omitempty" json:"doctor,omitempty"`
	Acquire ExternalLifecycleOperation `yaml:"acquire,omitempty" json:"acquire,omitempty"`
	Resolve ExternalLifecycleOperation `yaml:"resolve,omitempty" json:"resolve,omitempty"`
	List    ExternalLifecycleOperation `yaml:"list,omitempty" json:"list,omitempty"`
	Release ExternalLifecycleOperation `yaml:"release,omitempty" json:"release,omitempty"`
	Touch   ExternalLifecycleOperation `yaml:"touch,omitempty" json:"touch,omitempty"`
	Cleanup ExternalLifecycleOperation `yaml:"cleanup,omitempty" json:"cleanup,omitempty"`
}

type ExternalLifecycleOperation struct {
	Argv              []string          `yaml:"argv,omitempty" json:"argv,omitempty"`
	Steps             [][]string        `yaml:"steps,omitempty" json:"steps,omitempty"`
	Env               map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	AllowEnvArgv      bool              `yaml:"allowEnvArgv,omitempty" json:"allowEnvArgv,omitempty"`
	Output            string            `yaml:"output,omitempty" json:"output,omitempty"`
	NamePrefix        string            `yaml:"namePrefix,omitempty" json:"namePrefix,omitempty"`
	RollbackOnFailure bool              `yaml:"rollbackOnFailure,omitempty" json:"rollbackOnFailure,omitempty"`
}

type ExternalConnectionConfig struct {
	ResourceName         string                      `yaml:"resourceName,omitempty" json:"resourceName,omitempty"`
	AllowEnvResourceName bool                        `yaml:"allowEnvResourceName,omitempty" json:"allowEnvResourceName,omitempty"`
	CloudID              string                      `yaml:"cloudId,omitempty" json:"cloudId,omitempty"`
	ServerType           string                      `yaml:"serverType,omitempty" json:"serverType,omitempty"`
	Labels               map[string]string           `yaml:"labels,omitempty" json:"labels,omitempty"`
	SSH                  ExternalSSHConnectionConfig `yaml:"ssh,omitempty" json:"ssh,omitempty"`
}

type ExternalSSHConnectionConfig struct {
	User            string   `yaml:"user,omitempty" json:"user,omitempty"`
	Host            string   `yaml:"host,omitempty" json:"host,omitempty"`
	Key             string   `yaml:"key,omitempty" json:"key,omitempty"`
	Port            string   `yaml:"port,omitempty" json:"port,omitempty"`
	FallbackPorts   []string `yaml:"fallbackPorts,omitempty" json:"fallbackPorts,omitempty"`
	ReadyCheck      string   `yaml:"readyCheck,omitempty" json:"readyCheck,omitempty"`
	AuthSecret      bool     `yaml:"authSecret,omitempty" json:"authSecret,omitempty"`
	NoControlMaster bool     `yaml:"noControlMaster,omitempty" json:"noControlMaster,omitempty"`
	SSHConfigProxy  bool     `yaml:"sshConfigProxy,omitempty" json:"sshConfigProxy,omitempty"`
	ProxyCommand    string   `yaml:"proxyCommand,omitempty" json:"proxyCommand,omitempty"`
}

type NamespaceConfig struct {
	Image               string
	Size                string
	Repository          string
	Site                string
	VolumeSizeGB        int
	AutoStopIdleTimeout time.Duration
	WorkRoot            string
	DeleteOnRelease     bool
}

type NamespaceInstanceConfig struct {
	CLIPath     string
	MachineType string
	Duration    time.Duration
	Region      string
	Endpoint    string
	Keychain    string
	TenantID    string
	Volumes     []string
	WorkRoot    string
	Bare        bool
}

// PhalaConfig configures the Phala Cloud confidential TDX CVM provider. Phala
// authenticates through its own stored credentials (device flow or
// PHALA_CLOUD_API_KEY), so no API key is held here.
type PhalaConfig struct {
	CLIPath      string
	InstanceType string
	WorkRoot     string
	NodeID       string
	Compose      string
	// Attest gates the TDX remote-attestation check the Phala backend runs after
	// a leased CVM becomes reachable. nil means "default" (attestation ON); the
	// backend treats nil as true. A non-nil false value (set only by the local
	// --phala-skip-attestation flag or CRABBOX_PHALA_ATTEST=false env) opts out.
	Attest *bool
}

type MorphConfig struct {
	APIKey          string
	APIURL          string
	Snapshot        string
	SSHGatewayHost  string
	WorkRoot        string
	DeleteOnRelease bool
	WakeOnSSH       bool
}

type DaytonaConfig struct {
	APIKey           string
	JWTToken         string
	OrganizationID   string
	APIURL           string
	Snapshot         string
	Target           string
	User             string
	WorkRoot         string
	SSHGatewayHost   string
	SSHAccessMinutes int
}

type E2BConfig struct {
	APIKey   string
	APIURL   string
	Domain   string
	Template string
	Workdir  string
	User     string
}

type AzureDynamicSessionsConfig struct {
	Endpoint    string
	Pool        string
	APIVersion  string
	Workdir     string
	TimeoutSecs int
}

const (
	AzureBackendVM              = "vm"
	AzureBackendDynamicSessions = "dynamic-sessions"
)

func NormalizeAzureBackend(backend string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(backend)) {
	case "", "vm", "vms", "virtual-machine", "virtual-machines":
		return AzureBackendVM, nil
	case "dynamic-sessions", "dynamic-session", "sessions", "azds":
		return AzureBackendDynamicSessions, nil
	default:
		return "", fmt.Errorf("azure backend must be vm or dynamic-sessions")
	}
}

type ExeDevConfig struct {
	ControlHost string
	Image       string
	CPUs        int
	Memory      string
	Disk        string
	Command     string
	User        string
	WorkRoot    string
	NoEmail     bool
}

type RailwayConfig struct {
	APIToken      string
	APIURL        string
	ProjectID     string
	EnvironmentID string
}

type RunpodConfig struct {
	APIKey     string
	APIURL     string
	CloudType  string
	InstanceID string
	Image      string
	TemplateID string
	DiskGB     int
	User       string
	WorkRoot   string
}

// NvidiaBrevConfig is intentionally non-secret. Authentication stays in the
// NVIDIA Brev CLI's own credential store and is never accepted as Crabbox
// config or argv.
type NvidiaBrevConfig struct {
	CLI           string
	Org           string
	Type          string
	GPUName       string
	Provider      string
	Mode          string
	Launchable    string
	StartupScript string
	ReleaseAction string
	Target        string
	User          string
	WorkRoot      string
}

type HostingerConfig struct {
	APIToken        string
	APIURL          string
	ItemID          string
	PaymentMethodID string
	TemplateID      string
	DataCenterID    string
	HostnamePrefix  string
	User            string
	WorkRoot        string
	AllowPurchase   bool
	ReleaseAction   string
}

// WandbConfig drives the W&B Sandboxes (CoreWeave Sandboxes) provider. The
// API key is the same one `wandb login` writes to ~/.netrc — the value
// proposition of this provider is that AI researchers already have it.
type WandbConfig struct {
	APIKey             string
	DefaultImage       string
	MaxLifetimeSeconds int
}

type IsloConfig struct {
	APIKey         string
	BaseURL        string
	Image          string
	Workdir        string
	GatewayProfile string
	SnapshotName   string
	VCPUs          int
	MemoryMB       int
	DiskGB         int
}

type FreestyleConfig struct {
	APIKey   string
	APIURL   string
	Workdir  string
	VCPUs    int
	MemoryGB int
}

type TenkiConfig struct {
	CLIPath   string
	Endpoint  string
	Gateway   string
	Workspace string
	Project   string
	Image     string
	Snapshot  string
	WorkRoot  string
	CPUs      int
	MemoryMB  int
	DiskGB    int
}

type TensorlakeConfig struct {
	APIKey         string
	APIURL         string
	CLIPath        string
	Image          string
	Snapshot       string
	OrganizationID string
	ProjectID      string
	Namespace      string
	Workdir        string
	CPUs           float64
	MemoryMB       int
	DiskMB         int
	TimeoutSecs    int
	NoInternet     bool
}

// OpenComputerConfig configures the delegated OpenComputer provider, which
// talks to the OpenComputer REST API. The API key is intentionally absent: it
// is read at runtime from CRABBOX_OPENCOMPUTER_API_KEY / OPENCOMPUTER_API_KEY
// or the `oc` CLI config (`oc config set api-key`), and sent only in the
// X-API-Key header — never persisted in Crabbox config or placed on argv.
type OpenComputerConfig struct {
	APIURL          string
	Workdir         string
	CPU             int
	MemoryMB        int
	TimeoutSecs     int
	ExecTimeoutSecs int
	Burst           bool
	ForgetMissing   bool
}

// CodeSandboxConfig configures the delegated CodeSandbox provider. The API key
// is intentionally absent: it is read at runtime from
// CRABBOX_CODESANDBOX_API_KEY / CSB_API_KEY and passed to the SDK bridge through
// environment only, never persisted in Crabbox config or placed on argv.
type CodeSandboxConfig struct {
	TemplateID               string
	Workdir                  string
	VMTier                   string
	Privacy                  string
	HibernationTimeoutSecs   int
	AutomaticWakeupHTTP      bool
	AutomaticWakeupWebSocket bool
	BridgeCommand            string
	SDKPackage               string
	DoctorListLimit          int
	OperationTimeoutSecs     int
}

// OpenSandboxConfig configures the delegated OpenSandbox provider. The API key
// is intentionally absent: it is read at runtime from
// CRABBOX_OPENSANDBOX_API_KEY / OPEN_SANDBOX_API_KEY and sent only in request
// headers, never persisted in Crabbox config or placed on argv.
type OpenSandboxConfig struct {
	APIURL          string
	Image           string
	Workdir         string
	CPU             string
	Memory          string
	TimeoutSecs     int
	ExecTimeoutSecs int
	PlatformOS      string
	PlatformArch    string
	SecureAccess    bool
	UseServerProxy  bool
	ForgetMissing   bool
}

// VercelSandboxConfig configures the delegated Vercel Sandbox provider. Token
// fields are intentionally absent: SDK and CLI credentials are resolved from
// environment/auth stores at runtime and are never persisted in Crabbox config
// or passed on argv.
type VercelSandboxConfig struct {
	Runtime         string
	Workdir         string
	ProjectID       string
	TeamID          string
	Scope           string
	VCPUs           float64
	TimeoutSecs     int
	ExecTimeoutSecs int
	Persistent      bool
	Snapshot        string
	SnapshotMode    string
	NetworkPolicy   string
	NetworkAllow    []string
	NetworkDeny     []string
	Ports           []string
	ForgetMissing   bool
}

// SuperserveConfig configures the delegated Superserve provider. The API key is
// intentionally absent: it is read at runtime from
// CRABBOX_SUPERSERVE_API_KEY / SUPERSERVE_API_KEY and sent only in request
// headers, never persisted in Crabbox config or placed on argv.
type SuperserveConfig struct {
	BaseURL         string
	Template        string
	Snapshot        string
	Workdir         string
	TimeoutSecs     int
	ExecTimeoutSecs int
	NetworkAllowOut []string
	NetworkDenyOut  []string
	ForgetMissing   bool
}

type DockerSandboxConfig struct {
	CLIPath         string
	Agent           string
	Template        string
	CPUs            float64
	Memory          string
	Clone           bool
	Workdir         string
	ExtraWorkspaces []string
	MCP             []string
	Kit             []string
}

type AnthropicSRTConfig struct {
	CLIPath  string
	Settings string
	Debug    bool
}

type ModalConfig struct {
	App     string
	Image   string
	Workdir string
	Python  string
}

type UpstashBoxConfig struct {
	APIKey    string
	BaseURL   string
	Runtime   string
	Size      string
	Workdir   string
	KeepAlive bool
}

type SmolvmConfig struct {
	APIKey   string
	BaseURL  string
	Image    string
	Workdir  string
	CPUs     int
	MemoryMB int
	Network  string
	Keep     bool
}

type AsciiBoxConfig struct {
	APIKey  string
	BaseURL string
	CLIPath string
	Workdir string
}

type CloudflareConfig struct {
	APIURL  string
	Token   string
	Workdir string
}

const DefaultCloudflareDynamicWorkersCompatibilityDate = "2026-06-12"

type CloudflareDynamicWorkersConfig struct {
	LoaderURL                      string
	Token                          string
	CompatibilityDate              string
	CompatibilityFlags             []string
	CacheMode                      string
	Egress                         string
	CPUMs                          int
	Subrequests                    int
	TimeoutSecs                    int
	Metadata                       map[string]string
	repositoryCPUMsCap             int
	repositoryCPUMsCapActive       bool
	repositorySubrequestsCap       int
	repositorySubrequestsCapActive bool
	repositoryTimeoutSecsCap       int
	repositoryTimeoutSecsCapActive bool
}

type ProxmoxConfig struct {
	APIURL      string
	TokenID     string
	TokenSecret string
	Node        string
	TemplateID  int
	Storage     string
	Pool        string
	Bridge      string
	User        string
	WorkRoot    string
	FullClone   bool
	InsecureTLS bool
}

type XCPNgConfig struct {
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

type IncusConfig struct {
	Remote            string
	Project           string
	Address           string
	Socket            string
	InstanceType      string
	Image             string
	Profile           string
	User              string
	WorkRoot          string
	DeleteOnRelease   bool
	StartTimeout      time.Duration
	LaunchPort        string
	ProxyListenHost   string
	ProxyListenPort   string
	ProxyDevice       string
	TLSServerCert     string
	InsecureTLS       bool
	RemoteImageServer string
}

type ParallelsConfig struct {
	Template         string
	Source           string
	SourceID         string
	SourceSnapshot   string
	SourceSnapshotID string
	CloneMode        string
	Host             string
	HostUser         string
	HostKey          string
	VMRoot           string
	User             string
	WorkRoot         string
	StartupTimeout   time.Duration
	Templates        map[string]ParallelsTemplateConfig
	Hosts            []ParallelsHostConfig
	SelectedHost     string
}

type ParallelsTemplateConfig struct {
	Source           string
	SourceID         string
	SourceSnapshot   string
	SourceSnapshotID string
	TargetOS         string
	WindowsMode      string
	CloneMode        string
	Host             string
	HostUser         string
	HostKey          string
	VMRoot           string
	User             string
	WorkRoot         string
}

type ParallelsHostConfig struct {
	Name    string
	Host    string
	User    string
	Key     string
	VMRoot  string
	Targets []string
	MaxVMs  int
}

type SemaphoreConfig struct {
	Host        string
	Token       string
	Project     string
	Machine     string
	OSImage     string
	IdleTimeout string
}

type SpritesConfig struct {
	Token    string
	APIURL   string
	WorkRoot string
}

type LocalContainerConfig struct {
	Runtime            string
	Image              string
	User               string
	WorkRoot           string
	CPUs               int
	Memory             string
	Network            string
	DockerSocket       bool
	Volumes            []string
	CheckpointMetadata map[string]string `yaml:"-" json:"-"`
}

type AppleContainerConfig struct {
	CLIPath      string
	Image        string
	User         string
	WorkRoot     string
	CPUs         int
	Memory       string
	ExtraRunArgs []string
}

type AppleVZConfig struct {
	HelperPath  string
	Image       string
	ImageSHA256 string
	User        string
	WorkRoot    string
	CPUs        int
	MemoryMiB   int
	DiskGiB     int
}

type MXCConfig struct {
	CLIPath           string
	Version           string
	Containment       string
	Network           string
	ReadOnlyPaths     []string
	ReadWritePaths    []string
	AllowedHosts      []string
	BlockedHosts      []string
	AllowDACLMutation bool
	AllowWindowsUI    bool
	Experimental      bool
}

type MultipassConfig struct {
	CLIPath       string
	Image         string
	User          string
	WorkRoot      string
	CPUs          int
	Memory        string
	Disk          string
	LaunchTimeout time.Duration
}

type TartConfig struct {
	Image    string
	User     string
	Password string
	WorkRoot string
	CPUs     int
	Memory   int
	Disk     int
}

type HyperVConfig struct {
	Image         string
	User          string
	WorkRoot      string
	CPUs          int
	Memory        int
	Switch        string
	GuestPassword string
	InitPassword  bool
}

type WindowsSandboxConfig struct {
	Workdir            string
	TempRoot           string
	Networking         string
	VGPU               string
	Clipboard          string
	ProtectedClient    string
	AudioInput         string
	VideoInput         string
	PrinterRedirection string
	MemoryMB           int
}

type StaticConfig struct {
	ID       string
	Name     string
	Host     string
	User     string
	Port     string
	WorkRoot string
}

type ResultsConfig struct {
	JUnit []string
	Auto  bool
}

type CacheConfig struct {
	Pnpm           bool
	Npm            bool
	Docker         bool
	Git            bool
	MaxGB          int
	PurgeOnRelease bool
	Volumes        []CacheVolumeConfig
}

type CacheVolumeConfig struct {
	Name     string `json:"name,omitempty"`
	Key      string `json:"key"`
	Path     string `json:"path"`
	SizeGB   int    `json:"sizeGB,omitempty"`
	Required bool   `json:"required,omitempty"`
}

func ParseCacheVolumeSpecs(specs []string) ([]CacheVolumeConfig, error) {
	volumes := []CacheVolumeConfig{}
	for _, raw := range specs {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		volume, err := ParseCacheVolumeSpec(raw)
		if err != nil {
			return nil, err
		}
		volumes = append(volumes, volume)
	}
	return volumes, nil
}

func ParseCacheVolumeSpec(spec string) (CacheVolumeConfig, error) {
	spec = strings.TrimSpace(spec)
	name := ""
	if before, after, ok := strings.Cut(spec, "="); ok {
		name = strings.TrimSpace(before)
		spec = strings.TrimSpace(after)
	}
	key, path, ok := strings.Cut(spec, ":")
	if !ok {
		return CacheVolumeConfig{}, exit(2, "cache volume %q must use [name=]key:path", spec)
	}
	volume := CacheVolumeConfig{
		Name: name,
		Key:  strings.TrimSpace(key),
		Path: strings.TrimSpace(path),
	}
	if err := validateCacheVolume(volume); err != nil {
		return CacheVolumeConfig{}, err
	}
	if volume.Name == "" {
		volume.Name = volume.Key
	}
	return volume, nil
}

func CacheVolumeStickyDiskSpecs(volumes []CacheVolumeConfig) []string {
	specs := []string{}
	for _, volume := range volumes {
		if validateCacheVolume(volume) != nil {
			continue
		}
		specs = append(specs, volume.Key+":"+volume.Path)
	}
	return specs
}

func normalizeFileCacheVolumes(files []fileCacheVolumeConfig) ([]CacheVolumeConfig, error) {
	volumes := make([]CacheVolumeConfig, 0, len(files))
	for _, file := range files {
		volume := CacheVolumeConfig{
			Name:   strings.TrimSpace(file.Name),
			Key:    strings.TrimSpace(file.Key),
			Path:   strings.TrimSpace(file.Path),
			SizeGB: file.SizeGB,
		}
		if file.Required != nil {
			volume.Required = *file.Required
		}
		if volume.Key == "" && volume.Name != "" {
			volume.Key = volume.Name
		}
		if volume.Name == "" {
			volume.Name = volume.Key
		}
		if err := validateCacheVolume(volume); err != nil {
			return nil, err
		}
		volumes = append(volumes, volume)
	}
	return volumes, nil
}

func validateCacheVolume(volume CacheVolumeConfig) error {
	if strings.TrimSpace(volume.Key) == "" {
		return exit(2, "cache volume key is required")
	}
	if strings.Contains(volume.Key, ":") {
		return exit(2, "cache volume key %q must not contain ':'", volume.Key)
	}
	if strings.TrimSpace(volume.Path) == "" {
		return exit(2, "cache volume path is required")
	}
	if !strings.HasPrefix(volume.Path, "/") {
		return exit(2, "cache volume path %q must be absolute", volume.Path)
	}
	if volume.SizeGB < 0 {
		return exit(2, "cache volume sizeGB must be non-negative")
	}
	return nil
}

func validateCacheVolumesForProvider(cfg Config) error {
	if len(cfg.Cache.Volumes) == 0 {
		return nil
	}
	provider, err := ProviderFor(cfg.Provider)
	if err != nil {
		return err
	}
	if provider.Spec().Features.Has(FeatureCacheVolume) {
		return nil
	}
	for _, volume := range cfg.Cache.Volumes {
		if volume.Required {
			return exit(2, "provider=%s does not support required cache volume %q", cfg.Provider, firstNonBlank(volume.Name, volume.Key))
		}
	}
	return nil
}

type ProfileConfig struct {
	Env            map[string]string
	EnvAllow       []string
	ArtifactGlobs  []string
	Doctor         DoctorProfileConfig
	Presets        map[string]PresetConfig
	ProofTemplates map[string]ProofTemplateConfig
}

type DoctorProfileConfig struct {
	Enabled        bool
	Tools          []string
	NodeMajor      int
	MinDiskGB      int
	RequireDocker  bool
	RequireCompose bool
}

type PresetConfig struct {
	Command       string
	Shell         bool
	Env           map[string]string
	Preflight     bool
	ArtifactGlobs []string
	ProofTemplate string
}

type ProofTemplateConfig struct {
	BehaviorAddressed     string
	RealEnvironmentTested string
	ExactSteps            string
	ObservedResult        string
	NotTested             string
}

type JobConfig struct {
	Provider          string
	Target            string
	WindowsMode       string
	Profile           string
	Class             string
	Architecture      string
	ServerType        string
	Market            string
	TTL               time.Duration
	IdleTimeout       time.Duration
	Desktop           *bool
	DesktopEnv        string
	Browser           *bool
	Code              *bool
	Network           string
	Hydrate           JobHydrateConfig
	Actions           JobActionsConfig
	Shell             bool
	Command           string
	NoSync            bool
	SyncOnly          bool
	Checksum          *bool
	ForceSyncLarge    bool
	JUnit             []string
	Label             string
	ArtifactGlobs     []string
	RequiredArtifacts []string
	Downloads         []string
	Stop              string
}

type JobHydrateConfig struct {
	Actions          bool
	GitHubRunner     bool
	WaitTimeout      time.Duration
	KeepAliveMinutes int
}

type JobActionsConfig struct {
	Repo     string
	Workflow string
	Job      string
	Ref      string
	Fields   []string
}

type AccessConfig struct {
	ClientID     string
	ClientSecret string
	Token        string
}

type BrokerMode string

const (
	BrokerModeManaged    BrokerMode = "managed"
	BrokerModeRegistered BrokerMode = "registered"
)

func defaultConfig() Config {
	cfg, err := loadConfig()
	if err != nil {
		return baseConfig()
	}
	return cfg
}

func loadConfig() (Config, error) {
	return loadConfigWithOverrides("", "")
}

func loadConfigWithOverrides(coordinator, provider string) (Config, error) {
	cfg := baseConfig()
	for _, path := range configPaths() {
		freestyleAPIURL := cfg.Freestyle.APIURL
		if err := applyConfigFile(&cfg, path); err != nil {
			return Config{}, err
		}
		if !trustedProviderEndpointConfigPath(path) {
			cfg.Freestyle.APIURL = freestyleAPIURL
		}
	}
	if err := applyEnv(&cfg); err != nil {
		return Config{}, err
	}
	applyCloudflareDynamicWorkersRepositoryCaps(&cfg)
	if coordinator = strings.TrimSpace(coordinator); coordinator != "" {
		cfg.Coordinator = coordinator
		markCoordinatorDestinationExplicit(&cfg)
	}
	if provider = strings.TrimSpace(provider); provider != "" {
		cfg.Provider = provider
		cfg.brokerProvider = ""
	}
	if err := normalizeBrokerConfig(&cfg); err != nil {
		return Config{}, err
	}
	canonicalizeConfigProvider(&cfg)
	if err := routeConfiguredProvider(&cfg); err != nil {
		return Config{}, err
	}
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		return Config{}, err
	}
	normalizeTargetConfig(&cfg)
	if err := validateTargetConfig(cfg); err != nil {
		return Config{}, err
	}
	if err := validateNetworkConfig(cfg); err != nil {
		return Config{}, err
	}
	if cfg.ServerType == "" {
		cfg.ServerType = serverTypeForConfig(cfg)
	}
	return cfg, nil
}

func normalizeBrokerConfig(cfg *Config) error {
	mode, err := normalizeBrokerMode(string(cfg.BrokerMode))
	if err != nil {
		return err
	}
	cfg.BrokerMode = mode
	if mode == BrokerModeRegistered && strings.TrimSpace(cfg.Coordinator) == "" {
		return exit(2, "broker.mode=registered requires broker.url or coordinator")
	}
	return nil
}

func normalizeBrokerMode(value string) (BrokerMode, error) {
	mode := BrokerMode(strings.ToLower(strings.TrimSpace(value)))
	if mode == "" {
		mode = BrokerModeManaged
	}
	switch mode {
	case BrokerModeManaged, BrokerModeRegistered:
		return mode, nil
	default:
		return "", exit(2, "broker.mode must be managed or registered")
	}
}

func canonicalizeConfigProvider(cfg *Config) {
	provider, err := ProviderFor(cfg.Provider)
	if err == nil {
		cfg.Provider = provider.Name()
	}
}

func prepareProviderSelection(cfg *Config, provider string) error {
	cfg.Provider = strings.TrimSpace(provider)
	prepareProviderDefaults(cfg)
	return nil
}

func finalizeProviderSelection(cfg *Config) error {
	if err := routeConfiguredProvider(cfg); err != nil {
		return err
	}
	return applyProviderConfigDefaults(cfg)
}

func applyProviderConfigDefaults(cfg *Config) error {
	prepareProviderDefaults(cfg)
	if normalized, err := normalizeArchitecture(cfg.Architecture); err != nil {
		return err
	} else {
		cfg.Architecture = normalized
	}
	if normalized, err := normalizeOSImage(cfg.OSImage); err != nil {
		return err
	} else {
		cfg.OSImage = normalized
	}
	applySingleProviderTargetDefault(cfg)
	applyOSImageProviderDefaults(cfg, false)
	if cfg.Provider == "digitalocean" {
		if cfg.DigitalOcean.Region == "" {
			cfg.DigitalOcean.Region = "nyc3"
		}
		if cfg.osImageExplicit && !cfg.digitalOceanImageExplicit {
			if cfg.OSImage == "ubuntu:24.04" {
				cfg.DigitalOcean.Image = "ubuntu-24-04-x64"
			} else {
				cfg.DigitalOcean.Image = ""
			}
		} else if cfg.DigitalOcean.Image == "" {
			cfg.DigitalOcean.Image = "ubuntu-24-04-x64"
		}
		if !IsTargetExplicit(cfg) {
			cfg.TargetOS = targetLinux
		}
		if cfg.explicitWindowsMode != "" {
			cfg.WindowsMode = cfg.explicitWindowsMode
		} else {
			cfg.WindowsMode = windowsModeNormal
		}
		if cfg.explicitWorkRoot != "" {
			cfg.WorkRoot = cfg.explicitWorkRoot
		} else {
			cfg.WorkRoot = defaultPOSIXWorkRoot
		}
		if cfg.explicitSSHUser != "" {
			cfg.SSHUser = cfg.explicitSSHUser
		} else {
			cfg.SSHUser = baseConfig().SSHUser
		}
		if cfg.explicitSSHPort != "" {
			cfg.SSHPort = cfg.explicitSSHPort
		} else {
			cfg.SSHPort = baseConfig().SSHPort
		}
		normalizeTargetConfig(cfg)
		return validateTargetConfig(*cfg)
	}
	if cfg.Provider == "linode" {
		if cfg.Linode.Region == "" {
			cfg.Linode.Region = "us-ord"
		}
		if cfg.osImageExplicit && !cfg.linodeImageExplicit {
			if cfg.OSImage == "ubuntu:24.04" {
				cfg.Linode.Image = "linode/ubuntu24.04"
			} else {
				cfg.Linode.Image = ""
			}
		} else if cfg.Linode.Image == "" {
			cfg.Linode.Image = "linode/ubuntu24.04"
		}
		if cfg.Linode.Type == "" {
			cfg.Linode.Type = "g6-standard-1"
		}
		if !IsTargetExplicit(cfg) {
			cfg.TargetOS = targetLinux
		}
		if cfg.explicitWindowsMode != "" {
			cfg.WindowsMode = cfg.explicitWindowsMode
		} else {
			cfg.WindowsMode = windowsModeNormal
		}
		if cfg.explicitWorkRoot != "" {
			cfg.WorkRoot = cfg.explicitWorkRoot
		} else {
			cfg.WorkRoot = defaultPOSIXWorkRoot
		}
		if cfg.explicitSSHUser != "" {
			cfg.SSHUser = cfg.explicitSSHUser
		} else {
			cfg.SSHUser = baseConfig().SSHUser
		}
		if cfg.explicitSSHPort != "" {
			cfg.SSHPort = cfg.explicitSSHPort
		} else {
			cfg.SSHPort = baseConfig().SSHPort
		}
		normalizeTargetConfig(cfg)
		return validateTargetConfig(*cfg)
	}
	if cfg.Provider == "ovh" {
		if cfg.OVH.Endpoint == "" {
			cfg.OVH.Endpoint = "https://api.us.ovhcloud.com/1.0"
		}
		if cfg.OVH.Image == "" {
			cfg.OVH.Image = "Ubuntu 24.04"
		}
		if cfg.OVH.Flavor == "" {
			cfg.OVH.Flavor = "b3-8"
		}
		if !IsTargetExplicit(cfg) {
			cfg.TargetOS = targetLinux
		}
		if cfg.explicitWindowsMode != "" {
			cfg.WindowsMode = cfg.explicitWindowsMode
		} else {
			cfg.WindowsMode = windowsModeNormal
		}
		if cfg.explicitWorkRoot != "" {
			cfg.WorkRoot = cfg.explicitWorkRoot
		} else {
			cfg.WorkRoot = defaultPOSIXWorkRoot
		}
		if cfg.explicitSSHUser != "" {
			cfg.SSHUser = cfg.explicitSSHUser
		} else {
			cfg.SSHUser = baseConfig().SSHUser
		}
		if cfg.explicitSSHPort != "" {
			cfg.SSHPort = cfg.explicitSSHPort
		} else {
			cfg.SSHPort = baseConfig().SSHPort
		}
		normalizeTargetConfig(cfg)
		return validateTargetConfig(*cfg)
	}
	if cfg.Provider == "scaleway" {
		if cfg.Scaleway.Region == "" {
			cfg.Scaleway.Region = "fr-par"
		}
		if cfg.Scaleway.Zone == "" {
			cfg.Scaleway.Zone = "fr-par-1"
		}
		if cfg.osImageExplicit && !cfg.scalewayImageExplicit {
			if cfg.OSImage == "ubuntu:24.04" {
				cfg.Scaleway.Image = "ubuntu_noble"
			} else {
				cfg.Scaleway.Image = ""
			}
		} else if cfg.Scaleway.Image == "" {
			cfg.Scaleway.Image = "ubuntu_noble"
		}
		if cfg.Scaleway.Type == "" {
			cfg.Scaleway.Type = "DEV1-S"
		}
		if !IsTargetExplicit(cfg) {
			cfg.TargetOS = targetLinux
		}
		if cfg.explicitWindowsMode != "" {
			cfg.WindowsMode = cfg.explicitWindowsMode
		} else {
			cfg.WindowsMode = windowsModeNormal
		}
		if cfg.explicitWorkRoot != "" {
			cfg.WorkRoot = cfg.explicitWorkRoot
		} else {
			cfg.WorkRoot = defaultPOSIXWorkRoot
		}
		if cfg.explicitSSHUser != "" {
			cfg.SSHUser = cfg.explicitSSHUser
		} else {
			cfg.SSHUser = "root"
		}
		if cfg.explicitSSHPort != "" {
			cfg.SSHPort = cfg.explicitSSHPort
		} else {
			cfg.SSHPort = "22"
		}
		normalizeTargetConfig(cfg)
		return validateTargetConfig(*cfg)
	}
	if cfg.Provider == "hyperv" {
		if !IsTargetExplicit(cfg) {
			cfg.TargetOS = targetWindows
		}
		cfg.SSHFallbackPorts = nil
		if cfg.HyperV.User != "" {
			cfg.SSHUser = cfg.HyperV.User
		}
		if cfg.HyperV.WorkRoot != "" {
			cfg.WorkRoot = cfg.HyperV.WorkRoot
		}
		cfg.SSHPort = "22"
		return nil
	}
	if cfg.Provider == "windows-sandbox" || cfg.Provider == "wsb" || cfg.Provider == "windows-sandbox-provider" {
		if IsTargetExplicit(cfg) && normalizeTargetOS(cfg.TargetOS) != targetWindows {
			return exit(2, "provider=windows-sandbox supports target=windows only")
		}
		if cfg.TargetOS == "" || (!IsTargetExplicit(cfg) && cfg.TargetOS == targetLinux) {
			cfg.TargetOS = targetWindows
		}
		if cfg.explicitWindowsMode != "" && normalizeWindowsMode(cfg.explicitWindowsMode) != windowsModeNormal {
			return exit(2, "provider=windows-sandbox supports windows.mode=normal only")
		}
		cfg.WindowsMode = windowsModeNormal
		if cfg.WindowsSandbox.Workdir == "" {
			cfg.WindowsSandbox.Workdir = `C:\crabbox-work`
		}
		cfg.WorkRoot = cfg.WindowsSandbox.Workdir
		return nil
	}
	if cfg.Provider == "exe-dev" || cfg.Provider == "exedev" || cfg.Provider == "exe" {
		if cfg.ExeDev.User != "" {
			cfg.SSHUser = cfg.ExeDev.User
		} else if cfg.SSHUser == baseConfig().SSHUser {
			cfg.SSHUser = getenv("USER", cfg.SSHUser)
		}
		if cfg.SSHPort == "" || cfg.SSHPort == baseConfig().SSHPort {
			cfg.SSHPort = "22"
		}
		cfg.SSHFallbackPorts = nil
		if cfg.ExeDev.WorkRoot == "" {
			if !isDefaultWorkRoot(cfg.WorkRoot) {
				cfg.ExeDev.WorkRoot = cfg.WorkRoot
			} else {
				cfg.ExeDev.WorkRoot = "/tmp/crabbox"
			}
		}
		if cfg.ExeDev.WorkRoot != "" {
			cfg.WorkRoot = cfg.ExeDev.WorkRoot
		}
		if cfg.TargetOS == "" {
			cfg.TargetOS = targetLinux
		}
		return nil
	}
	if cfg.Provider == "tart" || cfg.Provider == "local-tart" || cfg.Provider == "macos-vm" {
		if cfg.Tart.User != "" {
			cfg.SSHUser = cfg.Tart.User
		}
		if cfg.SSHPort == "" || cfg.SSHPort == baseConfig().SSHPort {
			cfg.SSHPort = "22"
		}
		cfg.SSHFallbackPorts = nil
		if cfg.Tart.WorkRoot != "" {
			cfg.WorkRoot = cfg.Tart.WorkRoot
		}
		if !IsTargetExplicit(cfg) && (cfg.TargetOS == "" || cfg.TargetOS == targetLinux) {
			cfg.TargetOS = targetMacOS
		}
		if !cfg.ServerTypeExplicit && cfg.Tart.Image != "" {
			cfg.ServerType = cfg.Tart.Image
		}
		return nil
	}
	if cfg.Provider == "apple-vz" || cfg.Provider == "applevz" {
		if cfg.AppleVZ.User != "" {
			cfg.SSHUser = cfg.AppleVZ.User
		}
		if cfg.SSHPort == "" || cfg.SSHPort == baseConfig().SSHPort {
			cfg.SSHPort = "22"
		}
		cfg.SSHFallbackPorts = nil
		base := baseConfig()
		if cfg.AppleVZ.WorkRoot != "" && (IsDefaultWorkRoot(cfg.WorkRoot) || cfg.AppleVZ.WorkRoot != base.AppleVZ.WorkRoot) {
			cfg.WorkRoot = cfg.AppleVZ.WorkRoot
		}
		if cfg.TargetOS == "" {
			cfg.TargetOS = targetLinux
		}
		if !cfg.ServerTypeExplicit && cfg.AppleVZ.Image != "" {
			cfg.ServerType = redactRemoteURL(cfg.AppleVZ.Image)
		}
		return nil
	}
	if cfg.Provider == "incus" {
		base := baseConfig()
		if cfg.Incus.User != "" && (cfg.SSHUser == "" || cfg.SSHUser == base.SSHUser || cfg.Incus.User != base.Incus.User) {
			cfg.SSHUser = cfg.Incus.User
		}
		if cfg.SSHPort == "" || cfg.SSHPort == base.SSHPort {
			cfg.SSHPort = blank(cfg.Incus.ProxyListenPort, "22")
		}
		cfg.SSHFallbackPorts = nil
		if cfg.Incus.WorkRoot != "" && (isDefaultWorkRoot(cfg.WorkRoot) || cfg.Incus.WorkRoot != base.Incus.WorkRoot) {
			cfg.WorkRoot = cfg.Incus.WorkRoot
		}
		if cfg.TargetOS == "" {
			cfg.TargetOS = targetLinux
		}
		if !cfg.ServerTypeExplicit {
			cfg.ServerType = incusServerTypeForConfig(*cfg)
		}
		return nil
	}
	if cfg.Provider != "proxmox" {
		if cfg.Provider == "xcp-ng" {
			if cfg.XCPNg.User != "" {
				cfg.SSHUser = cfg.XCPNg.User
			}
			if cfg.XCPNg.WorkRoot != "" {
				cfg.WorkRoot = cfg.XCPNg.WorkRoot
			}
			return nil
		}
		if cfg.Provider != "parallels" {
			return nil
		}
		if cfg.Parallels.Template != "" && !cfg.parallelsTemplateApplied {
			if err := ApplyParallelsTemplateConfig(cfg, cfg.Parallels.Template); err != nil {
				return err
			}
		}
		if cfg.Parallels.User != "" {
			cfg.SSHUser = cfg.Parallels.User
		}
		if cfg.Parallels.WorkRoot != "" {
			cfg.WorkRoot = cfg.Parallels.WorkRoot
		}
		return nil
	}
	if cfg.Proxmox.User != "" {
		cfg.SSHUser = cfg.Proxmox.User
	}
	if cfg.Proxmox.WorkRoot != "" {
		cfg.WorkRoot = cfg.Proxmox.WorkRoot
	}
	return nil
}

func applySingleProviderTargetDefault(cfg *Config) {
	if cfg == nil {
		return
	}
	provider, err := ProviderFor(cfg.Provider)
	if err != nil {
		return
	}
	providerName := provider.Name()
	if IsTargetExplicit(cfg) {
		cfg.inferredTargetProvider = ""
		return
	}
	if cfg.inferredTargetProvider != "" && cfg.inferredTargetProvider != providerName {
		cfg.TargetOS = targetLinux
		cfg.inferredTargetProvider = ""
		if cfg.explicitWindowsMode != "" {
			cfg.WindowsMode = cfg.explicitWindowsMode
		} else {
			cfg.WindowsMode = windowsModeNormal
		}
	}
	if cfg.TargetOS != "" && cfg.TargetOS != targetLinux {
		return
	}
	spec := provider.Spec()
	if len(spec.Targets) != 1 {
		return
	}
	target := spec.Targets[0]
	if strings.TrimSpace(target.OS) == "" {
		return
	}
	cfg.TargetOS = strings.TrimSpace(target.OS)
	cfg.inferredTargetProvider = providerName
	if cfg.TargetOS == targetWindows {
		if strings.TrimSpace(target.WindowsMode) != "" {
			cfg.WindowsMode = strings.TrimSpace(target.WindowsMode)
		}
	} else if cfg.explicitWindowsMode == "" {
		cfg.WindowsMode = windowsModeNormal
	}
}

func prepareProviderDefaults(cfg *Config) {
	provider, err := ProviderFor(cfg.Provider)
	if err != nil {
		return
	}
	providerName := provider.Name()
	if cfg.providerDefaultsApplied != "" && cfg.providerDefaultsApplied != providerName {
		if cfg.providerDefaultsApplied == parallelsProvider {
			cfg.parallelsTemplateApplied = false
		}
		resetProviderDerivedDefaults(cfg)
		if !IsTargetExplicit(cfg) && cfg.inferredTargetProvider != "" {
			cfg.TargetOS = targetLinux
			cfg.inferredTargetProvider = ""
			if cfg.explicitWindowsMode != "" {
				cfg.WindowsMode = cfg.explicitWindowsMode
			} else {
				cfg.WindowsMode = windowsModeNormal
			}
		}
	}
	cfg.providerDefaultsApplied = providerName
}

func resetProviderDerivedDefaults(cfg *Config) {
	base := baseConfig()
	if cfg.explicitSSHUser != "" {
		cfg.SSHUser = cfg.explicitSSHUser
	} else {
		cfg.SSHUser = base.SSHUser
	}
	if cfg.explicitSSHPort != "" {
		cfg.SSHPort = cfg.explicitSSHPort
	} else {
		cfg.SSHPort = base.SSHPort
	}
	if !cfg.sshFallbackPortsExplicit {
		cfg.SSHFallbackPorts = append([]string(nil), base.SSHFallbackPorts...)
	} else {
		cfg.SSHFallbackPorts = append([]string(nil), cfg.explicitSSHFallbackPorts...)
	}
	if cfg.explicitWorkRoot != "" {
		cfg.WorkRoot = cfg.explicitWorkRoot
	} else {
		cfg.WorkRoot = base.WorkRoot
	}
	if !cfg.locationExplicit {
		cfg.Location = base.Location
	}
	if !cfg.imageExplicit {
		cfg.Image = base.Image
	}
	if !cfg.ServerTypeExplicit {
		cfg.ServerType = base.ServerType
	}
}

func applyOSImageProviderDefaults(cfg *Config, force bool) {
	if normalizeTargetOS(cfg.TargetOS) != targetLinux {
		return
	}
	hetznerImage, azureImage, gcpImage, linodeImage, isloImage, containerImage, err := osImageDefaultProviderImagesForArchitecture(cfg.OSImage, effectiveArchitectureForConfig(*cfg))
	if err != nil {
		return
	}
	multipassImage, err := osImageDefaultMultipassImage(cfg.OSImage)
	if err != nil {
		return
	}
	appleVZImage, err := osImageDefaultAppleVZImage(cfg.OSImage)
	if err != nil {
		return
	}
	appleVZSHA256, err := osImageDefaultAppleVZSHA256(cfg.OSImage)
	if err != nil {
		return
	}
	base := baseConfig()
	wasOSDefault := cfg.osImageProviderDefaults != ""
	if force || cfg.Image == "" || (!cfg.imageExplicit && (cfg.Image == base.Image || wasOSDefault)) {
		cfg.Image = hetznerImage
	}
	if force || cfg.AzureImage == "" || (!cfg.azureImageExplicit && (cfg.AzureImage == base.AzureImage || wasOSDefault)) {
		cfg.AzureImage = azureImage
	}
	if force || cfg.GCPImage == "" || (!cfg.gcpImageExplicit && (cfg.GCPImage == base.GCPImage || wasOSDefault)) {
		cfg.GCPImage = gcpImage
	}
	if force || cfg.Linode.Image == "" || (!cfg.linodeImageExplicit && (cfg.Linode.Image == base.Linode.Image || wasOSDefault)) {
		cfg.Linode.Image = linodeImage
	}
	if force || cfg.Islo.Image == "" || (!cfg.isloImageExplicit && (cfg.Islo.Image == base.Islo.Image || wasOSDefault)) {
		cfg.Islo.Image = isloImage
	}
	if force || cfg.LocalContainer.Image == "" || (!cfg.localContainerImageExplicit && (cfg.LocalContainer.Image == base.LocalContainer.Image || wasOSDefault)) {
		cfg.LocalContainer.Image = containerImage
	}
	if force || cfg.AppleContainer.Image == "" || (!cfg.appleContainerImageExplicit && (cfg.AppleContainer.Image == base.AppleContainer.Image || wasOSDefault)) {
		cfg.AppleContainer.Image = containerImage
	}
	if force || cfg.AppleVZ.Image == "" || (!cfg.appleVZImageExplicit && (cfg.AppleVZ.Image == base.AppleVZ.Image || wasOSDefault)) {
		cfg.AppleVZ.Image = appleVZImage
	}
	if !cfg.appleVZImageSHA256Explicit && (force || (cfg.AppleVZ.ImageSHA256 == "" && cfg.AppleVZ.Image == appleVZImage) || (!cfg.appleVZImageExplicit && (cfg.AppleVZ.ImageSHA256 == "" || wasOSDefault))) {
		cfg.AppleVZ.ImageSHA256 = appleVZSHA256
	}
	if force || cfg.Multipass.Image == "" || (!cfg.multipassImageExplicit && (cfg.Multipass.Image == base.Multipass.Image || wasOSDefault)) {
		cfg.Multipass.Image = multipassImage
	}
	cfg.osImageProviderDefaults = cfg.OSImage
}

func MarkIsloImageExplicit(cfg *Config) {
	cfg.isloImageExplicit = true
}

func IsloImageExplicit(cfg Config) bool {
	return cfg.isloImageExplicit
}

func MarkIsloVCPUsExplicit(cfg *Config) {
	cfg.isloVCPUsExplicit = true
}

func IsloVCPUsExplicit(cfg Config) bool {
	return cfg.isloVCPUsExplicit
}

func MarkIsloMemoryMBExplicit(cfg *Config) {
	cfg.isloMemoryMBExplicit = true
}

func IsloMemoryMBExplicit(cfg Config) bool {
	return cfg.isloMemoryMBExplicit
}

func MarkIsloDiskGBExplicit(cfg *Config) {
	cfg.isloDiskGBExplicit = true
}

func IsloDiskGBExplicit(cfg Config) bool {
	return cfg.isloDiskGBExplicit
}

func MarkLocalContainerImageExplicit(cfg *Config) {
	cfg.localContainerImageExplicit = true
}

func MarkLocalContainerRuntimeExplicit(cfg *Config) {
	cfg.localContainerRuntimeExplicit = true
}

func LocalContainerRuntimeExplicit(cfg Config) bool {
	return cfg.localContainerRuntimeExplicit
}

func MarkAppleContainerImageExplicit(cfg *Config) {
	cfg.appleContainerImageExplicit = true
}

func AppleContainerImageExplicit(cfg Config) bool {
	return cfg.appleContainerImageExplicit
}

func MarkAppleVZImageExplicit(cfg *Config) {
	cfg.appleVZImageExplicit = true
	cfg.appleVZImageSHA256Explicit = false
}

func AppleVZImageExplicit(cfg Config) bool {
	return cfg.appleVZImageExplicit
}

func MarkAppleVZImageSHA256Explicit(cfg *Config) {
	cfg.appleVZImageSHA256Explicit = true
}

func AppleVZCPUsExplicit(cfg Config) bool {
	return cfg.appleVZCPUsExplicit
}

func MarkAppleVZCPUsExplicit(cfg *Config) {
	cfg.appleVZCPUsExplicit = true
}

func AppleVZMemoryExplicit(cfg Config) bool {
	return cfg.appleVZMemoryExplicit
}

func MarkAppleVZMemoryExplicit(cfg *Config) {
	cfg.appleVZMemoryExplicit = true
}

func AppleVZDiskExplicit(cfg Config) bool {
	return cfg.appleVZDiskExplicit
}

func MarkAppleVZDiskExplicit(cfg *Config) {
	cfg.appleVZDiskExplicit = true
}

func MarkMultipassImageExplicit(cfg *Config) {
	cfg.multipassImageExplicit = true
}

func MarkTartImageExplicit(cfg *Config) {
	cfg.tartImageExplicit = true
}

func IsTartDiskExplicit(cfg *Config) bool {
	return cfg.tartDiskExplicit
}

func MarkTartDiskExplicit(cfg *Config) {
	cfg.tartDiskExplicit = true
}

func IsTartCPUsExplicit(cfg *Config) bool {
	return cfg.tartCPUsExplicit
}

func MarkTartCPUsExplicit(cfg *Config) {
	cfg.tartCPUsExplicit = true
}

func IsTartMemoryExplicit(cfg *Config) bool {
	return cfg.tartMemoryExplicit
}

func MarkTartMemoryExplicit(cfg *Config) {
	cfg.tartMemoryExplicit = true
}

func IsTargetExplicit(cfg *Config) bool {
	return cfg.targetExplicit
}

func MarkTargetExplicit(cfg *Config) {
	cfg.targetExplicit = true
}

func IsSSHUserExplicit(cfg *Config) bool {
	return cfg.explicitSSHUser != ""
}

func MarkSSHUserExplicit(cfg *Config) {
	cfg.explicitSSHUser = cfg.SSHUser
}

func IsSSHKeyExplicit(cfg *Config) bool {
	return cfg != nil && cfg.SSHKey != "" && (cfg.explicitSSHKey != "" || cfg.SSHKey != baseConfig().SSHKey)
}

func MarkSSHKeyExplicit(cfg *Config) {
	cfg.explicitSSHKey = cfg.SSHKey
}

func IsSSHPortExplicit(cfg *Config) bool {
	return cfg.explicitSSHPort != ""
}

func MarkSSHPortExplicit(cfg *Config) {
	cfg.explicitSSHPort = cfg.SSHPort
}

func IsWorkRootExplicit(cfg *Config) bool {
	return cfg.explicitWorkRoot != ""
}

func MarkWorkRootExplicit(cfg *Config) {
	cfg.explicitWorkRoot = cfg.WorkRoot
}

func IsHostingerWorkRootExplicit(cfg *Config) bool {
	return cfg.hostingerWorkRootExplicit
}

func IsHostingerUserExplicit(cfg *Config) bool {
	return cfg.hostingerUserExplicit
}

func MarkHostingerUserExplicit(cfg *Config) {
	cfg.hostingerUserExplicit = true
}

func MarkHostingerWorkRootExplicit(cfg *Config) {
	cfg.hostingerWorkRootExplicit = true
}

func IsNvidiaBrevWorkRootExplicit(cfg *Config) bool {
	return cfg.nvidiaBrevWorkRootExplicit
}

func MarkNvidiaBrevWorkRootExplicit(cfg *Config) {
	cfg.nvidiaBrevWorkRootExplicit = true
}

func EffectiveNvidiaBrevWorkRoot(cfg Config) string {
	workRoot := cfg.NvidiaBrev.WorkRoot
	providerDefault := workRoot == "" || workRoot == "/tmp/crabbox"
	if !IsNvidiaBrevWorkRootExplicit(&cfg) && providerDefault && cfg.explicitWorkRoot != "" {
		return cfg.explicitWorkRoot
	}
	if workRoot == "" {
		return "/tmp/crabbox"
	}
	return workRoot
}

func DeleteOnReleaseExplicit(cfg Config, provider string) bool {
	return cfg.deleteOnReleaseExplicit[normalizeProviderName(provider)]
}

func MarkDeleteOnReleaseExplicit(cfg *Config, provider string) {
	if cfg.deleteOnReleaseExplicit == nil {
		cfg.deleteOnReleaseExplicit = map[string]bool{}
	}
	cfg.deleteOnReleaseExplicit[normalizeProviderName(provider)] = true
}

func EffectiveHostingerWorkRoot(cfg Config) string {
	if cfg.Hostinger.WorkRoot != "" {
		return cfg.Hostinger.WorkRoot
	}
	if cfg.explicitWorkRoot != "" {
		return cfg.explicitWorkRoot
	}
	user := strings.TrimSpace(cfg.Hostinger.User)
	if user == "" {
		user = "root"
	}
	return "/home/" + user + "/crabbox"
}

func baseConfig() Config {
	home, _ := os.UserHomeDir()
	sshKey := ""
	if home != "" {
		sshKey = filepath.Join(home, ".ssh", "id_ed25519")
	}

	class := "beast"
	provider := "hetzner"
	osImage := defaultOSImage
	hetznerImage, azureImage, gcpImage, linodeImage, isloImage, containerImage, _ := osImageDefaultProviderImages(osImage)
	multipassImage, _ := osImageDefaultMultipassImage(osImage)
	return Config{
		Profile:            "default",
		Provider:           provider,
		TargetOS:           "linux",
		Architecture:       ArchitectureAMD64,
		OSImage:            osImage,
		WindowsMode:        "normal",
		DesktopEnv:         desktopEnvXFCE,
		Network:            NetworkAuto,
		Class:              class,
		ServerType:         "",
		BrokerMode:         BrokerModeManaged,
		BrokerAutoWebVNC:   true,
		Location:           "fsn1",
		Image:              hetznerImage,
		AWSRegion:          "eu-west-1",
		AWSRootGB:          400,
		AzureBackend:       "vm",
		AzureLocation:      "eastus",
		AzureResourceGroup: "crabbox-leases",
		AzureImage:         azureImage,
		AzureOSDisk:        AzureOSDiskManaged,
		AzureVNet:          "crabbox-vnet",
		AzureSubnet:        "crabbox-subnet",
		AzureNSG:           "crabbox-nsg",
		AzureDynamicSessions: AzureDynamicSessionsConfig{
			APIVersion:  "2025-02-02-preview",
			Workdir:     "/workspace/crabbox",
			TimeoutSecs: 1800,
		},
		GCPZone:    "europe-west2-a",
		GCPImage:   gcpImage,
		GCPNetwork: "default",
		GCPTags:    []string{"crabbox-ssh"},
		GCPRootGB:  400,
		Linode: LinodeConfig{
			Region: "us-ord",
			Image:  linodeImage,
			Type:   "g6-standard-1",
		},
		OVH: OVHConfig{
			Endpoint: "https://api.us.ovhcloud.com/1.0",
			Image:    "Ubuntu 24.04",
			Flavor:   "b3-8",
		},
		Scaleway: ScalewayConfig{
			Region: "fr-par",
			Zone:   "fr-par-1",
			Image:  "ubuntu_noble",
			Type:   "DEV1-S",
		},
		Incus: IncusConfig{
			Remote:          "local",
			Project:         "",
			InstanceType:    "container",
			Image:           "images:ubuntu/24.04/cloud",
			User:            "crabbox",
			WorkRoot:        defaultPOSIXWorkRoot,
			DeleteOnRelease: true,
			StartTimeout:    10 * time.Minute,
			LaunchPort:      "22",
			ProxyListenHost: "127.0.0.1",
			ProxyDevice:     "crabbox-ssh",
		},
		SSHUser:          "crabbox",
		SSHKey:           sshKey,
		SSHPort:          "2222",
		SSHFallbackPorts: []string{"22"},
		ProviderKey:      "crabbox-steipete",
		WorkRoot:         defaultPOSIXWorkRoot,
		TTL:              90 * time.Minute,
		IdleTimeout:      30 * time.Minute,
		Sync: SyncConfig{
			Delete:      true,
			Checksum:    false,
			GitSeed:     true,
			Fingerprint: true,
			Timeout:     15 * time.Minute,
			WarnFiles:   50_000,
			WarnBytes:   5 * 1024 * 1024 * 1024,
			FailFiles:   150_000,
			FailBytes:   20 * 1024 * 1024 * 1024,
		},
		EnvAllow: []string{"CI", "NODE_OPTIONS"},
		Capacity: CapacityConfig{
			Market:   "spot",
			Strategy: "most-available",
			Fallback: "on-demand-after-120s",
			Hints:    true,
		},
		Actions: ActionsConfig{
			RunnerVersion: "latest",
			Ephemeral:     true,
		},
		KubeVirt: KubeVirtConfig{
			Kubectl:         "kubectl",
			Virtctl:         "virtctl",
			Namespace:       "default",
			SSHUser:         "crabbox",
			SSHPort:         "22",
			WorkRoot:        "/home/crabbox/crabbox",
			DeleteOnRelease: true,
		},
		AgentSandbox: AgentSandboxConfig{
			Kubectl:             "kubectl",
			Namespace:           "default",
			Workdir:             "/workspace/crabbox",
			SandboxReadyTimeout: 180 * time.Second,
			PodReadyTimeout:     180 * time.Second,
			ExecTimeoutSecs:     600,
			DeleteOnRelease:     true,
		},
		External: ExternalConfig{
			WorkRoot: defaultPOSIXWorkRoot,
		},
		Namespace: NamespaceConfig{
			Image:               "builtin:base",
			WorkRoot:            "/workspaces/crabbox",
			AutoStopIdleTimeout: 30 * time.Minute,
		},
		NamespaceInstance: NamespaceInstanceConfig{
			CLIPath:  "nsc",
			WorkRoot: "/work/crabbox",
			Bare:     true,
		},
		Phala: PhalaConfig{
			CLIPath:      "phala",
			InstanceType: "tdx.small",
			// The dstack --dev-os guest roots on a read-only squashfs; /work is not
			// writable. /var/volatile is a writable tmpfs on every dstack guest.
			WorkRoot: "/var/volatile/crabbox",
		},
		Morph: MorphConfig{
			APIURL:         "https://cloud.morph.so",
			SSHGatewayHost: "ssh.cloud.morph.so",
			WorkRoot:       "/tmp/crabbox",
			WakeOnSSH:      true,
		},
		Daytona: DaytonaConfig{
			APIURL:           "https://app.daytona.io/api",
			User:             "daytona",
			WorkRoot:         "/home/daytona/crabbox",
			SSHGatewayHost:   "ssh.app.daytona.io",
			SSHAccessMinutes: 30,
		},
		E2B: E2BConfig{
			APIURL:   "https://api.e2b.app",
			Domain:   "e2b.app",
			Template: "base",
			Workdir:  "crabbox",
		},
		ExeDev: ExeDevConfig{
			ControlHost: "exe.dev",
			CPUs:        2,
			Memory:      "4GB",
			Disk:        "10GB",
			NoEmail:     true,
		},
		Railway: RailwayConfig{
			APIURL: "https://backboard.railway.com/graphql/v2",
		},
		Runpod: RunpodConfig{
			APIURL:     "https://rest.runpod.io/v1",
			CloudType:  "SECURE",
			InstanceID: "NVIDIA L4,NVIDIA RTX 4000 Ada Generation,NVIDIA RTX A4000,NVIDIA GeForce RTX 3090,NVIDIA GeForce RTX 4090,NVIDIA RTX A5000,NVIDIA RTX A4500",
			Image:      "runpod/pytorch:2.8.0-py3.11-cuda12.8.1-cudnn-devel-ubuntu22.04",
			DiskGB:     20,
		},
		NvidiaBrev: NvidiaBrevConfig{
			CLI:           "brev",
			GPUName:       "A100",
			Mode:          "vm",
			ReleaseAction: "delete",
			Target:        "container",
			WorkRoot:      "/tmp/crabbox",
		},
		Hostinger: HostingerConfig{
			APIURL:         "https://developers.hostinger.com",
			HostnamePrefix: "crabbox",
			User:           "root",
			ReleaseAction:  "stop",
		},
		Islo: IsloConfig{
			BaseURL:  "https://api.islo.dev",
			Image:    isloImage,
			Workdir:  "crabbox",
			VCPUs:    2,
			MemoryMB: 4096,
			DiskGB:   20,
		},
		Freestyle: FreestyleConfig{
			APIURL:  "https://api.freestyle.sh",
			Workdir: "crabbox",
		},
		Tenki: TenkiConfig{
			CLIPath:  "tenki",
			WorkRoot: "/home/tenki/crabbox",
		},
		Tensorlake: TensorlakeConfig{
			APIURL:   "https://api.tensorlake.ai",
			CLIPath:  "tensorlake",
			Workdir:  "/workspace/crabbox",
			CPUs:     1.0,
			MemoryMB: 1024,
			DiskMB:   10240,
		},
		OpenComputer: OpenComputerConfig{
			// APIURL is intentionally unset here so the `oc` config file's
			// api_url is honored before the built-in default; the provider
			// applies the default (https://app.opencomputer.dev) as the final
			// fallback in newOCAPIClient.
			Workdir:         "/workspace/crabbox",
			ExecTimeoutSecs: 3600,
		},
		CodeSandbox: CodeSandboxConfig{
			Workdir:                  "/project/workspace",
			Privacy:                  "private",
			AutomaticWakeupHTTP:      true,
			AutomaticWakeupWebSocket: false,
			BridgeCommand:            "node",
			SDKPackage:               "@codesandbox/sdk@2.4.2",
			DoctorListLimit:          1,
			OperationTimeoutSecs:     30,
		},
		OpenSandbox: OpenSandboxConfig{
			// APIURL is intentionally unset here so repository YAML cannot
			// redirect a shell-provided API key. The provider requires an
			// explicit trusted endpoint from flags or environment.
			Image:           "ubuntu:24.04",
			Workdir:         "/workspace/crabbox",
			CPU:             "1",
			Memory:          "2Gi",
			ExecTimeoutSecs: 600,
			PlatformOS:      "linux",
			PlatformArch:    "amd64",
		},
		VercelSandbox: VercelSandboxConfig{
			Runtime:         "node24",
			Workdir:         "/vercel/sandbox/crabbox",
			ExecTimeoutSecs: 600,
			NetworkPolicy:   "default",
		},
		Superserve: SuperserveConfig{
			BaseURL:         "https://api.superserve.ai",
			Template:        "superserve/base",
			Workdir:         "/workspace/crabbox",
			ExecTimeoutSecs: 600,
		},
		DockerSandbox: DockerSandboxConfig{
			CLIPath: "sbx",
			Agent:   "shell",
		},
		AnthropicSRT: AnthropicSRTConfig{
			CLIPath: "srt",
		},
		Modal: ModalConfig{
			App:     "crabbox",
			Image:   "python:3.13-slim",
			Workdir: "/workspace/crabbox",
			Python:  "python3",
		},
		UpstashBox: UpstashBoxConfig{
			BaseURL: "https://us-east-1.box.upstash.com",
			Runtime: "node",
			Size:    "small",
			Workdir: "/workspace/home/crabbox",
		},
		Smolvm: SmolvmConfig{
			BaseURL:  "https://api.smolmachines.com",
			Image:    "alpine",
			Workdir:  "/workspace",
			CPUs:     2,
			MemoryMB: 2048,
			Network:  "open",
		},
		AsciiBox: AsciiBoxConfig{
			BaseURL: "https://ascii.dev",
			CLIPath: "box",
			Workdir: "/home/user/crabbox",
		},
		Cloudflare: CloudflareConfig{
			Workdir: "/workspace/crabbox",
		},
		CloudflareDynamicWorkers: CloudflareDynamicWorkersConfig{
			CompatibilityDate: DefaultCloudflareDynamicWorkersCompatibilityDate,
			CacheMode:         "stable",
			Egress:            "blocked",
			TimeoutSecs:       60,
			Metadata:          map[string]string{},
		},
		Proxmox: ProxmoxConfig{
			User:      "crabbox",
			WorkRoot:  defaultPOSIXWorkRoot,
			FullClone: true,
		},
		XCPNg: XCPNgConfig{
			User:     "crabbox",
			WorkRoot: defaultPOSIXWorkRoot,
		},
		Parallels: ParallelsConfig{
			CloneMode:      "linked",
			User:           "crabbox",
			StartupTimeout: 15 * time.Minute,
		},
		Sprites: SpritesConfig{
			APIURL:   "https://api.sprites.dev",
			WorkRoot: "/home/sprite/crabbox",
		},
		LocalContainer: LocalContainerConfig{
			Runtime: "docker",
			Image:   containerImage,
			User:    "crabbox",
			Network: "bridge",
		},
		AppleContainer: AppleContainerConfig{
			CLIPath:  "container",
			Image:    containerImage,
			User:     "crabbox",
			WorkRoot: "/work/crabbox",
		},
		AppleVZ: AppleVZConfig{
			Image:       osImageSpecs[osImage].AppleVZImage,
			ImageSHA256: osImageSpecs[osImage].AppleVZSHA256,
			User:        "crabbox",
			WorkRoot:    "/work/crabbox",
			CPUs:        4,
			MemoryMiB:   8192,
			DiskGiB:     30,
		},
		MXC: MXCConfig{
			CLIPath:     "wxc-exec.exe",
			Version:     "0.6.0-alpha",
			Containment: "processcontainer",
			Network:     "block",
		},
		Multipass: MultipassConfig{
			CLIPath:       "multipass",
			Image:         multipassImage,
			User:          "crabbox",
			WorkRoot:      defaultPOSIXWorkRoot,
			CPUs:          4,
			Memory:        "8G",
			Disk:          "30G",
			LaunchTimeout: 20 * time.Minute,
		},
		Tart: TartConfig{
			Image:    "ghcr.io/cirruslabs/macos-sequoia-base:latest",
			User:     "admin",
			WorkRoot: "/Users/admin/crabbox",
			CPUs:     4,
			Memory:   8192,
		},
		HyperV: HyperVConfig{
			User:     "crabbox",
			WorkRoot: defaultWindowsWorkRoot,
			CPUs:     4,
			Memory:   8192,
			Switch:   "Default Switch",
		},
		WindowsSandbox: WindowsSandboxConfig{
			Workdir:            `C:\crabbox-work`,
			Networking:         "Enable",
			VGPU:               "Disable",
			Clipboard:          "Disable",
			ProtectedClient:    "Default",
			AudioInput:         "Disable",
			VideoInput:         "Disable",
			PrinterRedirection: "Disable",
		},
		Tailscale: TailscaleConfig{
			Tags:             []string{"tag:crabbox"},
			HostnameTemplate: "crabbox-{slug}",
			AuthKeyEnv:       "CRABBOX_TAILSCALE_AUTH_KEY",
		},
		Cache: CacheConfig{
			Pnpm:   true,
			Npm:    true,
			Docker: true,
			Git:    true,
			MaxGB:  80,
		},
	}
}

type fileConfig struct {
	Profile                  string                              `yaml:"profile,omitempty"`
	Provider                 string                              `yaml:"provider,omitempty"`
	Target                   string                              `yaml:"target,omitempty"`
	TargetOS                 string                              `yaml:"targetOS,omitempty"`
	Architecture             string                              `yaml:"architecture,omitempty"`
	OSImage                  string                              `yaml:"os,omitempty"`
	Windows                  *fileWindowsConfig                  `yaml:"windows,omitempty"`
	Desktop                  *bool                               `yaml:"desktop,omitempty"`
	DesktopEnv               string                              `yaml:"desktopEnv,omitempty"`
	Browser                  *bool                               `yaml:"browser,omitempty"`
	Code                     *bool                               `yaml:"code,omitempty"`
	Network                  string                              `yaml:"network,omitempty"`
	Class                    string                              `yaml:"class,omitempty"`
	ServerType               string                              `yaml:"serverType,omitempty"`
	Coordinator              string                              `yaml:"coordinator,omitempty"`
	CoordinatorToken         string                              `yaml:"coordinatorToken,omitempty"`
	HostID                   string                              `yaml:"hostId,omitempty"`
	Broker                   *fileBrokerConfig                   `yaml:"broker,omitempty"`
	Hetzner                  *fileHetznerConfig                  `yaml:"hetzner,omitempty"`
	DigitalOcean             *fileDigitalOceanConfig             `yaml:"digitalocean,omitempty"`
	Linode                   *fileLinodeConfig                   `yaml:"linode,omitempty"`
	OVH                      *fileOVHConfig                      `yaml:"ovh,omitempty"`
	Scaleway                 *fileScalewayConfig                 `yaml:"scaleway,omitempty"`
	AWS                      *fileAWSConfig                      `yaml:"aws,omitempty"`
	Azure                    *fileAzureConfig                    `yaml:"azure,omitempty"`
	AzureDynamicSessions     *fileAzureDynamicSessionsConfig     `yaml:"azureDynamicSessions,omitempty"`
	GCP                      *fileGCPConfig                      `yaml:"gcp,omitempty"`
	Incus                    *fileIncusConfig                    `yaml:"incus,omitempty"`
	Proxmox                  *fileProxmoxConfig                  `yaml:"proxmox,omitempty"`
	XCPNg                    *fileXCPNgConfig                    `yaml:"xcpNg,omitempty"`
	Parallels                *fileParallelsConfig                `yaml:"parallels,omitempty"`
	SSH                      *fileSSHConfig                      `yaml:"ssh,omitempty"`
	Sync                     *fileSyncConfig                     `yaml:"sync,omitempty"`
	Run                      *fileRunConfig                      `yaml:"run,omitempty"`
	Env                      *fileEnvConfig                      `yaml:"env,omitempty"`
	Capacity                 *fileCapacityConfig                 `yaml:"capacity,omitempty"`
	Actions                  *fileActionsConfig                  `yaml:"actions,omitempty"`
	Blacksmith               *fileBlacksmithConfig               `yaml:"blacksmith,omitempty"`
	KubeVirt                 *fileKubeVirtConfig                 `yaml:"kubevirt,omitempty"`
	AgentSandbox             *fileAgentSandboxConfig             `yaml:"agentSandbox,omitempty"`
	External                 *fileExternalConfig                 `yaml:"external,omitempty"`
	Namespace                *fileNamespaceConfig                `yaml:"namespace,omitempty"`
	NamespaceInstance        *fileNamespaceInstanceConfig        `yaml:"namespaceInstance,omitempty"`
	Phala                    *filePhalaConfig                    `yaml:"phala,omitempty"`
	Morph                    *fileMorphConfig                    `yaml:"morph,omitempty"`
	Daytona                  *fileDaytonaConfig                  `yaml:"daytona,omitempty"`
	E2B                      *fileE2BConfig                      `yaml:"e2b,omitempty"`
	ExeDev                   *fileExeDevConfig                   `yaml:"exeDev,omitempty"`
	Railway                  *fileRailwayConfig                  `yaml:"railway,omitempty"`
	Runpod                   *fileRunpodConfig                   `yaml:"runpod,omitempty"`
	NvidiaBrev               *fileNvidiaBrevConfig               `yaml:"nvidiaBrev,omitempty"`
	Hostinger                *fileHostingerConfig                `yaml:"hostinger,omitempty"`
	Wandb                    *fileWandbConfig                    `yaml:"wandb,omitempty"`
	Islo                     *fileIsloConfig                     `yaml:"islo,omitempty"`
	Freestyle                *fileFreestyleConfig                `yaml:"freestyle,omitempty"`
	Tenki                    *fileTenkiConfig                    `yaml:"tenki,omitempty"`
	Tensorlake               *fileTensorlakeConfig               `yaml:"tensorlake,omitempty"`
	OpenComputer             *fileOpenComputerConfig             `yaml:"openComputer,omitempty"`
	CodeSandbox              *fileCodeSandboxConfig              `yaml:"codeSandbox,omitempty"`
	OpenSandbox              *fileOpenSandboxConfig              `yaml:"openSandbox,omitempty"`
	VercelSandbox            *fileVercelSandboxConfig            `yaml:"vercelSandbox,omitempty"`
	Superserve               *fileSuperserveConfig               `yaml:"superserve,omitempty"`
	DockerSandbox            *fileDockerSandboxConfig            `yaml:"dockerSandbox,omitempty"`
	AnthropicSRT             *fileAnthropicSRTConfig             `yaml:"anthropicSandboxRuntime,omitempty"`
	Modal                    *fileModalConfig                    `yaml:"modal,omitempty"`
	UpstashBox               *fileUpstashBoxConfig               `yaml:"upstashBox,omitempty"`
	Smolvm                   *fileSmolvmConfig                   `yaml:"smolvm,omitempty"`
	AsciiBox                 *fileAsciiBoxConfig                 `yaml:"asciiBox,omitempty"`
	Cloudflare               *fileCloudflareConfig               `yaml:"cloudflare,omitempty"`
	CloudflareDynamicWorkers *fileCloudflareDynamicWorkersConfig `yaml:"cloudflareDynamicWorkers,omitempty"`
	Semaphore                *fileSemaphoreConfig                `yaml:"semaphore,omitempty"`
	Sprites                  *fileSpritesConfig                  `yaml:"sprites,omitempty"`
	LocalContainer           *fileLocalContainerConfig           `yaml:"localContainer,omitempty"`
	AppleContainer           *fileAppleContainerConfig           `yaml:"appleContainer,omitempty"`
	AppleVZ                  *fileAppleVZConfig                  `yaml:"appleVZ,omitempty"`
	MXC                      *fileMXCConfig                      `yaml:"mxc,omitempty"`
	Multipass                *fileMultipassConfig                `yaml:"multipass,omitempty"`
	Tart                     *fileTartConfig                     `yaml:"tart,omitempty"`
	HyperV                   *fileHyperVConfig                   `yaml:"hyperv,omitempty"`
	WindowsSandbox           *fileWindowsSandboxConfig           `yaml:"windowsSandbox,omitempty"`
	Tailscale                *fileTailscaleConfig                `yaml:"tailscale,omitempty"`
	Static                   *fileStaticConfig                   `yaml:"static,omitempty"`
	Results                  *fileResultsConfig                  `yaml:"results,omitempty"`
	Cache                    *fileCacheConfig                    `yaml:"cache,omitempty"`
	Lease                    *fileLeaseConfig                    `yaml:"lease,omitempty"`
	Profiles                 map[string]fileProfileConfig        `yaml:"profiles,omitempty"`
	Presets                  map[string]filePresetConfig         `yaml:"presets,omitempty"`
	ProofTemplates           map[string]fileProofTemplateConfig  `yaml:"proofTemplates,omitempty"`
	Jobs                     map[string]fileJobConfig            `yaml:"jobs,omitempty"`
	TTL                      string                              `yaml:"ttl,omitempty"`
	IdleTimeout              string                              `yaml:"idleTimeout,omitempty"`
	WorkRoot                 string                              `yaml:"workRoot,omitempty"`
}

type fileWindowsConfig struct {
	Mode string `yaml:"mode,omitempty"`
}

type fileBrokerConfig struct {
	URL        string            `yaml:"url,omitempty"`
	Mode       string            `yaml:"mode,omitempty"`
	AutoWebVNC *bool             `yaml:"autoWebVNC,omitempty"`
	Token      string            `yaml:"token,omitempty"`
	AdminToken string            `yaml:"adminToken,omitempty"`
	Provider   string            `yaml:"provider,omitempty"`
	Access     *fileAccessConfig `yaml:"access,omitempty"`
}

type fileAccessConfig struct {
	ClientID     string `yaml:"clientId,omitempty"`
	ClientSecret string `yaml:"clientSecret,omitempty"`
	Token        string `yaml:"token,omitempty"`
}

type fileHetznerConfig struct {
	Location string `yaml:"location,omitempty"`
	Image    string `yaml:"image,omitempty"`
	SSHKey   string `yaml:"sshKey,omitempty"`
}

type fileDigitalOceanConfig struct {
	Region   string   `yaml:"region,omitempty"`
	Image    string   `yaml:"image,omitempty"`
	VPCUUID  string   `yaml:"vpc,omitempty"`
	SSHCIDRs []string `yaml:"sshCIDRs,omitempty"`
}

type fileLinodeConfig struct {
	Region     string   `yaml:"region,omitempty"`
	Image      string   `yaml:"image,omitempty"`
	Type       string   `yaml:"type,omitempty"`
	FirewallID string   `yaml:"firewall,omitempty"`
	SSHCIDRs   []string `yaml:"sshCIDRs,omitempty"`
}

type fileOVHConfig struct {
	Endpoint  string `yaml:"endpoint,omitempty"`
	ProjectID string `yaml:"projectId,omitempty"`
	Region    string `yaml:"region,omitempty"`
	Image     string `yaml:"image,omitempty"`
	Flavor    string `yaml:"flavor,omitempty"`
}

type fileScalewayConfig struct {
	Region         string   `yaml:"region,omitempty"`
	Zone           string   `yaml:"zone,omitempty"`
	Image          string   `yaml:"image,omitempty"`
	Type           string   `yaml:"type,omitempty"`
	ProjectID      string   `yaml:"projectId,omitempty"`
	OrganizationID string   `yaml:"organizationId,omitempty"`
	SecurityGroup  string   `yaml:"securityGroup,omitempty"`
	SSHCIDRs       []string `yaml:"sshCIDRs,omitempty"`
}

type fileAWSConfig struct {
	Region          string   `yaml:"region,omitempty"`
	AMI             string   `yaml:"ami,omitempty"`
	SecurityGroupID string   `yaml:"securityGroupId,omitempty"`
	SubnetID        string   `yaml:"subnetId,omitempty"`
	InstanceProfile string   `yaml:"instanceProfile,omitempty"`
	RootGB          int32    `yaml:"rootGB,omitempty"`
	SSHCIDRs        []string `yaml:"sshCIDRs,omitempty"`
	MacHostID       string   `yaml:"macHostId,omitempty"`
}

type fileAzureConfig struct {
	SubscriptionID string   `yaml:"subscriptionId,omitempty"`
	TenantID       string   `yaml:"tenantId,omitempty"`
	ClientID       string   `yaml:"clientId,omitempty"`
	Backend        string   `yaml:"backend,omitempty"`
	Location       string   `yaml:"location,omitempty"`
	ResourceGroup  string   `yaml:"resourceGroup,omitempty"`
	Image          string   `yaml:"image,omitempty"`
	OSDisk         string   `yaml:"osDisk,omitempty"`
	VNet           string   `yaml:"vnet,omitempty"`
	Subnet         string   `yaml:"subnet,omitempty"`
	NSG            string   `yaml:"nsg,omitempty"`
	SSHCIDRs       []string `yaml:"sshCIDRs,omitempty"`
	Network        string   `yaml:"network,omitempty"`
}

type fileGCPConfig struct {
	Project        string   `yaml:"project,omitempty"`
	Zone           string   `yaml:"zone,omitempty"`
	Image          string   `yaml:"image,omitempty"`
	Network        string   `yaml:"network,omitempty"`
	Subnet         string   `yaml:"subnet,omitempty"`
	Tags           []string `yaml:"tags,omitempty"`
	SSHCIDRs       []string `yaml:"sshCIDRs,omitempty"`
	RootGB         int64    `yaml:"rootGB,omitempty"`
	ServiceAccount string   `yaml:"serviceAccount,omitempty"`
}

type fileIncusConfig struct {
	Remote            string `yaml:"remote,omitempty"`
	Project           string `yaml:"project,omitempty"`
	Address           string `yaml:"address,omitempty"`
	Socket            string `yaml:"socket,omitempty"`
	InstanceType      string `yaml:"instanceType,omitempty"`
	Image             string `yaml:"image,omitempty"`
	Profile           string `yaml:"profile,omitempty"`
	User              string `yaml:"user,omitempty"`
	WorkRoot          string `yaml:"workRoot,omitempty"`
	DeleteOnRelease   *bool  `yaml:"deleteOnRelease,omitempty"`
	StartTimeout      string `yaml:"startTimeout,omitempty"`
	LaunchPort        string `yaml:"launchPort,omitempty"`
	ProxyListenHost   string `yaml:"proxyListenHost,omitempty"`
	ProxyListenPort   string `yaml:"proxyListenPort,omitempty"`
	ProxyDevice       string `yaml:"proxyDevice,omitempty"`
	TLSServerCert     string `yaml:"tlsServerCert,omitempty"`
	InsecureTLS       *bool  `yaml:"insecureTLS,omitempty"`
	RemoteImageServer string `yaml:"remoteImageServer,omitempty"`
}

type fileProxmoxConfig struct {
	APIURL      string `yaml:"apiUrl,omitempty"`
	TokenID     string `yaml:"tokenId,omitempty"`
	TokenSecret string `yaml:"tokenSecret,omitempty"`
	Node        string `yaml:"node,omitempty"`
	TemplateID  int    `yaml:"templateId,omitempty"`
	Storage     string `yaml:"storage,omitempty"`
	Pool        string `yaml:"pool,omitempty"`
	Bridge      string `yaml:"bridge,omitempty"`
	User        string `yaml:"user,omitempty"`
	WorkRoot    string `yaml:"workRoot,omitempty"`
	FullClone   *bool  `yaml:"fullClone,omitempty"`
	InsecureTLS *bool  `yaml:"insecureTLS,omitempty"`
}

type fileXCPNgConfig struct {
	APIURL       string `yaml:"apiUrl,omitempty"`
	Username     string `yaml:"username,omitempty"`
	Password     string `yaml:"password,omitempty"`
	Template     string `yaml:"template,omitempty"`
	TemplateUUID string `yaml:"templateUuid,omitempty"`
	SR           string `yaml:"sr,omitempty"`
	SRUUID       string `yaml:"srUuid,omitempty"`
	Network      string `yaml:"network,omitempty"`
	NetworkUUID  string `yaml:"networkUuid,omitempty"`
	Host         string `yaml:"host,omitempty"`
	User         string `yaml:"user,omitempty"`
	WorkRoot     string `yaml:"workRoot,omitempty"`
	InsecureTLS  *bool  `yaml:"insecureTLS,omitempty"`
}

type fileParallelsConfig struct {
	Template         string                                 `yaml:"template,omitempty"`
	Source           string                                 `yaml:"source,omitempty"`
	SourceID         string                                 `yaml:"sourceId,omitempty"`
	SourceSnapshot   string                                 `yaml:"sourceSnapshot,omitempty"`
	SourceSnapshotID string                                 `yaml:"sourceSnapshotId,omitempty"`
	CloneMode        string                                 `yaml:"cloneMode,omitempty"`
	Host             string                                 `yaml:"host,omitempty"`
	HostUser         string                                 `yaml:"hostUser,omitempty"`
	HostKey          string                                 `yaml:"hostKey,omitempty"`
	VMRoot           string                                 `yaml:"vmRoot,omitempty"`
	User             string                                 `yaml:"user,omitempty"`
	WorkRoot         string                                 `yaml:"workRoot,omitempty"`
	StartupTimeout   string                                 `yaml:"startupTimeout,omitempty"`
	Templates        map[string]fileParallelsTemplateConfig `yaml:"templates,omitempty"`
	Hosts            []fileParallelsHostConfig              `yaml:"hosts,omitempty"`
}

type fileParallelsTemplateConfig struct {
	Source           string `yaml:"source,omitempty"`
	SourceID         string `yaml:"sourceId,omitempty"`
	SourceSnapshot   string `yaml:"sourceSnapshot,omitempty"`
	SourceSnapshotID string `yaml:"sourceSnapshotId,omitempty"`
	Target           string `yaml:"target,omitempty"`
	TargetOS         string `yaml:"targetOS,omitempty"`
	WindowsMode      string `yaml:"windowsMode,omitempty"`
	CloneMode        string `yaml:"cloneMode,omitempty"`
	Host             string `yaml:"host,omitempty"`
	HostUser         string `yaml:"hostUser,omitempty"`
	HostKey          string `yaml:"hostKey,omitempty"`
	VMRoot           string `yaml:"vmRoot,omitempty"`
	User             string `yaml:"user,omitempty"`
	WorkRoot         string `yaml:"workRoot,omitempty"`
}

type fileParallelsHostConfig struct {
	Name    string   `yaml:"name,omitempty"`
	Host    string   `yaml:"host,omitempty"`
	User    string   `yaml:"user,omitempty"`
	Key     string   `yaml:"key,omitempty"`
	VMRoot  string   `yaml:"vmRoot,omitempty"`
	Targets []string `yaml:"targets,omitempty"`
	MaxVMs  int      `yaml:"maxVMs,omitempty"`
}

type fileSSHConfig struct {
	User          string    `yaml:"user,omitempty"`
	Key           string    `yaml:"key,omitempty"`
	Port          string    `yaml:"port,omitempty"`
	FallbackPorts *[]string `yaml:"fallbackPorts,omitempty"`
}

type fileSyncConfig struct {
	Exclude     []string `yaml:"exclude,omitempty"`
	Excludes    []string `yaml:"excludes,omitempty"`
	Include     []string `yaml:"include,omitempty"`
	Includes    []string `yaml:"includes,omitempty"`
	Delete      *bool    `yaml:"delete,omitempty"`
	Checksum    *bool    `yaml:"checksum,omitempty"`
	GitSeed     *bool    `yaml:"gitSeed,omitempty"`
	Fingerprint *bool    `yaml:"fingerprint,omitempty"`
	BaseRef     string   `yaml:"baseRef,omitempty"`
	Timeout     string   `yaml:"timeout,omitempty"`
	WarnFiles   int      `yaml:"warnFiles,omitempty"`
	WarnBytes   int64    `yaml:"warnBytes,omitempty"`
	FailFiles   int      `yaml:"failFiles,omitempty"`
	FailBytes   int64    `yaml:"failBytes,omitempty"`
	AllowLarge  *bool    `yaml:"allowLarge,omitempty"`
}

type fileEnvConfig struct {
	Allow []string `yaml:"allow,omitempty"`
}

type fileRunConfig struct {
	PreflightTools []string `yaml:"preflightTools,omitempty"`
}

type fileCapacityConfig struct {
	Market            string   `yaml:"market,omitempty"`
	Strategy          string   `yaml:"strategy,omitempty"`
	Fallback          string   `yaml:"fallback,omitempty"`
	Regions           []string `yaml:"regions,omitempty"`
	AvailabilityZones []string `yaml:"availabilityZones,omitempty"`
	Hints             *bool    `yaml:"hints,omitempty"`
}

type fileActionsConfig struct {
	Repo          string   `yaml:"repo,omitempty"`
	Workflow      string   `yaml:"workflow,omitempty"`
	Job           string   `yaml:"job,omitempty"`
	Ref           string   `yaml:"ref,omitempty"`
	Fields        []string `yaml:"fields,omitempty"`
	RunnerLabels  []string `yaml:"runnerLabels,omitempty"`
	RunnerVersion string   `yaml:"runnerVersion,omitempty"`
	Ephemeral     *bool    `yaml:"ephemeral,omitempty"`
}

type fileBlacksmithConfig struct {
	Org         string `yaml:"org,omitempty"`
	Workflow    string `yaml:"workflow,omitempty"`
	Job         string `yaml:"job,omitempty"`
	Ref         string `yaml:"ref,omitempty"`
	IdleTimeout string `yaml:"idleTimeout,omitempty"`
	Debug       *bool  `yaml:"debug,omitempty"`
}

type fileKubeVirtConfig struct {
	Kubectl         string `yaml:"kubectl,omitempty"`
	Virtctl         string `yaml:"virtctl,omitempty"`
	Kubeconfig      string `yaml:"kubeconfig,omitempty"`
	Context         string `yaml:"context,omitempty"`
	Namespace       string `yaml:"namespace,omitempty"`
	Template        string `yaml:"template,omitempty"`
	SSHUser         string `yaml:"sshUser,omitempty"`
	SSHKey          string `yaml:"sshKey,omitempty"`
	SSHPublicKey    string `yaml:"sshPublicKey,omitempty"`
	SSHPort         string `yaml:"sshPort,omitempty"`
	WorkRoot        string `yaml:"workRoot,omitempty"`
	DeleteOnRelease *bool  `yaml:"deleteOnRelease,omitempty"`
}

type fileAgentSandboxConfig struct {
	Kubectl             string `yaml:"kubectl,omitempty"`
	Kubeconfig          string `yaml:"kubeconfig,omitempty"`
	Context             string `yaml:"context,omitempty"`
	Namespace           string `yaml:"namespace,omitempty"`
	WarmPool            string `yaml:"warmPool,omitempty"`
	Container           string `yaml:"container,omitempty"`
	Workdir             string `yaml:"workdir,omitempty"`
	SandboxReadyTimeout string `yaml:"sandboxReadyTimeout,omitempty"`
	PodReadyTimeout     string `yaml:"podReadyTimeout,omitempty"`
	ExecTimeoutSecs     *int   `yaml:"execTimeoutSecs,omitempty"`
	DeleteOnRelease     *bool  `yaml:"deleteOnRelease,omitempty"`
	ForgetMissing       *bool  `yaml:"forgetMissing,omitempty"`
}

type fileExternalConfig struct {
	Command      string                      `yaml:"command,omitempty"`
	Args         []string                    `yaml:"args,omitempty"`
	Config       map[string]any              `yaml:"config,omitempty"`
	Capabilities *ExternalCapabilitiesConfig `yaml:"capabilities,omitempty"`
	Lifecycle    *ExternalLifecycleConfig    `yaml:"lifecycle,omitempty"`
	Connection   *ExternalConnectionConfig   `yaml:"connection,omitempty"`
	WorkRoot     string                      `yaml:"workRoot,omitempty"`
	RoutingFile  string                      `yaml:"routingFile,omitempty"`
}

type fileNamespaceConfig struct {
	Image               string `yaml:"image,omitempty"`
	Size                string `yaml:"size,omitempty"`
	Repository          string `yaml:"repository,omitempty"`
	Site                string `yaml:"site,omitempty"`
	VolumeSizeGB        int    `yaml:"volumeSizeGB,omitempty"`
	AutoStopIdleTimeout string `yaml:"autoStopIdleTimeout,omitempty"`
	WorkRoot            string `yaml:"workRoot,omitempty"`
	DeleteOnRelease     *bool  `yaml:"deleteOnRelease,omitempty"`
}

type fileNamespaceInstanceConfig struct {
	CLIPath     string   `yaml:"cli,omitempty"`
	MachineType string   `yaml:"machineType,omitempty"`
	Duration    string   `yaml:"duration,omitempty"`
	Region      string   `yaml:"region,omitempty"`
	Endpoint    string   `yaml:"endpoint,omitempty"`
	Keychain    string   `yaml:"keychain,omitempty"`
	Volumes     []string `yaml:"volumes,omitempty"`
	WorkRoot    string   `yaml:"workRoot,omitempty"`
	Bare        *bool    `yaml:"bare,omitempty"`
}

type filePhalaConfig struct {
	CLIPath      string `yaml:"cli,omitempty"`
	InstanceType string `yaml:"instanceType,omitempty"`
	WorkRoot     string `yaml:"workRoot,omitempty"`
	NodeID       string `yaml:"nodeId,omitempty"`
	Compose      string `yaml:"compose,omitempty"`
	Attest       *bool  `yaml:"attest,omitempty"`
}

type fileMorphConfig struct {
	APIKey          string `yaml:"apiKey,omitempty"`
	APIURL          string `yaml:"apiUrl,omitempty"`
	Snapshot        string `yaml:"snapshot,omitempty"`
	SSHGatewayHost  string `yaml:"sshGatewayHost,omitempty"`
	WorkRoot        string `yaml:"workRoot,omitempty"`
	DeleteOnRelease *bool  `yaml:"deleteOnRelease,omitempty"`
	WakeOnSSH       *bool  `yaml:"wakeOnSSH,omitempty"`
}

type fileDaytonaConfig struct {
	APIURL           string `yaml:"apiUrl,omitempty"`
	Snapshot         string `yaml:"snapshot,omitempty"`
	Target           string `yaml:"target,omitempty"`
	User             string `yaml:"user,omitempty"`
	WorkRoot         string `yaml:"workRoot,omitempty"`
	SSHGatewayHost   string `yaml:"sshGatewayHost,omitempty"`
	SSHAccessMinutes int    `yaml:"sshAccessMinutes,omitempty"`
}

type fileE2BConfig struct {
	APIURL   string `yaml:"apiUrl,omitempty"`
	Domain   string `yaml:"domain,omitempty"`
	Template string `yaml:"template,omitempty"`
	Workdir  string `yaml:"workdir,omitempty"`
	User     string `yaml:"user,omitempty"`
}

type fileAzureDynamicSessionsConfig struct {
	Endpoint    string `yaml:"endpoint,omitempty"`
	Pool        string `yaml:"pool,omitempty"`
	APIVersion  string `yaml:"apiVersion,omitempty"`
	Workdir     string `yaml:"workdir,omitempty"`
	TimeoutSecs int    `yaml:"timeoutSecs,omitempty"`
}

type fileFreestyleConfig struct {
	APIURL   string `yaml:"apiUrl,omitempty"`
	Workdir  string `yaml:"workdir,omitempty"`
	VCPUs    int    `yaml:"vcpus,omitempty"`
	MemoryGB int    `yaml:"memoryGB,omitempty"`
}

type fileExeDevConfig struct {
	ControlHost string `yaml:"controlHost,omitempty"`
	Image       string `yaml:"image,omitempty"`
	CPUs        int    `yaml:"cpus,omitempty"`
	Memory      string `yaml:"memory,omitempty"`
	Disk        string `yaml:"disk,omitempty"`
	Command     string `yaml:"command,omitempty"`
	User        string `yaml:"user,omitempty"`
	WorkRoot    string `yaml:"workRoot,omitempty"`
	NoEmail     *bool  `yaml:"noEmail,omitempty"`
}

type fileRailwayConfig struct {
	APIURL        string `yaml:"apiUrl,omitempty"`
	ProjectID     string `yaml:"projectId,omitempty"`
	EnvironmentID string `yaml:"environmentId,omitempty"`
}

type fileRunpodConfig struct {
	APIURL     string `yaml:"apiUrl,omitempty"`
	CloudType  string `yaml:"cloudType,omitempty"`
	InstanceID string `yaml:"instanceId,omitempty"`
	Image      string `yaml:"image,omitempty"`
	TemplateID string `yaml:"templateId,omitempty"`
	DiskGB     int    `yaml:"diskGB,omitempty"`
	User       string `yaml:"user,omitempty"`
	WorkRoot   string `yaml:"workRoot,omitempty"`
}

type fileNvidiaBrevConfig struct {
	CLI           string `yaml:"cli,omitempty"`
	Org           string `yaml:"org,omitempty"`
	Type          string `yaml:"type,omitempty"`
	GPUName       string `yaml:"gpuName,omitempty"`
	Provider      string `yaml:"provider,omitempty"`
	Mode          string `yaml:"mode,omitempty"`
	Launchable    string `yaml:"launchable,omitempty"`
	StartupScript string `yaml:"startupScript,omitempty"`
	ReleaseAction string `yaml:"releaseAction,omitempty"`
	Target        string `yaml:"target,omitempty"`
	User          string `yaml:"user,omitempty"`
	WorkRoot      string `yaml:"workRoot,omitempty"`
}

type fileHostingerConfig struct {
	APIToken        string `yaml:"apiToken,omitempty"`
	APIURL          string `yaml:"apiUrl,omitempty"`
	ItemID          string `yaml:"itemId,omitempty"`
	PaymentMethodID string `yaml:"paymentMethodId,omitempty"`
	TemplateID      string `yaml:"templateId,omitempty"`
	DataCenterID    string `yaml:"dataCenterId,omitempty"`
	HostnamePrefix  string `yaml:"hostnamePrefix,omitempty"`
	User            string `yaml:"user,omitempty"`
	WorkRoot        string `yaml:"workRoot,omitempty"`
	AllowPurchase   *bool  `yaml:"allowPurchase,omitempty"`
	ReleaseAction   string `yaml:"releaseAction,omitempty"`
}

type fileWandbConfig struct {
	APIKey             string `yaml:"apiKey,omitempty"`
	DefaultImage       string `yaml:"defaultImage,omitempty"`
	MaxLifetimeSeconds int    `yaml:"maxLifetimeSeconds,omitempty"`
}

type fileIsloConfig struct {
	BaseURL        string `yaml:"baseUrl,omitempty"`
	Image          string `yaml:"image,omitempty"`
	Workdir        string `yaml:"workdir,omitempty"`
	GatewayProfile string `yaml:"gatewayProfile,omitempty"`
	SnapshotName   string `yaml:"snapshotName,omitempty"`
	VCPUs          int    `yaml:"vcpus,omitempty"`
	MemoryMB       int    `yaml:"memoryMB,omitempty"`
	DiskGB         int    `yaml:"diskGB,omitempty"`
}

type fileTenkiConfig struct {
	CLIPath   string `yaml:"cliPath,omitempty"`
	Endpoint  string `yaml:"endpoint,omitempty"`
	Gateway   string `yaml:"gateway,omitempty"`
	Workspace string `yaml:"workspace,omitempty"`
	Project   string `yaml:"project,omitempty"`
	Image     string `yaml:"image,omitempty"`
	Snapshot  string `yaml:"snapshot,omitempty"`
	WorkRoot  string `yaml:"workRoot,omitempty"`
	CPUs      int    `yaml:"cpus,omitempty"`
	MemoryMB  int    `yaml:"memoryMB,omitempty"`
	DiskGB    int    `yaml:"diskGB,omitempty"`
}

type fileTensorlakeConfig struct {
	APIURL         string  `yaml:"apiUrl,omitempty"`
	CLIPath        string  `yaml:"cliPath,omitempty"`
	Image          string  `yaml:"image,omitempty"`
	Snapshot       string  `yaml:"snapshot,omitempty"`
	OrganizationID string  `yaml:"organizationId,omitempty"`
	ProjectID      string  `yaml:"projectId,omitempty"`
	Namespace      string  `yaml:"namespace,omitempty"`
	Workdir        string  `yaml:"workdir,omitempty"`
	CPUs           float64 `yaml:"cpus,omitempty"`
	MemoryMB       int     `yaml:"memoryMB,omitempty"`
	DiskMB         int     `yaml:"diskMB,omitempty"`
	TimeoutSecs    int     `yaml:"timeoutSecs,omitempty"`
	NoInternet     *bool   `yaml:"noInternet,omitempty"`
}

type fileOpenComputerConfig struct {
	Workdir         string `yaml:"workdir,omitempty"`
	CPU             *int   `yaml:"cpu,omitempty"`
	MemoryMB        *int   `yaml:"memoryMB,omitempty"`
	TimeoutSecs     *int   `yaml:"timeoutSecs,omitempty"`
	ExecTimeoutSecs *int   `yaml:"execTimeoutSecs,omitempty"`
	Burst           *bool  `yaml:"burst,omitempty"`
}

type fileCodeSandboxConfig struct {
	TemplateID               *string `yaml:"templateId,omitempty"`
	Workdir                  *string `yaml:"workdir,omitempty"`
	VMTier                   *string `yaml:"vmTier,omitempty"`
	Privacy                  *string `yaml:"privacy,omitempty"`
	HibernationTimeoutSecs   *int    `yaml:"hibernationTimeoutSecs,omitempty"`
	AutomaticWakeupHTTP      *bool   `yaml:"automaticWakeupHTTP,omitempty"`
	AutomaticWakeupWebSocket *bool   `yaml:"automaticWakeupWebSocket,omitempty"`
	BridgeCommand            *string `yaml:"bridgeCommand,omitempty"`
	SDKPackage               *string `yaml:"sdkPackage,omitempty"`
	DoctorListLimit          *int    `yaml:"doctorListLimit,omitempty"`
	OperationTimeoutSecs     *int    `yaml:"operationTimeoutSecs,omitempty"`
}

type fileOpenSandboxConfig struct {
	Image           *string `yaml:"image,omitempty"`
	Workdir         *string `yaml:"workdir,omitempty"`
	CPU             *string `yaml:"cpu,omitempty"`
	Memory          *string `yaml:"memory,omitempty"`
	TimeoutSecs     *int    `yaml:"timeoutSecs,omitempty"`
	ExecTimeoutSecs *int    `yaml:"execTimeoutSecs,omitempty"`
	PlatformOS      *string `yaml:"platformOS,omitempty"`
	PlatformArch    *string `yaml:"platformArch,omitempty"`
	SecureAccess    *bool   `yaml:"secureAccess,omitempty"`
	UseServerProxy  *bool   `yaml:"useServerProxy,omitempty"`
}

type fileVercelSandboxConfig struct {
	Runtime         *string   `yaml:"runtime,omitempty"`
	Workdir         *string   `yaml:"workdir,omitempty"`
	ProjectID       *string   `yaml:"projectId,omitempty"`
	TeamID          *string   `yaml:"teamId,omitempty"`
	Scope           *string   `yaml:"scope,omitempty"`
	VCPUs           *float64  `yaml:"vcpus,omitempty"`
	TimeoutSecs     *int      `yaml:"timeoutSecs,omitempty"`
	ExecTimeoutSecs *int      `yaml:"execTimeoutSecs,omitempty"`
	Persistent      *bool     `yaml:"persistent,omitempty"`
	Snapshot        *string   `yaml:"snapshot,omitempty"`
	SnapshotMode    *string   `yaml:"snapshotMode,omitempty"`
	NetworkPolicy   *string   `yaml:"networkPolicy,omitempty"`
	NetworkAllow    *[]string `yaml:"networkAllow,omitempty"`
	NetworkDeny     *[]string `yaml:"networkDeny,omitempty"`
	Ports           *[]string `yaml:"ports,omitempty"`
	ForgetMissing   *bool     `yaml:"forgetMissing,omitempty"`
}

type fileSuperserveConfig struct {
	BaseURL         string   `yaml:"baseUrl,omitempty"`
	Template        *string  `yaml:"template,omitempty"`
	Snapshot        *string  `yaml:"snapshot,omitempty"`
	Workdir         *string  `yaml:"workdir,omitempty"`
	TimeoutSecs     *int     `yaml:"timeoutSecs,omitempty"`
	ExecTimeoutSecs *int     `yaml:"execTimeoutSecs,omitempty"`
	NetworkAllowOut []string `yaml:"networkAllowOut,omitempty"`
	NetworkDenyOut  []string `yaml:"networkDenyOut,omitempty"`
	ForgetMissing   *bool    `yaml:"forgetMissing,omitempty"`
}

type fileDockerSandboxConfig struct {
	CLIPath         string    `yaml:"cliPath,omitempty"`
	Agent           string    `yaml:"agent,omitempty"`
	Template        *string   `yaml:"template,omitempty"`
	CPUs            *float64  `yaml:"cpus,omitempty"`
	Memory          *string   `yaml:"memory,omitempty"`
	Clone           *bool     `yaml:"clone,omitempty"`
	Workdir         *string   `yaml:"workdir,omitempty"`
	ExtraWorkspaces *[]string `yaml:"extraWorkspaces,omitempty"`
	MCP             *[]string `yaml:"mcp,omitempty"`
	Kit             *[]string `yaml:"kit,omitempty"`
}

type fileAnthropicSRTConfig struct {
	CLIPath  string  `yaml:"cliPath,omitempty"`
	Settings *string `yaml:"settings,omitempty"`
	Debug    *bool   `yaml:"debug,omitempty"`
}

type fileModalConfig struct {
	App     string `yaml:"app,omitempty"`
	Image   string `yaml:"image,omitempty"`
	Workdir string `yaml:"workdir,omitempty"`
	Python  string `yaml:"python,omitempty"`
}

type fileUpstashBoxConfig struct {
	BaseURL   string `yaml:"baseUrl,omitempty"`
	Runtime   string `yaml:"runtime,omitempty"`
	Size      string `yaml:"size,omitempty"`
	Workdir   string `yaml:"workdir,omitempty"`
	KeepAlive *bool  `yaml:"keepAlive,omitempty"`
}

type fileSmolvmConfig struct {
	BaseURL  string `yaml:"baseUrl,omitempty"`
	Image    string `yaml:"image,omitempty"`
	Workdir  string `yaml:"workdir,omitempty"`
	CPUs     int    `yaml:"cpus,omitempty"`
	MemoryMB int    `yaml:"memoryMB,omitempty"`
	Network  string `yaml:"network,omitempty"`
	Keep     *bool  `yaml:"keep,omitempty"`
}

type fileAsciiBoxConfig struct {
	BaseURL string `yaml:"baseUrl,omitempty"`
	CLIPath string `yaml:"cliPath,omitempty"`
	Workdir string `yaml:"workdir,omitempty"`
}

type fileCloudflareConfig struct {
	APIURL  string `yaml:"apiUrl,omitempty"`
	Token   string `yaml:"token,omitempty"`
	Workdir string `yaml:"workdir,omitempty"`
}

type fileCloudflareDynamicWorkersConfig struct {
	LoaderURL          string            `yaml:"loaderUrl,omitempty"`
	URL                string            `yaml:"url,omitempty"`
	Token              string            `yaml:"token,omitempty"`
	CompatibilityDate  string            `yaml:"compatibilityDate,omitempty"`
	CompatibilityFlags []string          `yaml:"compatibilityFlags,omitempty"`
	CacheMode          string            `yaml:"cacheMode,omitempty"`
	Egress             string            `yaml:"egress,omitempty"`
	CPUMs              int               `yaml:"cpuMs,omitempty"`
	Subrequests        int               `yaml:"subrequests,omitempty"`
	TimeoutSecs        int               `yaml:"timeoutSecs,omitempty"`
	Metadata           map[string]string `yaml:"metadata,omitempty"`
}

func applyCloudflareFileConfig(cfg *Config, file *fileCloudflareConfig, source credentialValueSource) {
	if file == nil {
		return
	}
	if file.APIURL != "" {
		cfg.Cloudflare.APIURL = file.APIURL
		cfg.credentialProvenance.cloudflareAPIURL = source
	}
	if file.Token != "" {
		cfg.Cloudflare.Token = file.Token
		cfg.credentialProvenance.cloudflareToken = source
	}
	if file.Workdir != "" {
		cfg.Cloudflare.Workdir = file.Workdir
	}
}

func applyCloudflareDynamicWorkersFileConfig(cfg *Config, file *fileCloudflareDynamicWorkersConfig, trusted bool) {
	if file == nil {
		return
	}
	if trusted {
		if file.LoaderURL != "" {
			cfg.CloudflareDynamicWorkers.LoaderURL = file.LoaderURL
		}
		if file.URL != "" {
			cfg.CloudflareDynamicWorkers.LoaderURL = file.URL
		}
		if file.Token != "" {
			cfg.CloudflareDynamicWorkers.Token = file.Token
		}
	}
	if file.CompatibilityDate != "" {
		cfg.CloudflareDynamicWorkers.CompatibilityDate = file.CompatibilityDate
	}
	if len(file.CompatibilityFlags) > 0 {
		cfg.CloudflareDynamicWorkers.CompatibilityFlags = append([]string(nil), file.CompatibilityFlags...)
	}
	if file.CacheMode != "" {
		cfg.CloudflareDynamicWorkers.CacheMode = file.CacheMode
	}
	if file.Egress != "" && (trusted || strings.EqualFold(strings.TrimSpace(file.Egress), "blocked")) {
		cfg.CloudflareDynamicWorkers.Egress = file.Egress
	}
	if trusted {
		if file.CPUMs > 0 {
			cfg.CloudflareDynamicWorkers.CPUMs = file.CPUMs
		}
		if file.Subrequests > 0 {
			cfg.CloudflareDynamicWorkers.Subrequests = file.Subrequests
		}
		if file.TimeoutSecs > 0 {
			cfg.CloudflareDynamicWorkers.TimeoutSecs = file.TimeoutSecs
		}
	} else {
		cfg.CloudflareDynamicWorkers.repositoryCPUMsCap = positiveMinimum(
			cfg.CloudflareDynamicWorkers.repositoryCPUMsCap,
			file.CPUMs,
		)
		cfg.CloudflareDynamicWorkers.repositorySubrequestsCap = positiveMinimum(
			cfg.CloudflareDynamicWorkers.repositorySubrequestsCap,
			file.Subrequests,
		)
		cfg.CloudflareDynamicWorkers.repositoryTimeoutSecsCap = positiveMinimum(
			cfg.CloudflareDynamicWorkers.repositoryTimeoutSecsCap,
			file.TimeoutSecs,
		)
		applyCloudflareDynamicWorkersRepositoryCaps(cfg)
	}
	if len(file.Metadata) > 0 {
		cfg.CloudflareDynamicWorkers.Metadata = map[string]string{}
		for key, value := range file.Metadata {
			key = strings.TrimSpace(key)
			if key != "" {
				cfg.CloudflareDynamicWorkers.Metadata[key] = value
			}
		}
	}
}

func applyCloudflareDynamicWorkersRepositoryCaps(cfg *Config) {
	dynamicWorkers := &cfg.CloudflareDynamicWorkers
	if dynamicWorkers.repositoryCPUMsCap > 0 &&
		(dynamicWorkers.CPUMs > 0 || dynamicWorkers.repositoryCPUMsCapActive) {
		if dynamicWorkers.CPUMs <= 0 {
			dynamicWorkers.CPUMs = dynamicWorkers.repositoryCPUMsCap
		} else {
			dynamicWorkers.CPUMs = min(dynamicWorkers.CPUMs, dynamicWorkers.repositoryCPUMsCap)
		}
		dynamicWorkers.repositoryCPUMsCapActive = true
	}
	if dynamicWorkers.repositorySubrequestsCap > 0 &&
		(dynamicWorkers.Subrequests > 0 || dynamicWorkers.repositorySubrequestsCapActive) {
		if dynamicWorkers.Subrequests <= 0 {
			dynamicWorkers.Subrequests = dynamicWorkers.repositorySubrequestsCap
		} else {
			dynamicWorkers.Subrequests = min(
				dynamicWorkers.Subrequests,
				dynamicWorkers.repositorySubrequestsCap,
			)
		}
		dynamicWorkers.repositorySubrequestsCapActive = true
	}
	if dynamicWorkers.repositoryTimeoutSecsCap > 0 &&
		(dynamicWorkers.TimeoutSecs > 0 || dynamicWorkers.repositoryTimeoutSecsCapActive) {
		if dynamicWorkers.TimeoutSecs <= 0 {
			dynamicWorkers.TimeoutSecs = dynamicWorkers.repositoryTimeoutSecsCap
		} else {
			dynamicWorkers.TimeoutSecs = min(
				dynamicWorkers.TimeoutSecs,
				dynamicWorkers.repositoryTimeoutSecsCap,
			)
		}
		dynamicWorkers.repositoryTimeoutSecsCapActive = true
	}
}

func positiveMinimum(current, candidate int) int {
	if candidate <= 0 {
		return current
	}
	if current <= 0 {
		return candidate
	}
	return min(current, candidate)
}

type fileSemaphoreConfig struct {
	Host        string `yaml:"host,omitempty"`
	Token       string `yaml:"token,omitempty"`
	Project     string `yaml:"project,omitempty"`
	Machine     string `yaml:"machine,omitempty"`
	OSImage     string `yaml:"osImage,omitempty"`
	IdleTimeout string `yaml:"idleTimeout,omitempty"`
}

type fileSpritesConfig struct {
	APIURL   string `yaml:"apiUrl,omitempty"`
	WorkRoot string `yaml:"workRoot,omitempty"`
}

type fileLocalContainerConfig struct {
	Runtime      string `yaml:"runtime,omitempty"`
	Image        string `yaml:"image,omitempty"`
	User         string `yaml:"user,omitempty"`
	WorkRoot     string `yaml:"workRoot,omitempty"`
	CPUs         int    `yaml:"cpus,omitempty"`
	Memory       string `yaml:"memory,omitempty"`
	Network      string `yaml:"network,omitempty"`
	DockerSocket *bool  `yaml:"dockerSocket,omitempty"`
}

type fileAppleContainerConfig struct {
	CLIPath      string   `yaml:"cliPath,omitempty"`
	Image        string   `yaml:"image,omitempty"`
	User         string   `yaml:"user,omitempty"`
	WorkRoot     string   `yaml:"workRoot,omitempty"`
	CPUs         int      `yaml:"cpus,omitempty"`
	Memory       string   `yaml:"memory,omitempty"`
	ExtraRunArgs []string `yaml:"extraRunArgs,omitempty"`
}

type fileAppleVZConfig struct {
	HelperPath  string `yaml:"helperPath,omitempty"`
	Image       string `yaml:"image,omitempty"`
	ImageSHA256 string `yaml:"imageSHA256,omitempty"`
	User        string `yaml:"user,omitempty"`
	WorkRoot    string `yaml:"workRoot,omitempty"`
	CPUs        *int   `yaml:"cpus,omitempty"`
	MemoryMiB   *int   `yaml:"memoryMiB,omitempty"`
	DiskGiB     *int   `yaml:"diskGiB,omitempty"`
}

type fileMXCConfig struct {
	CLIPath           string   `yaml:"cliPath,omitempty"`
	Version           string   `yaml:"version,omitempty"`
	Containment       string   `yaml:"containment,omitempty"`
	Network           string   `yaml:"network,omitempty"`
	ReadOnlyPaths     []string `yaml:"readOnlyPaths,omitempty"`
	ReadWritePaths    []string `yaml:"readWritePaths,omitempty"`
	AllowedHosts      []string `yaml:"allowedHosts,omitempty"`
	BlockedHosts      []string `yaml:"blockedHosts,omitempty"`
	AllowDACLMutation *bool    `yaml:"allowDaclMutation,omitempty"`
	AllowWindowsUI    *bool    `yaml:"allowWindowsUI,omitempty"`
	Experimental      *bool    `yaml:"experimental,omitempty"`
}

type fileMultipassConfig struct {
	CLIPath       string `yaml:"cliPath,omitempty"`
	Image         string `yaml:"image,omitempty"`
	User          string `yaml:"user,omitempty"`
	WorkRoot      string `yaml:"workRoot,omitempty"`
	CPUs          int    `yaml:"cpus,omitempty"`
	Memory        string `yaml:"memory,omitempty"`
	Disk          string `yaml:"disk,omitempty"`
	LaunchTimeout string `yaml:"launchTimeout,omitempty"`
}

type fileTartConfig struct {
	Image    string `yaml:"image,omitempty"`
	User     string `yaml:"user,omitempty"`
	Password string `yaml:"password,omitempty"`
	WorkRoot string `yaml:"workRoot,omitempty"`
	CPUs     *int   `yaml:"cpus,omitempty"`
	Memory   *int   `yaml:"memory,omitempty"`
	Disk     *int   `yaml:"disk,omitempty"`
}

type fileHyperVConfig struct {
	Image         string `yaml:"image,omitempty"`
	User          string `yaml:"user,omitempty"`
	WorkRoot      string `yaml:"workRoot,omitempty"`
	CPUs          int    `yaml:"cpus,omitempty"`
	Memory        int    `yaml:"memory,omitempty"`
	Switch        string `yaml:"switch,omitempty"`
	GuestPassword string `yaml:"guestPassword,omitempty"`
	InitPassword  *bool  `yaml:"initPassword,omitempty"`
}

type fileWindowsSandboxConfig struct {
	Workdir            string `yaml:"workdir,omitempty"`
	TempRoot           string `yaml:"tempRoot,omitempty"`
	Networking         string `yaml:"networking,omitempty"`
	VGPU               string `yaml:"vgpu,omitempty"`
	Clipboard          string `yaml:"clipboard,omitempty"`
	ProtectedClient    string `yaml:"protectedClient,omitempty"`
	AudioInput         string `yaml:"audioInput,omitempty"`
	VideoInput         string `yaml:"videoInput,omitempty"`
	PrinterRedirection string `yaml:"printerRedirection,omitempty"`
	MemoryMB           int    `yaml:"memoryMB,omitempty"`
}

type fileTailscaleConfig struct {
	Enabled                *bool    `yaml:"enabled,omitempty"`
	Network                string   `yaml:"network,omitempty"`
	Tags                   []string `yaml:"tags,omitempty"`
	HostnameTemplate       string   `yaml:"hostnameTemplate,omitempty"`
	AuthKeyEnv             string   `yaml:"authKeyEnv,omitempty"`
	ExitNode               string   `yaml:"exitNode,omitempty"`
	ExitNodeAllowLANAccess *bool    `yaml:"exitNodeAllowLanAccess,omitempty"`
}

type fileStaticConfig struct {
	ID       string `yaml:"id,omitempty"`
	Name     string `yaml:"name,omitempty"`
	Host     string `yaml:"host,omitempty"`
	User     string `yaml:"user,omitempty"`
	Port     string `yaml:"port,omitempty"`
	WorkRoot string `yaml:"workRoot,omitempty"`
}

type fileResultsConfig struct {
	JUnit []string `yaml:"junit,omitempty"`
	Auto  *bool    `yaml:"auto,omitempty"`
}

type fileCacheConfig struct {
	Pnpm           *bool                    `yaml:"pnpm,omitempty"`
	Npm            *bool                    `yaml:"npm,omitempty"`
	Docker         *bool                    `yaml:"docker,omitempty"`
	Git            *bool                    `yaml:"git,omitempty"`
	MaxGB          int                      `yaml:"maxGB,omitempty"`
	PurgeOnRelease *bool                    `yaml:"purgeOnRelease,omitempty"`
	Volumes        *[]fileCacheVolumeConfig `yaml:"volumes,omitempty"`
}

type fileCacheVolumeConfig struct {
	Name     string `yaml:"name,omitempty"`
	Key      string `yaml:"key,omitempty"`
	Path     string `yaml:"path,omitempty"`
	SizeGB   int    `yaml:"sizeGB,omitempty"`
	Required *bool  `yaml:"required,omitempty"`
}

type fileProfileConfig struct {
	Env            fileProfileEnvConfig               `yaml:"env,omitempty"`
	EnvAllow       []string                           `yaml:"envAllow,omitempty"`
	ArtifactGlobs  []string                           `yaml:"artifactGlobs,omitempty"`
	Doctor         *fileDoctorProfileConfig           `yaml:"doctor,omitempty"`
	Presets        map[string]filePresetConfig        `yaml:"presets,omitempty"`
	ProofTemplates map[string]fileProofTemplateConfig `yaml:"proofTemplates,omitempty"`
}

type fileProfileEnvConfig struct {
	Values map[string]string
	Allow  []string
}

func (env fileProfileEnvConfig) IsZero() bool {
	return len(env.Values) == 0 && len(env.Allow) == 0
}

func (env *fileProfileEnvConfig) UnmarshalYAML(node *yaml.Node) error {
	if node == nil || node.Kind == 0 {
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("profile env must be a mapping")
	}
	values := map[string]string{}
	var allow []string
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := strings.TrimSpace(node.Content[i].Value)
		valueNode := node.Content[i+1]
		if key == "" {
			continue
		}
		if key == "allow" {
			if err := valueNode.Decode(&allow); err != nil {
				return fmt.Errorf("profile env.allow: %w", err)
			}
			continue
		}
		var value string
		if err := valueNode.Decode(&value); err != nil {
			return fmt.Errorf("profile env.%s: %w", key, err)
		}
		values[key] = value
	}
	env.Values = values
	env.Allow = allow
	return nil
}

func (env fileProfileEnvConfig) MarshalYAML() (any, error) {
	node := &yaml.Node{Kind: yaml.MappingNode}
	keys := make([]string, 0, len(env.Values))
	for key := range env.Values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		node.Content = append(node.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: env.Values[key]},
		)
	}
	if len(env.Allow) > 0 {
		seq := &yaml.Node{Kind: yaml.SequenceNode}
		for _, value := range env.Allow {
			seq.Content = append(seq.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value})
		}
		node.Content = append(node.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "allow"},
			seq,
		)
	}
	return node, nil
}

type fileDoctorProfileConfig struct {
	Enabled        *bool    `yaml:"enabled,omitempty"`
	Tools          []string `yaml:"tools,omitempty"`
	NodeMajor      int      `yaml:"nodeMajor,omitempty"`
	MinDiskGB      int      `yaml:"minDiskGB,omitempty"`
	RequireDocker  *bool    `yaml:"requireDocker,omitempty"`
	RequireCompose *bool    `yaml:"requireCompose,omitempty"`
}

type filePresetConfig struct {
	Command       string            `yaml:"command,omitempty"`
	Shell         *bool             `yaml:"shell,omitempty"`
	Env           map[string]string `yaml:"env,omitempty"`
	Preflight     *bool             `yaml:"preflight,omitempty"`
	ArtifactGlobs []string          `yaml:"artifactGlobs,omitempty"`
	ProofTemplate string            `yaml:"proofTemplate,omitempty"`
}

type fileProofTemplateConfig struct {
	BehaviorAddressed     string `yaml:"behaviorAddressed,omitempty"`
	RealEnvironmentTested string `yaml:"realEnvironmentTested,omitempty"`
	ExactSteps            string `yaml:"exactSteps,omitempty"`
	ObservedResult        string `yaml:"observedResult,omitempty"`
	NotTested             string `yaml:"notTested,omitempty"`
}

type fileLeaseConfig struct {
	TTL         string `yaml:"ttl,omitempty"`
	IdleTimeout string `yaml:"idleTimeout,omitempty"`
}

type fileJobConfig struct {
	Provider          string                `yaml:"provider,omitempty"`
	Target            string                `yaml:"target,omitempty"`
	TargetOS          string                `yaml:"targetOS,omitempty"`
	Windows           *fileWindowsConfig    `yaml:"windows,omitempty"`
	Profile           string                `yaml:"profile,omitempty"`
	Class             string                `yaml:"class,omitempty"`
	Architecture      string                `yaml:"architecture,omitempty"`
	ServerType        string                `yaml:"serverType,omitempty"`
	Type              string                `yaml:"type,omitempty"`
	Capacity          *fileCapacityConfig   `yaml:"capacity,omitempty"`
	Market            string                `yaml:"market,omitempty"`
	TTL               string                `yaml:"ttl,omitempty"`
	IdleTimeout       string                `yaml:"idleTimeout,omitempty"`
	Desktop           *bool                 `yaml:"desktop,omitempty"`
	DesktopEnv        string                `yaml:"desktopEnv,omitempty"`
	Browser           *bool                 `yaml:"browser,omitempty"`
	Code              *bool                 `yaml:"code,omitempty"`
	Network           string                `yaml:"network,omitempty"`
	Hydrate           *fileJobHydrateConfig `yaml:"hydrate,omitempty"`
	Actions           *fileJobActionsConfig `yaml:"actions,omitempty"`
	Shell             *bool                 `yaml:"shell,omitempty"`
	Command           string                `yaml:"command,omitempty"`
	NoSync            *bool                 `yaml:"noSync,omitempty"`
	SyncOnly          *bool                 `yaml:"syncOnly,omitempty"`
	Checksum          *bool                 `yaml:"checksum,omitempty"`
	ForceSyncLarge    *bool                 `yaml:"forceSyncLarge,omitempty"`
	JUnit             []string              `yaml:"junit,omitempty"`
	Label             string                `yaml:"label,omitempty"`
	ArtifactGlobs     []string              `yaml:"artifactGlobs,omitempty"`
	RequiredArtifacts []string              `yaml:"requiredArtifacts,omitempty"`
	Downloads         []string              `yaml:"downloads,omitempty"`
	Stop              string                `yaml:"stop,omitempty"`
}

type fileJobHydrateConfig struct {
	Actions          *bool  `yaml:"actions,omitempty"`
	GitHubRunner     *bool  `yaml:"githubRunner,omitempty"`
	WaitTimeout      string `yaml:"waitTimeout,omitempty"`
	KeepAliveMinutes int    `yaml:"keepAliveMinutes,omitempty"`
}

type fileJobActionsConfig struct {
	Repo     string   `yaml:"repo,omitempty"`
	Workflow string   `yaml:"workflow,omitempty"`
	Job      string   `yaml:"job,omitempty"`
	Ref      string   `yaml:"ref,omitempty"`
	Fields   []string `yaml:"fields,omitempty"`
}

func configPaths() []string {
	if explicit := os.Getenv("CRABBOX_CONFIG"); explicit != "" {
		return []string{explicit}
	}
	paths := make([]string, 0, 3)
	if userPath := userConfigPath(); userPath != "" {
		paths = append(paths, userPath)
	}
	for _, path := range []string{"crabbox.yaml", ".crabbox.yaml"} {
		if _, err := os.Stat(path); err == nil {
			paths = append(paths, path)
		}
	}
	return paths
}

func trustedProviderEndpointConfigPath(path string) bool {
	if explicit := os.Getenv("CRABBOX_CONFIG"); explicit != "" {
		return path == explicit
	}
	return path == userConfigPath()
}

func userConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "crabbox", "config.yaml")
}

func readFileConfig(path string) (fileConfig, error) {
	var cfg fileConfig
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return cfg, exit(2, "read config %s: %v", path, err)
	}
	if len(data) == 0 {
		return cfg, nil
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, exit(2, "parse config %s: %v", path, err)
	}
	return cfg, nil
}

func writeUserFileConfig(cfg fileConfig) (string, error) {
	path := writableConfigPath()
	if path == "" {
		return "", exit(2, "user config directory is unavailable")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", exit(2, "create config directory: %v", err)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", exit(2, "write config %s: %v", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return "", exit(2, "secure config %s: %v", path, err)
	}
	return path, nil
}

func configFilePermissionProblem(path string) string {
	if path == "" {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ""
		}
		return err.Error()
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Sprintf("permissions %04o want 0600", info.Mode().Perm())
	}
	return ""
}

func writableConfigPath() string {
	if explicit := os.Getenv("CRABBOX_CONFIG"); explicit != "" {
		return explicit
	}
	return userConfigPath()
}

func applyConfigFile(cfg *Config, path string) error {
	file, err := readFileConfig(path)
	if err != nil {
		return err
	}
	return applyFileConfigWithTrust(cfg, file, trustedConfigPath(path))
}

func applyFileConfig(cfg *Config, file fileConfig) error {
	return applyFileConfigWithTrust(cfg, file, true)
}

func trustedConfigPath(path string) bool {
	if explicit := strings.TrimSpace(os.Getenv("CRABBOX_CONFIG")); explicit != "" {
		return filepath.Clean(path) == filepath.Clean(explicit)
	}
	userPath := userConfigPath()
	return userPath != "" && filepath.Clean(path) == filepath.Clean(userPath)
}

func applyFileConfigWithTrust(cfg *Config, file fileConfig, trusted bool) error {
	credentialSource := credentialSourceForFile(trusted)
	if file.Profile != "" {
		cfg.Profile = file.Profile
	}
	if file.Provider != "" {
		cfg.Provider = file.Provider
		cfg.brokerProvider = ""
	}
	if file.Target != "" {
		cfg.TargetOS = file.Target
		cfg.targetExplicit = true
	}
	if file.TargetOS != "" {
		cfg.TargetOS = file.TargetOS
		cfg.targetExplicit = true
	}
	if file.Architecture != "" {
		cfg.Architecture = file.Architecture
		cfg.architectureExplicit = true
	}
	if file.OSImage != "" {
		cfg.OSImage = file.OSImage
		cfg.osImageExplicit = true
		if normalized, err := normalizeOSImage(file.OSImage); err == nil {
			cfg.OSImage = normalized
			applyOSImageProviderDefaults(cfg, false)
		}
	}
	if file.Windows != nil && file.Windows.Mode != "" {
		cfg.WindowsMode = file.Windows.Mode
		cfg.explicitWindowsMode = file.Windows.Mode
	}
	if file.Desktop != nil {
		cfg.Desktop = *file.Desktop
	}
	if file.DesktopEnv != "" {
		cfg.DesktopEnv = file.DesktopEnv
	}
	if file.Browser != nil {
		cfg.Browser = *file.Browser
	}
	if file.Code != nil {
		cfg.Code = *file.Code
	}
	if file.Network != "" {
		cfg.Network = NetworkMode(strings.ToLower(strings.TrimSpace(file.Network)))
	}
	if file.Class != "" {
		cfg.Class = file.Class
		MarkClassExplicit(cfg)
	}
	if file.ServerType != "" {
		cfg.ServerType = file.ServerType
		cfg.ServerTypeExplicit = true
	}
	if file.Coordinator != "" {
		cfg.Coordinator = file.Coordinator
		cfg.credentialProvenance.coordinator = credentialSource
	}
	if file.CoordinatorToken != "" {
		cfg.CoordToken = file.CoordinatorToken
		cfg.credentialProvenance.coordToken = credentialSource
	}
	if file.HostID != "" {
		cfg.HostID = file.HostID
	}
	if file.Broker != nil {
		if file.Broker.URL != "" {
			cfg.Coordinator = file.Broker.URL
			cfg.credentialProvenance.coordinator = credentialSource
		}
		if file.Broker.Token != "" {
			cfg.CoordToken = file.Broker.Token
			cfg.credentialProvenance.coordToken = credentialSource
		}
		if file.Broker.Mode != "" {
			cfg.BrokerMode = BrokerMode(file.Broker.Mode)
		}
		if file.Broker.AutoWebVNC != nil {
			cfg.BrokerAutoWebVNC = *file.Broker.AutoWebVNC
		}
		if file.Broker.AdminToken != "" {
			cfg.CoordAdminToken = file.Broker.AdminToken
			cfg.credentialProvenance.coordAdminToken = credentialSource
		}
		if file.Broker.Provider != "" {
			cfg.Provider = file.Broker.Provider
			cfg.brokerProvider = file.Broker.Provider
		}
		if file.Broker.Access != nil {
			if file.Broker.Access.ClientID != "" {
				cfg.Access.ClientID = file.Broker.Access.ClientID
				cfg.credentialProvenance.accessClientID = credentialSource
			}
			if file.Broker.Access.ClientSecret != "" {
				cfg.Access.ClientSecret = file.Broker.Access.ClientSecret
				cfg.credentialProvenance.accessClientSecret = credentialSource
			}
			if file.Broker.Access.Token != "" {
				cfg.Access.Token = file.Broker.Access.Token
				cfg.credentialProvenance.accessToken = credentialSource
			}
		}
	}
	if file.Hetzner != nil {
		if file.Hetzner.Location != "" {
			cfg.Location = file.Hetzner.Location
			cfg.locationExplicit = true
		}
		if file.Hetzner.Image != "" {
			cfg.Image = file.Hetzner.Image
			cfg.imageExplicit = true
		}
		if file.Hetzner.SSHKey != "" {
			cfg.ProviderKey = file.Hetzner.SSHKey
		}
	}
	if file.DigitalOcean != nil {
		if file.DigitalOcean.Region != "" {
			cfg.DigitalOcean.Region = file.DigitalOcean.Region
		}
		if file.DigitalOcean.Image != "" {
			cfg.DigitalOcean.Image = file.DigitalOcean.Image
			cfg.digitalOceanImageExplicit = true
		}
		if file.DigitalOcean.VPCUUID != "" {
			cfg.DigitalOcean.VPCUUID = file.DigitalOcean.VPCUUID
		}
		if len(file.DigitalOcean.SSHCIDRs) > 0 {
			cfg.DigitalOcean.SSHCIDRs = file.DigitalOcean.SSHCIDRs
		}
	}
	if file.Linode != nil {
		if file.Linode.Region != "" {
			cfg.Linode.Region = file.Linode.Region
		}
		if file.Linode.Image != "" {
			cfg.Linode.Image = file.Linode.Image
			cfg.linodeImageExplicit = true
		}
		if file.Linode.Type != "" {
			cfg.Linode.Type = file.Linode.Type
			cfg.linodeTypeExplicit = true
		}
		if file.Linode.FirewallID != "" {
			cfg.Linode.FirewallID = file.Linode.FirewallID
		}
		if len(file.Linode.SSHCIDRs) > 0 {
			cfg.Linode.SSHCIDRs = file.Linode.SSHCIDRs
		}
	}
	if file.OVH != nil {
		if trusted && file.OVH.Endpoint != "" {
			cfg.OVH.Endpoint = file.OVH.Endpoint
		}
		if file.OVH.ProjectID != "" {
			cfg.OVH.ProjectID = file.OVH.ProjectID
		}
		if file.OVH.Region != "" {
			cfg.OVH.Region = file.OVH.Region
		}
		if file.OVH.Image != "" {
			cfg.OVH.Image = file.OVH.Image
			cfg.ovhImageExplicit = true
		}
		if file.OVH.Flavor != "" {
			cfg.OVH.Flavor = file.OVH.Flavor
		}
	}
	if file.Scaleway != nil {
		if file.Scaleway.Region != "" {
			cfg.Scaleway.Region = file.Scaleway.Region
			cfg.scalewayRegionExplicit = true
		}
		if file.Scaleway.Zone != "" {
			cfg.Scaleway.Zone = file.Scaleway.Zone
			cfg.scalewayZoneExplicit = true
		}
		if file.Scaleway.Image != "" {
			cfg.Scaleway.Image = file.Scaleway.Image
			cfg.scalewayImageExplicit = true
		}
		if file.Scaleway.Type != "" {
			cfg.Scaleway.Type = file.Scaleway.Type
			cfg.scalewayTypeExplicit = true
		}
		if file.Scaleway.ProjectID != "" {
			cfg.Scaleway.ProjectID = file.Scaleway.ProjectID
		}
		if file.Scaleway.OrganizationID != "" {
			cfg.Scaleway.OrganizationID = file.Scaleway.OrganizationID
		}
		if file.Scaleway.SecurityGroup != "" {
			cfg.Scaleway.SecurityGroup = file.Scaleway.SecurityGroup
		}
		if len(file.Scaleway.SSHCIDRs) > 0 {
			cfg.Scaleway.SSHCIDRs = file.Scaleway.SSHCIDRs
		}
	}
	if file.AWS != nil {
		if file.AWS.Region != "" {
			cfg.AWSRegion = file.AWS.Region
		}
		if file.AWS.AMI != "" {
			cfg.AWSAMI = file.AWS.AMI
		}
		if file.AWS.SecurityGroupID != "" {
			cfg.AWSSGID = file.AWS.SecurityGroupID
		}
		if file.AWS.SubnetID != "" {
			cfg.AWSSubnetID = file.AWS.SubnetID
		}
		if file.AWS.InstanceProfile != "" {
			cfg.AWSProfile = file.AWS.InstanceProfile
		}
		if file.AWS.RootGB > 0 {
			cfg.AWSRootGB = file.AWS.RootGB
		}
		if len(file.AWS.SSHCIDRs) > 0 {
			cfg.AWSSSHCIDRs = file.AWS.SSHCIDRs
		}
		if file.AWS.MacHostID != "" {
			cfg.AWSMacHostID = file.AWS.MacHostID
			if cfg.HostID == "" {
				cfg.HostID = file.AWS.MacHostID
			}
		}
	}
	if file.Azure != nil {
		if file.Azure.Backend != "" {
			cfg.AzureBackend = file.Azure.Backend
		}
		if file.Azure.SubscriptionID != "" {
			cfg.AzureSubscription = file.Azure.SubscriptionID
		}
		if file.Azure.TenantID != "" {
			cfg.AzureTenant = file.Azure.TenantID
		}
		if file.Azure.ClientID != "" {
			cfg.AzureClientID = file.Azure.ClientID
		}
		if file.Azure.Location != "" {
			cfg.AzureLocation = file.Azure.Location
		}
		if file.Azure.ResourceGroup != "" {
			cfg.AzureResourceGroup = file.Azure.ResourceGroup
		}
		if file.Azure.Image != "" {
			cfg.AzureImage = file.Azure.Image
			cfg.azureImageExplicit = true
		}
		if file.Azure.OSDisk != "" {
			cfg.AzureOSDisk = file.Azure.OSDisk
			cfg.AzureOSDiskExplicit = true
		}
		if file.Azure.VNet != "" {
			cfg.AzureVNet = file.Azure.VNet
		}
		if file.Azure.Subnet != "" {
			cfg.AzureSubnet = file.Azure.Subnet
		}
		if file.Azure.NSG != "" {
			cfg.AzureNSG = file.Azure.NSG
		}
		if len(file.Azure.SSHCIDRs) > 0 {
			cfg.AzureSSHCIDRs = file.Azure.SSHCIDRs
		}
		if file.Azure.Network != "" {
			cfg.AzureNetwork = file.Azure.Network
		}
	}
	if file.AzureDynamicSessions != nil {
		if file.AzureDynamicSessions.Endpoint != "" {
			cfg.AzureDynamicSessions.Endpoint = file.AzureDynamicSessions.Endpoint
			cfg.credentialProvenance.azSessionsEndpoint = credentialSource
		}
		if file.AzureDynamicSessions.Pool != "" {
			cfg.AzureDynamicSessions.Pool = file.AzureDynamicSessions.Pool
		}
		if file.AzureDynamicSessions.APIVersion != "" {
			cfg.AzureDynamicSessions.APIVersion = file.AzureDynamicSessions.APIVersion
		}
		if file.AzureDynamicSessions.Workdir != "" {
			cfg.AzureDynamicSessions.Workdir = file.AzureDynamicSessions.Workdir
		}
		if file.AzureDynamicSessions.TimeoutSecs > 0 {
			cfg.AzureDynamicSessions.TimeoutSecs = file.AzureDynamicSessions.TimeoutSecs
		}
	}
	if file.GCP != nil {
		if file.GCP.Project != "" {
			cfg.GCPProject = file.GCP.Project
			cfg.gcpProjectExplicit = true
		}
		if file.GCP.Zone != "" {
			cfg.GCPZone = file.GCP.Zone
			cfg.gcpZoneExplicit = true
		}
		if file.GCP.Image != "" {
			cfg.GCPImage = file.GCP.Image
			cfg.gcpImageExplicit = true
		}
		if file.GCP.Network != "" {
			cfg.GCPNetwork = file.GCP.Network
			cfg.gcpNetworkExplicit = true
		}
		if file.GCP.Subnet != "" {
			cfg.GCPSubnet = file.GCP.Subnet
		}
		if len(file.GCP.Tags) > 0 {
			cfg.GCPTags = file.GCP.Tags
			cfg.gcpTagsExplicit = true
		}
		if len(file.GCP.SSHCIDRs) > 0 {
			cfg.GCPSSHCIDRs = file.GCP.SSHCIDRs
		}
		if file.GCP.RootGB > 0 {
			cfg.GCPRootGB = file.GCP.RootGB
			cfg.gcpRootGBExplicit = true
		}
		if file.GCP.ServiceAccount != "" {
			cfg.GCPServiceAccount = file.GCP.ServiceAccount
		}
	}
	if file.Incus != nil {
		if file.Incus.Remote != "" {
			cfg.Incus.Remote = file.Incus.Remote
		}
		if file.Incus.Project != "" {
			cfg.Incus.Project = file.Incus.Project
		}
		if file.Incus.Address != "" {
			cfg.Incus.Address = file.Incus.Address
		}
		if file.Incus.Socket != "" {
			cfg.Incus.Socket = expandUserPath(file.Incus.Socket)
		}
		if file.Incus.InstanceType != "" {
			cfg.Incus.InstanceType = file.Incus.InstanceType
		}
		if file.Incus.Image != "" {
			cfg.Incus.Image = file.Incus.Image
		}
		if file.Incus.Profile != "" {
			cfg.Incus.Profile = file.Incus.Profile
		}
		if file.Incus.User != "" {
			cfg.Incus.User = file.Incus.User
		}
		if file.Incus.WorkRoot != "" {
			cfg.Incus.WorkRoot = file.Incus.WorkRoot
		}
		if file.Incus.DeleteOnRelease != nil {
			cfg.Incus.DeleteOnRelease = *file.Incus.DeleteOnRelease
			MarkDeleteOnReleaseExplicit(cfg, "incus")
		}
		if file.Incus.StartTimeout != "" {
			applyLeaseDuration(&cfg.Incus.StartTimeout, file.Incus.StartTimeout)
		}
		if file.Incus.LaunchPort != "" {
			cfg.Incus.LaunchPort = file.Incus.LaunchPort
		}
		if file.Incus.ProxyListenHost != "" {
			cfg.Incus.ProxyListenHost = file.Incus.ProxyListenHost
		}
		if file.Incus.ProxyListenPort != "" {
			cfg.Incus.ProxyListenPort = file.Incus.ProxyListenPort
		}
		if file.Incus.ProxyDevice != "" {
			cfg.Incus.ProxyDevice = file.Incus.ProxyDevice
		}
		if file.Incus.TLSServerCert != "" {
			cfg.Incus.TLSServerCert = expandUserPath(file.Incus.TLSServerCert)
		}
		if file.Incus.InsecureTLS != nil {
			cfg.Incus.InsecureTLS = *file.Incus.InsecureTLS
		}
		if file.Incus.RemoteImageServer != "" {
			cfg.Incus.RemoteImageServer = file.Incus.RemoteImageServer
		}
	}
	if file.Proxmox != nil {
		if file.Proxmox.APIURL != "" {
			cfg.Proxmox.APIURL = file.Proxmox.APIURL
			cfg.credentialProvenance.proxmoxAPIURL = credentialSource
		}
		if file.Proxmox.TokenID != "" {
			cfg.Proxmox.TokenID = file.Proxmox.TokenID
			cfg.credentialProvenance.proxmoxTokenID = credentialSource
		}
		if file.Proxmox.TokenSecret != "" {
			cfg.Proxmox.TokenSecret = file.Proxmox.TokenSecret
			cfg.credentialProvenance.proxmoxTokenSecret = credentialSource
		}
		if file.Proxmox.Node != "" {
			cfg.Proxmox.Node = file.Proxmox.Node
		}
		if file.Proxmox.TemplateID > 0 {
			cfg.Proxmox.TemplateID = file.Proxmox.TemplateID
		}
		if file.Proxmox.Storage != "" {
			cfg.Proxmox.Storage = file.Proxmox.Storage
		}
		if file.Proxmox.Pool != "" {
			cfg.Proxmox.Pool = file.Proxmox.Pool
		}
		if file.Proxmox.Bridge != "" {
			cfg.Proxmox.Bridge = file.Proxmox.Bridge
		}
		if file.Proxmox.User != "" {
			cfg.Proxmox.User = file.Proxmox.User
		}
		if file.Proxmox.WorkRoot != "" {
			cfg.Proxmox.WorkRoot = file.Proxmox.WorkRoot
		}
		if file.Proxmox.FullClone != nil {
			cfg.Proxmox.FullClone = *file.Proxmox.FullClone
		}
		if file.Proxmox.InsecureTLS != nil {
			cfg.Proxmox.InsecureTLS = *file.Proxmox.InsecureTLS
			cfg.credentialProvenance.proxmoxInsecureTLS = credentialSource
		}
	}
	if file.XCPNg != nil {
		// Project config is repository-controlled. Do not let it redirect
		// or replace inherited user or environment XAPI credentials.
		if trusted && file.XCPNg.APIURL != "" {
			cfg.XCPNg.APIURL = file.XCPNg.APIURL
		}
		if trusted && file.XCPNg.Username != "" {
			cfg.XCPNg.Username = file.XCPNg.Username
		}
		if trusted && file.XCPNg.Password != "" {
			cfg.XCPNg.Password = file.XCPNg.Password
		}
		if file.XCPNg.Template != "" {
			cfg.XCPNg.Template = file.XCPNg.Template
			if file.XCPNg.TemplateUUID == "" {
				cfg.XCPNg.TemplateUUID = ""
			}
		}
		if file.XCPNg.TemplateUUID != "" {
			cfg.XCPNg.TemplateUUID = file.XCPNg.TemplateUUID
			if file.XCPNg.Template == "" {
				cfg.XCPNg.Template = ""
			}
		}
		if file.XCPNg.SR != "" {
			cfg.XCPNg.SR = file.XCPNg.SR
			if file.XCPNg.SRUUID == "" {
				cfg.XCPNg.SRUUID = ""
			}
		}
		if file.XCPNg.SRUUID != "" {
			cfg.XCPNg.SRUUID = file.XCPNg.SRUUID
			if file.XCPNg.SR == "" {
				cfg.XCPNg.SR = ""
			}
		}
		if file.XCPNg.Network != "" {
			cfg.XCPNg.Network = file.XCPNg.Network
			if file.XCPNg.NetworkUUID == "" {
				cfg.XCPNg.NetworkUUID = ""
			}
		}
		if file.XCPNg.NetworkUUID != "" {
			cfg.XCPNg.NetworkUUID = file.XCPNg.NetworkUUID
			if file.XCPNg.Network == "" {
				cfg.XCPNg.Network = ""
			}
		}
		if file.XCPNg.Host != "" {
			cfg.XCPNg.Host = file.XCPNg.Host
		}
		if file.XCPNg.User != "" {
			cfg.XCPNg.User = file.XCPNg.User
		}
		if file.XCPNg.WorkRoot != "" {
			cfg.XCPNg.WorkRoot = file.XCPNg.WorkRoot
		}
		if trusted && file.XCPNg.InsecureTLS != nil {
			cfg.XCPNg.InsecureTLS = *file.XCPNg.InsecureTLS
		}
	}
	if file.Parallels != nil {
		if file.Parallels.Template != "" {
			cfg.Parallels.Template = file.Parallels.Template
		}
		if file.Parallels.Source != "" {
			cfg.Parallels.Source = file.Parallels.Source
		}
		if file.Parallels.SourceID != "" {
			cfg.Parallels.SourceID = file.Parallels.SourceID
		}
		if file.Parallels.SourceSnapshot != "" {
			cfg.Parallels.SourceSnapshot = file.Parallels.SourceSnapshot
		}
		if file.Parallels.SourceSnapshotID != "" {
			cfg.Parallels.SourceSnapshotID = file.Parallels.SourceSnapshotID
		}
		if file.Parallels.CloneMode != "" {
			cfg.Parallels.CloneMode = file.Parallels.CloneMode
		}
		if file.Parallels.Host != "" {
			cfg.Parallels.Host = file.Parallels.Host
		}
		if file.Parallels.HostUser != "" {
			cfg.Parallels.HostUser = file.Parallels.HostUser
		}
		if file.Parallels.HostKey != "" {
			cfg.Parallels.HostKey = expandUserPath(file.Parallels.HostKey)
		}
		if file.Parallels.VMRoot != "" {
			cfg.Parallels.VMRoot = expandUserPath(file.Parallels.VMRoot)
		}
		if file.Parallels.User != "" {
			cfg.Parallels.User = file.Parallels.User
		}
		if file.Parallels.WorkRoot != "" {
			cfg.Parallels.WorkRoot = file.Parallels.WorkRoot
		}
		applyLeaseDuration(&cfg.Parallels.StartupTimeout, file.Parallels.StartupTimeout)
		if len(file.Parallels.Templates) > 0 {
			if cfg.Parallels.Templates == nil {
				cfg.Parallels.Templates = map[string]ParallelsTemplateConfig{}
			}
			for name, template := range file.Parallels.Templates {
				name = strings.TrimSpace(name)
				if name == "" {
					continue
				}
				cfg.Parallels.Templates[name] = applyFileParallelsTemplateConfig(cfg.Parallels.Templates[name], template)
			}
		}
		if len(file.Parallels.Hosts) > 0 {
			cfg.Parallels.Hosts = cfg.Parallels.Hosts[:0]
			for _, host := range file.Parallels.Hosts {
				cfg.Parallels.Hosts = append(cfg.Parallels.Hosts, applyFileParallelsHostConfig(host))
			}
		}
	}
	if file.SSH != nil {
		if file.SSH.User != "" {
			cfg.SSHUser = file.SSH.User
			MarkSSHUserExplicit(cfg)
		}
		if file.SSH.Key != "" {
			cfg.SSHKey = expandUserPath(file.SSH.Key)
			MarkSSHKeyExplicit(cfg)
		}
		if file.SSH.Port != "" {
			cfg.SSHPort = file.SSH.Port
			MarkSSHPortExplicit(cfg)
		}
		if file.SSH.FallbackPorts != nil {
			cfg.SSHFallbackPorts = normalizeList(*file.SSH.FallbackPorts)
			cfg.sshFallbackPortsExplicit = true
			cfg.explicitSSHFallbackPorts = append([]string(nil), cfg.SSHFallbackPorts...)
		}
	}
	if file.WorkRoot != "" {
		cfg.WorkRoot = file.WorkRoot
		cfg.explicitWorkRoot = file.WorkRoot
	}
	applyLeaseDuration(&cfg.TTL, file.TTL)
	applyLeaseDuration(&cfg.IdleTimeout, file.IdleTimeout)
	if file.Lease != nil {
		applyLeaseDuration(&cfg.TTL, file.Lease.TTL)
		applyLeaseDuration(&cfg.IdleTimeout, file.Lease.IdleTimeout)
	}
	if file.Sync != nil {
		cfg.Sync.Excludes = appendUniqueStrings(cfg.Sync.Excludes, file.Sync.Exclude...)
		cfg.Sync.Excludes = appendUniqueStrings(cfg.Sync.Excludes, file.Sync.Excludes...)
		cfg.Sync.Includes = appendUniqueStrings(cfg.Sync.Includes, file.Sync.Include...)
		cfg.Sync.Includes = appendUniqueStrings(cfg.Sync.Includes, file.Sync.Includes...)
		if file.Sync.Delete != nil {
			cfg.Sync.Delete = *file.Sync.Delete
		}
		if file.Sync.Checksum != nil {
			cfg.Sync.Checksum = *file.Sync.Checksum
		}
		if file.Sync.GitSeed != nil {
			cfg.Sync.GitSeed = *file.Sync.GitSeed
		}
		if file.Sync.Fingerprint != nil {
			cfg.Sync.Fingerprint = *file.Sync.Fingerprint
		}
		if file.Sync.BaseRef != "" {
			cfg.Sync.BaseRef = file.Sync.BaseRef
		}
		if file.Sync.Timeout != "" {
			if timeout, err := time.ParseDuration(file.Sync.Timeout); err == nil {
				cfg.Sync.Timeout = timeout
			}
		}
		if file.Sync.WarnFiles > 0 {
			cfg.Sync.WarnFiles = file.Sync.WarnFiles
		}
		if file.Sync.WarnBytes > 0 {
			cfg.Sync.WarnBytes = file.Sync.WarnBytes
		}
		if file.Sync.FailFiles > 0 {
			cfg.Sync.FailFiles = file.Sync.FailFiles
		}
		if file.Sync.FailBytes > 0 {
			cfg.Sync.FailBytes = file.Sync.FailBytes
		}
		if file.Sync.AllowLarge != nil {
			cfg.Sync.AllowLarge = *file.Sync.AllowLarge
		}
	}
	if file.Run != nil && len(file.Run.PreflightTools) > 0 {
		cfg.Run.PreflightTools = normalizePreflightToolNames(file.Run.PreflightTools)
	}
	if file.Env != nil && len(file.Env.Allow) > 0 {
		cfg.EnvAllow = appendUniqueStrings(nil, file.Env.Allow...)
	}
	if file.Capacity != nil {
		if file.Capacity.Market != "" {
			cfg.Capacity.Market = file.Capacity.Market
		}
		if file.Capacity.Strategy != "" {
			cfg.Capacity.Strategy = file.Capacity.Strategy
		}
		if file.Capacity.Fallback != "" {
			cfg.Capacity.Fallback = file.Capacity.Fallback
		}
		if len(file.Capacity.Regions) > 0 {
			cfg.Capacity.Regions = appendUniqueStrings(nil, file.Capacity.Regions...)
		}
		if len(file.Capacity.AvailabilityZones) > 0 {
			cfg.Capacity.AvailabilityZones = appendUniqueStrings(nil, file.Capacity.AvailabilityZones...)
		}
		if file.Capacity.Hints != nil {
			cfg.Capacity.Hints = *file.Capacity.Hints
		}
	}
	if file.Actions != nil {
		if file.Actions.Repo != "" {
			cfg.Actions.Repo = file.Actions.Repo
		}
		if file.Actions.Workflow != "" {
			cfg.Actions.Workflow = file.Actions.Workflow
		}
		if file.Actions.Job != "" {
			cfg.Actions.Job = file.Actions.Job
		}
		if file.Actions.Ref != "" {
			cfg.Actions.Ref = file.Actions.Ref
		}
		if len(file.Actions.Fields) > 0 {
			cfg.Actions.Fields = appendUniqueStrings(nil, file.Actions.Fields...)
		}
		if len(file.Actions.RunnerLabels) > 0 {
			cfg.Actions.RunnerLabels = appendUniqueStrings(nil, file.Actions.RunnerLabels...)
		}
		if file.Actions.RunnerVersion != "" {
			cfg.Actions.RunnerVersion = file.Actions.RunnerVersion
		}
		if file.Actions.Ephemeral != nil {
			cfg.Actions.Ephemeral = *file.Actions.Ephemeral
		}
	}
	if file.Blacksmith != nil {
		if file.Blacksmith.Org != "" {
			cfg.Blacksmith.Org = file.Blacksmith.Org
		}
		if file.Blacksmith.Workflow != "" {
			cfg.Blacksmith.Workflow = file.Blacksmith.Workflow
		}
		if file.Blacksmith.Job != "" {
			cfg.Blacksmith.Job = file.Blacksmith.Job
		}
		if file.Blacksmith.Ref != "" {
			cfg.Blacksmith.Ref = file.Blacksmith.Ref
		}
		applyLeaseDuration(&cfg.Blacksmith.IdleTimeout, file.Blacksmith.IdleTimeout)
		if file.Blacksmith.Debug != nil {
			cfg.Blacksmith.Debug = *file.Blacksmith.Debug
		}
	}
	if file.KubeVirt != nil {
		if file.KubeVirt.Kubectl != "" {
			cfg.KubeVirt.Kubectl = expandUserPath(file.KubeVirt.Kubectl)
		}
		if file.KubeVirt.Virtctl != "" {
			cfg.KubeVirt.Virtctl = expandUserPath(file.KubeVirt.Virtctl)
		}
		if file.KubeVirt.Kubeconfig != "" {
			cfg.KubeVirt.Kubeconfig = expandUserPath(file.KubeVirt.Kubeconfig)
		}
		if file.KubeVirt.Context != "" {
			cfg.KubeVirt.Context = file.KubeVirt.Context
		}
		if file.KubeVirt.Namespace != "" {
			cfg.KubeVirt.Namespace = file.KubeVirt.Namespace
		}
		if file.KubeVirt.Template != "" {
			cfg.KubeVirt.Template = expandUserPath(file.KubeVirt.Template)
		}
		if file.KubeVirt.SSHUser != "" {
			cfg.KubeVirt.SSHUser = file.KubeVirt.SSHUser
		}
		if file.KubeVirt.SSHKey != "" {
			cfg.KubeVirt.SSHKey = expandUserPath(file.KubeVirt.SSHKey)
		}
		if file.KubeVirt.SSHPublicKey != "" {
			cfg.KubeVirt.SSHPublicKey = expandUserPath(file.KubeVirt.SSHPublicKey)
		}
		if file.KubeVirt.SSHPort != "" {
			cfg.KubeVirt.SSHPort = file.KubeVirt.SSHPort
		}
		if file.KubeVirt.WorkRoot != "" {
			cfg.KubeVirt.WorkRoot = file.KubeVirt.WorkRoot
		}
		if file.KubeVirt.DeleteOnRelease != nil {
			cfg.KubeVirt.DeleteOnRelease = *file.KubeVirt.DeleteOnRelease
			MarkDeleteOnReleaseExplicit(cfg, "kubevirt")
		}
	}
	if file.AgentSandbox != nil {
		if trusted && file.AgentSandbox.Kubectl != "" {
			cfg.AgentSandbox.Kubectl = file.AgentSandbox.Kubectl
		}
		if trusted && file.AgentSandbox.Kubeconfig != "" {
			cfg.AgentSandbox.Kubeconfig = expandUserPath(file.AgentSandbox.Kubeconfig)
		}
		if trusted && file.AgentSandbox.Context != "" {
			cfg.AgentSandbox.Context = file.AgentSandbox.Context
		}
		if trusted && file.AgentSandbox.Namespace != "" {
			cfg.AgentSandbox.Namespace = file.AgentSandbox.Namespace
		}
		if trusted && file.AgentSandbox.WarmPool != "" {
			cfg.AgentSandbox.WarmPool = file.AgentSandbox.WarmPool
		}
		if trusted && file.AgentSandbox.Container != "" {
			cfg.AgentSandbox.Container = file.AgentSandbox.Container
		}
		if trusted && file.AgentSandbox.Workdir != "" {
			cfg.AgentSandbox.Workdir = file.AgentSandbox.Workdir
		}
		if file.AgentSandbox.SandboxReadyTimeout != "" {
			applyLeaseDuration(&cfg.AgentSandbox.SandboxReadyTimeout, file.AgentSandbox.SandboxReadyTimeout)
		}
		if file.AgentSandbox.PodReadyTimeout != "" {
			applyLeaseDuration(&cfg.AgentSandbox.PodReadyTimeout, file.AgentSandbox.PodReadyTimeout)
		}
		if file.AgentSandbox.ExecTimeoutSecs != nil {
			if *file.AgentSandbox.ExecTimeoutSecs < 0 {
				return exit(2, "agentSandbox execTimeoutSecs must be non-negative")
			}
			cfg.AgentSandbox.ExecTimeoutSecs = *file.AgentSandbox.ExecTimeoutSecs
		}
		if file.AgentSandbox.DeleteOnRelease != nil {
			cfg.AgentSandbox.DeleteOnRelease = *file.AgentSandbox.DeleteOnRelease
			MarkDeleteOnReleaseExplicit(cfg, "agent-sandbox")
		}
		if file.AgentSandbox.ForgetMissing != nil {
			cfg.AgentSandbox.ForgetMissing = *file.AgentSandbox.ForgetMissing
		}
	}
	if file.External != nil {
		if file.External.Command != "" {
			cfg.External.Command = file.External.Command
		}
		if len(file.External.Args) > 0 {
			cfg.External.Args = append([]string(nil), file.External.Args...)
		}
		if file.External.Config != nil {
			cfg.External.Config = file.External.Config
		}
		if file.External.Capabilities != nil {
			cfg.External.Capabilities = *file.External.Capabilities
		}
		if file.External.Lifecycle != nil {
			cfg.External.Lifecycle = *file.External.Lifecycle
		}
		if file.External.Connection != nil {
			cfg.External.Connection = *file.External.Connection
		}
		if file.External.WorkRoot != "" {
			cfg.External.WorkRoot = file.External.WorkRoot
		}
		if file.External.RoutingFile != "" {
			cfg.External.RoutingFile = file.External.RoutingFile
		}
	}
	if file.Namespace != nil {
		if file.Namespace.Image != "" {
			cfg.Namespace.Image = file.Namespace.Image
		}
		if file.Namespace.Size != "" {
			cfg.Namespace.Size = file.Namespace.Size
		}
		if file.Namespace.Repository != "" {
			cfg.Namespace.Repository = file.Namespace.Repository
		}
		if file.Namespace.Site != "" {
			cfg.Namespace.Site = file.Namespace.Site
		}
		if file.Namespace.VolumeSizeGB > 0 {
			cfg.Namespace.VolumeSizeGB = file.Namespace.VolumeSizeGB
		}
		applyLeaseDuration(&cfg.Namespace.AutoStopIdleTimeout, file.Namespace.AutoStopIdleTimeout)
		if file.Namespace.WorkRoot != "" {
			cfg.Namespace.WorkRoot = file.Namespace.WorkRoot
		}
		if file.Namespace.DeleteOnRelease != nil {
			cfg.Namespace.DeleteOnRelease = *file.Namespace.DeleteOnRelease
			MarkDeleteOnReleaseExplicit(cfg, "namespace-devbox")
		}
	}
	if file.NamespaceInstance != nil {
		if trusted {
			if file.NamespaceInstance.CLIPath != "" {
				cfg.NamespaceInstance.CLIPath = expandUserPath(file.NamespaceInstance.CLIPath)
			}
			if file.NamespaceInstance.Region != "" {
				cfg.NamespaceInstance.Region = file.NamespaceInstance.Region
			}
			if file.NamespaceInstance.Endpoint != "" {
				cfg.NamespaceInstance.Endpoint = file.NamespaceInstance.Endpoint
			}
			if file.NamespaceInstance.Keychain != "" {
				cfg.NamespaceInstance.Keychain = file.NamespaceInstance.Keychain
			}
			if file.NamespaceInstance.Volumes != nil {
				cfg.NamespaceInstance.Volumes = append([]string(nil), file.NamespaceInstance.Volumes...)
			}
		}
		if file.NamespaceInstance.MachineType != "" {
			cfg.NamespaceInstance.MachineType = file.NamespaceInstance.MachineType
		}
		applyLeaseDuration(&cfg.NamespaceInstance.Duration, file.NamespaceInstance.Duration)
		if file.NamespaceInstance.WorkRoot != "" {
			cfg.NamespaceInstance.WorkRoot = file.NamespaceInstance.WorkRoot
		}
		if file.NamespaceInstance.Bare != nil {
			cfg.NamespaceInstance.Bare = *file.NamespaceInstance.Bare
		}
	}
	if file.Phala != nil {
		if trusted {
			if file.Phala.CLIPath != "" {
				cfg.Phala.CLIPath = expandUserPath(file.Phala.CLIPath)
			}
			if file.Phala.NodeID != "" {
				cfg.Phala.NodeID = file.Phala.NodeID
			}
			if file.Phala.Compose != "" {
				cfg.Phala.Compose = expandUserPath(file.Phala.Compose)
			}
		}
		if file.Phala.InstanceType != "" {
			cfg.Phala.InstanceType = file.Phala.InstanceType
			MarkPhalaInstanceTypeExplicit(cfg)
		}
		if file.Phala.WorkRoot != "" {
			cfg.Phala.WorkRoot = file.Phala.WorkRoot
		}
		// attest is read from untrusted config ONLY when it tightens security
		// (enabling the TDX attestation gate). Disabling it (attest: false)
		// requires trusted config, the local --phala-skip-attestation flag, or the
		// env var, so an untrusted repo config can never weaken the security gate.
		if file.Phala.Attest != nil && (trusted || *file.Phala.Attest) {
			value := *file.Phala.Attest
			cfg.Phala.Attest = &value
		}
	}
	if file.Morph != nil {
		if file.Morph.APIKey != "" {
			cfg.Morph.APIKey = file.Morph.APIKey
			cfg.credentialProvenance.morphAPIKey = credentialSource
		}
		if file.Morph.APIURL != "" {
			cfg.Morph.APIURL = file.Morph.APIURL
			cfg.credentialProvenance.morphAPIURL = credentialSource
		}
		if file.Morph.Snapshot != "" {
			cfg.Morph.Snapshot = file.Morph.Snapshot
		}
		if file.Morph.SSHGatewayHost != "" {
			cfg.Morph.SSHGatewayHost = file.Morph.SSHGatewayHost
			cfg.credentialProvenance.morphSSHGatewayHost = credentialSource
		}
		if file.Morph.WorkRoot != "" {
			cfg.Morph.WorkRoot = file.Morph.WorkRoot
		}
		if file.Morph.DeleteOnRelease != nil {
			cfg.Morph.DeleteOnRelease = *file.Morph.DeleteOnRelease
			MarkDeleteOnReleaseExplicit(cfg, "morph")
		}
		if file.Morph.WakeOnSSH != nil {
			cfg.Morph.WakeOnSSH = *file.Morph.WakeOnSSH
		}
	}
	if file.Daytona != nil {
		if file.Daytona.APIURL != "" {
			cfg.Daytona.APIURL = file.Daytona.APIURL
			cfg.credentialProvenance.daytonaAPIURL = credentialSource
		}
		if file.Daytona.Snapshot != "" {
			cfg.Daytona.Snapshot = file.Daytona.Snapshot
		}
		if file.Daytona.Target != "" {
			cfg.Daytona.Target = file.Daytona.Target
		}
		if file.Daytona.User != "" {
			cfg.Daytona.User = file.Daytona.User
		}
		if file.Daytona.WorkRoot != "" {
			cfg.Daytona.WorkRoot = file.Daytona.WorkRoot
		}
		if file.Daytona.SSHGatewayHost != "" {
			cfg.Daytona.SSHGatewayHost = file.Daytona.SSHGatewayHost
			cfg.credentialProvenance.daytonaSSHGateway = credentialSource
		}
		if file.Daytona.SSHAccessMinutes > 0 {
			cfg.Daytona.SSHAccessMinutes = file.Daytona.SSHAccessMinutes
		}
	}
	if file.E2B != nil {
		if file.E2B.APIURL != "" {
			cfg.E2B.APIURL = file.E2B.APIURL
			cfg.credentialProvenance.e2bAPIURL = credentialSource
		}
		if file.E2B.Domain != "" {
			cfg.E2B.Domain = file.E2B.Domain
			cfg.credentialProvenance.e2bDomain = credentialSource
		}
		if file.E2B.Template != "" {
			cfg.E2B.Template = file.E2B.Template
		}
		if file.E2B.Workdir != "" {
			cfg.E2B.Workdir = file.E2B.Workdir
		}
		if file.E2B.User != "" {
			cfg.E2B.User = file.E2B.User
		}
	}
	if file.ExeDev != nil {
		if file.ExeDev.ControlHost != "" {
			cfg.ExeDev.ControlHost = file.ExeDev.ControlHost
		}
		if file.ExeDev.Image != "" {
			cfg.ExeDev.Image = file.ExeDev.Image
		}
		if file.ExeDev.CPUs > 0 {
			cfg.ExeDev.CPUs = file.ExeDev.CPUs
		}
		if file.ExeDev.Memory != "" {
			cfg.ExeDev.Memory = file.ExeDev.Memory
		}
		if file.ExeDev.Disk != "" {
			cfg.ExeDev.Disk = file.ExeDev.Disk
		}
		if file.ExeDev.Command != "" {
			cfg.ExeDev.Command = file.ExeDev.Command
		}
		if file.ExeDev.User != "" {
			cfg.ExeDev.User = file.ExeDev.User
		}
		if file.ExeDev.WorkRoot != "" {
			cfg.ExeDev.WorkRoot = file.ExeDev.WorkRoot
		}
		if file.ExeDev.NoEmail != nil {
			cfg.ExeDev.NoEmail = *file.ExeDev.NoEmail
		}
	}
	if file.Railway != nil {
		if file.Railway.APIURL != "" {
			cfg.Railway.APIURL = file.Railway.APIURL
			cfg.credentialProvenance.railwayAPIURL = credentialSource
		}
		if file.Railway.ProjectID != "" {
			cfg.Railway.ProjectID = file.Railway.ProjectID
		}
		if file.Railway.EnvironmentID != "" {
			cfg.Railway.EnvironmentID = file.Railway.EnvironmentID
		}
	}
	if file.Runpod != nil {
		if file.Runpod.APIURL != "" {
			cfg.Runpod.APIURL = file.Runpod.APIURL
			cfg.credentialProvenance.runpodAPIURL = credentialSource
		}
		if file.Runpod.CloudType != "" {
			cfg.Runpod.CloudType = file.Runpod.CloudType
		}
		if file.Runpod.InstanceID != "" {
			cfg.Runpod.InstanceID = file.Runpod.InstanceID
		}
		if file.Runpod.Image != "" {
			cfg.Runpod.Image = file.Runpod.Image
		}
		if file.Runpod.TemplateID != "" {
			cfg.Runpod.TemplateID = file.Runpod.TemplateID
		}
		if file.Runpod.DiskGB != 0 {
			cfg.Runpod.DiskGB = file.Runpod.DiskGB
		}
		if file.Runpod.User != "" {
			cfg.Runpod.User = file.Runpod.User
		}
		if file.Runpod.WorkRoot != "" {
			cfg.Runpod.WorkRoot = file.Runpod.WorkRoot
		}
	}
	if file.NvidiaBrev != nil {
		if trusted && file.NvidiaBrev.CLI != "" {
			cfg.NvidiaBrev.CLI = file.NvidiaBrev.CLI
		}
		if file.NvidiaBrev.Org != "" {
			cfg.NvidiaBrev.Org = file.NvidiaBrev.Org
		}
		if file.NvidiaBrev.Type != "" {
			cfg.NvidiaBrev.Type = file.NvidiaBrev.Type
		}
		if file.NvidiaBrev.GPUName != "" {
			cfg.NvidiaBrev.GPUName = file.NvidiaBrev.GPUName
		}
		if file.NvidiaBrev.Provider != "" {
			cfg.NvidiaBrev.Provider = file.NvidiaBrev.Provider
		}
		if file.NvidiaBrev.Mode != "" {
			cfg.NvidiaBrev.Mode = file.NvidiaBrev.Mode
		}
		if file.NvidiaBrev.Launchable != "" {
			cfg.NvidiaBrev.Launchable = file.NvidiaBrev.Launchable
		}
		if file.NvidiaBrev.StartupScript != "" &&
			(trusted || !strings.HasPrefix(strings.TrimSpace(file.NvidiaBrev.StartupScript), "@")) {
			cfg.NvidiaBrev.StartupScript = file.NvidiaBrev.StartupScript
		}
		if file.NvidiaBrev.ReleaseAction != "" {
			cfg.NvidiaBrev.ReleaseAction = file.NvidiaBrev.ReleaseAction
			MarkDeleteOnReleaseExplicit(cfg, "nvidia-brev")
		}
		if file.NvidiaBrev.Target != "" {
			cfg.NvidiaBrev.Target = file.NvidiaBrev.Target
		}
		if file.NvidiaBrev.User != "" {
			cfg.NvidiaBrev.User = file.NvidiaBrev.User
		}
		if file.NvidiaBrev.WorkRoot != "" {
			cfg.NvidiaBrev.WorkRoot = file.NvidiaBrev.WorkRoot
			MarkNvidiaBrevWorkRootExplicit(cfg)
		}
	}
	if file.Hostinger != nil {
		if trusted && file.Hostinger.APIToken != "" {
			cfg.Hostinger.APIToken = file.Hostinger.APIToken
		}
		if trusted && file.Hostinger.APIURL != "" {
			cfg.Hostinger.APIURL = file.Hostinger.APIURL
		}
		if trusted && file.Hostinger.ItemID != "" {
			cfg.Hostinger.ItemID = file.Hostinger.ItemID
		}
		if trusted && file.Hostinger.PaymentMethodID != "" {
			cfg.Hostinger.PaymentMethodID = file.Hostinger.PaymentMethodID
		}
		if trusted && file.Hostinger.TemplateID != "" {
			cfg.Hostinger.TemplateID = file.Hostinger.TemplateID
		}
		if trusted && file.Hostinger.DataCenterID != "" {
			cfg.Hostinger.DataCenterID = file.Hostinger.DataCenterID
		}
		if file.Hostinger.HostnamePrefix != "" {
			cfg.Hostinger.HostnamePrefix = file.Hostinger.HostnamePrefix
		}
		if file.Hostinger.User != "" {
			cfg.Hostinger.User = file.Hostinger.User
			MarkHostingerUserExplicit(cfg)
		}
		if file.Hostinger.WorkRoot != "" {
			cfg.Hostinger.WorkRoot = file.Hostinger.WorkRoot
			MarkHostingerWorkRootExplicit(cfg)
		}
		if file.Hostinger.AllowPurchase != nil && (trusted || !*file.Hostinger.AllowPurchase) {
			cfg.Hostinger.AllowPurchase = *file.Hostinger.AllowPurchase
		}
		if file.Hostinger.ReleaseAction != "" {
			cfg.Hostinger.ReleaseAction = file.Hostinger.ReleaseAction
		}
	}
	if file.Wandb != nil {
		if file.Wandb.APIKey != "" {
			cfg.Wandb.APIKey = file.Wandb.APIKey
		}
		if file.Wandb.DefaultImage != "" {
			cfg.Wandb.DefaultImage = file.Wandb.DefaultImage
		}
		if file.Wandb.MaxLifetimeSeconds > 0 {
			cfg.Wandb.MaxLifetimeSeconds = file.Wandb.MaxLifetimeSeconds
		}
	}
	if file.Islo != nil {
		if file.Islo.BaseURL != "" {
			cfg.Islo.BaseURL = file.Islo.BaseURL
			cfg.credentialProvenance.isloBaseURL = credentialSource
		}
		if file.Islo.Image != "" {
			cfg.Islo.Image = file.Islo.Image
			cfg.isloImageExplicit = true
		}
		if file.Islo.Workdir != "" {
			cfg.Islo.Workdir = file.Islo.Workdir
		}
		if file.Islo.GatewayProfile != "" {
			cfg.Islo.GatewayProfile = file.Islo.GatewayProfile
		}
		if file.Islo.SnapshotName != "" {
			cfg.Islo.SnapshotName = file.Islo.SnapshotName
		}
		if file.Islo.VCPUs > 0 {
			cfg.Islo.VCPUs = file.Islo.VCPUs
			cfg.isloVCPUsExplicit = true
		}
		if file.Islo.MemoryMB > 0 {
			cfg.Islo.MemoryMB = file.Islo.MemoryMB
			cfg.isloMemoryMBExplicit = true
		}
		if file.Islo.DiskGB > 0 {
			cfg.Islo.DiskGB = file.Islo.DiskGB
			cfg.isloDiskGBExplicit = true
		}
	}
	if file.Freestyle != nil {
		if file.Freestyle.APIURL != "" {
			cfg.Freestyle.APIURL = file.Freestyle.APIURL
		}
		if file.Freestyle.Workdir != "" {
			cfg.Freestyle.Workdir = file.Freestyle.Workdir
		}
		if file.Freestyle.VCPUs > 0 {
			cfg.Freestyle.VCPUs = file.Freestyle.VCPUs
		}
		if file.Freestyle.MemoryGB > 0 {
			cfg.Freestyle.MemoryGB = file.Freestyle.MemoryGB
		}
	}
	if file.Tenki != nil {
		if file.Tenki.CLIPath != "" {
			cfg.Tenki.CLIPath = file.Tenki.CLIPath
		}
		if file.Tenki.Endpoint != "" {
			cfg.Tenki.Endpoint = file.Tenki.Endpoint
			cfg.credentialProvenance.tenkiEndpoint = credentialSource
		}
		if file.Tenki.Gateway != "" {
			cfg.Tenki.Gateway = file.Tenki.Gateway
			cfg.credentialProvenance.tenkiGateway = credentialSource
		}
		if file.Tenki.Workspace != "" {
			cfg.Tenki.Workspace = file.Tenki.Workspace
		}
		if file.Tenki.Project != "" {
			cfg.Tenki.Project = file.Tenki.Project
		}
		if file.Tenki.Image != "" {
			cfg.Tenki.Image = file.Tenki.Image
		}
		if file.Tenki.Snapshot != "" {
			cfg.Tenki.Snapshot = file.Tenki.Snapshot
		}
		if file.Tenki.WorkRoot != "" {
			cfg.Tenki.WorkRoot = file.Tenki.WorkRoot
		}
		if file.Tenki.CPUs > 0 {
			cfg.Tenki.CPUs = file.Tenki.CPUs
		}
		if file.Tenki.MemoryMB > 0 {
			cfg.Tenki.MemoryMB = file.Tenki.MemoryMB
		}
		if file.Tenki.DiskGB > 0 {
			cfg.Tenki.DiskGB = file.Tenki.DiskGB
		}
	}
	if file.Tensorlake != nil {
		if file.Tensorlake.APIURL != "" {
			cfg.Tensorlake.APIURL = file.Tensorlake.APIURL
			cfg.credentialProvenance.tensorlakeAPIURL = credentialSource
		}
		if file.Tensorlake.CLIPath != "" {
			cfg.Tensorlake.CLIPath = file.Tensorlake.CLIPath
		}
		if file.Tensorlake.Image != "" {
			cfg.Tensorlake.Image = file.Tensorlake.Image
		}
		if file.Tensorlake.Snapshot != "" {
			cfg.Tensorlake.Snapshot = file.Tensorlake.Snapshot
		}
		if file.Tensorlake.OrganizationID != "" {
			cfg.Tensorlake.OrganizationID = file.Tensorlake.OrganizationID
		}
		if file.Tensorlake.ProjectID != "" {
			cfg.Tensorlake.ProjectID = file.Tensorlake.ProjectID
		}
		if file.Tensorlake.Namespace != "" {
			cfg.Tensorlake.Namespace = file.Tensorlake.Namespace
		}
		if file.Tensorlake.Workdir != "" {
			cfg.Tensorlake.Workdir = file.Tensorlake.Workdir
		}
		if file.Tensorlake.CPUs > 0 {
			cfg.Tensorlake.CPUs = file.Tensorlake.CPUs
		}
		if file.Tensorlake.MemoryMB > 0 {
			cfg.Tensorlake.MemoryMB = file.Tensorlake.MemoryMB
		}
		if file.Tensorlake.DiskMB > 0 {
			cfg.Tensorlake.DiskMB = file.Tensorlake.DiskMB
		}
		if file.Tensorlake.TimeoutSecs > 0 {
			cfg.Tensorlake.TimeoutSecs = file.Tensorlake.TimeoutSecs
		}
		if file.Tensorlake.NoInternet != nil {
			cfg.Tensorlake.NoInternet = *file.Tensorlake.NoInternet
		}
	}
	if file.OpenComputer != nil {
		if file.OpenComputer.Workdir != "" {
			cfg.OpenComputer.Workdir = file.OpenComputer.Workdir
		}
		if file.OpenComputer.CPU != nil {
			cfg.OpenComputer.CPU = *file.OpenComputer.CPU
		}
		if file.OpenComputer.MemoryMB != nil {
			cfg.OpenComputer.MemoryMB = *file.OpenComputer.MemoryMB
		}
		if file.OpenComputer.TimeoutSecs != nil {
			cfg.OpenComputer.TimeoutSecs = *file.OpenComputer.TimeoutSecs
		}
		if file.OpenComputer.ExecTimeoutSecs != nil {
			cfg.OpenComputer.ExecTimeoutSecs = *file.OpenComputer.ExecTimeoutSecs
		}
		if file.OpenComputer.Burst != nil {
			cfg.OpenComputer.Burst = *file.OpenComputer.Burst
		}
	}
	if file.CodeSandbox != nil {
		if file.CodeSandbox.TemplateID != nil {
			cfg.CodeSandbox.TemplateID = *file.CodeSandbox.TemplateID
		}
		if file.CodeSandbox.Workdir != nil {
			cfg.CodeSandbox.Workdir = *file.CodeSandbox.Workdir
		}
		if file.CodeSandbox.VMTier != nil {
			cfg.CodeSandbox.VMTier = *file.CodeSandbox.VMTier
		}
		if file.CodeSandbox.Privacy != nil {
			cfg.CodeSandbox.Privacy = *file.CodeSandbox.Privacy
		}
		if file.CodeSandbox.HibernationTimeoutSecs != nil {
			if *file.CodeSandbox.HibernationTimeoutSecs < 0 {
				return exit(2, "codesandbox hibernationTimeoutSecs must be non-negative")
			}
			cfg.CodeSandbox.HibernationTimeoutSecs = *file.CodeSandbox.HibernationTimeoutSecs
		}
		if file.CodeSandbox.AutomaticWakeupHTTP != nil {
			cfg.CodeSandbox.AutomaticWakeupHTTP = *file.CodeSandbox.AutomaticWakeupHTTP
		}
		if file.CodeSandbox.AutomaticWakeupWebSocket != nil {
			cfg.CodeSandbox.AutomaticWakeupWebSocket = *file.CodeSandbox.AutomaticWakeupWebSocket
		}
		if trusted && file.CodeSandbox.BridgeCommand != nil {
			cfg.CodeSandbox.BridgeCommand = *file.CodeSandbox.BridgeCommand
		}
		if trusted && file.CodeSandbox.SDKPackage != nil {
			cfg.CodeSandbox.SDKPackage = *file.CodeSandbox.SDKPackage
		}
		if file.CodeSandbox.DoctorListLimit != nil {
			if *file.CodeSandbox.DoctorListLimit < 0 {
				return exit(2, "codesandbox doctorListLimit must be non-negative")
			}
			cfg.CodeSandbox.DoctorListLimit = *file.CodeSandbox.DoctorListLimit
		}
		if file.CodeSandbox.OperationTimeoutSecs != nil {
			if *file.CodeSandbox.OperationTimeoutSecs < 0 {
				return exit(2, "codesandbox operationTimeoutSecs must be non-negative")
			}
			cfg.CodeSandbox.OperationTimeoutSecs = *file.CodeSandbox.OperationTimeoutSecs
		}
	}
	if file.OpenSandbox != nil {
		if file.OpenSandbox.Image != nil {
			cfg.OpenSandbox.Image = *file.OpenSandbox.Image
		}
		if file.OpenSandbox.Workdir != nil {
			cfg.OpenSandbox.Workdir = *file.OpenSandbox.Workdir
		}
		if file.OpenSandbox.CPU != nil {
			cfg.OpenSandbox.CPU = *file.OpenSandbox.CPU
		}
		if file.OpenSandbox.Memory != nil {
			cfg.OpenSandbox.Memory = *file.OpenSandbox.Memory
		}
		if file.OpenSandbox.TimeoutSecs != nil {
			if *file.OpenSandbox.TimeoutSecs < 0 {
				return exit(2, "opensandbox timeoutSecs must be non-negative")
			}
			cfg.OpenSandbox.TimeoutSecs = *file.OpenSandbox.TimeoutSecs
		}
		if file.OpenSandbox.ExecTimeoutSecs != nil {
			if *file.OpenSandbox.ExecTimeoutSecs < 0 {
				return exit(2, "opensandbox execTimeoutSecs must be non-negative")
			}
			cfg.OpenSandbox.ExecTimeoutSecs = *file.OpenSandbox.ExecTimeoutSecs
		}
		if file.OpenSandbox.PlatformOS != nil {
			cfg.OpenSandbox.PlatformOS = *file.OpenSandbox.PlatformOS
		}
		if file.OpenSandbox.PlatformArch != nil {
			cfg.OpenSandbox.PlatformArch = *file.OpenSandbox.PlatformArch
		}
		if file.OpenSandbox.SecureAccess != nil {
			cfg.OpenSandbox.SecureAccess = *file.OpenSandbox.SecureAccess
		}
		if file.OpenSandbox.UseServerProxy != nil {
			cfg.OpenSandbox.UseServerProxy = *file.OpenSandbox.UseServerProxy
		}
	}
	if file.VercelSandbox != nil {
		if file.VercelSandbox.Runtime != nil {
			cfg.VercelSandbox.Runtime = *file.VercelSandbox.Runtime
		}
		if file.VercelSandbox.Workdir != nil {
			cfg.VercelSandbox.Workdir = *file.VercelSandbox.Workdir
		}
		if file.VercelSandbox.ProjectID != nil {
			cfg.VercelSandbox.ProjectID = *file.VercelSandbox.ProjectID
		}
		if file.VercelSandbox.TeamID != nil {
			cfg.VercelSandbox.TeamID = *file.VercelSandbox.TeamID
		}
		if file.VercelSandbox.Scope != nil {
			cfg.VercelSandbox.Scope = *file.VercelSandbox.Scope
		}
		if file.VercelSandbox.VCPUs != nil {
			cfg.VercelSandbox.VCPUs = *file.VercelSandbox.VCPUs
		}
		if file.VercelSandbox.TimeoutSecs != nil {
			if *file.VercelSandbox.TimeoutSecs < 0 {
				return exit(2, "vercel-sandbox timeoutSecs must be non-negative")
			}
			cfg.VercelSandbox.TimeoutSecs = *file.VercelSandbox.TimeoutSecs
		}
		if file.VercelSandbox.ExecTimeoutSecs != nil {
			if *file.VercelSandbox.ExecTimeoutSecs < 0 {
				return exit(2, "vercel-sandbox execTimeoutSecs must be non-negative")
			}
			cfg.VercelSandbox.ExecTimeoutSecs = *file.VercelSandbox.ExecTimeoutSecs
		}
		if file.VercelSandbox.Persistent != nil {
			cfg.VercelSandbox.Persistent = *file.VercelSandbox.Persistent
		}
		if file.VercelSandbox.Snapshot != nil {
			cfg.VercelSandbox.Snapshot = *file.VercelSandbox.Snapshot
		}
		if file.VercelSandbox.SnapshotMode != nil {
			cfg.VercelSandbox.SnapshotMode = *file.VercelSandbox.SnapshotMode
		}
		if file.VercelSandbox.NetworkPolicy != nil {
			cfg.VercelSandbox.NetworkPolicy = *file.VercelSandbox.NetworkPolicy
		}
		if file.VercelSandbox.NetworkAllow != nil {
			cfg.VercelSandbox.NetworkAllow = normalizeList(*file.VercelSandbox.NetworkAllow)
		}
		if file.VercelSandbox.NetworkDeny != nil {
			cfg.VercelSandbox.NetworkDeny = normalizeList(*file.VercelSandbox.NetworkDeny)
		}
		if file.VercelSandbox.Ports != nil {
			cfg.VercelSandbox.Ports = normalizeList(*file.VercelSandbox.Ports)
		}
		if file.VercelSandbox.ForgetMissing != nil {
			cfg.VercelSandbox.ForgetMissing = *file.VercelSandbox.ForgetMissing
		}
	}
	if file.Superserve != nil {
		if trusted && strings.TrimSpace(file.Superserve.BaseURL) != "" {
			cfg.Superserve.BaseURL = file.Superserve.BaseURL
		}
		if file.Superserve.Template != nil {
			cfg.Superserve.Template = *file.Superserve.Template
		}
		if file.Superserve.Snapshot != nil {
			cfg.Superserve.Snapshot = *file.Superserve.Snapshot
		}
		if file.Superserve.Workdir != nil {
			cfg.Superserve.Workdir = *file.Superserve.Workdir
		}
		if file.Superserve.TimeoutSecs != nil {
			if *file.Superserve.TimeoutSecs < 0 {
				return exit(2, "superserve timeoutSecs must be non-negative")
			}
			cfg.Superserve.TimeoutSecs = *file.Superserve.TimeoutSecs
		}
		if file.Superserve.ExecTimeoutSecs != nil {
			if *file.Superserve.ExecTimeoutSecs < 0 {
				return exit(2, "superserve execTimeoutSecs must be non-negative")
			}
			cfg.Superserve.ExecTimeoutSecs = *file.Superserve.ExecTimeoutSecs
		}
		if file.Superserve.NetworkAllowOut != nil {
			cfg.Superserve.NetworkAllowOut = normalizeList(file.Superserve.NetworkAllowOut)
		}
		if file.Superserve.NetworkDenyOut != nil {
			cfg.Superserve.NetworkDenyOut = normalizeList(file.Superserve.NetworkDenyOut)
		}
		if file.Superserve.ForgetMissing != nil {
			cfg.Superserve.ForgetMissing = *file.Superserve.ForgetMissing
		}
	}
	if file.DockerSandbox != nil {
		if file.DockerSandbox.CLIPath != "" {
			cfg.DockerSandbox.CLIPath = file.DockerSandbox.CLIPath
		}
		if file.DockerSandbox.Agent != "" {
			cfg.DockerSandbox.Agent = file.DockerSandbox.Agent
		}
		if file.DockerSandbox.Template != nil {
			cfg.DockerSandbox.Template = *file.DockerSandbox.Template
		}
		if file.DockerSandbox.CPUs != nil {
			if *file.DockerSandbox.CPUs < 0 {
				return exit(2, "docker-sandbox cpus must be non-negative")
			}
			cfg.DockerSandbox.CPUs = *file.DockerSandbox.CPUs
		}
		if file.DockerSandbox.Memory != nil {
			cfg.DockerSandbox.Memory = *file.DockerSandbox.Memory
		}
		if file.DockerSandbox.Clone != nil {
			cfg.DockerSandbox.Clone = *file.DockerSandbox.Clone
		}
		if file.DockerSandbox.Workdir != nil {
			cfg.DockerSandbox.Workdir = *file.DockerSandbox.Workdir
		}
		if file.DockerSandbox.ExtraWorkspaces != nil {
			cfg.DockerSandbox.ExtraWorkspaces = append([]string(nil), (*file.DockerSandbox.ExtraWorkspaces)...)
		}
		if file.DockerSandbox.MCP != nil {
			cfg.DockerSandbox.MCP = append([]string(nil), (*file.DockerSandbox.MCP)...)
		}
		if file.DockerSandbox.Kit != nil {
			cfg.DockerSandbox.Kit = append([]string(nil), (*file.DockerSandbox.Kit)...)
		}
	}
	if file.AnthropicSRT != nil {
		if file.AnthropicSRT.CLIPath != "" {
			cfg.AnthropicSRT.CLIPath = file.AnthropicSRT.CLIPath
		}
		if file.AnthropicSRT.Settings != nil {
			cfg.AnthropicSRT.Settings = *file.AnthropicSRT.Settings
		}
		if file.AnthropicSRT.Debug != nil {
			cfg.AnthropicSRT.Debug = *file.AnthropicSRT.Debug
		}
	}
	if file.Modal != nil {
		if file.Modal.App != "" {
			cfg.Modal.App = file.Modal.App
		}
		if file.Modal.Image != "" {
			cfg.Modal.Image = file.Modal.Image
		}
		if file.Modal.Workdir != "" {
			cfg.Modal.Workdir = file.Modal.Workdir
		}
		if file.Modal.Python != "" {
			cfg.Modal.Python = file.Modal.Python
		}
	}
	if file.UpstashBox != nil {
		if file.UpstashBox.BaseURL != "" {
			cfg.UpstashBox.BaseURL = file.UpstashBox.BaseURL
			cfg.credentialProvenance.upstashBoxBaseURL = credentialSource
		}
		if file.UpstashBox.Runtime != "" {
			cfg.UpstashBox.Runtime = file.UpstashBox.Runtime
		}
		if file.UpstashBox.Size != "" {
			cfg.UpstashBox.Size = file.UpstashBox.Size
		}
		if file.UpstashBox.Workdir != "" {
			cfg.UpstashBox.Workdir = file.UpstashBox.Workdir
		}
		if file.UpstashBox.KeepAlive != nil {
			cfg.UpstashBox.KeepAlive = *file.UpstashBox.KeepAlive
		}
	}
	if file.Smolvm != nil {
		if file.Smolvm.BaseURL != "" {
			cfg.Smolvm.BaseURL = file.Smolvm.BaseURL
			cfg.credentialProvenance.smolvmBaseURL = credentialSource
		}
		if file.Smolvm.Image != "" {
			cfg.Smolvm.Image = file.Smolvm.Image
		}
		if file.Smolvm.Workdir != "" {
			cfg.Smolvm.Workdir = file.Smolvm.Workdir
		}
		if file.Smolvm.CPUs > 0 {
			cfg.Smolvm.CPUs = file.Smolvm.CPUs
		}
		if file.Smolvm.MemoryMB > 0 {
			cfg.Smolvm.MemoryMB = file.Smolvm.MemoryMB
		}
		if file.Smolvm.Network != "" {
			cfg.Smolvm.Network = file.Smolvm.Network
		}
		if file.Smolvm.Keep != nil {
			cfg.Smolvm.Keep = *file.Smolvm.Keep
		}
	}
	if file.AsciiBox != nil {
		if file.AsciiBox.BaseURL != "" {
			cfg.AsciiBox.BaseURL = file.AsciiBox.BaseURL
			cfg.credentialProvenance.asciiBoxBaseURL = credentialSource
		}
		if file.AsciiBox.CLIPath != "" {
			cfg.AsciiBox.CLIPath = file.AsciiBox.CLIPath
		}
		if file.AsciiBox.Workdir != "" {
			cfg.AsciiBox.Workdir = file.AsciiBox.Workdir
		}
	}
	applyCloudflareFileConfig(cfg, file.Cloudflare, credentialSource)
	applyCloudflareDynamicWorkersFileConfig(cfg, file.CloudflareDynamicWorkers, trusted)
	if file.Semaphore != nil {
		if file.Semaphore.Host != "" {
			cfg.Semaphore.Host = file.Semaphore.Host
			cfg.credentialProvenance.semaphoreHost = credentialSource
		}
		if file.Semaphore.Token != "" {
			cfg.Semaphore.Token = file.Semaphore.Token
			cfg.credentialProvenance.semaphoreToken = credentialSource
		}
		if file.Semaphore.Project != "" {
			cfg.Semaphore.Project = file.Semaphore.Project
		}
		if file.Semaphore.Machine != "" {
			cfg.Semaphore.Machine = file.Semaphore.Machine
		}
		if file.Semaphore.OSImage != "" {
			cfg.Semaphore.OSImage = file.Semaphore.OSImage
		}
		if file.Semaphore.IdleTimeout != "" {
			cfg.Semaphore.IdleTimeout = file.Semaphore.IdleTimeout
		}
	}
	if file.Sprites != nil {
		if file.Sprites.APIURL != "" {
			cfg.Sprites.APIURL = file.Sprites.APIURL
			cfg.credentialProvenance.spritesAPIURL = credentialSource
		}
		if file.Sprites.WorkRoot != "" {
			cfg.Sprites.WorkRoot = file.Sprites.WorkRoot
		}
	}
	if file.LocalContainer != nil {
		if file.LocalContainer.Runtime != "" {
			cfg.LocalContainer.Runtime = file.LocalContainer.Runtime
			cfg.localContainerRuntimeExplicit = true
		}
		if file.LocalContainer.Image != "" {
			cfg.LocalContainer.Image = file.LocalContainer.Image
			cfg.localContainerImageExplicit = true
		}
		if file.LocalContainer.User != "" {
			cfg.LocalContainer.User = file.LocalContainer.User
		}
		if file.LocalContainer.WorkRoot != "" {
			cfg.LocalContainer.WorkRoot = file.LocalContainer.WorkRoot
		}
		if file.LocalContainer.CPUs > 0 {
			cfg.LocalContainer.CPUs = file.LocalContainer.CPUs
		}
		if file.LocalContainer.Memory != "" {
			cfg.LocalContainer.Memory = file.LocalContainer.Memory
		}
		if file.LocalContainer.Network != "" {
			cfg.LocalContainer.Network = file.LocalContainer.Network
		}
		if file.LocalContainer.DockerSocket != nil {
			cfg.LocalContainer.DockerSocket = *file.LocalContainer.DockerSocket
		}
		// NOTE: localContainer.volumes is intentionally NOT loaded from
		// repo-local config files. Bind mounts expose host paths and must
		// be an explicit CLI action (--local-container-volume), not
		// something an untrusted checkout can request via .crabbox.yaml.
	}
	if file.AppleContainer != nil {
		if file.AppleContainer.CLIPath != "" {
			cfg.AppleContainer.CLIPath = file.AppleContainer.CLIPath
		}
		if file.AppleContainer.Image != "" {
			cfg.AppleContainer.Image = file.AppleContainer.Image
			cfg.appleContainerImageExplicit = true
		}
		if file.AppleContainer.User != "" {
			cfg.AppleContainer.User = file.AppleContainer.User
		}
		if file.AppleContainer.WorkRoot != "" {
			cfg.AppleContainer.WorkRoot = file.AppleContainer.WorkRoot
		}
		if file.AppleContainer.CPUs > 0 {
			cfg.AppleContainer.CPUs = file.AppleContainer.CPUs
		}
		if file.AppleContainer.Memory != "" {
			cfg.AppleContainer.Memory = file.AppleContainer.Memory
		}
		if len(file.AppleContainer.ExtraRunArgs) > 0 {
			cfg.AppleContainer.ExtraRunArgs = append([]string(nil), file.AppleContainer.ExtraRunArgs...)
		}
	}
	if file.AppleVZ != nil {
		if file.AppleVZ.HelperPath != "" {
			cfg.AppleVZ.HelperPath = file.AppleVZ.HelperPath
		}
		if file.AppleVZ.Image != "" {
			cfg.AppleVZ.Image = file.AppleVZ.Image
			cfg.AppleVZ.ImageSHA256 = ""
			cfg.appleVZImageExplicit = true
			cfg.appleVZImageSHA256Explicit = false
		}
		if file.AppleVZ.ImageSHA256 != "" {
			cfg.AppleVZ.ImageSHA256 = file.AppleVZ.ImageSHA256
			cfg.appleVZImageSHA256Explicit = true
		}
		if file.AppleVZ.User != "" {
			cfg.AppleVZ.User = file.AppleVZ.User
		}
		if file.AppleVZ.WorkRoot != "" {
			cfg.AppleVZ.WorkRoot = file.AppleVZ.WorkRoot
		}
		if file.AppleVZ.CPUs != nil {
			cfg.AppleVZ.CPUs = *file.AppleVZ.CPUs
			cfg.appleVZCPUsExplicit = true
		}
		if file.AppleVZ.MemoryMiB != nil {
			cfg.AppleVZ.MemoryMiB = *file.AppleVZ.MemoryMiB
			cfg.appleVZMemoryExplicit = true
		}
		if file.AppleVZ.DiskGiB != nil {
			cfg.AppleVZ.DiskGiB = *file.AppleVZ.DiskGiB
			cfg.appleVZDiskExplicit = true
		}
	}
	if file.MXC != nil {
		if file.MXC.CLIPath != "" {
			cfg.MXC.CLIPath = file.MXC.CLIPath
		}
		if file.MXC.Version != "" {
			cfg.MXC.Version = file.MXC.Version
		}
		if file.MXC.Containment != "" {
			cfg.MXC.Containment = file.MXC.Containment
		}
		if file.MXC.Network != "" {
			cfg.MXC.Network = file.MXC.Network
		}
		if file.MXC.ReadOnlyPaths != nil {
			cfg.MXC.ReadOnlyPaths = append([]string(nil), file.MXC.ReadOnlyPaths...)
		}
		if file.MXC.ReadWritePaths != nil {
			cfg.MXC.ReadWritePaths = append([]string(nil), file.MXC.ReadWritePaths...)
		}
		if file.MXC.AllowedHosts != nil {
			cfg.MXC.AllowedHosts = append([]string(nil), file.MXC.AllowedHosts...)
		}
		if file.MXC.BlockedHosts != nil {
			cfg.MXC.BlockedHosts = append([]string(nil), file.MXC.BlockedHosts...)
		}
		if file.MXC.AllowDACLMutation != nil {
			cfg.MXC.AllowDACLMutation = *file.MXC.AllowDACLMutation
		}
		if file.MXC.AllowWindowsUI != nil {
			cfg.MXC.AllowWindowsUI = *file.MXC.AllowWindowsUI
		}
		if file.MXC.Experimental != nil {
			cfg.MXC.Experimental = *file.MXC.Experimental
		}
	}
	if file.Multipass != nil {
		if file.Multipass.CLIPath != "" {
			cfg.Multipass.CLIPath = file.Multipass.CLIPath
		}
		if file.Multipass.Image != "" {
			cfg.Multipass.Image = file.Multipass.Image
			cfg.multipassImageExplicit = true
		}
		if file.Multipass.User != "" {
			cfg.Multipass.User = file.Multipass.User
		}
		if file.Multipass.WorkRoot != "" {
			cfg.Multipass.WorkRoot = file.Multipass.WorkRoot
		}
		if file.Multipass.CPUs > 0 {
			cfg.Multipass.CPUs = file.Multipass.CPUs
		}
		if file.Multipass.Memory != "" {
			cfg.Multipass.Memory = file.Multipass.Memory
		}
		if file.Multipass.Disk != "" {
			cfg.Multipass.Disk = file.Multipass.Disk
		}
		if file.Multipass.LaunchTimeout != "" {
			applyLeaseDuration(&cfg.Multipass.LaunchTimeout, file.Multipass.LaunchTimeout)
		}
	}
	if file.Tart != nil {
		if file.Tart.Image != "" {
			cfg.Tart.Image = file.Tart.Image
			cfg.tartImageExplicit = true
		}
		if file.Tart.User != "" {
			cfg.Tart.User = file.Tart.User
		}
		if file.Tart.Password != "" {
			cfg.Tart.Password = file.Tart.Password
		}
		if file.Tart.WorkRoot != "" {
			cfg.Tart.WorkRoot = file.Tart.WorkRoot
		}
		if file.Tart.CPUs != nil {
			cfg.Tart.CPUs = *file.Tart.CPUs
			cfg.tartCPUsExplicit = true
		}
		if file.Tart.Memory != nil {
			cfg.Tart.Memory = *file.Tart.Memory
			cfg.tartMemoryExplicit = true
		}
		if file.Tart.Disk != nil {
			cfg.Tart.Disk = *file.Tart.Disk
			cfg.tartDiskExplicit = true
		}
	}
	if file.HyperV != nil {
		if file.HyperV.Image != "" {
			cfg.HyperV.Image = file.HyperV.Image
		}
		if file.HyperV.User != "" {
			cfg.HyperV.User = file.HyperV.User
		}
		if file.HyperV.WorkRoot != "" {
			cfg.HyperV.WorkRoot = file.HyperV.WorkRoot
		}
		if file.HyperV.CPUs > 0 {
			cfg.HyperV.CPUs = file.HyperV.CPUs
		}
		if file.HyperV.Memory > 0 {
			cfg.HyperV.Memory = file.HyperV.Memory
		}
		if file.HyperV.Switch != "" {
			cfg.HyperV.Switch = file.HyperV.Switch
		}
		if file.HyperV.GuestPassword != "" {
			cfg.HyperV.GuestPassword = file.HyperV.GuestPassword
		}
		if file.HyperV.InitPassword != nil {
			cfg.HyperV.InitPassword = *file.HyperV.InitPassword
		}
	}
	if file.WindowsSandbox != nil {
		if file.WindowsSandbox.Workdir != "" {
			cfg.WindowsSandbox.Workdir = file.WindowsSandbox.Workdir
		}
		if trusted {
			if file.WindowsSandbox.TempRoot != "" {
				cfg.WindowsSandbox.TempRoot = expandUserPath(file.WindowsSandbox.TempRoot)
			}
			if file.WindowsSandbox.Networking != "" {
				cfg.WindowsSandbox.Networking = file.WindowsSandbox.Networking
			}
			if file.WindowsSandbox.VGPU != "" {
				cfg.WindowsSandbox.VGPU = file.WindowsSandbox.VGPU
			}
			if file.WindowsSandbox.Clipboard != "" {
				cfg.WindowsSandbox.Clipboard = file.WindowsSandbox.Clipboard
			}
			if file.WindowsSandbox.ProtectedClient != "" {
				cfg.WindowsSandbox.ProtectedClient = file.WindowsSandbox.ProtectedClient
			}
			if file.WindowsSandbox.AudioInput != "" {
				cfg.WindowsSandbox.AudioInput = file.WindowsSandbox.AudioInput
			}
			if file.WindowsSandbox.VideoInput != "" {
				cfg.WindowsSandbox.VideoInput = file.WindowsSandbox.VideoInput
			}
			if file.WindowsSandbox.PrinterRedirection != "" {
				cfg.WindowsSandbox.PrinterRedirection = file.WindowsSandbox.PrinterRedirection
			}
			if file.WindowsSandbox.MemoryMB > 0 {
				cfg.WindowsSandbox.MemoryMB = file.WindowsSandbox.MemoryMB
			}
		}
	}
	if file.Tailscale != nil {
		if file.Tailscale.Enabled != nil {
			cfg.Tailscale.Enabled = *file.Tailscale.Enabled
		}
		if file.Tailscale.Network != "" {
			cfg.Network = NetworkMode(strings.ToLower(strings.TrimSpace(file.Tailscale.Network)))
		}
		if len(file.Tailscale.Tags) > 0 {
			cfg.Tailscale.Tags = normalizeTailscaleTags(file.Tailscale.Tags)
		}
		if file.Tailscale.HostnameTemplate != "" {
			cfg.Tailscale.HostnameTemplate = file.Tailscale.HostnameTemplate
		}
		if file.Tailscale.AuthKeyEnv != "" {
			cfg.Tailscale.AuthKeyEnv = file.Tailscale.AuthKeyEnv
		}
		if file.Tailscale.ExitNode != "" {
			cfg.Tailscale.ExitNode = strings.TrimSpace(file.Tailscale.ExitNode)
		}
		if file.Tailscale.ExitNodeAllowLANAccess != nil {
			cfg.Tailscale.ExitNodeAllowLANAccess = *file.Tailscale.ExitNodeAllowLANAccess
		}
	}
	if file.Static != nil {
		if file.Static.ID != "" {
			cfg.Static.ID = file.Static.ID
		}
		if file.Static.Name != "" {
			cfg.Static.Name = file.Static.Name
		}
		if file.Static.Host != "" {
			cfg.Static.Host = file.Static.Host
		}
		if file.Static.User != "" {
			cfg.Static.User = file.Static.User
		}
		if file.Static.Port != "" {
			cfg.Static.Port = file.Static.Port
		}
		if file.Static.WorkRoot != "" {
			cfg.Static.WorkRoot = file.Static.WorkRoot
		}
	}
	if file.Results != nil {
		if len(file.Results.JUnit) > 0 {
			cfg.Results.JUnit = appendUniqueStrings(nil, file.Results.JUnit...)
		}
		if file.Results.Auto != nil {
			cfg.Results.Auto = *file.Results.Auto
		}
	}
	if file.Cache != nil {
		if file.Cache.Pnpm != nil {
			cfg.Cache.Pnpm = *file.Cache.Pnpm
		}
		if file.Cache.Npm != nil {
			cfg.Cache.Npm = *file.Cache.Npm
		}
		if file.Cache.Docker != nil {
			cfg.Cache.Docker = *file.Cache.Docker
		}
		if file.Cache.Git != nil {
			cfg.Cache.Git = *file.Cache.Git
		}
		if file.Cache.MaxGB > 0 {
			cfg.Cache.MaxGB = file.Cache.MaxGB
		}
		if file.Cache.PurgeOnRelease != nil {
			cfg.Cache.PurgeOnRelease = *file.Cache.PurgeOnRelease
		}
		if file.Cache.Volumes != nil {
			volumes, err := normalizeFileCacheVolumes(*file.Cache.Volumes)
			if err != nil {
				return err
			}
			cfg.Cache.Volumes = volumes
		}
	}
	if len(file.Presets) > 0 {
		if cfg.Presets == nil {
			cfg.Presets = map[string]PresetConfig{}
		}
		for name, preset := range file.Presets {
			name = strings.TrimSpace(name)
			if name != "" {
				cfg.Presets[name] = applyFilePresetConfig(cfg.Presets[name], preset)
			}
		}
	}
	if len(file.ProofTemplates) > 0 {
		if cfg.ProofTemplates == nil {
			cfg.ProofTemplates = map[string]ProofTemplateConfig{}
		}
		for name, tmpl := range file.ProofTemplates {
			name = strings.TrimSpace(name)
			if name != "" {
				cfg.ProofTemplates[name] = applyFileProofTemplateConfig(cfg.ProofTemplates[name], tmpl)
			}
		}
	}
	if len(file.Profiles) > 0 {
		if cfg.Profiles == nil {
			cfg.Profiles = map[string]ProfileConfig{}
		}
		for name, profile := range file.Profiles {
			name = strings.TrimSpace(name)
			if name != "" {
				cfg.Profiles[name] = applyFileProfileConfig(cfg.Profiles[name], profile)
			}
		}
	}
	if len(file.Jobs) > 0 {
		if cfg.Jobs == nil {
			cfg.Jobs = map[string]JobConfig{}
		}
		for name, job := range file.Jobs {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			cfg.Jobs[name] = applyFileJobConfig(cfg.Jobs[name], job)
		}
	}
	return nil
}

func applyFileProfileConfig(profile ProfileConfig, file fileProfileConfig) ProfileConfig {
	if len(file.Env.Values) > 0 {
		if profile.Env == nil {
			profile.Env = map[string]string{}
		}
		for key, value := range file.Env.Values {
			key = strings.TrimSpace(key)
			if key != "" {
				profile.Env[key] = value
			}
		}
	}
	if len(file.Env.Allow) > 0 {
		profile.EnvAllow = appendUniqueStrings(profile.EnvAllow, file.Env.Allow...)
	}
	if len(file.EnvAllow) > 0 {
		profile.EnvAllow = appendUniqueStrings(profile.EnvAllow, file.EnvAllow...)
	}
	if len(file.ArtifactGlobs) > 0 {
		profile.ArtifactGlobs = appendUniqueStrings(nil, file.ArtifactGlobs...)
	}
	if file.Doctor != nil {
		profile.Doctor = applyFileDoctorProfileConfig(profile.Doctor, *file.Doctor)
	}
	if len(file.Presets) > 0 {
		if profile.Presets == nil {
			profile.Presets = map[string]PresetConfig{}
		}
		for name, preset := range file.Presets {
			name = strings.TrimSpace(name)
			if name != "" {
				profile.Presets[name] = applyFilePresetConfig(profile.Presets[name], preset)
			}
		}
	}
	if len(file.ProofTemplates) > 0 {
		if profile.ProofTemplates == nil {
			profile.ProofTemplates = map[string]ProofTemplateConfig{}
		}
		for name, tmpl := range file.ProofTemplates {
			name = strings.TrimSpace(name)
			if name != "" {
				profile.ProofTemplates[name] = applyFileProofTemplateConfig(profile.ProofTemplates[name], tmpl)
			}
		}
	}
	return profile
}

func applyFileDoctorProfileConfig(doctor DoctorProfileConfig, file fileDoctorProfileConfig) DoctorProfileConfig {
	if file.Enabled != nil {
		doctor.Enabled = *file.Enabled
	}
	if len(file.Tools) > 0 {
		doctor.Tools = normalizePreflightToolNames(file.Tools)
	}
	if file.NodeMajor > 0 {
		doctor.NodeMajor = file.NodeMajor
	}
	if file.MinDiskGB > 0 {
		doctor.MinDiskGB = file.MinDiskGB
	}
	if file.RequireDocker != nil {
		doctor.RequireDocker = *file.RequireDocker
	}
	if file.RequireCompose != nil {
		doctor.RequireCompose = *file.RequireCompose
	}
	return doctor
}

func applyFilePresetConfig(preset PresetConfig, file filePresetConfig) PresetConfig {
	if file.Command != "" {
		preset.Command = file.Command
	}
	if file.Shell != nil {
		preset.Shell = *file.Shell
	}
	if len(file.Env) > 0 {
		if preset.Env == nil {
			preset.Env = map[string]string{}
		}
		for key, value := range file.Env {
			key = strings.TrimSpace(key)
			if key != "" {
				preset.Env[key] = value
			}
		}
	}
	if file.Preflight != nil {
		preset.Preflight = *file.Preflight
	}
	if len(file.ArtifactGlobs) > 0 {
		preset.ArtifactGlobs = appendUniqueStrings(nil, file.ArtifactGlobs...)
	}
	if file.ProofTemplate != "" {
		preset.ProofTemplate = file.ProofTemplate
	}
	return preset
}

func applyFileProofTemplateConfig(tmpl ProofTemplateConfig, file fileProofTemplateConfig) ProofTemplateConfig {
	if file.BehaviorAddressed != "" {
		tmpl.BehaviorAddressed = file.BehaviorAddressed
	}
	if file.RealEnvironmentTested != "" {
		tmpl.RealEnvironmentTested = file.RealEnvironmentTested
	}
	if file.ExactSteps != "" {
		tmpl.ExactSteps = file.ExactSteps
	}
	if file.ObservedResult != "" {
		tmpl.ObservedResult = file.ObservedResult
	}
	if file.NotTested != "" {
		tmpl.NotTested = file.NotTested
	}
	return tmpl
}

func applyFileJobConfig(job JobConfig, file fileJobConfig) JobConfig {
	if file.Provider != "" {
		job.Provider = file.Provider
	}
	if file.Target != "" {
		job.Target = file.Target
	}
	if file.TargetOS != "" {
		job.Target = file.TargetOS
	}
	if file.Windows != nil && file.Windows.Mode != "" {
		job.WindowsMode = file.Windows.Mode
	}
	if file.Profile != "" {
		job.Profile = file.Profile
	}
	if file.Class != "" {
		job.Class = file.Class
	}
	if file.Architecture != "" {
		job.Architecture = file.Architecture
	}
	if file.ServerType != "" {
		job.ServerType = file.ServerType
	}
	if file.Type != "" {
		job.ServerType = file.Type
	}
	if file.Capacity != nil && file.Capacity.Market != "" {
		job.Market = file.Capacity.Market
	}
	if file.Market != "" {
		job.Market = file.Market
	}
	applyLeaseDuration(&job.TTL, file.TTL)
	applyLeaseDuration(&job.IdleTimeout, file.IdleTimeout)
	if file.Desktop != nil {
		value := *file.Desktop
		job.Desktop = &value
	}
	if file.DesktopEnv != "" {
		job.DesktopEnv = file.DesktopEnv
	}
	if file.Browser != nil {
		value := *file.Browser
		job.Browser = &value
	}
	if file.Code != nil {
		value := *file.Code
		job.Code = &value
	}
	if file.Network != "" {
		job.Network = file.Network
	}
	if file.Hydrate != nil {
		if file.Hydrate.Actions != nil {
			job.Hydrate.Actions = *file.Hydrate.Actions
		}
		if file.Hydrate.GitHubRunner != nil {
			job.Hydrate.GitHubRunner = *file.Hydrate.GitHubRunner
		}
		if file.Hydrate.WaitTimeout != "" {
			if duration, err := time.ParseDuration(file.Hydrate.WaitTimeout); err == nil {
				job.Hydrate.WaitTimeout = duration
			}
		}
		if file.Hydrate.KeepAliveMinutes > 0 {
			job.Hydrate.KeepAliveMinutes = file.Hydrate.KeepAliveMinutes
		}
	}
	if file.Actions != nil {
		if file.Actions.Repo != "" {
			job.Actions.Repo = file.Actions.Repo
		}
		if file.Actions.Workflow != "" {
			job.Actions.Workflow = file.Actions.Workflow
		}
		if file.Actions.Job != "" {
			job.Actions.Job = file.Actions.Job
		}
		if file.Actions.Ref != "" {
			job.Actions.Ref = file.Actions.Ref
		}
		if len(file.Actions.Fields) > 0 {
			job.Actions.Fields = appendUniqueStrings(nil, file.Actions.Fields...)
		}
	}
	if file.Shell != nil {
		job.Shell = *file.Shell
	}
	if file.Command != "" {
		job.Command = file.Command
	}
	if file.NoSync != nil {
		job.NoSync = *file.NoSync
	}
	if file.SyncOnly != nil {
		job.SyncOnly = *file.SyncOnly
	}
	if file.Checksum != nil {
		value := *file.Checksum
		job.Checksum = &value
	}
	if file.ForceSyncLarge != nil {
		job.ForceSyncLarge = *file.ForceSyncLarge
	}
	if len(file.JUnit) > 0 {
		job.JUnit = appendUniqueStrings(nil, file.JUnit...)
	}
	if file.Label != "" {
		job.Label = file.Label
	}
	if len(file.ArtifactGlobs) > 0 {
		job.ArtifactGlobs = appendUniqueStrings(nil, file.ArtifactGlobs...)
	}
	if len(file.RequiredArtifacts) > 0 {
		job.RequiredArtifacts = appendUniqueStrings(nil, file.RequiredArtifacts...)
	}
	if len(file.Downloads) > 0 {
		job.Downloads = appendUniqueStrings(nil, file.Downloads...)
	}
	if file.Stop != "" {
		job.Stop = file.Stop
	}
	return job
}

func applyLeaseDuration(target *time.Duration, value string) {
	if value == "" {
		return
	}
	if parsed, err := time.ParseDuration(value); err == nil && parsed > 0 {
		*target = parsed
	}
}

func applyEnv(cfg *Config) error {
	cfg.Profile = getenv("CRABBOX_PROFILE", cfg.Profile)
	if provider := os.Getenv("CRABBOX_PROVIDER"); provider != "" {
		cfg.Provider = provider
		cfg.brokerProvider = ""
	}
	if t := os.Getenv("CRABBOX_TARGET"); t != "" {
		cfg.TargetOS = t
		cfg.targetExplicit = true
	} else if t := os.Getenv("CRABBOX_TARGET_OS"); t != "" {
		cfg.TargetOS = t
		cfg.targetExplicit = true
	}
	if arch := os.Getenv("CRABBOX_ARCH"); arch != "" {
		cfg.Architecture = arch
		cfg.architectureExplicit = true
	}
	if osImage := os.Getenv("CRABBOX_OS"); osImage != "" {
		cfg.OSImage = osImage
		cfg.osImageExplicit = true
		if normalized, err := normalizeOSImage(osImage); err == nil {
			cfg.OSImage = normalized
			applyOSImageProviderDefaults(cfg, false)
		}
	}
	if windowsMode := os.Getenv("CRABBOX_WINDOWS_MODE"); windowsMode != "" {
		cfg.WindowsMode = windowsMode
		cfg.explicitWindowsMode = windowsMode
	}
	if value, ok := getenvBool("CRABBOX_DESKTOP"); ok {
		cfg.Desktop = value
	}
	cfg.DesktopEnv = getenv("CRABBOX_DESKTOP_ENV", cfg.DesktopEnv)
	if value, ok := getenvBool("CRABBOX_BROWSER"); ok {
		cfg.Browser = value
	}
	if value, ok := getenvBool("CRABBOX_CODE"); ok {
		cfg.Code = value
	}
	if network := os.Getenv("CRABBOX_NETWORK"); network != "" {
		cfg.Network = NetworkMode(strings.ToLower(strings.TrimSpace(network)))
	}
	if value := os.Getenv("CRABBOX_DEFAULT_CLASS"); value != "" {
		cfg.Class = value
		MarkClassExplicit(cfg)
	}
	if os.Getenv("CRABBOX_SERVER_TYPE") != "" {
		cfg.ServerTypeExplicit = true
	}
	cfg.ServerType = getenv("CRABBOX_SERVER_TYPE", cfg.ServerType)
	if value := os.Getenv("CRABBOX_COORDINATOR"); value != "" {
		cfg.Coordinator = value
		cfg.credentialProvenance.coordinator = credentialSourceEnvironment
	}
	cfg.BrokerMode = BrokerMode(getenv("CRABBOX_COORDINATOR_MODE", string(cfg.BrokerMode)))
	if value, ok := getenvBool("CRABBOX_COORDINATOR_AUTO_WEBVNC"); ok {
		cfg.BrokerAutoWebVNC = value
	}
	if value := os.Getenv("CRABBOX_COORDINATOR_TOKEN"); value != "" {
		cfg.CoordToken = value
		cfg.credentialProvenance.coordToken = credentialSourceEnvironment
	}
	if raw := strings.TrimSpace(os.Getenv("CRABBOX_COORDINATOR_TOKEN_COMMAND")); raw != "" {
		var command []string
		if err := json.Unmarshal([]byte(raw), &command); err != nil {
			return fmt.Errorf("CRABBOX_COORDINATOR_TOKEN_COMMAND must be a JSON argv array: %w", err)
		}
		if len(command) == 0 {
			return errors.New("CRABBOX_COORDINATOR_TOKEN_COMMAND must contain an executable")
		}
		for _, arg := range command {
			if strings.TrimSpace(arg) == "" || strings.ContainsAny(arg, "\r\n\x00") {
				return errors.New("CRABBOX_COORDINATOR_TOKEN_COMMAND contains an invalid argv entry")
			}
		}
		cfg.CoordTokenCommand = append([]string(nil), command...)
		cfg.credentialProvenance.coordTokenCommand = credentialSourceEnvironment
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_COORDINATOR_ADMIN_TOKEN", "CRABBOX_ADMIN_TOKEN"); ok {
		cfg.CoordAdminToken = value
		cfg.credentialProvenance.coordAdminToken = credentialSourceEnvironment
	}
	cfg.HostID = getenv("CRABBOX_HOST_ID", cfg.HostID)
	if value, ok := firstNonEmptyEnv("CRABBOX_ACCESS_CLIENT_ID", "CF_ACCESS_CLIENT_ID"); ok {
		cfg.Access.ClientID = value
		cfg.credentialProvenance.accessClientID = credentialSourceEnvironment
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_ACCESS_CLIENT_SECRET", "CF_ACCESS_CLIENT_SECRET"); ok {
		cfg.Access.ClientSecret = value
		cfg.credentialProvenance.accessClientSecret = credentialSourceEnvironment
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_ACCESS_TOKEN", "CF_ACCESS_TOKEN"); ok {
		cfg.Access.Token = value
		cfg.credentialProvenance.accessToken = credentialSourceEnvironment
	}
	if location := os.Getenv("CRABBOX_HETZNER_LOCATION"); location != "" {
		cfg.Location = location
		cfg.locationExplicit = true
	}
	if image := os.Getenv("CRABBOX_HETZNER_IMAGE"); image != "" {
		cfg.Image = image
		cfg.imageExplicit = true
	}
	cfg.AWSRegion = getenv("CRABBOX_AWS_REGION", getenv("AWS_REGION", cfg.AWSRegion))
	cfg.AWSAMI = getenv("CRABBOX_AWS_AMI", cfg.AWSAMI)
	cfg.AWSSGID = getenv("CRABBOX_AWS_SECURITY_GROUP_ID", cfg.AWSSGID)
	cfg.AWSSubnetID = getenv("CRABBOX_AWS_SUBNET_ID", cfg.AWSSubnetID)
	cfg.AWSProfile = getenv("CRABBOX_AWS_INSTANCE_PROFILE", cfg.AWSProfile)
	cfg.AWSRootGB = getenvInt32("CRABBOX_AWS_ROOT_GB", cfg.AWSRootGB)
	cfg.AWSMacHostID = getenv("CRABBOX_AWS_MAC_HOST_ID", cfg.AWSMacHostID)
	if cfg.HostID == "" && cfg.AWSMacHostID != "" {
		cfg.HostID = cfg.AWSMacHostID
	}
	if cfg.AWSMacHostID == "" && cfg.Provider == "aws" && cfg.TargetOS == targetMacOS {
		cfg.AWSMacHostID = cfg.HostID
	}
	if cidrs := os.Getenv("CRABBOX_AWS_SSH_CIDRS"); cidrs != "" {
		cfg.AWSSSHCIDRs = splitCommaList(cidrs)
	}
	cfg.AzureSubscription = getenv("CRABBOX_AZURE_SUBSCRIPTION_ID", getenv("AZURE_SUBSCRIPTION_ID", cfg.AzureSubscription))
	cfg.AzureTenant = getenv("CRABBOX_AZURE_TENANT_ID", getenv("AZURE_TENANT_ID", cfg.AzureTenant))
	cfg.AzureClientID = getenv("CRABBOX_AZURE_CLIENT_ID", getenv("AZURE_CLIENT_ID", cfg.AzureClientID))
	cfg.AzureBackend = getenv("CRABBOX_AZURE_BACKEND", cfg.AzureBackend)
	cfg.AzureLocation = getenv("CRABBOX_AZURE_LOCATION", cfg.AzureLocation)
	cfg.AzureResourceGroup = getenv("CRABBOX_AZURE_RESOURCE_GROUP", cfg.AzureResourceGroup)
	if image := os.Getenv("CRABBOX_AZURE_IMAGE"); image != "" {
		cfg.AzureImage = image
		cfg.azureImageExplicit = true
	}
	if value := os.Getenv("CRABBOX_AZURE_OS_DISK"); value != "" {
		cfg.AzureOSDisk = value
		cfg.AzureOSDiskExplicit = true
	}
	cfg.AzureVNet = getenv("CRABBOX_AZURE_VNET", cfg.AzureVNet)
	cfg.AzureSubnet = getenv("CRABBOX_AZURE_SUBNET", cfg.AzureSubnet)
	cfg.AzureNSG = getenv("CRABBOX_AZURE_NSG", cfg.AzureNSG)
	if cidrs := os.Getenv("CRABBOX_AZURE_SSH_CIDRS"); cidrs != "" {
		cfg.AzureSSHCIDRs = splitCommaList(cidrs)
	}
	cfg.AzureNetwork = getenv("CRABBOX_AZURE_NETWORK", cfg.AzureNetwork)
	if value := os.Getenv("CRABBOX_AZURE_DYNAMIC_SESSIONS_ENDPOINT"); value != "" {
		cfg.AzureDynamicSessions.Endpoint = value
		cfg.credentialProvenance.azSessionsEndpoint = credentialSourceEnvironment
	}
	cfg.AzureDynamicSessions.Pool = getenv("CRABBOX_AZURE_DYNAMIC_SESSIONS_POOL", cfg.AzureDynamicSessions.Pool)
	cfg.AzureDynamicSessions.APIVersion = getenv("CRABBOX_AZURE_DYNAMIC_SESSIONS_API_VERSION", cfg.AzureDynamicSessions.APIVersion)
	cfg.AzureDynamicSessions.Workdir = getenv("CRABBOX_AZURE_DYNAMIC_SESSIONS_WORKDIR", cfg.AzureDynamicSessions.Workdir)
	cfg.AzureDynamicSessions.TimeoutSecs = getenvInt("CRABBOX_AZURE_DYNAMIC_SESSIONS_TIMEOUT_SECS", cfg.AzureDynamicSessions.TimeoutSecs)
	if project := os.Getenv("CRABBOX_GCP_PROJECT"); project != "" {
		cfg.GCPProject = project
		cfg.gcpProjectExplicit = true
	} else if cfg.GCPProject == "" {
		if project := os.Getenv("GOOGLE_CLOUD_PROJECT"); project != "" {
			cfg.GCPProject = project
			cfg.gcpProjectExplicit = false
		} else if project := os.Getenv("GCP_PROJECT_ID"); project != "" {
			cfg.GCPProject = project
			cfg.gcpProjectExplicit = false
		}
	}
	if zone := os.Getenv("CRABBOX_GCP_ZONE"); zone != "" {
		cfg.GCPZone = zone
		cfg.gcpZoneExplicit = true
	}
	if image := os.Getenv("CRABBOX_GCP_IMAGE"); image != "" {
		cfg.GCPImage = image
		cfg.gcpImageExplicit = true
	}
	if network := os.Getenv("CRABBOX_GCP_NETWORK"); network != "" {
		cfg.GCPNetwork = network
		cfg.gcpNetworkExplicit = true
	}
	cfg.GCPSubnet = getenv("CRABBOX_GCP_SUBNET", cfg.GCPSubnet)
	if rootGB := os.Getenv("CRABBOX_GCP_ROOT_GB"); rootGB != "" {
		cfg.GCPRootGB = int64(getenvInt("CRABBOX_GCP_ROOT_GB", int(cfg.GCPRootGB)))
		cfg.gcpRootGBExplicit = true
	}
	cfg.GCPServiceAccount = getenv("CRABBOX_GCP_SERVICE_ACCOUNT", cfg.GCPServiceAccount)
	cfg.Incus.Remote = getenv("CRABBOX_INCUS_REMOTE", cfg.Incus.Remote)
	cfg.Incus.Project = getenv("CRABBOX_INCUS_PROJECT", cfg.Incus.Project)
	cfg.Incus.Address = getenv("CRABBOX_INCUS_ADDRESS", cfg.Incus.Address)
	cfg.Incus.Socket = expandUserPath(getenv("CRABBOX_INCUS_SOCKET", cfg.Incus.Socket))
	cfg.Incus.InstanceType = getenv("CRABBOX_INCUS_INSTANCE_TYPE", cfg.Incus.InstanceType)
	cfg.Incus.Image = getenv("CRABBOX_INCUS_IMAGE", cfg.Incus.Image)
	cfg.Incus.Profile = getenv("CRABBOX_INCUS_PROFILE", cfg.Incus.Profile)
	cfg.Incus.User = getenv("CRABBOX_INCUS_USER", cfg.Incus.User)
	cfg.Incus.WorkRoot = getenv("CRABBOX_INCUS_WORK_ROOT", cfg.Incus.WorkRoot)
	if value, ok := getenvBool("CRABBOX_INCUS_DELETE_ON_RELEASE"); ok {
		cfg.Incus.DeleteOnRelease = value
		MarkDeleteOnReleaseExplicit(cfg, "incus")
	}
	if timeout := os.Getenv("CRABBOX_INCUS_START_TIMEOUT"); timeout != "" {
		applyLeaseDuration(&cfg.Incus.StartTimeout, timeout)
	}
	cfg.Incus.LaunchPort = getenv("CRABBOX_INCUS_LAUNCH_PORT", cfg.Incus.LaunchPort)
	cfg.Incus.ProxyListenHost = getenv("CRABBOX_INCUS_PROXY_LISTEN_HOST", cfg.Incus.ProxyListenHost)
	cfg.Incus.ProxyListenPort = getenv("CRABBOX_INCUS_PROXY_LISTEN_PORT", cfg.Incus.ProxyListenPort)
	cfg.Incus.ProxyDevice = getenv("CRABBOX_INCUS_PROXY_DEVICE", cfg.Incus.ProxyDevice)
	cfg.Incus.TLSServerCert = expandUserPath(getenv("CRABBOX_INCUS_TLS_SERVER_CERT", cfg.Incus.TLSServerCert))
	if value, ok := getenvBool("CRABBOX_INCUS_INSECURE_TLS"); ok {
		cfg.Incus.InsecureTLS = value
	}
	cfg.Incus.RemoteImageServer = getenv("CRABBOX_INCUS_REMOTE_IMAGE_SERVER", cfg.Incus.RemoteImageServer)
	if tags := os.Getenv("CRABBOX_GCP_TAGS"); tags != "" {
		cfg.GCPTags = splitCommaList(tags)
		cfg.gcpTagsExplicit = true
	}
	if cidrs := os.Getenv("CRABBOX_GCP_SSH_CIDRS"); cidrs != "" {
		cfg.GCPSSHCIDRs = splitCommaList(cidrs)
	}
	cfg.DigitalOcean.Region = getenv("CRABBOX_DIGITALOCEAN_REGION", cfg.DigitalOcean.Region)
	if image := os.Getenv("CRABBOX_DIGITALOCEAN_IMAGE"); image != "" {
		cfg.DigitalOcean.Image = image
		cfg.digitalOceanImageExplicit = true
	}
	cfg.DigitalOcean.VPCUUID = getenv("CRABBOX_DIGITALOCEAN_VPC", cfg.DigitalOcean.VPCUUID)
	if cidrs := os.Getenv("CRABBOX_DIGITALOCEAN_SSH_CIDRS"); cidrs != "" {
		cfg.DigitalOcean.SSHCIDRs = splitCommaList(cidrs)
	}
	cfg.Linode.Region = getenv("CRABBOX_LINODE_REGION", cfg.Linode.Region)
	if image := os.Getenv("CRABBOX_LINODE_IMAGE"); image != "" {
		cfg.Linode.Image = image
		cfg.linodeImageExplicit = true
	}
	if linodeType := os.Getenv("CRABBOX_LINODE_TYPE"); linodeType != "" {
		cfg.Linode.Type = linodeType
		cfg.linodeTypeExplicit = true
	}
	cfg.Linode.FirewallID = getenv("CRABBOX_LINODE_FIREWALL", cfg.Linode.FirewallID)
	if cidrs := os.Getenv("CRABBOX_LINODE_SSH_CIDRS"); cidrs != "" {
		cfg.Linode.SSHCIDRs = splitCommaList(cidrs)
	}
	cfg.OVH.Endpoint = getenv("OVH_ENDPOINT", cfg.OVH.Endpoint)
	cfg.OVH.ProjectID = getenv("CRABBOX_OVH_PROJECT_ID", cfg.OVH.ProjectID)
	cfg.OVH.Region = getenv("CRABBOX_OVH_REGION", cfg.OVH.Region)
	if image := os.Getenv("CRABBOX_OVH_IMAGE"); image != "" {
		cfg.OVH.Image = image
		cfg.ovhImageExplicit = true
	}
	cfg.OVH.Flavor = getenv("CRABBOX_OVH_FLAVOR", cfg.OVH.Flavor)
	if region := os.Getenv("CRABBOX_SCALEWAY_REGION"); region != "" {
		cfg.Scaleway.Region = region
		cfg.scalewayRegionExplicit = true
	}
	if zone := os.Getenv("CRABBOX_SCALEWAY_ZONE"); zone != "" {
		cfg.Scaleway.Zone = zone
		cfg.scalewayZoneExplicit = true
	}
	if image := os.Getenv("CRABBOX_SCALEWAY_IMAGE"); image != "" {
		cfg.Scaleway.Image = image
		cfg.scalewayImageExplicit = true
	}
	if serverType := os.Getenv("CRABBOX_SCALEWAY_TYPE"); serverType != "" {
		cfg.Scaleway.Type = serverType
		cfg.scalewayTypeExplicit = true
	}
	cfg.Scaleway.ProjectID = getenv("CRABBOX_SCALEWAY_PROJECT_ID", cfg.Scaleway.ProjectID)
	cfg.Scaleway.OrganizationID = getenv("CRABBOX_SCALEWAY_ORGANIZATION_ID", cfg.Scaleway.OrganizationID)
	cfg.Scaleway.SecurityGroup = getenv("CRABBOX_SCALEWAY_SECURITY_GROUP", cfg.Scaleway.SecurityGroup)
	if cidrs := os.Getenv("CRABBOX_SCALEWAY_SSH_CIDRS"); cidrs != "" {
		cfg.Scaleway.SSHCIDRs = splitCommaList(cidrs)
	}
	if value := os.Getenv("CRABBOX_PROXMOX_API_URL"); value != "" {
		cfg.Proxmox.APIURL = value
		cfg.credentialProvenance.proxmoxAPIURL = credentialSourceEnvironment
	}
	if value := os.Getenv("CRABBOX_PROXMOX_TOKEN_ID"); value != "" {
		cfg.Proxmox.TokenID = value
		cfg.credentialProvenance.proxmoxTokenID = credentialSourceEnvironment
	}
	if value := os.Getenv("CRABBOX_PROXMOX_TOKEN_SECRET"); value != "" {
		cfg.Proxmox.TokenSecret = value
		cfg.credentialProvenance.proxmoxTokenSecret = credentialSourceEnvironment
	}
	cfg.Proxmox.Node = getenv("CRABBOX_PROXMOX_NODE", cfg.Proxmox.Node)
	cfg.Proxmox.TemplateID = getenvInt("CRABBOX_PROXMOX_TEMPLATE_ID", cfg.Proxmox.TemplateID)
	cfg.Proxmox.Storage = getenv("CRABBOX_PROXMOX_STORAGE", cfg.Proxmox.Storage)
	cfg.Proxmox.Pool = getenv("CRABBOX_PROXMOX_POOL", cfg.Proxmox.Pool)
	cfg.Proxmox.Bridge = getenv("CRABBOX_PROXMOX_BRIDGE", cfg.Proxmox.Bridge)
	cfg.Proxmox.User = getenv("CRABBOX_PROXMOX_USER", cfg.Proxmox.User)
	cfg.Proxmox.WorkRoot = getenv("CRABBOX_PROXMOX_WORK_ROOT", cfg.Proxmox.WorkRoot)
	if value, ok := getenvBool("CRABBOX_PROXMOX_FULL_CLONE"); ok {
		cfg.Proxmox.FullClone = value
	}
	if value, ok := getenvBool("CRABBOX_PROXMOX_INSECURE_TLS"); ok {
		cfg.Proxmox.InsecureTLS = value
		cfg.credentialProvenance.proxmoxInsecureTLS = credentialSourceEnvironment
	}
	cfg.XCPNg.APIURL = getenv("CRABBOX_XCP_NG_API_URL", cfg.XCPNg.APIURL)
	cfg.XCPNg.Username = getenv("CRABBOX_XCP_NG_USERNAME", cfg.XCPNg.Username)
	cfg.XCPNg.Password = getenv("CRABBOX_XCP_NG_PASSWORD", cfg.XCPNg.Password)
	xcpNgTemplate, xcpNgTemplateUUID := os.Getenv("CRABBOX_XCP_NG_TEMPLATE"), os.Getenv("CRABBOX_XCP_NG_TEMPLATE_UUID")
	if xcpNgTemplate != "" {
		cfg.XCPNg.Template = xcpNgTemplate
		if xcpNgTemplateUUID == "" {
			cfg.XCPNg.TemplateUUID = ""
		}
	}
	if xcpNgTemplateUUID != "" {
		cfg.XCPNg.TemplateUUID = xcpNgTemplateUUID
		if xcpNgTemplate == "" {
			cfg.XCPNg.Template = ""
		}
	}
	xcpNgSR, xcpNgSRUUID := os.Getenv("CRABBOX_XCP_NG_SR"), os.Getenv("CRABBOX_XCP_NG_SR_UUID")
	if xcpNgSR != "" {
		cfg.XCPNg.SR = xcpNgSR
		if xcpNgSRUUID == "" {
			cfg.XCPNg.SRUUID = ""
		}
	}
	if xcpNgSRUUID != "" {
		cfg.XCPNg.SRUUID = xcpNgSRUUID
		if xcpNgSR == "" {
			cfg.XCPNg.SR = ""
		}
	}
	xcpNgNetwork, xcpNgNetworkUUID := os.Getenv("CRABBOX_XCP_NG_NETWORK"), os.Getenv("CRABBOX_XCP_NG_NETWORK_UUID")
	if xcpNgNetwork != "" {
		cfg.XCPNg.Network = xcpNgNetwork
		if xcpNgNetworkUUID == "" {
			cfg.XCPNg.NetworkUUID = ""
		}
	}
	if xcpNgNetworkUUID != "" {
		cfg.XCPNg.NetworkUUID = xcpNgNetworkUUID
		if xcpNgNetwork == "" {
			cfg.XCPNg.Network = ""
		}
	}
	cfg.XCPNg.Host = getenv("CRABBOX_XCP_NG_HOST", cfg.XCPNg.Host)
	cfg.XCPNg.User = getenv("CRABBOX_XCP_NG_USER", cfg.XCPNg.User)
	cfg.XCPNg.WorkRoot = getenv("CRABBOX_XCP_NG_WORK_ROOT", cfg.XCPNg.WorkRoot)
	if value, ok := getenvBool("CRABBOX_XCP_NG_INSECURE_TLS"); ok {
		cfg.XCPNg.InsecureTLS = value
	}
	cfg.Parallels.Source = getenv("CRABBOX_PARALLELS_SOURCE", cfg.Parallels.Source)
	cfg.Parallels.SourceID = getenv("CRABBOX_PARALLELS_SOURCE_ID", cfg.Parallels.SourceID)
	cfg.Parallels.SourceSnapshot = getenv("CRABBOX_PARALLELS_SOURCE_SNAPSHOT", cfg.Parallels.SourceSnapshot)
	cfg.Parallels.SourceSnapshotID = getenv("CRABBOX_PARALLELS_SOURCE_SNAPSHOT_ID", cfg.Parallels.SourceSnapshotID)
	cfg.Parallels.Template = getenv("CRABBOX_PARALLELS_TEMPLATE", cfg.Parallels.Template)
	cfg.Parallels.CloneMode = getenv("CRABBOX_PARALLELS_CLONE_MODE", cfg.Parallels.CloneMode)
	cfg.Parallels.Host = getenv("CRABBOX_PARALLELS_HOST", cfg.Parallels.Host)
	cfg.Parallels.HostUser = getenv("CRABBOX_PARALLELS_HOST_USER", cfg.Parallels.HostUser)
	cfg.Parallels.HostKey = expandUserPath(getenv("CRABBOX_PARALLELS_HOST_KEY", cfg.Parallels.HostKey))
	cfg.Parallels.VMRoot = expandUserPath(getenv("CRABBOX_PARALLELS_VM_ROOT", cfg.Parallels.VMRoot))
	cfg.Parallels.User = getenv("CRABBOX_PARALLELS_USER", cfg.Parallels.User)
	cfg.Parallels.WorkRoot = getenv("CRABBOX_PARALLELS_WORK_ROOT", cfg.Parallels.WorkRoot)
	if startupTimeout := os.Getenv("CRABBOX_PARALLELS_STARTUP_TIMEOUT"); startupTimeout != "" {
		applyLeaseDuration(&cfg.Parallels.StartupTimeout, startupTimeout)
	}
	if sshUser := os.Getenv("CRABBOX_SSH_USER"); sshUser != "" {
		cfg.SSHUser = sshUser
		MarkSSHUserExplicit(cfg)
	}
	if sshKey := os.Getenv("CRABBOX_SSH_KEY"); sshKey != "" {
		cfg.SSHKey = sshKey
		MarkSSHKeyExplicit(cfg)
	}
	if sshPort := os.Getenv("CRABBOX_SSH_PORT"); sshPort != "" {
		cfg.SSHPort = sshPort
		MarkSSHPortExplicit(cfg)
	}
	if ports, ok := getenvList("CRABBOX_SSH_FALLBACK_PORTS"); ok {
		cfg.SSHFallbackPorts = ports
		cfg.sshFallbackPortsExplicit = true
		cfg.explicitSSHFallbackPorts = append([]string(nil), ports...)
	}
	cfg.ProviderKey = getenv("CRABBOX_HETZNER_SSH_KEY", cfg.ProviderKey)
	if workRoot := os.Getenv("CRABBOX_WORK_ROOT"); workRoot != "" {
		cfg.WorkRoot = workRoot
		cfg.explicitWorkRoot = workRoot
	}
	if ttl := os.Getenv("CRABBOX_TTL"); ttl != "" {
		applyLeaseDuration(&cfg.TTL, ttl)
	}
	if idleTimeout := os.Getenv("CRABBOX_IDLE_TIMEOUT"); idleTimeout != "" {
		applyLeaseDuration(&cfg.IdleTimeout, idleTimeout)
	}
	cfg.Capacity.Market = getenv("CRABBOX_CAPACITY_MARKET", cfg.Capacity.Market)
	cfg.Capacity.Strategy = getenv("CRABBOX_CAPACITY_STRATEGY", cfg.Capacity.Strategy)
	cfg.Capacity.Fallback = getenv("CRABBOX_CAPACITY_FALLBACK", cfg.Capacity.Fallback)
	if value, ok := getenvBool("CRABBOX_CAPACITY_HINTS"); ok {
		cfg.Capacity.Hints = value
	}
	cfg.Actions.Workflow = getenv("CRABBOX_ACTIONS_WORKFLOW", cfg.Actions.Workflow)
	cfg.Actions.Job = getenv("CRABBOX_ACTIONS_JOB", cfg.Actions.Job)
	cfg.Actions.Ref = getenv("CRABBOX_ACTIONS_REF", cfg.Actions.Ref)
	cfg.Actions.Repo = getenv("CRABBOX_ACTIONS_REPO", cfg.Actions.Repo)
	cfg.Actions.RunnerVersion = getenv("CRABBOX_ACTIONS_RUNNER_VERSION", cfg.Actions.RunnerVersion)
	cfg.Blacksmith.Org = getenv("CRABBOX_BLACKSMITH_ORG", cfg.Blacksmith.Org)
	cfg.Blacksmith.Workflow = getenv("CRABBOX_BLACKSMITH_WORKFLOW", cfg.Blacksmith.Workflow)
	cfg.Blacksmith.Job = getenv("CRABBOX_BLACKSMITH_JOB", cfg.Blacksmith.Job)
	cfg.Blacksmith.Ref = getenv("CRABBOX_BLACKSMITH_REF", cfg.Blacksmith.Ref)
	cfg.KubeVirt.Kubectl = expandUserPath(getenv("CRABBOX_KUBEVIRT_KUBECTL", cfg.KubeVirt.Kubectl))
	cfg.KubeVirt.Virtctl = expandUserPath(getenv("CRABBOX_KUBEVIRT_VIRTCTL", cfg.KubeVirt.Virtctl))
	cfg.KubeVirt.Kubeconfig = expandUserPath(getenv("CRABBOX_KUBEVIRT_KUBECONFIG", cfg.KubeVirt.Kubeconfig))
	cfg.KubeVirt.Context = getenv("CRABBOX_KUBEVIRT_CONTEXT", cfg.KubeVirt.Context)
	cfg.KubeVirt.Namespace = getenv("CRABBOX_KUBEVIRT_NAMESPACE", cfg.KubeVirt.Namespace)
	cfg.KubeVirt.Template = expandUserPath(getenv("CRABBOX_KUBEVIRT_TEMPLATE", cfg.KubeVirt.Template))
	cfg.KubeVirt.SSHUser = getenv("CRABBOX_KUBEVIRT_SSH_USER", cfg.KubeVirt.SSHUser)
	cfg.KubeVirt.SSHKey = expandUserPath(getenv("CRABBOX_KUBEVIRT_SSH_KEY", cfg.KubeVirt.SSHKey))
	cfg.KubeVirt.SSHPublicKey = expandUserPath(getenv("CRABBOX_KUBEVIRT_SSH_PUBLIC_KEY", cfg.KubeVirt.SSHPublicKey))
	cfg.KubeVirt.SSHPort = getenv("CRABBOX_KUBEVIRT_SSH_PORT", cfg.KubeVirt.SSHPort)
	cfg.KubeVirt.WorkRoot = getenv("CRABBOX_KUBEVIRT_WORK_ROOT", cfg.KubeVirt.WorkRoot)
	if value, ok := getenvBool("CRABBOX_KUBEVIRT_DELETE_ON_RELEASE"); ok {
		cfg.KubeVirt.DeleteOnRelease = value
		MarkDeleteOnReleaseExplicit(cfg, "kubevirt")
	}
	cfg.AgentSandbox.Kubectl = getenv("CRABBOX_AGENT_SANDBOX_KUBECTL", cfg.AgentSandbox.Kubectl)
	cfg.AgentSandbox.Kubeconfig = expandUserPath(getenv("CRABBOX_AGENT_SANDBOX_KUBECONFIG", cfg.AgentSandbox.Kubeconfig))
	cfg.AgentSandbox.Context = getenv("CRABBOX_AGENT_SANDBOX_CONTEXT", cfg.AgentSandbox.Context)
	cfg.AgentSandbox.Namespace = getenv("CRABBOX_AGENT_SANDBOX_NAMESPACE", cfg.AgentSandbox.Namespace)
	cfg.AgentSandbox.WarmPool = getenv("CRABBOX_AGENT_SANDBOX_WARM_POOL", cfg.AgentSandbox.WarmPool)
	cfg.AgentSandbox.Container = getenv("CRABBOX_AGENT_SANDBOX_CONTAINER", cfg.AgentSandbox.Container)
	cfg.AgentSandbox.Workdir = getenv("CRABBOX_AGENT_SANDBOX_WORKDIR", cfg.AgentSandbox.Workdir)
	if timeout := os.Getenv("CRABBOX_AGENT_SANDBOX_SANDBOX_READY_TIMEOUT"); timeout != "" {
		applyLeaseDuration(&cfg.AgentSandbox.SandboxReadyTimeout, timeout)
	}
	if timeout := os.Getenv("CRABBOX_AGENT_SANDBOX_POD_READY_TIMEOUT"); timeout != "" {
		applyLeaseDuration(&cfg.AgentSandbox.PodReadyTimeout, timeout)
	}
	var agentSandboxEnvErr error
	cfg.AgentSandbox.ExecTimeoutSecs, agentSandboxEnvErr = getenvNonNegativeInt("CRABBOX_AGENT_SANDBOX_EXEC_TIMEOUT_SECS", cfg.AgentSandbox.ExecTimeoutSecs)
	if agentSandboxEnvErr != nil {
		return agentSandboxEnvErr
	}
	if value, ok := getenvBool("CRABBOX_AGENT_SANDBOX_DELETE_ON_RELEASE"); ok {
		cfg.AgentSandbox.DeleteOnRelease = value
		MarkDeleteOnReleaseExplicit(cfg, "agent-sandbox")
	}
	if value, ok := getenvBool("CRABBOX_AGENT_SANDBOX_FORGET_MISSING"); ok {
		cfg.AgentSandbox.ForgetMissing = value
	}
	cfg.External.Command = getenv("CRABBOX_EXTERNAL_COMMAND", cfg.External.Command)
	if arg := os.Getenv("CRABBOX_EXTERNAL_ARG"); arg != "" {
		cfg.External.Args = []string{arg}
	}
	cfg.External.WorkRoot = getenv("CRABBOX_EXTERNAL_WORK_ROOT", cfg.External.WorkRoot)
	cfg.External.RoutingFile = getenv("CRABBOX_EXTERNAL_ROUTING_FILE", cfg.External.RoutingFile)
	if value, ok := getenvBool("CRABBOX_EXTERNAL_IDEMPOTENT_LEASE_ID"); ok {
		cfg.External.Capabilities.IdempotentLeaseID = value
	}
	cfg.Namespace.Image = getenv("CRABBOX_NAMESPACE_IMAGE", cfg.Namespace.Image)
	cfg.Namespace.Size = getenv("CRABBOX_NAMESPACE_SIZE", cfg.Namespace.Size)
	cfg.Namespace.Repository = getenv("CRABBOX_NAMESPACE_REPOSITORY", cfg.Namespace.Repository)
	cfg.Namespace.Site = getenv("CRABBOX_NAMESPACE_SITE", cfg.Namespace.Site)
	cfg.Namespace.VolumeSizeGB = getenvInt("CRABBOX_NAMESPACE_VOLUME_SIZE_GB", cfg.Namespace.VolumeSizeGB)
	if idleTimeout := os.Getenv("CRABBOX_NAMESPACE_AUTO_STOP_IDLE_TIMEOUT"); idleTimeout != "" {
		applyLeaseDuration(&cfg.Namespace.AutoStopIdleTimeout, idleTimeout)
	}
	cfg.Namespace.WorkRoot = getenv("CRABBOX_NAMESPACE_WORK_ROOT", cfg.Namespace.WorkRoot)
	if value, ok := getenvBool("CRABBOX_NAMESPACE_DELETE_ON_RELEASE"); ok {
		cfg.Namespace.DeleteOnRelease = value
		MarkDeleteOnReleaseExplicit(cfg, "namespace-devbox")
	}
	cfg.NamespaceInstance.CLIPath = expandUserPath(getenv("CRABBOX_NAMESPACE_INSTANCE_CLI", cfg.NamespaceInstance.CLIPath))
	cfg.NamespaceInstance.MachineType = getenv("CRABBOX_NAMESPACE_INSTANCE_MACHINE_TYPE", cfg.NamespaceInstance.MachineType)
	if duration := os.Getenv("CRABBOX_NAMESPACE_INSTANCE_DURATION"); duration != "" {
		applyLeaseDuration(&cfg.NamespaceInstance.Duration, duration)
	}
	cfg.NamespaceInstance.Region = getenv("CRABBOX_NAMESPACE_INSTANCE_REGION", cfg.NamespaceInstance.Region)
	cfg.NamespaceInstance.Endpoint = getenv("CRABBOX_NAMESPACE_INSTANCE_ENDPOINT", cfg.NamespaceInstance.Endpoint)
	cfg.NamespaceInstance.Keychain = getenv("CRABBOX_NAMESPACE_INSTANCE_KEYCHAIN", cfg.NamespaceInstance.Keychain)
	if volumes, ok := getenvList("CRABBOX_NAMESPACE_INSTANCE_VOLUMES"); ok {
		cfg.NamespaceInstance.Volumes = volumes
	}
	cfg.NamespaceInstance.WorkRoot = getenv("CRABBOX_NAMESPACE_INSTANCE_WORK_ROOT", cfg.NamespaceInstance.WorkRoot)
	if value, ok := getenvBool("CRABBOX_NAMESPACE_INSTANCE_BARE"); ok {
		cfg.NamespaceInstance.Bare = value
	}
	cfg.Phala.CLIPath = expandUserPath(getenv("CRABBOX_PHALA_CLI", cfg.Phala.CLIPath))
	if value := os.Getenv("CRABBOX_PHALA_INSTANCE_TYPE"); value != "" {
		cfg.Phala.InstanceType = value
		MarkPhalaInstanceTypeExplicit(cfg)
	}
	cfg.Phala.WorkRoot = getenv("CRABBOX_PHALA_WORK_ROOT", cfg.Phala.WorkRoot)
	cfg.Phala.NodeID = getenv("CRABBOX_PHALA_NODE_ID", cfg.Phala.NodeID)
	cfg.Phala.Compose = expandUserPath(getenv("CRABBOX_PHALA_COMPOSE", cfg.Phala.Compose))
	if value, ok := getenvBool("CRABBOX_PHALA_ATTEST"); ok {
		cfg.Phala.Attest = &value
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_MORPH_API_KEY", "MORPH_API_KEY"); ok {
		cfg.Morph.APIKey = value
		cfg.credentialProvenance.morphAPIKey = credentialSourceEnvironment
	}
	if value := os.Getenv("CRABBOX_MORPH_API_URL"); value != "" {
		cfg.Morph.APIURL = value
		cfg.credentialProvenance.morphAPIURL = credentialSourceEnvironment
	}
	cfg.Morph.Snapshot = getenv("CRABBOX_MORPH_SNAPSHOT", cfg.Morph.Snapshot)
	if value := os.Getenv("CRABBOX_MORPH_SSH_GATEWAY_HOST"); value != "" {
		cfg.Morph.SSHGatewayHost = value
		cfg.credentialProvenance.morphSSHGatewayHost = credentialSourceEnvironment
	}
	cfg.Morph.WorkRoot = getenv("CRABBOX_MORPH_WORK_ROOT", cfg.Morph.WorkRoot)
	if value, ok := getenvBool("CRABBOX_MORPH_DELETE_ON_RELEASE"); ok {
		cfg.Morph.DeleteOnRelease = value
		MarkDeleteOnReleaseExplicit(cfg, "morph")
	}
	if value, ok := getenvBool("CRABBOX_MORPH_WAKE_ON_SSH"); ok {
		cfg.Morph.WakeOnSSH = value
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_DAYTONA_API_KEY", "DAYTONA_API_KEY"); ok {
		cfg.Daytona.APIKey = value
		cfg.credentialProvenance.daytonaAPIKey = credentialSourceEnvironment
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_DAYTONA_JWT_TOKEN", "DAYTONA_JWT_TOKEN"); ok {
		cfg.Daytona.JWTToken = value
		cfg.credentialProvenance.daytonaJWTToken = credentialSourceEnvironment
	}
	cfg.Daytona.OrganizationID = getenv("CRABBOX_DAYTONA_ORGANIZATION_ID", getenv("DAYTONA_ORGANIZATION_ID", cfg.Daytona.OrganizationID))
	if value, ok := firstNonEmptyEnv("CRABBOX_DAYTONA_API_URL", "DAYTONA_API_URL"); ok {
		cfg.Daytona.APIURL = value
		cfg.credentialProvenance.daytonaAPIURL = credentialSourceEnvironment
	}
	cfg.Daytona.Snapshot = getenv("CRABBOX_DAYTONA_SNAPSHOT", getenv("DAYTONA_SNAPSHOT", cfg.Daytona.Snapshot))
	cfg.Daytona.Target = getenv("CRABBOX_DAYTONA_TARGET", getenv("DAYTONA_TARGET", cfg.Daytona.Target))
	cfg.Daytona.User = getenv("CRABBOX_DAYTONA_USER", cfg.Daytona.User)
	cfg.Daytona.WorkRoot = getenv("CRABBOX_DAYTONA_WORK_ROOT", cfg.Daytona.WorkRoot)
	if value := os.Getenv("CRABBOX_DAYTONA_SSH_GATEWAY_HOST"); value != "" {
		cfg.Daytona.SSHGatewayHost = value
		cfg.credentialProvenance.daytonaSSHGateway = credentialSourceEnvironment
	}
	cfg.Daytona.SSHAccessMinutes = getenvInt("CRABBOX_DAYTONA_SSH_ACCESS_MINUTES", cfg.Daytona.SSHAccessMinutes)
	if value, ok := firstNonEmptyEnv("CRABBOX_E2B_API_KEY", "E2B_API_KEY"); ok {
		cfg.E2B.APIKey = value
		cfg.credentialProvenance.e2bAPIKey = credentialSourceEnvironment
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_E2B_API_URL", "E2B_API_URL"); ok {
		cfg.E2B.APIURL = value
		cfg.credentialProvenance.e2bAPIURL = credentialSourceEnvironment
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_E2B_DOMAIN", "E2B_DOMAIN"); ok {
		cfg.E2B.Domain = value
		cfg.credentialProvenance.e2bDomain = credentialSourceEnvironment
	}
	cfg.E2B.Template = getenv("CRABBOX_E2B_TEMPLATE", cfg.E2B.Template)
	cfg.E2B.Workdir = getenv("CRABBOX_E2B_WORKDIR", cfg.E2B.Workdir)
	cfg.E2B.User = getenv("CRABBOX_E2B_USER", cfg.E2B.User)
	cfg.ExeDev.ControlHost = getenv("CRABBOX_EXE_DEV_CONTROL_HOST", getenv("EXE_DEV_CONTROL_HOST", cfg.ExeDev.ControlHost))
	cfg.ExeDev.Image = getenv("CRABBOX_EXE_DEV_IMAGE", getenv("EXE_DEV_IMAGE", cfg.ExeDev.Image))
	cfg.ExeDev.CPUs = getenvInt("CRABBOX_EXE_DEV_CPUS", cfg.ExeDev.CPUs)
	cfg.ExeDev.Memory = getenv("CRABBOX_EXE_DEV_MEMORY", getenv("EXE_DEV_MEMORY", cfg.ExeDev.Memory))
	cfg.ExeDev.Disk = getenv("CRABBOX_EXE_DEV_DISK", getenv("EXE_DEV_DISK", cfg.ExeDev.Disk))
	cfg.ExeDev.Command = getenv("CRABBOX_EXE_DEV_COMMAND", cfg.ExeDev.Command)
	cfg.ExeDev.User = getenv("CRABBOX_EXE_DEV_USER", cfg.ExeDev.User)
	cfg.ExeDev.WorkRoot = getenv("CRABBOX_EXE_DEV_WORK_ROOT", cfg.ExeDev.WorkRoot)
	if value, ok := getenvBool("CRABBOX_EXE_DEV_NO_EMAIL"); ok {
		cfg.ExeDev.NoEmail = value
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_RAILWAY_API_TOKEN", "RAILWAY_API_TOKEN"); ok {
		cfg.Railway.APIToken = value
		cfg.credentialProvenance.railwayAPIToken = credentialSourceEnvironment
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_RAILWAY_API_URL", "RAILWAY_API_URL"); ok {
		cfg.Railway.APIURL = value
		cfg.credentialProvenance.railwayAPIURL = credentialSourceEnvironment
	}
	cfg.Railway.ProjectID = getenv("CRABBOX_RAILWAY_PROJECT_ID", getenv("RAILWAY_PROJECT_ID", cfg.Railway.ProjectID))
	cfg.Railway.EnvironmentID = getenv("CRABBOX_RAILWAY_ENVIRONMENT_ID", getenv("RAILWAY_ENVIRONMENT_ID", cfg.Railway.EnvironmentID))
	if value, ok := firstNonEmptyEnv("CRABBOX_RUNPOD_API_KEY", "RUNPOD_API_KEY"); ok {
		cfg.Runpod.APIKey = value
		cfg.credentialProvenance.runpodAPIKey = credentialSourceEnvironment
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_RUNPOD_API_URL", "RUNPOD_API_URL"); ok {
		cfg.Runpod.APIURL = value
		cfg.credentialProvenance.runpodAPIURL = credentialSourceEnvironment
	}
	cfg.Runpod.CloudType = getenv("CRABBOX_RUNPOD_CLOUD_TYPE", getenv("RUNPOD_CLOUD_TYPE", cfg.Runpod.CloudType))
	cfg.Runpod.InstanceID = getenv("CRABBOX_RUNPOD_INSTANCE_ID", getenv("RUNPOD_INSTANCE_ID", cfg.Runpod.InstanceID))
	cfg.Runpod.Image = getenv("CRABBOX_RUNPOD_IMAGE", getenv("RUNPOD_IMAGE", cfg.Runpod.Image))
	cfg.Runpod.TemplateID = getenv("CRABBOX_RUNPOD_TEMPLATE_ID", getenv("RUNPOD_TEMPLATE_ID", cfg.Runpod.TemplateID))
	cfg.Runpod.DiskGB = getenvInt("CRABBOX_RUNPOD_DISK_GB", cfg.Runpod.DiskGB)
	cfg.Runpod.User = getenv("CRABBOX_RUNPOD_USER", cfg.Runpod.User)
	cfg.Runpod.WorkRoot = getenv("CRABBOX_RUNPOD_WORK_ROOT", cfg.Runpod.WorkRoot)
	cfg.NvidiaBrev.CLI = getenv("CRABBOX_NVIDIA_BREV_CLI", cfg.NvidiaBrev.CLI)
	cfg.NvidiaBrev.Org = getenv("CRABBOX_NVIDIA_BREV_ORG", cfg.NvidiaBrev.Org)
	cfg.NvidiaBrev.Type = getenv("CRABBOX_NVIDIA_BREV_TYPE", cfg.NvidiaBrev.Type)
	cfg.NvidiaBrev.GPUName = getenv("CRABBOX_NVIDIA_BREV_GPU_NAME", cfg.NvidiaBrev.GPUName)
	cfg.NvidiaBrev.Provider = getenv("CRABBOX_NVIDIA_BREV_PROVIDER", cfg.NvidiaBrev.Provider)
	cfg.NvidiaBrev.Mode = getenv("CRABBOX_NVIDIA_BREV_MODE", cfg.NvidiaBrev.Mode)
	cfg.NvidiaBrev.Launchable = getenv("CRABBOX_NVIDIA_BREV_LAUNCHABLE", cfg.NvidiaBrev.Launchable)
	cfg.NvidiaBrev.StartupScript = getenv("CRABBOX_NVIDIA_BREV_STARTUP_SCRIPT", cfg.NvidiaBrev.StartupScript)
	if value := os.Getenv("CRABBOX_NVIDIA_BREV_RELEASE_ACTION"); value != "" {
		cfg.NvidiaBrev.ReleaseAction = value
		MarkDeleteOnReleaseExplicit(cfg, "nvidia-brev")
	}
	cfg.NvidiaBrev.Target = getenv("CRABBOX_NVIDIA_BREV_TARGET", cfg.NvidiaBrev.Target)
	cfg.NvidiaBrev.User = getenv("CRABBOX_NVIDIA_BREV_USER", cfg.NvidiaBrev.User)
	if value := os.Getenv("CRABBOX_NVIDIA_BREV_WORK_ROOT"); value != "" {
		cfg.NvidiaBrev.WorkRoot = value
		MarkNvidiaBrevWorkRootExplicit(cfg)
	}
	cfg.Hostinger.APIToken = getenv("CRABBOX_HOSTINGER_API_TOKEN", getenv("HOSTINGER_API_TOKEN", cfg.Hostinger.APIToken))
	cfg.Hostinger.APIURL = getenv("CRABBOX_HOSTINGER_API_URL", getenv("HOSTINGER_API_URL", cfg.Hostinger.APIURL))
	cfg.Hostinger.ItemID = getenv("CRABBOX_HOSTINGER_ITEM_ID", cfg.Hostinger.ItemID)
	cfg.Hostinger.PaymentMethodID = getenv("CRABBOX_HOSTINGER_PAYMENT_METHOD_ID", cfg.Hostinger.PaymentMethodID)
	cfg.Hostinger.TemplateID = getenv("CRABBOX_HOSTINGER_TEMPLATE_ID", cfg.Hostinger.TemplateID)
	cfg.Hostinger.DataCenterID = getenv("CRABBOX_HOSTINGER_DATA_CENTER_ID", cfg.Hostinger.DataCenterID)
	cfg.Hostinger.HostnamePrefix = getenv("CRABBOX_HOSTINGER_HOSTNAME_PREFIX", cfg.Hostinger.HostnamePrefix)
	if user := os.Getenv("CRABBOX_HOSTINGER_USER"); user != "" {
		cfg.Hostinger.User = user
		MarkHostingerUserExplicit(cfg)
	}
	if workRoot := os.Getenv("CRABBOX_HOSTINGER_WORK_ROOT"); workRoot != "" {
		cfg.Hostinger.WorkRoot = workRoot
		MarkHostingerWorkRootExplicit(cfg)
	}
	if value, ok := getenvBool("CRABBOX_HOSTINGER_ALLOW_PURCHASE"); ok {
		cfg.Hostinger.AllowPurchase = value
	}
	cfg.Hostinger.ReleaseAction = getenv("CRABBOX_HOSTINGER_RELEASE_ACTION", cfg.Hostinger.ReleaseAction)
	// WANDB_API_KEY is resolved by the W&B client after file config so a
	// generic shell login cannot override an explicit wandb.apiKey value.
	cfg.Wandb.APIKey = getenv("CRABBOX_WANDB_API_KEY", cfg.Wandb.APIKey)
	cfg.Wandb.DefaultImage = getenv("CRABBOX_WANDB_DEFAULT_IMAGE", getenv("WANDB_DEFAULT_IMAGE", cfg.Wandb.DefaultImage))
	cfg.Wandb.MaxLifetimeSeconds = getenvInt("CRABBOX_WANDB_MAX_LIFETIME_SECONDS", getenvInt("WANDB_MAX_LIFETIME_SECONDS", cfg.Wandb.MaxLifetimeSeconds))
	if value, ok := firstNonEmptyEnv("CRABBOX_ISLO_API_KEY", "ISLO_API_KEY"); ok {
		cfg.Islo.APIKey = value
		cfg.credentialProvenance.isloAPIKey = credentialSourceEnvironment
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_ISLO_BASE_URL", "ISLO_BASE_URL"); ok {
		cfg.Islo.BaseURL = value
		cfg.credentialProvenance.isloBaseURL = credentialSourceEnvironment
	}
	if image := os.Getenv("CRABBOX_ISLO_IMAGE"); image != "" {
		cfg.Islo.Image = image
		cfg.isloImageExplicit = true
	}
	cfg.Islo.Workdir = getenv("CRABBOX_ISLO_WORKDIR", cfg.Islo.Workdir)
	cfg.Islo.GatewayProfile = getenv("CRABBOX_ISLO_GATEWAY_PROFILE", cfg.Islo.GatewayProfile)
	cfg.Islo.SnapshotName = getenv("CRABBOX_ISLO_SNAPSHOT_NAME", cfg.Islo.SnapshotName)
	if raw := os.Getenv("CRABBOX_ISLO_VCPUS"); raw != "" {
		cfg.Islo.VCPUs = getenvInt("CRABBOX_ISLO_VCPUS", cfg.Islo.VCPUs)
		if _, err := strconv.Atoi(raw); err == nil {
			cfg.isloVCPUsExplicit = true
		}
	}
	if raw := os.Getenv("CRABBOX_ISLO_MEMORY_MB"); raw != "" {
		cfg.Islo.MemoryMB = getenvInt("CRABBOX_ISLO_MEMORY_MB", cfg.Islo.MemoryMB)
		if _, err := strconv.Atoi(raw); err == nil {
			cfg.isloMemoryMBExplicit = true
		}
	}
	if raw := os.Getenv("CRABBOX_ISLO_DISK_GB"); raw != "" {
		cfg.Islo.DiskGB = getenvInt("CRABBOX_ISLO_DISK_GB", cfg.Islo.DiskGB)
		if _, err := strconv.Atoi(raw); err == nil {
			cfg.isloDiskGBExplicit = true
		}
	}
	cfg.Freestyle.APIKey = getenv("CRABBOX_FREESTYLE_API_KEY", getenv("FREESTYLE_API_KEY", cfg.Freestyle.APIKey))
	cfg.Freestyle.APIURL = getenv("CRABBOX_FREESTYLE_API_URL", getenv("FREESTYLE_API_URL", cfg.Freestyle.APIURL))
	cfg.Freestyle.Workdir = getenv("CRABBOX_FREESTYLE_WORKDIR", cfg.Freestyle.Workdir)
	cfg.Freestyle.VCPUs = getenvInt("CRABBOX_FREESTYLE_VCPUS", cfg.Freestyle.VCPUs)
	cfg.Freestyle.MemoryGB = getenvInt("CRABBOX_FREESTYLE_MEMORY_GB", cfg.Freestyle.MemoryGB)
	cfg.Tenki.CLIPath = getenv("CRABBOX_TENKI_CLI", getenv("TENKI_CLI", cfg.Tenki.CLIPath))
	if value, ok := firstNonEmptyEnv("CRABBOX_TENKI_ENDPOINT", "TENKI_ENDPOINT"); ok {
		cfg.Tenki.Endpoint = value
		cfg.credentialProvenance.tenkiEndpoint = credentialSourceEnvironment
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_TENKI_GATEWAY", "TENKI_GATEWAY"); ok {
		cfg.Tenki.Gateway = value
		cfg.credentialProvenance.tenkiGateway = credentialSourceEnvironment
	}
	cfg.Tenki.Workspace = getenv("CRABBOX_TENKI_WORKSPACE", cfg.Tenki.Workspace)
	cfg.Tenki.Project = getenv("CRABBOX_TENKI_PROJECT", cfg.Tenki.Project)
	cfg.Tenki.Image = getenv("CRABBOX_TENKI_IMAGE", cfg.Tenki.Image)
	cfg.Tenki.Snapshot = getenv("CRABBOX_TENKI_SNAPSHOT", cfg.Tenki.Snapshot)
	cfg.Tenki.WorkRoot = getenv("CRABBOX_TENKI_WORK_ROOT", cfg.Tenki.WorkRoot)
	cfg.Tenki.CPUs = getenvInt("CRABBOX_TENKI_CPUS", cfg.Tenki.CPUs)
	cfg.Tenki.MemoryMB = getenvInt("CRABBOX_TENKI_MEMORY_MB", cfg.Tenki.MemoryMB)
	cfg.Tenki.DiskGB = getenvInt("CRABBOX_TENKI_DISK_GB", cfg.Tenki.DiskGB)
	if value, ok := firstNonEmptyEnv("CRABBOX_TENSORLAKE_API_KEY", "TENSORLAKE_API_KEY"); ok {
		cfg.Tensorlake.APIKey = value
		cfg.credentialProvenance.tensorlakeAPIKey = credentialSourceEnvironment
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_TENSORLAKE_API_URL", "TENSORLAKE_API_URL"); ok {
		cfg.Tensorlake.APIURL = value
		cfg.credentialProvenance.tensorlakeAPIURL = credentialSourceEnvironment
	}
	cfg.Tensorlake.CLIPath = getenv("CRABBOX_TENSORLAKE_CLI", cfg.Tensorlake.CLIPath)
	cfg.Tensorlake.Image = getenv("CRABBOX_TENSORLAKE_IMAGE", cfg.Tensorlake.Image)
	cfg.Tensorlake.Snapshot = getenv("CRABBOX_TENSORLAKE_SNAPSHOT", cfg.Tensorlake.Snapshot)
	cfg.Tensorlake.OrganizationID = getenv("CRABBOX_TENSORLAKE_ORGANIZATION_ID", getenv("TENSORLAKE_ORGANIZATION_ID", cfg.Tensorlake.OrganizationID))
	cfg.Tensorlake.ProjectID = getenv("CRABBOX_TENSORLAKE_PROJECT_ID", getenv("TENSORLAKE_PROJECT_ID", cfg.Tensorlake.ProjectID))
	cfg.Tensorlake.Namespace = getenv("CRABBOX_TENSORLAKE_NAMESPACE", getenv("INDEXIFY_NAMESPACE", cfg.Tensorlake.Namespace))
	cfg.Tensorlake.Workdir = getenv("CRABBOX_TENSORLAKE_WORKDIR", cfg.Tensorlake.Workdir)
	cfg.Tensorlake.CPUs = getenvFloat("CRABBOX_TENSORLAKE_CPUS", cfg.Tensorlake.CPUs)
	cfg.Tensorlake.MemoryMB = getenvInt("CRABBOX_TENSORLAKE_MEMORY_MB", cfg.Tensorlake.MemoryMB)
	cfg.Tensorlake.DiskMB = getenvInt("CRABBOX_TENSORLAKE_DISK_MB", cfg.Tensorlake.DiskMB)
	cfg.Tensorlake.TimeoutSecs = getenvInt("CRABBOX_TENSORLAKE_TIMEOUT_SECS", cfg.Tensorlake.TimeoutSecs)
	if v, ok := getenvBool("CRABBOX_TENSORLAKE_NO_INTERNET"); ok {
		cfg.Tensorlake.NoInternet = v
	}
	cfg.OpenComputer.APIURL = getenv("CRABBOX_OPENCOMPUTER_API_URL", getenv("OPENCOMPUTER_API_URL", cfg.OpenComputer.APIURL))
	cfg.OpenComputer.Workdir = getenv("CRABBOX_OPENCOMPUTER_WORKDIR", cfg.OpenComputer.Workdir)
	cfg.OpenComputer.CPU = getenvInt("CRABBOX_OPENCOMPUTER_CPU", cfg.OpenComputer.CPU)
	cfg.OpenComputer.MemoryMB = getenvInt("CRABBOX_OPENCOMPUTER_MEMORY_MB", cfg.OpenComputer.MemoryMB)
	cfg.OpenComputer.TimeoutSecs = getenvInt("CRABBOX_OPENCOMPUTER_TIMEOUT_SECS", cfg.OpenComputer.TimeoutSecs)
	cfg.OpenComputer.ExecTimeoutSecs = getenvInt("CRABBOX_OPENCOMPUTER_EXEC_TIMEOUT_SECS", cfg.OpenComputer.ExecTimeoutSecs)
	if v, ok := getenvBool("CRABBOX_OPENCOMPUTER_BURST"); ok {
		cfg.OpenComputer.Burst = v
	}
	var err error
	cfg.CodeSandbox.TemplateID = getenv("CRABBOX_CODESANDBOX_TEMPLATE_ID", cfg.CodeSandbox.TemplateID)
	cfg.CodeSandbox.Workdir = getenv("CRABBOX_CODESANDBOX_WORKDIR", cfg.CodeSandbox.Workdir)
	cfg.CodeSandbox.VMTier = getenv("CRABBOX_CODESANDBOX_VM_TIER", cfg.CodeSandbox.VMTier)
	cfg.CodeSandbox.Privacy = getenv("CRABBOX_CODESANDBOX_PRIVACY", cfg.CodeSandbox.Privacy)
	cfg.CodeSandbox.HibernationTimeoutSecs, err = getenvNonNegativeInt("CRABBOX_CODESANDBOX_HIBERNATION_TIMEOUT_SECS", cfg.CodeSandbox.HibernationTimeoutSecs)
	if err != nil {
		return err
	}
	if v, ok := getenvBool("CRABBOX_CODESANDBOX_AUTOMATIC_WAKEUP_HTTP"); ok {
		cfg.CodeSandbox.AutomaticWakeupHTTP = v
	}
	if v, ok := getenvBool("CRABBOX_CODESANDBOX_AUTOMATIC_WAKEUP_WEBSOCKET"); ok {
		cfg.CodeSandbox.AutomaticWakeupWebSocket = v
	}
	cfg.CodeSandbox.BridgeCommand = getenv("CRABBOX_CODESANDBOX_BRIDGE_COMMAND", cfg.CodeSandbox.BridgeCommand)
	cfg.CodeSandbox.SDKPackage = getenv("CRABBOX_CODESANDBOX_SDK_PACKAGE", cfg.CodeSandbox.SDKPackage)
	cfg.CodeSandbox.DoctorListLimit, err = getenvNonNegativeInt("CRABBOX_CODESANDBOX_DOCTOR_LIST_LIMIT", cfg.CodeSandbox.DoctorListLimit)
	if err != nil {
		return err
	}
	cfg.CodeSandbox.OperationTimeoutSecs, err = getenvNonNegativeInt("CRABBOX_CODESANDBOX_OPERATION_TIMEOUT_SECS", cfg.CodeSandbox.OperationTimeoutSecs)
	if err != nil {
		return err
	}
	cfg.OpenSandbox.APIURL = getenv("CRABBOX_OPENSANDBOX_API_URL", getenv("OPEN_SANDBOX_API_URL", cfg.OpenSandbox.APIURL))
	cfg.OpenSandbox.Image = getenv("CRABBOX_OPENSANDBOX_IMAGE", cfg.OpenSandbox.Image)
	cfg.OpenSandbox.Workdir = getenv("CRABBOX_OPENSANDBOX_WORKDIR", cfg.OpenSandbox.Workdir)
	cfg.OpenSandbox.CPU = getenv("CRABBOX_OPENSANDBOX_CPU", cfg.OpenSandbox.CPU)
	cfg.OpenSandbox.Memory = getenv("CRABBOX_OPENSANDBOX_MEMORY", cfg.OpenSandbox.Memory)
	cfg.OpenSandbox.TimeoutSecs, err = getenvNonNegativeInt("CRABBOX_OPENSANDBOX_TIMEOUT_SECS", cfg.OpenSandbox.TimeoutSecs)
	if err != nil {
		return err
	}
	cfg.OpenSandbox.ExecTimeoutSecs, err = getenvNonNegativeInt("CRABBOX_OPENSANDBOX_EXEC_TIMEOUT_SECS", cfg.OpenSandbox.ExecTimeoutSecs)
	if err != nil {
		return err
	}
	cfg.OpenSandbox.PlatformOS = getenv("CRABBOX_OPENSANDBOX_PLATFORM_OS", cfg.OpenSandbox.PlatformOS)
	cfg.OpenSandbox.PlatformArch = getenv("CRABBOX_OPENSANDBOX_PLATFORM_ARCH", cfg.OpenSandbox.PlatformArch)
	if v, ok := getenvBool("CRABBOX_OPENSANDBOX_SECURE_ACCESS"); ok {
		cfg.OpenSandbox.SecureAccess = v
	}
	if v, ok := getenvBool("CRABBOX_OPENSANDBOX_USE_SERVER_PROXY"); ok {
		cfg.OpenSandbox.UseServerProxy = v
	}
	cfg.VercelSandbox.Runtime = getenv("CRABBOX_VERCEL_SANDBOX_RUNTIME", cfg.VercelSandbox.Runtime)
	cfg.VercelSandbox.Workdir = getenv("CRABBOX_VERCEL_SANDBOX_WORKDIR", cfg.VercelSandbox.Workdir)
	cfg.VercelSandbox.ProjectID = getenv("CRABBOX_VERCEL_SANDBOX_PROJECT_ID", cfg.VercelSandbox.ProjectID)
	cfg.VercelSandbox.TeamID = getenv("CRABBOX_VERCEL_SANDBOX_TEAM_ID", cfg.VercelSandbox.TeamID)
	cfg.VercelSandbox.Scope = getenv("CRABBOX_VERCEL_SANDBOX_SCOPE", cfg.VercelSandbox.Scope)
	cfg.VercelSandbox.VCPUs = getenvFloat("CRABBOX_VERCEL_SANDBOX_VCPUS", cfg.VercelSandbox.VCPUs)
	cfg.VercelSandbox.TimeoutSecs, err = getenvNonNegativeInt("CRABBOX_VERCEL_SANDBOX_TIMEOUT_SECS", cfg.VercelSandbox.TimeoutSecs)
	if err != nil {
		return err
	}
	cfg.VercelSandbox.ExecTimeoutSecs, err = getenvNonNegativeInt("CRABBOX_VERCEL_SANDBOX_EXEC_TIMEOUT_SECS", cfg.VercelSandbox.ExecTimeoutSecs)
	if err != nil {
		return err
	}
	if v, ok := getenvBool("CRABBOX_VERCEL_SANDBOX_PERSISTENT"); ok {
		cfg.VercelSandbox.Persistent = v
	}
	cfg.VercelSandbox.Snapshot = getenv("CRABBOX_VERCEL_SANDBOX_SNAPSHOT", cfg.VercelSandbox.Snapshot)
	cfg.VercelSandbox.SnapshotMode = getenv("CRABBOX_VERCEL_SANDBOX_SNAPSHOT_MODE", cfg.VercelSandbox.SnapshotMode)
	cfg.VercelSandbox.NetworkPolicy = getenv("CRABBOX_VERCEL_SANDBOX_NETWORK_POLICY", cfg.VercelSandbox.NetworkPolicy)
	if allow := os.Getenv("CRABBOX_VERCEL_SANDBOX_NETWORK_ALLOW"); allow != "" {
		cfg.VercelSandbox.NetworkAllow = splitCommaList(allow)
	}
	if deny := os.Getenv("CRABBOX_VERCEL_SANDBOX_NETWORK_DENY"); deny != "" {
		cfg.VercelSandbox.NetworkDeny = splitCommaList(deny)
	}
	if ports := os.Getenv("CRABBOX_VERCEL_SANDBOX_PORTS"); ports != "" {
		cfg.VercelSandbox.Ports = splitCommaList(ports)
	}
	if v, ok := getenvBool("CRABBOX_VERCEL_SANDBOX_FORGET_MISSING"); ok {
		cfg.VercelSandbox.ForgetMissing = v
	}
	cfg.Superserve.BaseURL = getenv("CRABBOX_SUPERSERVE_BASE_URL", getenv("SUPERSERVE_BASE_URL", cfg.Superserve.BaseURL))
	cfg.Superserve.Template = getenv("CRABBOX_SUPERSERVE_TEMPLATE", cfg.Superserve.Template)
	cfg.Superserve.Snapshot = getenv("CRABBOX_SUPERSERVE_SNAPSHOT", cfg.Superserve.Snapshot)
	cfg.Superserve.Workdir = getenv("CRABBOX_SUPERSERVE_WORKDIR", cfg.Superserve.Workdir)
	cfg.Superserve.TimeoutSecs, err = getenvNonNegativeInt("CRABBOX_SUPERSERVE_TIMEOUT_SECS", cfg.Superserve.TimeoutSecs)
	if err != nil {
		return err
	}
	cfg.Superserve.ExecTimeoutSecs, err = getenvNonNegativeInt("CRABBOX_SUPERSERVE_EXEC_TIMEOUT_SECS", cfg.Superserve.ExecTimeoutSecs)
	if err != nil {
		return err
	}
	if allowOut := os.Getenv("CRABBOX_SUPERSERVE_NETWORK_ALLOW_OUT"); allowOut != "" {
		cfg.Superserve.NetworkAllowOut = splitCommaList(allowOut)
	}
	if denyOut := os.Getenv("CRABBOX_SUPERSERVE_NETWORK_DENY_OUT"); denyOut != "" {
		cfg.Superserve.NetworkDenyOut = splitCommaList(denyOut)
	}
	if v, ok := getenvBool("CRABBOX_SUPERSERVE_FORGET_MISSING"); ok {
		cfg.Superserve.ForgetMissing = v
	}
	cfg.DockerSandbox.CLIPath = getenv("CRABBOX_DOCKER_SANDBOX_CLI", cfg.DockerSandbox.CLIPath)
	cfg.DockerSandbox.Agent = getenv("CRABBOX_DOCKER_SANDBOX_AGENT", cfg.DockerSandbox.Agent)
	cfg.DockerSandbox.Template = getenv("CRABBOX_DOCKER_SANDBOX_TEMPLATE", cfg.DockerSandbox.Template)
	if cpus := os.Getenv("CRABBOX_DOCKER_SANDBOX_CPUS"); cpus != "" {
		parsed, err := strconv.ParseFloat(cpus, 64)
		if err != nil {
			return fmt.Errorf("parse CRABBOX_DOCKER_SANDBOX_CPUS: %w", err)
		}
		cfg.DockerSandbox.CPUs = parsed
	}
	cfg.DockerSandbox.Memory = getenv("CRABBOX_DOCKER_SANDBOX_MEMORY", cfg.DockerSandbox.Memory)
	if v, ok := getenvBool("CRABBOX_DOCKER_SANDBOX_CLONE"); ok {
		cfg.DockerSandbox.Clone = v
	}
	cfg.DockerSandbox.Workdir = getenv("CRABBOX_DOCKER_SANDBOX_WORKDIR", cfg.DockerSandbox.Workdir)
	if values, ok := getenvList("CRABBOX_DOCKER_SANDBOX_EXTRA_WORKSPACES"); ok {
		cfg.DockerSandbox.ExtraWorkspaces = values
	}
	if values, ok := getenvList("CRABBOX_DOCKER_SANDBOX_MCP"); ok {
		cfg.DockerSandbox.MCP = values
	}
	if values, ok := getenvList("CRABBOX_DOCKER_SANDBOX_KIT"); ok {
		cfg.DockerSandbox.Kit = values
	}
	cfg.AnthropicSRT.CLIPath = getenv("CRABBOX_ANTHROPIC_SANDBOX_RUNTIME_CLI", cfg.AnthropicSRT.CLIPath)
	cfg.AnthropicSRT.Settings = getenv("CRABBOX_ANTHROPIC_SANDBOX_RUNTIME_SETTINGS", cfg.AnthropicSRT.Settings)
	if value, ok := getenvBool("CRABBOX_ANTHROPIC_SANDBOX_RUNTIME_DEBUG"); ok {
		cfg.AnthropicSRT.Debug = value
	}
	cfg.Modal.App = getenv("CRABBOX_MODAL_APP", cfg.Modal.App)
	cfg.Modal.Image = getenv("CRABBOX_MODAL_IMAGE", cfg.Modal.Image)
	cfg.Modal.Workdir = getenv("CRABBOX_MODAL_WORKDIR", cfg.Modal.Workdir)
	cfg.Modal.Python = getenv("CRABBOX_MODAL_PYTHON", cfg.Modal.Python)
	if value, ok := firstNonEmptyEnv("CRABBOX_UPSTASH_BOX_API_KEY", "UPSTASH_BOX_API_KEY"); ok {
		cfg.UpstashBox.APIKey = value
		cfg.credentialProvenance.upstashBoxAPIKey = credentialSourceEnvironment
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_UPSTASH_BOX_BASE_URL", "UPSTASH_BOX_BASE_URL"); ok {
		cfg.UpstashBox.BaseURL = value
		cfg.credentialProvenance.upstashBoxBaseURL = credentialSourceEnvironment
	}
	cfg.UpstashBox.Runtime = getenv("CRABBOX_UPSTASH_BOX_RUNTIME", cfg.UpstashBox.Runtime)
	cfg.UpstashBox.Size = getenv("CRABBOX_UPSTASH_BOX_SIZE", cfg.UpstashBox.Size)
	cfg.UpstashBox.Workdir = getenv("CRABBOX_UPSTASH_BOX_WORKDIR", cfg.UpstashBox.Workdir)
	if value, ok := getenvBool("CRABBOX_UPSTASH_BOX_KEEP_ALIVE"); ok {
		cfg.UpstashBox.KeepAlive = value
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_SMOLVM_API_KEY", "SMOLMACHINES_API_KEY", "SMK_API_KEY"); ok {
		cfg.Smolvm.APIKey = value
		cfg.credentialProvenance.smolvmAPIKey = credentialSourceEnvironment
	}
	if value := os.Getenv("CRABBOX_SMOLVM_BASE_URL"); value != "" {
		cfg.Smolvm.BaseURL = value
		cfg.credentialProvenance.smolvmBaseURL = credentialSourceEnvironment
	}
	cfg.Smolvm.Image = getenv("CRABBOX_SMOLVM_IMAGE", cfg.Smolvm.Image)
	cfg.Smolvm.Workdir = getenv("CRABBOX_SMOLVM_WORKDIR", cfg.Smolvm.Workdir)
	cfg.Smolvm.CPUs = getenvInt("CRABBOX_SMOLVM_CPUS", cfg.Smolvm.CPUs)
	cfg.Smolvm.MemoryMB = getenvInt("CRABBOX_SMOLVM_MEMORY_MB", cfg.Smolvm.MemoryMB)
	cfg.Smolvm.Network = getenv("CRABBOX_SMOLVM_NETWORK", cfg.Smolvm.Network)
	if value, ok := getenvBool("CRABBOX_SMOLVM_KEEP"); ok {
		cfg.Smolvm.Keep = value
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_ASCII_BOX_API_KEY", "ASCII_BOX_API_KEY"); ok {
		cfg.AsciiBox.APIKey = value
		cfg.credentialProvenance.asciiBoxAPIKey = credentialSourceEnvironment
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_ASCII_BOX_BASE_URL", "ASCII_BOX_BASE_URL"); ok {
		cfg.AsciiBox.BaseURL = value
		cfg.credentialProvenance.asciiBoxBaseURL = credentialSourceEnvironment
	}
	cfg.AsciiBox.CLIPath = getenv("CRABBOX_ASCII_BOX_CLI", getenv("BOX_CLI", cfg.AsciiBox.CLIPath))
	cfg.AsciiBox.Workdir = getenv("CRABBOX_ASCII_BOX_WORKDIR", cfg.AsciiBox.Workdir)
	if value := os.Getenv("CRABBOX_CLOUDFLARE_RUNNER_URL"); value != "" {
		cfg.Cloudflare.APIURL = value
		cfg.credentialProvenance.cloudflareAPIURL = credentialSourceEnvironment
	}
	if value := os.Getenv("CRABBOX_CLOUDFLARE_RUNNER_TOKEN"); value != "" {
		cfg.Cloudflare.Token = value
		cfg.credentialProvenance.cloudflareToken = credentialSourceEnvironment
	}
	cfg.Cloudflare.Workdir = getenv("CRABBOX_CLOUDFLARE_WORKDIR", cfg.Cloudflare.Workdir)
	cfg.CloudflareDynamicWorkers.LoaderURL = getenv("CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_URL", getenv("CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_LOADER_URL", cfg.CloudflareDynamicWorkers.LoaderURL))
	cfg.CloudflareDynamicWorkers.Token = getenv("CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN", cfg.CloudflareDynamicWorkers.Token)
	cfg.CloudflareDynamicWorkers.CompatibilityDate = getenv("CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_COMPATIBILITY_DATE", cfg.CloudflareDynamicWorkers.CompatibilityDate)
	if flags, ok := getenvList("CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_COMPATIBILITY_FLAGS"); ok {
		cfg.CloudflareDynamicWorkers.CompatibilityFlags = flags
	}
	cfg.CloudflareDynamicWorkers.CacheMode = getenv("CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_CACHE_MODE", cfg.CloudflareDynamicWorkers.CacheMode)
	cfg.CloudflareDynamicWorkers.Egress = getenv("CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_EGRESS", cfg.CloudflareDynamicWorkers.Egress)
	cfg.CloudflareDynamicWorkers.CPUMs = getenvInt("CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_CPU_MS", cfg.CloudflareDynamicWorkers.CPUMs)
	cfg.CloudflareDynamicWorkers.Subrequests = getenvInt("CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_SUBREQUESTS", cfg.CloudflareDynamicWorkers.Subrequests)
	cfg.CloudflareDynamicWorkers.TimeoutSecs = getenvInt("CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TIMEOUT_SECS", cfg.CloudflareDynamicWorkers.TimeoutSecs)
	if value, ok := firstNonEmptyEnv("CRABBOX_SEMAPHORE_HOST", "SEMAPHORE_HOST"); ok {
		cfg.Semaphore.Host = value
		cfg.credentialProvenance.semaphoreHost = credentialSourceEnvironment
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_SEMAPHORE_TOKEN", "SEMAPHORE_API_TOKEN"); ok {
		cfg.Semaphore.Token = value
		cfg.credentialProvenance.semaphoreToken = credentialSourceEnvironment
	}
	cfg.Semaphore.Project = getenv("CRABBOX_SEMAPHORE_PROJECT", getenv("SEMAPHORE_PROJECT", cfg.Semaphore.Project))
	cfg.Semaphore.Machine = getenv("CRABBOX_SEMAPHORE_MACHINE", cfg.Semaphore.Machine)
	cfg.Semaphore.OSImage = getenv("CRABBOX_SEMAPHORE_OS_IMAGE", cfg.Semaphore.OSImage)
	cfg.Semaphore.IdleTimeout = getenv("CRABBOX_SEMAPHORE_IDLE_TIMEOUT", cfg.Semaphore.IdleTimeout)
	if value, ok := firstNonEmptyEnv("CRABBOX_SPRITES_TOKEN", "SPRITES_TOKEN", "SPRITE_TOKEN", "SETUP_SPRITE_TOKEN"); ok {
		cfg.Sprites.Token = value
		cfg.credentialProvenance.spritesToken = credentialSourceEnvironment
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_SPRITES_API_URL", "SPRITES_API_URL"); ok {
		cfg.Sprites.APIURL = value
		cfg.credentialProvenance.spritesAPIURL = credentialSourceEnvironment
	}
	cfg.Sprites.WorkRoot = getenv("CRABBOX_SPRITES_WORK_ROOT", cfg.Sprites.WorkRoot)
	if runtimeName := os.Getenv("CRABBOX_LOCAL_CONTAINER_RUNTIME"); runtimeName != "" {
		cfg.LocalContainer.Runtime = runtimeName
		cfg.localContainerRuntimeExplicit = true
	}
	if image := os.Getenv("CRABBOX_LOCAL_CONTAINER_IMAGE"); image != "" {
		cfg.LocalContainer.Image = image
		cfg.localContainerImageExplicit = true
	}
	cfg.LocalContainer.User = getenv("CRABBOX_LOCAL_CONTAINER_USER", cfg.LocalContainer.User)
	cfg.LocalContainer.WorkRoot = getenv("CRABBOX_LOCAL_CONTAINER_WORK_ROOT", cfg.LocalContainer.WorkRoot)
	cfg.LocalContainer.CPUs = getenvInt("CRABBOX_LOCAL_CONTAINER_CPUS", cfg.LocalContainer.CPUs)
	cfg.LocalContainer.Memory = getenv("CRABBOX_LOCAL_CONTAINER_MEMORY", cfg.LocalContainer.Memory)
	cfg.LocalContainer.Network = getenv("CRABBOX_LOCAL_CONTAINER_NETWORK", cfg.LocalContainer.Network)
	if value, ok := getenvBool("CRABBOX_LOCAL_CONTAINER_DOCKER_SOCKET"); ok {
		cfg.LocalContainer.DockerSocket = value
	}
	cfg.AppleContainer.CLIPath = getenv("CRABBOX_APPLE_CONTAINER_CLI", cfg.AppleContainer.CLIPath)
	if image := os.Getenv("CRABBOX_APPLE_CONTAINER_IMAGE"); image != "" {
		cfg.AppleContainer.Image = image
		cfg.appleContainerImageExplicit = true
	}
	cfg.AppleContainer.User = getenv("CRABBOX_APPLE_CONTAINER_USER", cfg.AppleContainer.User)
	cfg.AppleContainer.WorkRoot = getenv("CRABBOX_APPLE_CONTAINER_WORK_ROOT", cfg.AppleContainer.WorkRoot)
	cfg.AppleContainer.CPUs = getenvInt("CRABBOX_APPLE_CONTAINER_CPUS", cfg.AppleContainer.CPUs)
	cfg.AppleContainer.Memory = getenv("CRABBOX_APPLE_CONTAINER_MEMORY", cfg.AppleContainer.Memory)
	if extra := strings.Fields(os.Getenv("CRABBOX_APPLE_CONTAINER_EXTRA_RUN_ARGS")); len(extra) > 0 {
		cfg.AppleContainer.ExtraRunArgs = extra
	}
	cfg.AppleVZ.HelperPath = getenv("CRABBOX_APPLE_VZ_HELPER", cfg.AppleVZ.HelperPath)
	if image := os.Getenv("CRABBOX_APPLE_VZ_IMAGE"); image != "" {
		cfg.AppleVZ.Image = image
		cfg.AppleVZ.ImageSHA256 = ""
		cfg.appleVZImageExplicit = true
		cfg.appleVZImageSHA256Explicit = false
	}
	if checksum := os.Getenv("CRABBOX_APPLE_VZ_IMAGE_SHA256"); checksum != "" {
		cfg.AppleVZ.ImageSHA256 = checksum
		cfg.appleVZImageSHA256Explicit = true
	}
	cfg.AppleVZ.User = getenv("CRABBOX_APPLE_VZ_USER", cfg.AppleVZ.User)
	cfg.AppleVZ.WorkRoot = getenv("CRABBOX_APPLE_VZ_WORK_ROOT", cfg.AppleVZ.WorkRoot)
	if rawCPUs := os.Getenv("CRABBOX_APPLE_VZ_CPUS"); rawCPUs != "" {
		cpus, err := strconv.Atoi(strings.TrimSpace(rawCPUs))
		if err != nil {
			return fmt.Errorf("CRABBOX_APPLE_VZ_CPUS must be an integer: %w", err)
		}
		cfg.AppleVZ.CPUs = cpus
		cfg.appleVZCPUsExplicit = true
	}
	if rawMemory := os.Getenv("CRABBOX_APPLE_VZ_MEMORY"); rawMemory != "" {
		memoryMiB, err := strconv.Atoi(strings.TrimSpace(rawMemory))
		if err != nil {
			return fmt.Errorf("CRABBOX_APPLE_VZ_MEMORY must be an integer: %w", err)
		}
		cfg.AppleVZ.MemoryMiB = memoryMiB
		cfg.appleVZMemoryExplicit = true
	}
	if rawDisk := os.Getenv("CRABBOX_APPLE_VZ_DISK"); rawDisk != "" {
		diskGiB, err := strconv.Atoi(strings.TrimSpace(rawDisk))
		if err != nil {
			return fmt.Errorf("CRABBOX_APPLE_VZ_DISK must be an integer: %w", err)
		}
		cfg.AppleVZ.DiskGiB = diskGiB
		cfg.appleVZDiskExplicit = true
	}
	cfg.MXC.CLIPath = getenv("CRABBOX_MXC_CLI", cfg.MXC.CLIPath)
	cfg.MXC.Version = getenv("CRABBOX_MXC_VERSION", cfg.MXC.Version)
	cfg.MXC.Containment = getenv("CRABBOX_MXC_CONTAINMENT", cfg.MXC.Containment)
	cfg.MXC.Network = getenv("CRABBOX_MXC_NETWORK", cfg.MXC.Network)
	if value := os.Getenv("CRABBOX_MXC_READONLY_PATHS"); value != "" {
		cfg.MXC.ReadOnlyPaths = splitCommaList(value)
	}
	if value := os.Getenv("CRABBOX_MXC_READWRITE_PATHS"); value != "" {
		cfg.MXC.ReadWritePaths = splitCommaList(value)
	}
	if value := os.Getenv("CRABBOX_MXC_ALLOWED_HOSTS"); value != "" {
		cfg.MXC.AllowedHosts = splitCommaList(value)
	}
	if value := os.Getenv("CRABBOX_MXC_BLOCKED_HOSTS"); value != "" {
		cfg.MXC.BlockedHosts = splitCommaList(value)
	}
	if value, ok := getenvBool("CRABBOX_MXC_ALLOW_DACL_MUTATION"); ok {
		cfg.MXC.AllowDACLMutation = value
	}
	if value, ok := getenvBool("CRABBOX_MXC_ALLOW_WINDOWS_UI"); ok {
		cfg.MXC.AllowWindowsUI = value
	}
	if value, ok := getenvBool("CRABBOX_MXC_EXPERIMENTAL"); ok {
		cfg.MXC.Experimental = value
	}
	cfg.Multipass.CLIPath = getenv("CRABBOX_MULTIPASS_CLI", cfg.Multipass.CLIPath)
	if image := os.Getenv("CRABBOX_MULTIPASS_IMAGE"); image != "" {
		cfg.Multipass.Image = image
		cfg.multipassImageExplicit = true
	}
	cfg.Multipass.User = getenv("CRABBOX_MULTIPASS_USER", cfg.Multipass.User)
	cfg.Multipass.WorkRoot = getenv("CRABBOX_MULTIPASS_WORK_ROOT", cfg.Multipass.WorkRoot)
	cfg.Multipass.CPUs = getenvInt("CRABBOX_MULTIPASS_CPUS", cfg.Multipass.CPUs)
	cfg.Multipass.Memory = getenv("CRABBOX_MULTIPASS_MEMORY", cfg.Multipass.Memory)
	cfg.Multipass.Disk = getenv("CRABBOX_MULTIPASS_DISK", cfg.Multipass.Disk)
	if timeout := os.Getenv("CRABBOX_MULTIPASS_LAUNCH_TIMEOUT"); timeout != "" {
		applyLeaseDuration(&cfg.Multipass.LaunchTimeout, timeout)
	}
	if image := os.Getenv("CRABBOX_TART_IMAGE"); image != "" {
		cfg.Tart.Image = image
		cfg.tartImageExplicit = true
	}
	cfg.Tart.User = getenv("CRABBOX_TART_USER", cfg.Tart.User)
	cfg.Tart.Password = getenv("CRABBOX_TART_PASSWORD", cfg.Tart.Password)
	cfg.Tart.WorkRoot = getenv("CRABBOX_TART_WORK_ROOT", cfg.Tart.WorkRoot)
	if v := os.Getenv("CRABBOX_TART_CPUS"); v != "" {
		cfg.Tart.CPUs = getenvInt("CRABBOX_TART_CPUS", cfg.Tart.CPUs)
		cfg.tartCPUsExplicit = true
	}
	if v := os.Getenv("CRABBOX_TART_MEMORY"); v != "" {
		cfg.Tart.Memory = getenvInt("CRABBOX_TART_MEMORY", cfg.Tart.Memory)
		cfg.tartMemoryExplicit = true
	}
	if v := os.Getenv("CRABBOX_TART_DISK"); v != "" {
		cfg.Tart.Disk = getenvInt("CRABBOX_TART_DISK", cfg.Tart.Disk)
		cfg.tartDiskExplicit = cfg.Tart.Disk > 0
	}
	cfg.HyperV.Image = getenv("CRABBOX_HYPERV_IMAGE", cfg.HyperV.Image)
	cfg.HyperV.User = getenv("CRABBOX_HYPERV_USER", cfg.HyperV.User)
	cfg.HyperV.WorkRoot = getenv("CRABBOX_HYPERV_WORK_ROOT", cfg.HyperV.WorkRoot)
	cfg.HyperV.CPUs = getenvInt("CRABBOX_HYPERV_CPUS", cfg.HyperV.CPUs)
	cfg.HyperV.Memory = getenvInt("CRABBOX_HYPERV_MEMORY", cfg.HyperV.Memory)
	cfg.HyperV.Switch = getenv("CRABBOX_HYPERV_SWITCH", cfg.HyperV.Switch)
	cfg.HyperV.GuestPassword = getenv("CRABBOX_HYPERV_GUEST_PASSWORD", cfg.HyperV.GuestPassword)
	if value, ok := getenvBool("CRABBOX_HYPERV_INIT_PASSWORD"); ok {
		cfg.HyperV.InitPassword = value
	}
	cfg.WindowsSandbox.Workdir = getenv("CRABBOX_WINDOWS_SANDBOX_WORKDIR", cfg.WindowsSandbox.Workdir)
	cfg.WindowsSandbox.TempRoot = expandUserPath(getenv("CRABBOX_WINDOWS_SANDBOX_TEMP_ROOT", cfg.WindowsSandbox.TempRoot))
	cfg.WindowsSandbox.Networking = getenv("CRABBOX_WINDOWS_SANDBOX_NETWORKING", cfg.WindowsSandbox.Networking)
	cfg.WindowsSandbox.VGPU = getenv("CRABBOX_WINDOWS_SANDBOX_VGPU", cfg.WindowsSandbox.VGPU)
	cfg.WindowsSandbox.Clipboard = getenv("CRABBOX_WINDOWS_SANDBOX_CLIPBOARD", cfg.WindowsSandbox.Clipboard)
	cfg.WindowsSandbox.ProtectedClient = getenv("CRABBOX_WINDOWS_SANDBOX_PROTECTED_CLIENT", cfg.WindowsSandbox.ProtectedClient)
	cfg.WindowsSandbox.AudioInput = getenv("CRABBOX_WINDOWS_SANDBOX_AUDIO_INPUT", cfg.WindowsSandbox.AudioInput)
	cfg.WindowsSandbox.VideoInput = getenv("CRABBOX_WINDOWS_SANDBOX_VIDEO_INPUT", cfg.WindowsSandbox.VideoInput)
	cfg.WindowsSandbox.PrinterRedirection = getenv("CRABBOX_WINDOWS_SANDBOX_PRINTER_REDIRECTION", cfg.WindowsSandbox.PrinterRedirection)
	cfg.WindowsSandbox.MemoryMB = getenvInt("CRABBOX_WINDOWS_SANDBOX_MEMORY_MB", cfg.WindowsSandbox.MemoryMB)
	if value, ok := getenvBool("CRABBOX_TAILSCALE"); ok {
		cfg.Tailscale.Enabled = value
	}
	if tags := os.Getenv("CRABBOX_TAILSCALE_TAGS"); tags != "" {
		cfg.Tailscale.Tags = normalizeTailscaleTags(splitCommaList(tags))
	}
	cfg.Tailscale.HostnameTemplate = getenv("CRABBOX_TAILSCALE_HOSTNAME_TEMPLATE", cfg.Tailscale.HostnameTemplate)
	cfg.Tailscale.AuthKeyEnv = getenv("CRABBOX_TAILSCALE_AUTH_KEY_ENV", cfg.Tailscale.AuthKeyEnv)
	cfg.Tailscale.ExitNode = getenv("CRABBOX_TAILSCALE_EXIT_NODE", cfg.Tailscale.ExitNode)
	if value, ok := getenvBool("CRABBOX_TAILSCALE_EXIT_NODE_ALLOW_LAN_ACCESS"); ok {
		cfg.Tailscale.ExitNodeAllowLANAccess = value
	}
	if cfg.Tailscale.AuthKeyEnv != "" {
		cfg.Tailscale.AuthKey = getenv(cfg.Tailscale.AuthKeyEnv, "")
	}
	cfg.Static.ID = getenv("CRABBOX_STATIC_ID", cfg.Static.ID)
	cfg.Static.Name = getenv("CRABBOX_STATIC_NAME", cfg.Static.Name)
	cfg.Static.Host = getenv("CRABBOX_STATIC_HOST", cfg.Static.Host)
	cfg.Static.User = getenv("CRABBOX_STATIC_USER", cfg.Static.User)
	cfg.Static.Port = getenv("CRABBOX_STATIC_PORT", cfg.Static.Port)
	cfg.Static.WorkRoot = getenv("CRABBOX_STATIC_WORK_ROOT", cfg.Static.WorkRoot)
	if idleTimeout := os.Getenv("CRABBOX_BLACKSMITH_IDLE_TIMEOUT"); idleTimeout != "" {
		applyLeaseDuration(&cfg.Blacksmith.IdleTimeout, idleTimeout)
	}
	if value, ok := getenvBool("CRABBOX_BLACKSMITH_DEBUG"); ok {
		cfg.Blacksmith.Debug = value
	}
	if labels := os.Getenv("CRABBOX_ACTIONS_RUNNER_LABELS"); labels != "" {
		cfg.Actions.RunnerLabels = splitCommaList(labels)
	}
	if value, ok := getenvBool("CRABBOX_ACTIONS_EPHEMERAL"); ok {
		cfg.Actions.Ephemeral = value
	}
	if junit := os.Getenv("CRABBOX_RESULTS_JUNIT"); junit != "" {
		cfg.Results.JUnit = splitCommaList(junit)
	}
	if value, ok := getenvBool("CRABBOX_RESULTS_AUTO"); ok {
		cfg.Results.Auto = value
	}
	if value, ok := getenvBool("CRABBOX_CACHE_PNPM"); ok {
		cfg.Cache.Pnpm = value
	}
	if value, ok := getenvBool("CRABBOX_CACHE_NPM"); ok {
		cfg.Cache.Npm = value
	}
	if value, ok := getenvBool("CRABBOX_CACHE_DOCKER"); ok {
		cfg.Cache.Docker = value
	}
	if value, ok := getenvBool("CRABBOX_CACHE_GIT"); ok {
		cfg.Cache.Git = value
	}
	cfg.Cache.MaxGB = getenvInt("CRABBOX_CACHE_MAX_GB", cfg.Cache.MaxGB)
	if value, ok := getenvBool("CRABBOX_CACHE_PURGE_ON_RELEASE"); ok {
		cfg.Cache.PurgeOnRelease = value
	}
	if volumes := os.Getenv("CRABBOX_CACHE_VOLUMES"); volumes != "" {
		parsed, err := ParseCacheVolumeSpecs(splitCommaList(volumes))
		if err != nil {
			return err
		}
		cfg.Cache.Volumes = parsed
	}
	if regions := os.Getenv("CRABBOX_CAPACITY_REGIONS"); regions != "" {
		cfg.Capacity.Regions = splitCommaList(regions)
	}
	if zones := os.Getenv("CRABBOX_CAPACITY_AVAILABILITY_ZONES"); zones != "" {
		cfg.Capacity.AvailabilityZones = splitCommaList(zones)
	}
	if value, ok := getenvBool("CRABBOX_SYNC_CHECKSUM"); ok {
		cfg.Sync.Checksum = value
	}
	if value, ok := getenvBool("CRABBOX_SYNC_DELETE"); ok {
		cfg.Sync.Delete = value
	}
	if value, ok := getenvBool("CRABBOX_SYNC_GIT_SEED"); ok {
		cfg.Sync.GitSeed = value
	}
	if value, ok := getenvBool("CRABBOX_SYNC_FINGERPRINT"); ok {
		cfg.Sync.Fingerprint = value
	}
	if timeout := os.Getenv("CRABBOX_SYNC_TIMEOUT"); timeout != "" {
		if parsed, err := time.ParseDuration(timeout); err == nil {
			cfg.Sync.Timeout = parsed
		}
	}
	cfg.Sync.WarnFiles = getenvInt("CRABBOX_SYNC_WARN_FILES", cfg.Sync.WarnFiles)
	cfg.Sync.WarnBytes = int64(getenvInt("CRABBOX_SYNC_WARN_BYTES", int(cfg.Sync.WarnBytes)))
	cfg.Sync.FailFiles = getenvInt("CRABBOX_SYNC_FAIL_FILES", cfg.Sync.FailFiles)
	cfg.Sync.FailBytes = int64(getenvInt("CRABBOX_SYNC_FAIL_BYTES", int(cfg.Sync.FailBytes)))
	if value, ok := getenvBool("CRABBOX_SYNC_ALLOW_LARGE"); ok {
		cfg.Sync.AllowLarge = value
	}
	cfg.Sync.BaseRef = getenv("CRABBOX_SYNC_BASE_REF", cfg.Sync.BaseRef)
	if envAllow := os.Getenv("CRABBOX_ENV_ALLOW"); envAllow != "" {
		cfg.EnvAllow = splitCommaList(envAllow)
	}
	if tools := os.Getenv("CRABBOX_PREFLIGHT_TOOLS"); tools != "" {
		cfg.Run.PreflightTools = normalizePreflightToolNames(splitCommaList(tools))
	}
	return nil
}

func expandUserPath(path string) string {
	if path == "~" {
		home, _ := os.UserHomeDir()
		if home != "" {
			return home
		}
	}
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		if home != "" {
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}

func redactRemoteURL(value string) string {
	value = strings.TrimSpace(value)
	parsed, err := url.Parse(value)
	if err != nil {
		lower := strings.ToLower(value)
		if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
			return "<remote-image>"
		}
		return value
	}
	if !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
		return value
	}
	return "<remote-image>"
}

func serverTypeForClass(class string) string {
	return serverTypeCandidatesForClass(class)[0]
}

func serverTypeForConfig(cfg Config) string {
	if resolved, err := ProviderFor(cfg.Provider); err == nil {
		cfg.Provider = resolved.Name()
		if typer, ok := resolved.(ProviderServerTypeProvider); ok {
			return typer.ServerTypeForConfig(cfg)
		}
	}
	if isBlacksmithProvider(cfg.Provider) || isStaticProvider(cfg.Provider) || cfg.Provider == "islo" || cfg.Provider == "sprites" || cfg.Provider == "local-container" || cfg.Provider == "multipass" {
		return ""
	}
	if cfg.Provider == "namespace-devbox" || cfg.Provider == "namespace" {
		return namespaceDevboxSizeForConfig(cfg)
	}
	if cfg.Provider == "e2b" {
		return blank(cfg.E2B.Template, "base")
	}
	if cfg.Provider == "exe-dev" || cfg.Provider == "exedev" || cfg.Provider == "exe" {
		return blank(cfg.ExeDev.Image, "default")
	}
	if cfg.Provider == "modal" {
		return blank(cfg.Modal.Image, "python:3.13-slim")
	}
	if cfg.Provider == "upstash-box" || cfg.Provider == "upstash" {
		return blank(cfg.UpstashBox.Size, "small")
	}
	if cfg.Provider == "daytona" {
		return "snapshot"
	}
	if cfg.Provider == "cloudflare" {
		return cloudflareContainerInstanceTypeForClass(cfg.Class)
	}
	if cfg.Provider == "aws" {
		return awsInstanceTypeCandidatesForConfig(cfg)[0]
	}
	if cfg.Provider == "azure" {
		return azureVMSizeCandidatesForConfig(cfg)[0]
	}
	if cfg.Provider == "gcp" {
		return gcpMachineTypeCandidatesForClass(cfg.Class)[0]
	}
	if cfg.Provider == "proxmox" {
		return proxmoxServerTypeForConfig(cfg)
	}
	if cfg.Provider == "incus" {
		return incusServerTypeForConfig(cfg)
	}
	if cfg.Provider == "parallels" {
		return parallelsServerTypeForConfig(cfg)
	}
	return serverTypeForClass(cfg.Class)
}

func serverTypeForProviderClass(provider, class string) string {
	if resolved, err := ProviderFor(provider); err == nil {
		provider = resolved.Name()
		if typer, ok := resolved.(ProviderServerTypeProvider); ok {
			return typer.ServerTypeForClass(class)
		}
	}
	if isBlacksmithProvider(provider) || isStaticProvider(provider) || provider == "islo" || provider == "sprites" || provider == "local-container" || provider == "multipass" {
		return ""
	}
	if provider == "namespace-devbox" || provider == "namespace" {
		return namespaceDevboxSizeForClass(class)
	}
	if provider == "e2b" {
		return "base"
	}
	if provider == "exe-dev" {
		return "default"
	}
	if provider == "modal" {
		return "python:3.13-slim"
	}
	if provider == "daytona" {
		return "snapshot"
	}
	if provider == "cloudflare" {
		return cloudflareContainerInstanceTypeForClass(class)
	}
	if provider == "aws" {
		return awsInstanceTypeCandidatesForClass(class)[0]
	}
	if provider == "azure" {
		return azureVMSizeCandidatesForClass(class)[0]
	}
	if provider == "gcp" {
		return gcpMachineTypeCandidatesForClass(class)[0]
	}
	if provider == "proxmox" {
		return "template"
	}
	if provider == "incus" {
		return "container"
	}
	if provider == "parallels" {
		return "template"
	}
	return serverTypeForClass(class)
}

func incusServerTypeForConfig(cfg Config) string {
	instanceType := strings.ToLower(strings.TrimSpace(cfg.Incus.InstanceType))
	if instanceType == "" {
		instanceType = "container"
	}
	if image := strings.TrimSpace(cfg.Incus.Image); image != "" {
		return instanceType + ":" + image
	}
	return instanceType
}

func proxmoxServerTypeForConfig(cfg Config) string {
	if cfg.Proxmox.TemplateID > 0 {
		return "template-" + strconv.Itoa(cfg.Proxmox.TemplateID)
	}
	return "template"
}

func parallelsServerTypeForConfig(cfg Config) string {
	source := strings.TrimSpace(firstNonBlank(cfg.Parallels.Source, cfg.Parallels.SourceID))
	if source == "" {
		if cfg.Parallels.Template != "" {
			return "template-" + normalizeLeaseSlug(cfg.Parallels.Template)
		}
		return "template"
	}
	return "template-" + normalizeLeaseSlug(source)
}

func applyFileParallelsTemplateConfig(template ParallelsTemplateConfig, file fileParallelsTemplateConfig) ParallelsTemplateConfig {
	if file.Source != "" {
		template.Source = file.Source
	}
	if file.SourceID != "" {
		template.SourceID = file.SourceID
	}
	if file.SourceSnapshot != "" {
		template.SourceSnapshot = file.SourceSnapshot
	}
	if file.SourceSnapshotID != "" {
		template.SourceSnapshotID = file.SourceSnapshotID
	}
	if file.Target != "" {
		template.TargetOS = file.Target
	}
	if file.TargetOS != "" {
		template.TargetOS = file.TargetOS
	}
	if file.WindowsMode != "" {
		template.WindowsMode = file.WindowsMode
	}
	if file.CloneMode != "" {
		template.CloneMode = file.CloneMode
	}
	if file.Host != "" {
		template.Host = file.Host
	}
	if file.HostUser != "" {
		template.HostUser = file.HostUser
	}
	if file.HostKey != "" {
		template.HostKey = expandUserPath(file.HostKey)
	}
	if file.VMRoot != "" {
		template.VMRoot = expandUserPath(file.VMRoot)
	}
	if file.User != "" {
		template.User = file.User
	}
	if file.WorkRoot != "" {
		template.WorkRoot = file.WorkRoot
	}
	return template
}

func applyFileParallelsHostConfig(file fileParallelsHostConfig) ParallelsHostConfig {
	return ParallelsHostConfig{
		Name:    strings.TrimSpace(file.Name),
		Host:    strings.TrimSpace(file.Host),
		User:    strings.TrimSpace(file.User),
		Key:     expandUserPath(strings.TrimSpace(file.Key)),
		VMRoot:  expandUserPath(strings.TrimSpace(file.VMRoot)),
		Targets: append([]string(nil), file.Targets...),
		MaxVMs:  file.MaxVMs,
	}
}

func ApplyParallelsTemplateConfig(cfg *Config, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	template, ok := cfg.Parallels.Templates[name]
	if !ok {
		return exit(2, "parallels template %q not found", name)
	}
	cfg.Parallels.Template = name
	if template.Source != "" {
		cfg.Parallels.Source = template.Source
		cfg.Parallels.SourceID = ""
	}
	if template.SourceID != "" {
		cfg.Parallels.SourceID = template.SourceID
	}
	if template.SourceSnapshot != "" {
		cfg.Parallels.SourceSnapshot = template.SourceSnapshot
		cfg.Parallels.SourceSnapshotID = ""
	}
	if template.SourceSnapshotID != "" {
		cfg.Parallels.SourceSnapshotID = template.SourceSnapshotID
	}
	if template.TargetOS != "" {
		cfg.TargetOS = normalizeTargetOS(template.TargetOS)
		if !IsTargetExplicit(cfg) {
			cfg.inferredTargetProvider = parallelsProvider
		}
	}
	if template.WindowsMode != "" {
		cfg.WindowsMode = template.WindowsMode
	}
	if template.CloneMode != "" {
		cfg.Parallels.CloneMode = template.CloneMode
	}
	if template.Host != "" {
		cfg.Parallels.Host = template.Host
	}
	if template.HostUser != "" {
		cfg.Parallels.HostUser = template.HostUser
	}
	if template.HostKey != "" {
		cfg.Parallels.HostKey = template.HostKey
	}
	if template.VMRoot != "" {
		cfg.Parallels.VMRoot = template.VMRoot
	}
	if template.User != "" {
		cfg.Parallels.User = template.User
		cfg.SSHUser = template.User
	}
	if template.WorkRoot != "" {
		cfg.Parallels.WorkRoot = template.WorkRoot
		cfg.WorkRoot = template.WorkRoot
	}
	cfg.parallelsTemplateApplied = true
	return nil
}

func namespaceDevboxSizeForConfig(cfg Config) string {
	if strings.TrimSpace(cfg.Namespace.Size) != "" {
		return strings.ToUpper(strings.TrimSpace(cfg.Namespace.Size))
	}
	if cfg.ServerTypeExplicit && strings.TrimSpace(cfg.ServerType) != "" {
		return strings.ToUpper(strings.TrimSpace(cfg.ServerType))
	}
	return namespaceDevboxSizeForClass(cfg.Class)
}

func namespaceDevboxSizeForClass(class string) string {
	switch strings.ToLower(strings.TrimSpace(class)) {
	case "standard":
		return "S"
	case "fast":
		return "M"
	case "large":
		return "L"
	case "beast":
		return "XL"
	default:
		if class == "" {
			return "M"
		}
		return strings.ToUpper(strings.TrimSpace(class))
	}
}

func cloudflareContainerInstanceTypes() []string {
	return []string{"lite", "basic", "standard-1", "standard-2", "standard-3", "standard-4"}
}

func CloudflareContainerInstanceTypes() []string {
	return cloudflareContainerInstanceTypes()
}

func normalizeCloudflareContainerInstanceType(value string) (string, bool) {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	for _, instanceType := range cloudflareContainerInstanceTypes() {
		if trimmed == instanceType {
			return instanceType, true
		}
	}
	return "", false
}

func NormalizeCloudflareContainerInstanceType(value string) (string, bool) {
	return normalizeCloudflareContainerInstanceType(value)
}

func cloudflareContainerInstanceTypeForClass(class string) string {
	switch strings.ToLower(strings.TrimSpace(class)) {
	case "", "standard", "fast", "large", "beast":
		return "standard-4"
	default:
		if instanceType, ok := normalizeCloudflareContainerInstanceType(class); ok {
			return instanceType
		}
		return strings.TrimSpace(class)
	}
}

func CloudflareContainerInstanceTypeForClass(class string) string {
	return cloudflareContainerInstanceTypeForClass(class)
}

func serverTypeCandidatesForClass(class string) []string {
	switch class {
	case "standard":
		return []string{"ccx33", "cpx62", "cx53"}
	case "fast":
		return []string{"ccx43", "cpx62", "cx53"}
	case "large":
		return []string{"ccx53", "ccx43", "cpx62", "cx53"}
	case "beast":
		return []string{"ccx63", "ccx53", "ccx43", "cpx62", "cx53"}
	default:
		return []string{class}
	}
}

func awsInstanceTypeCandidatesForTargetClass(target, class string) []string {
	return awsInstanceTypeCandidatesForTargetModeClass(target, windowsModeNormal, class)
}

func awsInstanceTypeCandidatesForConfig(cfg Config) []string {
	return awsInstanceTypeCandidatesForTargetModeArchitectureClass(cfg.TargetOS, cfg.WindowsMode, effectiveArchitectureForConfig(cfg), cfg.Class)
}

func awsInstanceTypeCandidatesForTargetModeClass(target, windowsMode, class string) []string {
	return awsInstanceTypeCandidatesForTargetModeArchitectureClass(target, windowsMode, ArchitectureAMD64, class)
}

func awsInstanceTypeCandidatesForTargetModeArchitectureClass(target, windowsMode, architecture, class string) []string {
	switch target {
	case targetMacOS:
		return awsMacOSInstanceTypeCandidates()
	case targetWindows:
		if windowsMode == windowsModeWSL2 {
			switch class {
			case "standard":
				return []string{"m8i.large", "m8i-flex.large", "c8i.large", "r8i.large"}
			case "fast":
				return []string{"m8i.xlarge", "m8i-flex.xlarge", "c8i.xlarge", "r8i.xlarge"}
			case "large":
				return []string{"m8i.2xlarge", "m8i-flex.2xlarge", "c8i.2xlarge", "r8i.2xlarge"}
			case "beast":
				return []string{"m8i.4xlarge", "m8i-flex.4xlarge", "c8i.4xlarge", "r8i.4xlarge", "m8i.2xlarge"}
			default:
				return []string{class}
			}
		}
		switch class {
		case "standard":
			return []string{"m7i.large", "m7a.large", "t3.large"}
		case "fast":
			return []string{"m7i.xlarge", "m7a.xlarge", "t3.xlarge"}
		case "large":
			return []string{"m7i.2xlarge", "m7a.2xlarge", "t3.2xlarge"}
		case "beast":
			return []string{"m7i.4xlarge", "m7a.4xlarge", "m7i.2xlarge"}
		default:
			return []string{class}
		}
	default:
		return awsInstanceTypeCandidatesForArchitectureClass(architecture, class)
	}
}

func awsMacOSInstanceTypeCandidates() []string {
	return []string{
		"mac2.metal",
		"mac2-m2.metal",
		"mac2-m2pro.metal",
		"mac-m4.metal",
		"mac-m4pro.metal",
		"mac-m4max.metal",
		"mac2-m1ultra.metal",
		"mac-m3ultra.metal",
		"mac1.metal",
	}
}

func awsInstanceTypeCandidatesForClass(class string) []string {
	return awsInstanceTypeCandidatesForArchitectureClass(ArchitectureAMD64, class)
}

func awsInstanceTypeCandidatesForArchitectureClass(architecture, class string) []string {
	if architecture == ArchitectureARM64 {
		return awsARM64InstanceTypeCandidatesForClass(class)
	}
	switch class {
	case "standard":
		return []string{"c7a.8xlarge", "c7i.8xlarge", "m7a.8xlarge", "m7i.8xlarge", "c7a.4xlarge"}
	case "fast":
		return []string{"c7a.16xlarge", "c7i.16xlarge", "m7a.16xlarge", "m7i.16xlarge", "c7a.12xlarge", "c7a.8xlarge"}
	case "large":
		return []string{"c7a.24xlarge", "c7i.24xlarge", "m7a.24xlarge", "m7i.24xlarge", "r7a.24xlarge", "c7a.16xlarge", "c7a.12xlarge"}
	case "beast":
		return []string{"c7a.48xlarge", "c7i.48xlarge", "m7a.48xlarge", "m7i.48xlarge", "r7a.48xlarge", "c7a.32xlarge", "c7i.32xlarge", "m7a.32xlarge", "c7a.24xlarge", "c7a.16xlarge"}
	default:
		return []string{class}
	}
}

func awsARM64InstanceTypeCandidatesForClass(class string) []string {
	switch class {
	case "standard":
		return []string{"c7g.8xlarge", "m7g.8xlarge", "r7g.8xlarge", "c7g.4xlarge"}
	case "fast":
		return []string{"c7g.16xlarge", "m7g.16xlarge", "r7g.16xlarge", "c7g.12xlarge", "c7g.8xlarge"}
	case "large":
		return []string{"c7g.16xlarge", "m7g.16xlarge", "r7g.16xlarge", "c7g.12xlarge"}
	case "beast":
		return []string{"c7g.16xlarge", "m7g.16xlarge", "r7g.16xlarge", "c7g.12xlarge"}
	default:
		return []string{class}
	}
}

func awsInstanceTypeIsARM64(instanceType string) bool {
	name := strings.ToLower(strings.SplitN(instanceType, ".", 2)[0])
	switch name {
	case "a1", "g5g", "hpc7g", "i4g", "im4gn", "is4gen", "t4g", "x2gd":
		return true
	}
	for _, prefix := range []string{"c", "m", "r"} {
		if strings.HasPrefix(name, prefix) && awsGravitonFamilySuffix(strings.TrimPrefix(name, prefix)) {
			return true
		}
	}
	return false
}

func awsGravitonFamilySuffix(value string) bool {
	digitEnd := 0
	for digitEnd < len(value) && value[digitEnd] >= '0' && value[digitEnd] <= '9' {
		digitEnd++
	}
	if digitEnd == 0 {
		return false
	}
	switch value[digitEnd:] {
	case "g", "gd", "gn":
		return true
	default:
		return false
	}
}

func getenv(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func getenvInt(name string, fallback int) int {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func getenvNonNegativeInt(name string, fallback int) (int, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, exit(2, "%s must be an integer", name)
	}
	if parsed < 0 {
		return 0, exit(2, "%s must be non-negative", name)
	}
	return parsed, nil
}

func getenvInt32(name string, fallback int32) int32 {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		return fallback
	}
	return int32(n)
}

func getenvFloat(name string, fallback float64) float64 {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return fallback
	}
	return n
}

func getenvBool(name string) (bool, bool) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return false, false
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	default:
		return false, false
	}
}

func getenvList(name string) ([]string, bool) {
	value, ok := os.LookupEnv(name)
	if !ok {
		return nil, false
	}
	if strings.EqualFold(strings.TrimSpace(value), "none") {
		return []string{}, true
	}
	return splitCommaList(value), true
}

func splitCommaList(value string) []string {
	parts := strings.Split(value, ",")
	return normalizeList(parts)
}

func normalizeList(values []string) []string {
	out := make([]string, 0, len(values))
	for _, part := range values {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func appendUniqueStrings(values []string, extra ...string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values)+len(extra))
	for _, value := range append(values, extra...) {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}
