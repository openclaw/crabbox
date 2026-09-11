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
	providerSelectionSource       providerSelectionSource
	externalDesktopCredentialName string
	externalDesktopCredential     transientSecret
	externalDesktopEnvDenylist    []string
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
	imageRequirements             imageRequirements
	Code                          bool
	Network                       NetworkMode
	Class                         string
	classFlagExplicit             bool
	classExplicitOrder            uint64
	explicitSelectionOrder        uint64
	Pond                          string
	ExposedPorts                  []string
	ServerType                    string
	ServerTypeExplicit            bool
	Coordinator                   string
	BrokerMode                    BrokerMode
	brokerProvider                string
	BrokerLoginRedirectOrigins    []string
	BrokerAutoWebVNC              bool
	macOSPortalAuto               bool
	macOSPortalCoordinator        string
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
	AWSSSHCIDRsPinned             bool
	AWSMacHostID                  string
	AWSLambdaMicroVM              AWSLambdaMicroVMConfig
	AzureSubscription             string
	AzureTenant                   string
	AzureClientID                 string
	AzureLocation                 string
	AzureBackend                  string
	AzureResourceGroup            string
	AzureImage                    string
	azureImageExplicit            bool
	AzureSnapshot                 string
	AzureSnapshotSKU              string
	AzureOSDisk                   string
	AzureOSDiskExplicit           bool
	AzureOSDiskSKU                string
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
	Vultr                         VultrConfig
	Linode                        LinodeConfig
	linodeImageExplicit           bool
	linodeTypeExplicit            bool
	GitHubCodespaces              GitHubCodespacesConfig
	githubCodespacesRetentionSet  bool
	Lambda                        LambdaConfig
	lambdaImageExplicit           bool
	lambdaImageFamilyExplicit     bool
	lambdaTypeExplicit            bool
	Nebius                        NebiusConfig
	OVH                           OVHConfig
	ovhImageExplicit              bool
	Scaleway                      ScalewayConfig
	scalewayRegionExplicit        bool
	scalewayZoneExplicit          bool
	scalewayImageExplicit         bool
	scalewayTypeExplicit          bool
	TencentCloud                  TencentCloudConfig
	tencentCloudRegionExplicit    bool
	tencentCloudZoneExplicit      bool
	tencentCloudImageExplicit     bool
	tencentCloudTypeExplicit      bool
	Incus                         IncusConfig
	Proxmox                       ProxmoxConfig
	Firecracker                   FirecrackerConfig
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
	envAllowOverriddenByEnv       bool
	Capacity                      CapacityConfig
	capacityMarketExplicit        bool
	Actions                       ActionsConfig
	Blacksmith                    BlacksmithConfig
	KubeVirt                      KubeVirtConfig
	SealosDevbox                  SealosDevboxConfig
	sealosDevboxWorkRootExplicit  bool
	AgentSandbox                  AgentSandboxConfig
	deleteOnReleaseExplicit       map[string]bool
	External                      ExternalConfig
	Namespace                     NamespaceConfig
	NamespaceInstance             NamespaceInstanceConfig
	Phala                         PhalaConfig
	phalaTypeExplicitOrder        uint64
	Boxd                          BoxdConfig
	boxdWorkRootExplicit          bool
	Coder                         CoderConfig
	Morph                         MorphConfig
	Daytona                       DaytonaConfig
	E2B                           E2BConfig
	CubeSandbox                   CubeSandboxConfig
	ExeDev                        ExeDevConfig
	Railway                       RailwayConfig
	FastAPICloud                  FastAPICloudConfig
	UnikraftCloud                 UnikraftCloudConfig
	Runpod                        RunpodConfig
	Vast                          VastConfig
	vastWorkRootExplicit          bool
	NvidiaBrev                    NvidiaBrevConfig
	nvidiaBrevWorkRootExplicit    bool
	Hostinger                     HostingerConfig
	hostingerUserExplicit         bool
	hostingerWorkRootExplicit     bool
	Wandb                         WandbConfig
	Orgo                          OrgoConfig
	Islo                          IsloConfig
	isloImageExplicit             bool
	isloVCPUsExplicit             bool
	isloMemoryMBExplicit          bool
	isloDiskGBExplicit            bool
	Freestyle                     FreestyleConfig
	Tenki                         TenkiConfig
	Tensorlake                    TensorlakeConfig
	Cua                           CuaConfig
	OpenComputer                  OpenComputerConfig
	CodeSandbox                   CodeSandboxConfig
	OpenSandbox                   OpenSandboxConfig
	Nomad                         NomadConfig
	Blaxel                        BlaxelConfig
	VercelSandbox                 VercelSandboxConfig
	CloudflareSandbox             CloudflareSandboxConfig
	Superserve                    SuperserveConfig
	Crownest                      CrownestConfig
	DockerSandbox                 DockerSandboxConfig
	AnthropicSRT                  AnthropicSRTConfig
	CloudRunSandbox               CloudRunSandboxConfig
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
	localContainerRootExplicit    bool
	AppleContainer                AppleContainerConfig
	appleContainerImageExplicit   bool
	AppleVM                       AppleVMConfig
	appleVMImageExplicit          bool
	appleVMImageSHA256Explicit    bool
	appleVMCPUsExplicit           bool
	appleVMMemoryExplicit         bool
	appleVMDiskExplicit           bool
	MXC                           MXCConfig
	Multipass                     MultipassConfig
	multipassImageExplicit        bool
	Machine0                      Machine0Config
	Tart                          TartConfig
	tartImageExplicit             bool
	tartDiskExplicit              bool
	tartCPUsExplicit              bool
	tartMemoryExplicit            bool
	Lume                          LumeConfig
	HyperV                        HyperVConfig
	WindowsSandbox                WindowsSandboxConfig
	Tailscale                     TailscaleConfig
	Static                        StaticConfig
	Results                       ResultsConfig
	Shard                         ShardConfig
	Cache                         CacheConfig
	Profiles                      map[string]ProfileConfig
	Presets                       map[string]PresetConfig
	ProofTemplates                map[string]ProofTemplateConfig
	Jobs                          map[string]JobConfig
}

type providerSelectionSource string

const providerSelectionRequiredDiagnostic = "no provider selected; use --provider <name>, set CRABBOX_PROVIDER, or configure a provider; run 'crabbox providers recommend' to compare options"

const (
	providerSelectionCompiledDefault providerSelectionSource = "compiled_default"
	providerSelectionUserConfig      providerSelectionSource = "user_config"
	providerSelectionRepoConfig      providerSelectionSource = "repo_config"
	providerSelectionEnvironment     providerSelectionSource = "environment"
	providerSelectionFlag            providerSelectionSource = "flag"
	providerSelectionRecordedRun     providerSelectionSource = "recorded_run"
	providerSelectionLeaseContext    providerSelectionSource = "lease_context"
)

func setProviderSelection(cfg *Config, provider string, source providerSelectionSource) {
	cfg.Provider = provider
	cfg.providerSelectionSource = source
}

func providerSelectionIsActionable(cfg Config) bool {
	if strings.TrimSpace(cfg.Provider) == "" {
		return false
	}
	switch cfg.providerSelectionSource {
	case providerSelectionUserConfig,
		providerSelectionRepoConfig,
		providerSelectionEnvironment,
		providerSelectionFlag,
		providerSelectionRecordedRun,
		providerSelectionLeaseContext:
		return true
	default:
		return false
	}
}

func providerSelectionIsAuthoritativeRoute(cfg Config) bool {
	switch cfg.providerSelectionSource {
	case providerSelectionRecordedRun, providerSelectionLeaseContext:
		return true
	default:
		return false
	}
}

func providerSelectionSourceForConfigPath(trust configPathTrust) providerSelectionSource {
	if !trust.trusted {
		return providerSelectionRepoConfig
	}
	return providerSelectionUserConfig
}

type SyncConfig struct {
	Excludes    []string
	Includes    []string
	Delete      bool
	Checksum    bool
	GitSeed     bool
	GitOverlay  bool
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

// GitHubCodespacesConfig is intentionally token-free. Authentication comes
// from the GitHub CLI credential store or GitHub's standard environment
// variables at the point of use, never from Crabbox config or argv.
type GitHubCodespacesConfig struct {
	APIURL           string
	GHPath           string
	Repo             string
	Ref              string
	Machine          string
	DevcontainerPath string
	WorkingDirectory string
	Geo              string
	IdleTimeout      time.Duration
	RetentionPeriod  time.Duration
	DeleteOnRelease  bool
	WorkRoot         string
}

// NebiusConfig is intentionally non-secret. Authentication stays in the
// Nebius CLI profile store and is never accepted as Crabbox config or argv.
type NebiusConfig struct {
	CLI              string
	Profile          string
	ParentID         string
	SubnetID         string
	Platform         string
	Preset           string
	ImageFamily      string
	DiskType         string
	DiskSizeGiB      int
	User             string
	PublicIP         string
	SecurityGroupIDs []string
	ServiceAccountID string
	RecoveryPolicy   string
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

type ExternalConfig struct {
	Command                  string
	Args                     []string
	Config                   map[string]any
	Capabilities             ExternalCapabilitiesConfig
	Lifecycle                ExternalLifecycleConfig
	Connection               ExternalConnectionConfig
	WorkRoot                 string
	RoutingFile              string
	routingLoaded            bool
	routingCredentialVersion int
	routingDigest            string
	routingGeneration        string
	routingTargetOS          string
	routingWindowsMode       string
	routingArchitecture      string
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
	AllowConfigArgv   bool              `yaml:"allowConfigArgv,omitempty" json:"allowConfigArgv,omitempty"`
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
	Desktop              ExternalDesktopConfig       `yaml:"desktop,omitempty" json:"desktop,omitzero"`
}

type ExternalSSHConnectionConfig struct {
	User                string   `yaml:"user,omitempty" json:"user,omitempty"`
	Host                string   `yaml:"host,omitempty" json:"host,omitempty"`
	Key                 string   `yaml:"key,omitempty" json:"key,omitempty"`
	Port                string   `yaml:"port,omitempty" json:"port,omitempty"`
	FallbackPorts       []string `yaml:"fallbackPorts,omitempty" json:"fallbackPorts,omitempty"`
	ReadyCheck          string   `yaml:"readyCheck,omitempty" json:"readyCheck,omitempty"`
	AuthSecret          bool     `yaml:"authSecret,omitempty" json:"authSecret,omitempty"`
	NoControlMaster     bool     `yaml:"noControlMaster,omitempty" json:"noControlMaster,omitempty"`
	SSHConfigProxy      bool     `yaml:"sshConfigProxy,omitempty" json:"sshConfigProxy,omitempty"`
	ProxyCommand        string   `yaml:"proxyCommand,omitempty" json:"proxyCommand,omitempty"`
	AllowEnv            bool     `yaml:"allowEnv,omitempty" json:"allowEnv,omitempty"`
	TrustProviderOutput bool     `yaml:"trustProviderOutput,omitempty" json:"trustProviderOutput,omitempty"`
}

type ExternalDesktopConfig struct {
	Username    string `yaml:"username,omitempty" json:"username,omitempty"`
	PasswordEnv string `yaml:"passwordEnv,omitempty" json:"passwordEnv,omitempty"`
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

type CubeSandboxConfig struct {
	APIKey        string
	APIURL        string
	Domain        string
	Template      string
	Workdir       string
	User          string
	ProxyNodeIP   string
	ProxyPortHTTP int
	ProxyScheme   string
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

type UnikraftCloudConfig struct {
	APIKey   string
	APIURL   string
	Metro    string
	Image    string
	MemoryMB int
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

// NomadConfig configures the delegated Nomad provider. The ACL token is
// intentionally absent: it is read at runtime from NOMAD_TOKEN or TokenEnv and
// is never persisted in Crabbox config or placed on argv.
type NomadConfig struct {
	Address           string
	Region            string
	Namespace         string
	TokenEnv          string
	CACert            string
	CAPath            string
	ClientCert        string
	ClientKey         string
	TLSServerName     string
	SkipVerify        bool
	Task              string
	Driver            string
	Image             string
	Workdir           string
	JobSpecTemplate   string
	NodePool          string
	Datacenters       []string
	CPU               int
	MemoryMB          int
	DiskMB            int
	AllocReadyTimeout time.Duration
	EvalTimeout       time.Duration
	ExecTimeoutSecs   int
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

// CrownestConfig configures the delegated CrowNest provider. The API key is
// intentionally absent: it is read at runtime from
// CRABBOX_CROWNEST_API_KEY / CROWNEST_API_KEY and sent only in request headers,
// never persisted in Crabbox config or placed on argv.
type CrownestConfig struct {
	APIURL        string
	ProjectID     string
	Template      string
	TimeoutSecs   int
	ForgetMissing bool
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

type AsciiBoxConfig struct {
	APIKey  string
	BaseURL string
	CLIPath string
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

type FirecrackerConfig struct {
	Binary          string
	Jailer          string
	Kernel          string
	RootFS          string
	User            string
	WorkRoot        string
	CPUs            int
	MemoryMiB       int
	DiskMiB         int
	Network         string
	CNINetwork      string
	CNIConfDir      string
	CNIBinDir       string
	LaunchTimeout   time.Duration
	DeleteOnRelease bool
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

// A nonempty selector replaces the whole layer's name/UUID pair; two empty inputs inherit it.
func applyXCPNgNameUUIDPair(dstName, dstUUID *string, incomingName, incomingUUID string) {
	if incomingName == "" && incomingUUID == "" {
		return
	}
	*dstName = incomingName
	*dstUUID = incomingUUID
}

type IncusConfig struct {
	CheckpointMetadata map[string]string `yaml:"-" json:"-"`
	Remote             string
	Project            string
	Address            string
	Socket             string
	InstanceType       string
	Image              string
	Profile            string
	User               string
	WorkRoot           string
	DeleteOnRelease    bool
	StartTimeout       time.Duration
	LaunchPort         string
	ProxyListenHost    string
	ProxyListenPort    string
	ProxyDevice        string
	TLSServerCert      string
	InsecureTLS        bool
	RemoteImageServer  string
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
	hostSource       credentialValueSource
	hostKeySource    credentialValueSource
}

type ParallelsHostConfig struct {
	Name       string
	Host       string
	User       string
	Key        string
	VMRoot     string
	Targets    []string
	MaxVMs     int
	hostSource credentialValueSource
	keySource  credentialValueSource
}

type SpritesConfig struct {
	Token    string
	APIURL   string
	WorkRoot string
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

type Machine0Config struct {
	CLIPath       string
	Image         string
	ImageVersion  int
	DesktopImage  string
	Size          string
	SizeExplicit  bool
	Region        string
	Key           string
	WorkRoot      string
	ReleasePolicy string
	CreateTimeout time.Duration
	PollInterval  time.Duration
}

// DefaultTartImage is the immutable built-in image; the Tart adapter verifies its contents.
const DefaultTartImage = "ghcr.io/cirruslabs/macos-sequoia-base@sha256:785c3acb40fa5af6dd5aab96cd60408372c26125e173c14ea417498d086f829c"

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
	JUnit          []string
	Auto           bool
	FailOnFailures bool
}

type ShardConfig struct {
	MaxCount int
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

// ValidateCacheVolumesForProvider checks provider support for configured cache volumes.
func ValidateCacheVolumesForProvider(cfg Config) error {
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
		trust := classifyConfigPath(path)
		freestyleAPIURL := cfg.Freestyle.APIURL
		if err := applyConfigFile(&cfg, path, trust); err != nil {
			return Config{}, err
		}
		if !trust.trusted {
			cfg.Freestyle.APIURL = freestyleAPIURL
		}
	}
	if err := applyEnv(&cfg); err != nil {
		return Config{}, err
	}
	// Validate this cross-provider environment destination before provider
	// dispatch. The selected value may itself be CRABBOX_PROVIDER, so waiting
	// for External provider validation would let that value route around it.
	if err := ValidateExternalDesktopPasswordEnvironmentName(cfg.External.Connection.Desktop.PasswordEnv); err != nil {
		return Config{}, exit(2, "%v", err)
	}
	applyCloudflareDynamicWorkersRepositoryCaps(&cfg)
	if coordinator = strings.TrimSpace(coordinator); coordinator != "" {
		cfg.Coordinator = coordinator
		markCoordinatorDestinationExplicit(&cfg)
	}
	if provider = strings.TrimSpace(provider); provider != "" {
		setProviderSelection(&cfg, provider, providerSelectionFlag)
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

func applyLinuxConnectionDefaults(cfg *Config, defaultSSHUser, defaultSSHPort string) {
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
		cfg.SSHUser = defaultSSHUser
	}
	if cfg.explicitSSHPort != "" {
		cfg.SSHPort = cfg.explicitSSHPort
	} else {
		cfg.SSHPort = defaultSSHPort
	}
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
	if provider, err := ProviderFor(cfg.Provider); err == nil {
		if defaulter, ok := provider.(ProviderConfigDefaulter); ok {
			if err := defaulter.ApplyConfigDefaults(cfg); err != nil {
				return err
			}
			normalizeTargetConfig(cfg)
			return validateTargetConfig(*cfg)
		}
	}
	if cfg.Provider == "digitalocean" {
		if cfg.DigitalOcean.Region == "" {
			cfg.DigitalOcean.Region = DigitalOceanRegionFallback
		}
		if cfg.osImageExplicit && !cfg.digitalOceanImageExplicit {
			if cfg.OSImage == "ubuntu:24.04" {
				cfg.DigitalOcean.Image = "ubuntu-24-04-x64"
			} else {
				cfg.DigitalOcean.Image = ""
			}
		} else if cfg.DigitalOcean.Image == "" {
			cfg.DigitalOcean.Image = DigitalOceanImageFallback
		}
		applyLinuxConnectionDefaults(cfg, baseConfig().SSHUser, baseConfig().SSHPort)
		normalizeTargetConfig(cfg)
		return validateTargetConfig(*cfg)
	}
	if cfg.Provider == "vultr" {
		cfg.Vultr = cfg.Vultr.WithRuntimeDefaults()
		applyLinuxConnectionDefaults(cfg, "root", "22")
		cfg.SSHFallbackPorts = nil
		normalizeTargetConfig(cfg)
		return validateTargetConfig(*cfg)
	}
	if cfg.Provider == "linode" {
		if cfg.Linode.Region == "" {
			cfg.Linode.Region = LinodeConfiguredRegionDefault
		}
		if cfg.osImageExplicit && !cfg.linodeImageExplicit {
			if cfg.OSImage == "ubuntu:24.04" {
				cfg.Linode.Image = "linode/ubuntu24.04"
			} else {
				cfg.Linode.Image = ""
			}
		} else if cfg.Linode.Image == "" {
			cfg.Linode.Image = LinodeImageFallback
		}
		if cfg.Linode.Type == "" {
			cfg.Linode.Type = LinodeConfiguredTypeDefault
		}
		applyLinuxConnectionDefaults(cfg, baseConfig().SSHUser, baseConfig().SSHPort)
		normalizeTargetConfig(cfg)
		return validateTargetConfig(*cfg)
	}
	if cfg.Provider == "lambda" {
		cfg.Lambda = cfg.Lambda.WithRuntimeDefaults()
		if cfg.osImageExplicit && !cfg.lambdaImageExplicit && !cfg.lambdaImageFamilyExplicit {
			if cfg.OSImage == "ubuntu:24.04" {
				cfg.Lambda.ImageFamily = "lambda-stack-24-04"
			} else {
				cfg.Lambda.ImageFamily = ""
			}
		}
		applyLinuxConnectionDefaults(cfg, "ubuntu", "22")
		cfg.SSHFallbackPorts = nil
		normalizeTargetConfig(cfg)
		return validateTargetConfig(*cfg)
	}
	if cfg.Provider == "vast" {
		cfg.Vast.InstanceType = normalizeVastInstanceType(cfg.Vast.InstanceType)
		if cfg.Vast.APIURL == "" {
			cfg.Vast.APIURL = VastConfigDefaultAPIURL
		}
		if cfg.Vast.InstanceType == "" {
			cfg.Vast.InstanceType = VastConfigDefaultInstanceType
		}
		if cfg.Vast.Image == "" {
			cfg.Vast.Image = VastConfigDefaultImage
		}
		if cfg.Vast.Runtype == "" {
			cfg.Vast.Runtype = VastConfigDefaultRuntype
		}
		if cfg.Vast.DiskGB == 0 {
			cfg.Vast.DiskGB = VastConfigDefaultDiskGB
		}
		if cfg.Vast.Order == "" {
			cfg.Vast.Order = VastConfigDefaultOrder
		}
		if cfg.Vast.User == "" {
			cfg.Vast.User = VastConfigDefaultUser
		}
		if cfg.Vast.WorkRoot == "" {
			cfg.Vast.WorkRoot = VastConfigDefaultWorkRoot
		}
		if cfg.Vast.ReleaseAction == "" {
			cfg.Vast.ReleaseAction = VastConfigDefaultReleaseAction
		}
		if !IsTargetExplicit(cfg) {
			cfg.TargetOS = targetLinux
		}
		if cfg.explicitWindowsMode != "" {
			cfg.WindowsMode = cfg.explicitWindowsMode
		} else {
			cfg.WindowsMode = windowsModeNormal
		}
		if cfg.explicitWorkRoot != "" && !IsVastWorkRootExplicit(cfg) {
			cfg.Vast.WorkRoot = cfg.explicitWorkRoot
		}
		cfg.WorkRoot = cfg.Vast.WorkRoot
		if cfg.explicitSSHUser != "" {
			cfg.SSHUser = cfg.explicitSSHUser
		} else {
			cfg.SSHUser = cfg.Vast.User
		}
		if cfg.explicitSSHPort != "" {
			cfg.SSHPort = cfg.explicitSSHPort
		} else {
			cfg.SSHPort = "22"
		}
		cfg.SSHFallbackPorts = nil
		normalizeTargetConfig(cfg)
		return validateTargetConfig(*cfg)
	}
	if cfg.Provider == "nebius" {
		if cfg.Nebius.CLI == "" {
			cfg.Nebius.CLI = "nebius"
		}
		if cfg.Nebius.Platform == "" {
			cfg.Nebius.Platform = "cpu-d3"
		}
		if cfg.Nebius.Preset == "" {
			cfg.Nebius.Preset = "4vcpu-16gb"
		}
		if cfg.Nebius.ImageFamily == "" {
			cfg.Nebius.ImageFamily = "ubuntu24.04-driverless"
		}
		if cfg.Nebius.DiskType == "" {
			cfg.Nebius.DiskType = "network_ssd"
		}
		if cfg.Nebius.DiskSizeGiB == 0 {
			cfg.Nebius.DiskSizeGiB = 50
		}
		if cfg.Nebius.User == "" {
			cfg.Nebius.User = "crabbox"
		}
		if cfg.Nebius.PublicIP == "" {
			cfg.Nebius.PublicIP = "dynamic"
		}
		if cfg.Nebius.RecoveryPolicy == "" {
			cfg.Nebius.RecoveryPolicy = "fail"
		}
		applyLinuxConnectionDefaults(cfg, cfg.Nebius.User, baseConfig().SSHPort)
		normalizeTargetConfig(cfg)
		return validateTargetConfig(*cfg)
	}
	if cfg.Provider == "ovh" {
		if cfg.OVH.Endpoint == "" {
			cfg.OVH.Endpoint = OVHConfigDefaultEndpoint
		}
		if cfg.OVH.Image == "" {
			cfg.OVH.Image = OVHConfigDefaultImage
		}
		if cfg.OVH.Flavor == "" {
			cfg.OVH.Flavor = OVHConfigDefaultFlavor
		}
		applyLinuxConnectionDefaults(cfg, baseConfig().SSHUser, baseConfig().SSHPort)
		normalizeTargetConfig(cfg)
		return validateTargetConfig(*cfg)
	}
	if cfg.Provider == "scaleway" {
		if cfg.Scaleway.Region == "" {
			cfg.Scaleway.Region = ScalewayConfigDefaultRegion
		}
		if cfg.Scaleway.Zone == "" {
			cfg.Scaleway.Zone = ScalewayConfigDefaultZone
		}
		if cfg.osImageExplicit && !cfg.scalewayImageExplicit {
			if cfg.OSImage == "ubuntu:24.04" {
				cfg.Scaleway.Image = "ubuntu_noble"
			} else {
				cfg.Scaleway.Image = ""
			}
		} else if cfg.Scaleway.Image == "" {
			cfg.Scaleway.Image = ScalewayConfigDefaultImage
		}
		if cfg.Scaleway.Type == "" {
			cfg.Scaleway.Type = ScalewayConfigDefaultType
		}
		applyLinuxConnectionDefaults(cfg, "root", "22")
		normalizeTargetConfig(cfg)
		return validateTargetConfig(*cfg)
	}
	if cfg.Provider == "tencentcloud" {
		if cfg.TencentCloud.Region == "" {
			cfg.TencentCloud.Region = TencentCloudRegionFallback
		}
		if cfg.TencentCloud.Zone == "" {
			cfg.TencentCloud.Zone = TencentCloudZoneFallback
		}
		if cfg.TencentCloud.Type == "" {
			cfg.TencentCloud.Type = TencentCloudTypeFallback
		}
		if cfg.TencentCloud.RootGB == 0 {
			cfg.TencentCloud.RootGB = TencentCloudRootGBFallback
		}
		if cfg.TencentCloud.InternetChargeType == "" {
			cfg.TencentCloud.InternetChargeType = TencentCloudInternetChargeTypeFallback
		}
		if cfg.TencentCloud.InternetMaxBandwidthOut == 0 {
			cfg.TencentCloud.InternetMaxBandwidthOut = TencentCloudInternetMaxBandwidthOutFallback
		}
		applyLinuxConnectionDefaults(cfg, "ubuntu", "22")
		cfg.SSHFallbackPorts = nil
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
		cfg.ExeDev.WorkRoot = ResolveInheritedWorkRoot(cfg.ExeDev.WorkRoot, cfg.WorkRoot, ExeDevWorkRootFallback)
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
	if cfg.Provider == "lume" || cfg.Provider == "local-lume" || cfg.Provider == "lume-macos" {
		if cfg.Lume.User != "" {
			cfg.SSHUser = cfg.Lume.User
		}
		if cfg.SSHPort == "" || cfg.SSHPort == baseConfig().SSHPort {
			cfg.SSHPort = "22"
		}
		cfg.SSHFallbackPorts = nil
		if cfg.Lume.WorkRoot != "" {
			cfg.WorkRoot = cfg.Lume.WorkRoot
		}
		if !IsTargetExplicit(cfg) && (cfg.TargetOS == "" || cfg.TargetOS == targetLinux) {
			cfg.TargetOS = targetMacOS
		}
		if !cfg.ServerTypeExplicit && cfg.Lume.Base != "" {
			cfg.ServerType = cfg.Lume.Base
		}
		return nil
	}
	if cfg.Provider == "apple-vm" || cfg.Provider == "applevm" {
		if cfg.AppleVM.User != "" {
			cfg.SSHUser = cfg.AppleVM.User
		}
		if cfg.SSHPort == "" || cfg.SSHPort == baseConfig().SSHPort {
			cfg.SSHPort = "22"
		}
		cfg.SSHFallbackPorts = nil
		base := baseConfig()
		if cfg.AppleVM.WorkRoot != "" && (IsDefaultWorkRoot(cfg.WorkRoot) || cfg.AppleVM.WorkRoot != base.AppleVM.WorkRoot) {
			cfg.WorkRoot = cfg.AppleVM.WorkRoot
		}
		if cfg.TargetOS == "" {
			cfg.TargetOS = targetLinux
		}
		if !cfg.ServerTypeExplicit && cfg.AppleVM.Image != "" {
			cfg.ServerType = redactRemoteURL(cfg.AppleVM.Image)
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
	if cfg.Provider == "coder" {
		base := baseConfig()
		if cfg.SSHUser == "" || cfg.SSHUser == base.SSHUser {
			cfg.SSHUser = "coder"
		}
		if cfg.SSHPort == "" || cfg.SSHPort == base.SSHPort {
			cfg.SSHPort = "22"
		}
		cfg.SSHFallbackPorts = nil
		if !isDefaultWorkRoot(cfg.WorkRoot) && (cfg.Coder.WorkRoot == "" || cfg.Coder.WorkRoot == base.Coder.WorkRoot) {
			cfg.Coder.WorkRoot = cfg.WorkRoot
		} else if cfg.Coder.WorkRoot == "" {
			cfg.Coder.WorkRoot = base.Coder.WorkRoot
		}
		if cfg.Coder.WorkRoot != "" {
			cfg.WorkRoot = cfg.Coder.WorkRoot
		}
		if cfg.TargetOS == "" {
			cfg.TargetOS = targetLinux
		}
		return nil
	}
	if cfg.Provider == "nomad" {
		if !IsTargetExplicit(cfg) {
			cfg.TargetOS = targetLinux
		}
		if cfg.Nomad.Workdir != "" {
			cfg.WorkRoot = cfg.Nomad.Workdir
		}
		cfg.SSHFallbackPorts = nil
		return nil
	}
	if cfg.Provider == "firecracker" {
		base := baseConfig()
		if cfg.Firecracker.User != "" && (cfg.SSHUser == "" || cfg.SSHUser == base.SSHUser || cfg.Firecracker.User != base.Firecracker.User) {
			cfg.SSHUser = cfg.Firecracker.User
		}
		if cfg.SSHPort == "" || cfg.SSHPort == base.SSHPort {
			cfg.SSHPort = "22"
		}
		cfg.SSHFallbackPorts = nil
		if cfg.Firecracker.WorkRoot != "" && (isDefaultWorkRoot(cfg.WorkRoot) || cfg.Firecracker.WorkRoot != base.Firecracker.WorkRoot) {
			cfg.WorkRoot = cfg.Firecracker.WorkRoot
		}
		if cfg.TargetOS == "" {
			cfg.TargetOS = targetLinux
		}
		if !cfg.ServerTypeExplicit {
			cfg.ServerType = firecrackerServerTypeForConfig(*cfg)
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
	appleVMImage, err := osImageDefaultAppleVMImage(cfg.OSImage)
	if err != nil {
		return
	}
	appleVMSHA256, err := osImageDefaultAppleVMSHA256(cfg.OSImage)
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
	if force || cfg.AppleVM.Image == "" || (!cfg.appleVMImageExplicit && (cfg.AppleVM.Image == base.AppleVM.Image || wasOSDefault)) {
		cfg.AppleVM.Image = appleVMImage
	}
	if !cfg.appleVMImageSHA256Explicit && (force || (cfg.AppleVM.ImageSHA256 == "" && cfg.AppleVM.Image == appleVMImage) || (!cfg.appleVMImageExplicit && (cfg.AppleVM.ImageSHA256 == "" || wasOSDefault))) {
		cfg.AppleVM.ImageSHA256 = appleVMSHA256
	}
	if force || cfg.Multipass.Image == "" || (!cfg.multipassImageExplicit && (cfg.Multipass.Image == base.Multipass.Image || wasOSDefault)) {
		cfg.Multipass.Image = multipassImage
	}
	cfg.osImageProviderDefaults = cfg.OSImage
}

func MarkIsloImageExplicit(cfg *Config) {
	cfg.isloImageExplicit = true
}

func MarkCapacityMarketExplicit(cfg *Config) {
	cfg.capacityMarketExplicit = true
}

func CapacityMarketExplicit(cfg Config) bool {
	return cfg.capacityMarketExplicit
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

func MarkLocalContainerWorkRootExplicit(cfg *Config) {
	cfg.localContainerRootExplicit = true
}

func LocalContainerWorkRootExplicit(cfg Config) bool {
	return cfg.localContainerRootExplicit
}

func MarkAppleContainerImageExplicit(cfg *Config) {
	cfg.appleContainerImageExplicit = true
}

func AppleContainerImageExplicit(cfg Config) bool {
	return cfg.appleContainerImageExplicit
}

func MarkAppleVMImageExplicit(cfg *Config) {
	cfg.appleVMImageExplicit = true
	cfg.appleVMImageSHA256Explicit = false
}

func AppleVMImageExplicit(cfg Config) bool {
	return cfg.appleVMImageExplicit
}

func MarkAppleVMImageSHA256Explicit(cfg *Config) {
	cfg.appleVMImageSHA256Explicit = true
}

func AppleVMCPUsExplicit(cfg Config) bool {
	return cfg.appleVMCPUsExplicit
}

func MarkAppleVMCPUsExplicit(cfg *Config) {
	cfg.appleVMCPUsExplicit = true
}

func AppleVMMemoryExplicit(cfg Config) bool {
	return cfg.appleVMMemoryExplicit
}

func MarkAppleVMMemoryExplicit(cfg *Config) {
	cfg.appleVMMemoryExplicit = true
}

func AppleVMDiskExplicit(cfg Config) bool {
	return cfg.appleVMDiskExplicit
}

func MarkAppleVMDiskExplicit(cfg *Config) {
	cfg.appleVMDiskExplicit = true
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
	cfg.credentialProvenance.externalDesktopTarget = credentialSourceFlag
	if normalizeTargetOS(cfg.TargetOS) != targetWindows {
		cfg.WindowsMode = windowsModeNormal
		cfg.credentialProvenance.externalDesktopMode = credentialSourceFlag
	}
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

func IsBoxdWorkRootExplicit(cfg *Config) bool {
	return cfg.boxdWorkRootExplicit
}

func MarkBoxdWorkRootExplicit(cfg *Config) {
	cfg.boxdWorkRootExplicit = true
}

func IsSealosDevboxWorkRootExplicit(cfg *Config) bool {
	return cfg != nil && cfg.sealosDevboxWorkRootExplicit
}

func MarkSealosDevboxWorkRootExplicit(cfg *Config) {
	cfg.sealosDevboxWorkRootExplicit = true
}

func EffectiveSealosDevboxWorkRoot(cfg Config) string {
	if IsSealosDevboxWorkRootExplicit(&cfg) {
		return Blank(strings.TrimSpace(cfg.SealosDevbox.WorkRoot), baseConfig().SealosDevbox.WorkRoot)
	}
	if IsWorkRootExplicit(&cfg) {
		return strings.TrimSpace(cfg.WorkRoot)
	}
	return Blank(strings.TrimSpace(cfg.SealosDevbox.WorkRoot), baseConfig().SealosDevbox.WorkRoot)
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

func IsVastWorkRootExplicit(cfg *Config) bool {
	return cfg.vastWorkRootExplicit
}

func MarkVastWorkRootExplicit(cfg *Config) {
	cfg.vastWorkRootExplicit = true
}

func EffectiveVastWorkRoot(cfg Config) string {
	return resolveExplicitProviderWorkRoot(cfg.Vast.WorkRoot, VastConfigDefaultWorkRoot, cfg.explicitWorkRoot, IsVastWorkRootExplicit(&cfg))
}

func NormalizeVastInstanceType(value string) string {
	return normalizeVastInstanceType(value)
}

func normalizeVastInstanceType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "on-demand", "on_demand":
		return "ondemand"
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func EffectiveNvidiaBrevWorkRoot(cfg Config) string {
	return resolveExplicitProviderWorkRoot(cfg.NvidiaBrev.WorkRoot, "/tmp/crabbox", cfg.explicitWorkRoot, IsNvidiaBrevWorkRootExplicit(&cfg))
}

func resolveExplicitProviderWorkRoot(providerRoot, providerFallback, explicitGenericRoot string, providerExplicit bool) string {
	if !providerExplicit && (providerRoot == "" || providerRoot == providerFallback) && explicitGenericRoot != "" {
		return explicitGenericRoot
	}
	if providerRoot == "" {
		return providerFallback
	}
	return providerRoot
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

func GitHubCodespacesRetentionExplicit(cfg Config) bool {
	return cfg.githubCodespacesRetentionSet
}

func MarkGitHubCodespacesRetentionExplicit(cfg *Config) {
	cfg.githubCodespacesRetentionSet = true
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
		Profile:                 "default",
		Provider:                provider,
		providerSelectionSource: providerSelectionCompiledDefault,
		TargetOS:                "linux",
		Architecture:            ArchitectureAMD64,
		OSImage:                 osImage,
		WindowsMode:             "normal",
		DesktopEnv:              desktopEnvXFCE,
		Network:                 NetworkAuto,
		Class:                   class,
		ServerType:              "",
		BrokerMode:              BrokerModeManaged,
		BrokerAutoWebVNC:        true,
		Location:                "fsn1",
		Image:                   hetznerImage,
		AWSRegion:               "eu-west-1",
		AWSRootGB:               400,
		AWSLambdaMicroVM:        defaultAWSLambdaMicroVMConfig(),
		AzureBackend:            "vm",
		AzureLocation:           "eastus",
		AzureResourceGroup:      "crabbox-leases",
		AzureImage:              azureImage,
		AzureOSDisk:             AzureOSDiskManaged,
		AzureVNet:               "crabbox-vnet",
		AzureSubnet:             "crabbox-subnet",
		AzureNSG:                "crabbox-nsg",
		AzureDynamicSessions:    defaultAzureDynamicSessionsConfig(),
		GCPZone:                 "europe-west2-a",
		GCPImage:                gcpImage,
		GCPNetwork:              "default",
		GCPTags:                 []string{"crabbox-ssh"},
		GCPRootGB:               400,
		DigitalOcean:            defaultDigitalOceanConfig(),
		Vultr:                   defaultVultrConfig(),
		Linode:                  initialLinodeConfig(linodeImage),
		GitHubCodespaces: GitHubCodespacesConfig{
			APIURL:          "https://api.github.com",
			GHPath:          "gh",
			Machine:         "basicLinux32gb",
			IdleTimeout:     30 * time.Minute,
			RetentionPeriod: 7 * 24 * time.Hour,
			DeleteOnRelease: true,
			WorkRoot:        "/workspaces/crabbox",
		},
		Lambda:       initialLambdaConfig(),
		OVH:          defaultOVHConfig(),
		Scaleway:     defaultScalewayConfig(),
		TencentCloud: defaultTencentCloudConfig(),
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
		KubeVirt:     defaultKubeVirtConfig(),
		SealosDevbox: defaultSealosDevboxConfig(),
		AgentSandbox: defaultAgentSandboxConfig(),
		External: ExternalConfig{
			WorkRoot: defaultPOSIXWorkRoot,
		},
		Namespace:         defaultNamespaceConfig(),
		NamespaceInstance: defaultNamespaceInstanceConfig(),
		Phala: PhalaConfig{
			CLIPath:      "phala",
			InstanceType: "tdx.small",
			// The dstack --dev-os guest roots on a read-only squashfs; /work is not
			// writable. /var/volatile is a writable tmpfs on every dstack guest.
			WorkRoot: "/var/volatile/crabbox",
		},
		Boxd: BoxdConfig{
			APIURL:          "https://app.boxd.sh",
			WorkRoot:        "/home/boxd/crabbox",
			DeleteOnRelease: true,
		},
		Coder: defaultCoderConfig(),
		Morph: defaultMorphConfig(),
		Orgo:  defaultOrgoConfig(),
		Daytona: DaytonaConfig{
			APIURL:           "https://app.daytona.io/api",
			User:             "daytona",
			WorkRoot:         "/home/daytona/crabbox",
			SSHGatewayHost:   "ssh.app.daytona.io",
			SSHAccessMinutes: 30,
		},
		E2B: defaultE2BConfig(),
		CubeSandbox: CubeSandboxConfig{
			APIURL:        "http://127.0.0.1:3000",
			Domain:        "cube.app",
			Template:      "",
			Workdir:       "crabbox",
			ProxyPortHTTP: 80,
		},
		ExeDev:       defaultExeDevConfig(),
		Railway:      defaultRailwayConfig(),
		FastAPICloud: defaultFastAPICloudConfig(),
		UnikraftCloud: UnikraftCloudConfig{
			Metro: "fra",
		},
		Runpod: defaultRunpodConfig(),
		Vast:   defaultVastConfig(),
		NvidiaBrev: NvidiaBrevConfig{
			CLI:           "brev",
			GPUName:       "A100",
			Mode:          "vm",
			ReleaseAction: "delete",
			Target:        "container",
			WorkRoot:      "/tmp/crabbox",
		},
		Nebius: NebiusConfig{
			CLI:            "nebius",
			Platform:       "cpu-d3",
			Preset:         "4vcpu-16gb",
			ImageFamily:    "ubuntu24.04-driverless",
			DiskType:       "network_ssd",
			DiskSizeGiB:    50,
			User:           "crabbox",
			PublicIP:       "dynamic",
			RecoveryPolicy: "fail",
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
		Wandb: defaultWandbConfig(),
		Freestyle: FreestyleConfig{
			APIURL:  "https://api.freestyle.sh",
			Workdir: "crabbox",
		},
		Tenki: TenkiConfig{
			CLIPath:  "tenki",
			WorkRoot: "/home/tenki/crabbox",
		},
		Tensorlake:   defaultTensorlakeConfig(),
		Cua:          defaultCuaConfig(),
		OpenComputer: defaultOpenComputerConfig(),
		CodeSandbox:  defaultCodeSandboxConfig(),
		OpenSandbox:  defaultOpenSandboxConfig(),
		Nomad: NomadConfig{
			TokenEnv:          "NOMAD_TOKEN",
			Task:              "crabbox",
			Driver:            "docker",
			Image:             "ubuntu:24.04",
			Workdir:           "/workspace/crabbox",
			Datacenters:       []string{"dc1"},
			CPU:               1000,
			MemoryMB:          2048,
			DiskMB:            1024,
			AllocReadyTimeout: 5 * time.Minute,
			EvalTimeout:       5 * time.Minute,
			ExecTimeoutSecs:   600,
		},
		Blaxel:            defaultBlaxelConfig(),
		VercelSandbox:     defaultVercelSandboxConfig(),
		CloudflareSandbox: defaultCloudflareSandboxConfig(),
		Superserve: SuperserveConfig{
			BaseURL:         "https://api.superserve.ai",
			Template:        "superserve/base",
			Workdir:         "/workspace/crabbox",
			ExecTimeoutSecs: 600,
		},
		Crownest: CrownestConfig{
			APIURL:      "https://api.crownest.dev",
			Template:    "python-node",
			TimeoutSecs: 600,
		},
		DockerSandbox: DockerSandboxConfig{
			CLIPath: "sbx",
			Agent:   "shell",
		},
		AnthropicSRT:    defaultAnthropicSRTConfig(),
		CloudRunSandbox: defaultCloudRunSandboxConfig(),
		Modal:           defaultModalConfig(),
		UpstashBox:      defaultUpstashBoxConfig(),
		Smolvm:          defaultSmolvmConfig(),
		AsciiBox: AsciiBoxConfig{
			BaseURL: "https://ascii.dev",
			CLIPath: "box",
			Workdir: "/home/user/crabbox",
		},
		Cloudflare: defaultCloudflareConfig(),
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
		Firecracker: FirecrackerConfig{
			Binary:          "firecracker",
			Kernel:          "/var/lib/crabbox/firecracker/vmlinux",
			RootFS:          "/var/lib/crabbox/firecracker/rootfs.ext4",
			User:            "crabbox",
			WorkRoot:        defaultPOSIXWorkRoot,
			CPUs:            4,
			MemoryMiB:       4096,
			DiskMiB:         16384,
			Network:         "cni",
			CNINetwork:      "crabbox-firecracker",
			CNIConfDir:      "/etc/cni/conf.d",
			CNIBinDir:       "/opt/cni/bin",
			LaunchTimeout:   2 * time.Minute,
			DeleteOnRelease: true,
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
		LocalContainer: initialLocalContainerConfig(containerImage),
		AppleContainer: initialAppleContainerConfig(containerImage),
		AppleVM:        initialAppleVMConfig(osImageSpecs[osImage].AppleVMImage, osImageSpecs[osImage].AppleVMSHA256),
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
		Machine0: Machine0Config{
			CLIPath:       "machine0",
			Image:         "ubuntu-24-04-loaded",
			Size:          "large",
			Region:        "eu",
			ReleasePolicy: "destroy",
			CreateTimeout: 15 * time.Minute,
			PollInterval:  60 * time.Second,
		},
		Tart: TartConfig{
			Image:    DefaultTartImage,
			User:     "admin",
			WorkRoot: "/Users/admin/crabbox",
			CPUs:     4,
			Memory:   8192,
		},
		Lume: defaultLumeConfig(),
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
	Vultr                    *fileVultrConfig                    `yaml:"vultr,omitempty"`
	Linode                   *fileLinodeConfig                   `yaml:"linode,omitempty"`
	GitHubCodespaces         *fileGitHubCodespacesConfig         `yaml:"githubCodespaces,omitempty"`
	Lambda                   *fileLambdaConfig                   `yaml:"lambda,omitempty"`
	Nebius                   *fileNebiusConfig                   `yaml:"nebius,omitempty"`
	OVH                      *fileOVHConfig                      `yaml:"ovh,omitempty"`
	Scaleway                 *fileScalewayConfig                 `yaml:"scaleway,omitempty"`
	TencentCloud             *fileTencentCloudConfig             `yaml:"tencentcloud,omitempty"`
	AWS                      *fileAWSConfig                      `yaml:"aws,omitempty"`
	AWSLambdaMicroVM         *fileAWSLambdaMicroVMConfig         `yaml:"awsLambdaMicroVM,omitempty"`
	Azure                    *fileAzureConfig                    `yaml:"azure,omitempty"`
	AzureDynamicSessions     *fileAzureDynamicSessionsConfig     `yaml:"azureDynamicSessions,omitempty"`
	GCP                      *fileGCPConfig                      `yaml:"gcp,omitempty"`
	Incus                    *fileIncusConfig                    `yaml:"incus,omitempty"`
	Proxmox                  *fileProxmoxConfig                  `yaml:"proxmox,omitempty"`
	Firecracker              *fileFirecrackerConfig              `yaml:"firecracker,omitempty"`
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
	SealosDevbox             *fileSealosDevboxConfig             `yaml:"sealosDevbox,omitempty"`
	AgentSandbox             *fileAgentSandboxConfig             `yaml:"agentSandbox,omitempty"`
	External                 *fileExternalConfig                 `yaml:"external,omitempty"`
	Namespace                *fileNamespaceConfig                `yaml:"namespace,omitempty"`
	NamespaceInstance        *fileNamespaceInstanceConfig        `yaml:"namespaceInstance,omitempty"`
	Phala                    *filePhalaConfig                    `yaml:"phala,omitempty"`
	Boxd                     *fileBoxdConfig                     `yaml:"boxd,omitempty"`
	Coder                    *fileCoderConfig                    `yaml:"coder,omitempty"`
	Morph                    *fileMorphConfig                    `yaml:"morph,omitempty"`
	Daytona                  *fileDaytonaConfig                  `yaml:"daytona,omitempty"`
	E2B                      *fileE2BConfig                      `yaml:"e2b,omitempty"`
	CubeSandbox              *fileCubeSandboxConfig              `yaml:"cubeSandbox,omitempty"`
	ExeDev                   *fileExeDevConfig                   `yaml:"exeDev,omitempty"`
	Railway                  *fileRailwayConfig                  `yaml:"railway,omitempty"`
	FastAPICloud             *fileFastAPICloudConfig             `yaml:"fastapiCloud,omitempty"`
	UnikraftCloud            *fileUnikraftCloudConfig            `yaml:"unikraftCloud,omitempty"`
	Runpod                   *fileRunpodConfig                   `yaml:"runpod,omitempty"`
	Vast                     *fileVastConfig                     `yaml:"vast,omitempty"`
	NvidiaBrev               *fileNvidiaBrevConfig               `yaml:"nvidiaBrev,omitempty"`
	Hostinger                *fileHostingerConfig                `yaml:"hostinger,omitempty"`
	Wandb                    *fileWandbConfig                    `yaml:"wandb,omitempty"`
	Orgo                     *fileOrgoConfig                     `yaml:"orgo,omitempty"`
	Islo                     *fileIsloConfig                     `yaml:"islo,omitempty"`
	Freestyle                *fileFreestyleConfig                `yaml:"freestyle,omitempty"`
	Tenki                    *fileTenkiConfig                    `yaml:"tenki,omitempty"`
	Tensorlake               *fileTensorlakeConfig               `yaml:"tensorlake,omitempty"`
	Cua                      *fileCuaConfig                      `yaml:"cua,omitempty"`
	OpenComputer             *fileOpenComputerConfig             `yaml:"openComputer,omitempty"`
	CodeSandbox              *fileCodeSandboxConfig              `yaml:"codeSandbox,omitempty"`
	OpenSandbox              *fileOpenSandboxConfig              `yaml:"openSandbox,omitempty"`
	Nomad                    *fileNomadConfig                    `yaml:"nomad,omitempty"`
	Blaxel                   *fileBlaxelConfig                   `yaml:"blaxel,omitempty"`
	VercelSandbox            *fileVercelSandboxConfig            `yaml:"vercelSandbox,omitempty"`
	CloudflareSandbox        *fileCloudflareSandboxConfig        `yaml:"cloudflareSandbox,omitempty"`
	Superserve               *fileSuperserveConfig               `yaml:"superserve,omitempty"`
	Crownest                 *fileCrownestConfig                 `yaml:"crownest,omitempty"`
	DockerSandbox            *fileDockerSandboxConfig            `yaml:"dockerSandbox,omitempty"`
	AnthropicSRT             *fileAnthropicSRTConfig             `yaml:"anthropicSandboxRuntime,omitempty"`
	CloudRunSandbox          *fileCloudRunSandboxConfig          `yaml:"cloudRunSandbox,omitempty"`
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
	AppleVM                  *fileAppleVMConfig                  `yaml:"appleVM,omitempty"`
	AppleVZLegacy            *fileAppleVMConfig                  `yaml:"appleVZ,omitempty"`
	MXC                      *fileMXCConfig                      `yaml:"mxc,omitempty"`
	Multipass                *fileMultipassConfig                `yaml:"multipass,omitempty"`
	Machine0                 *fileMachine0Config                 `yaml:"machine0,omitempty"`
	Tart                     *fileTartConfig                     `yaml:"tart,omitempty"`
	Lume                     *fileLumeConfig                     `yaml:"lume,omitempty"`
	HyperV                   *fileHyperVConfig                   `yaml:"hyperv,omitempty"`
	WindowsSandbox           *fileWindowsSandboxConfig           `yaml:"windowsSandbox,omitempty"`
	Tailscale                *fileTailscaleConfig                `yaml:"tailscale,omitempty"`
	Static                   *fileStaticConfig                   `yaml:"static,omitempty"`
	Results                  *fileResultsConfig                  `yaml:"results,omitempty"`
	Shard                    *fileShardConfig                    `yaml:"shard,omitempty"`
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
	URL                  string            `yaml:"url,omitempty"`
	Mode                 string            `yaml:"mode,omitempty"`
	AutoWebVNC           *bool             `yaml:"autoWebVNC,omitempty"`
	LoginRedirectOrigins []string          `yaml:"loginRedirectOrigins,omitempty"`
	Token                string            `yaml:"token,omitempty"`
	AdminToken           string            `yaml:"adminToken,omitempty"`
	Provider             string            `yaml:"provider,omitempty"`
	Access               *fileAccessConfig `yaml:"access,omitempty"`
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

type fileGitHubCodespacesConfig struct {
	APIURL           string `yaml:"apiUrl,omitempty"`
	GHPath           string `yaml:"ghPath,omitempty"`
	Repo             string `yaml:"repo,omitempty"`
	Ref              string `yaml:"ref,omitempty"`
	Machine          string `yaml:"machine,omitempty"`
	DevcontainerPath string `yaml:"devcontainerPath,omitempty"`
	WorkingDirectory string `yaml:"workingDirectory,omitempty"`
	Geo              string `yaml:"geo,omitempty"`
	IdleTimeout      string `yaml:"idleTimeout,omitempty"`
	RetentionPeriod  string `yaml:"retentionPeriod,omitempty"`
	DeleteOnRelease  *bool  `yaml:"deleteOnRelease,omitempty"`
	WorkRoot         string `yaml:"workRoot,omitempty"`
}

type fileNebiusConfig struct {
	CLI              string   `yaml:"cli,omitempty"`
	Profile          string   `yaml:"profile,omitempty"`
	ParentID         string   `yaml:"parentId,omitempty"`
	SubnetID         string   `yaml:"subnetId,omitempty"`
	Platform         string   `yaml:"platform,omitempty"`
	Preset           string   `yaml:"preset,omitempty"`
	ImageFamily      string   `yaml:"imageFamily,omitempty"`
	DiskType         string   `yaml:"diskType,omitempty"`
	DiskSizeGiB      int      `yaml:"diskSizeGiB,omitempty"`
	User             string   `yaml:"user,omitempty"`
	PublicIP         string   `yaml:"publicIP,omitempty"`
	SecurityGroupIDs []string `yaml:"securityGroupIds,omitempty"`
	ServiceAccountID string   `yaml:"serviceAccountId,omitempty"`
	RecoveryPolicy   string   `yaml:"recoveryPolicy,omitempty"`
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
	SnapshotSKU    string   `yaml:"snapshotSKU,omitempty"`
	OSDiskSKU      string   `yaml:"osDiskSKU,omitempty"`
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

type fileFirecrackerConfig struct {
	Binary          string `yaml:"binary,omitempty"`
	Jailer          string `yaml:"jailer,omitempty"`
	Kernel          string `yaml:"kernel,omitempty"`
	RootFS          string `yaml:"rootfs,omitempty"`
	User            string `yaml:"user,omitempty"`
	WorkRoot        string `yaml:"workRoot,omitempty"`
	CPUs            *int   `yaml:"cpus,omitempty"`
	MemoryMiB       *int   `yaml:"memoryMiB,omitempty"`
	DiskMiB         *int   `yaml:"diskMiB,omitempty"`
	Network         string `yaml:"network,omitempty"`
	CNINetwork      string `yaml:"cniNetwork,omitempty"`
	CNIConfDir      string `yaml:"cniConfDir,omitempty"`
	CNIBinDir       string `yaml:"cniBinDir,omitempty"`
	LaunchTimeout   string `yaml:"launchTimeout,omitempty"`
	DeleteOnRelease *bool  `yaml:"deleteOnRelease,omitempty"`
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
	GitOverlay  *bool    `yaml:"gitOverlay,omitempty"`
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

// BoxdConfig contains non-secret HTTPS console routing and lease settings.
// Interactive session tokens are read only from the environment by the provider.
type BoxdConfig struct {
	APIURL          string
	Org             string // Empty selects the fixed personal account context.
	WorkRoot        string
	DeleteOnRelease bool
}

type fileBoxdConfig struct {
	APIURL          string `yaml:"apiUrl,omitempty"`
	Org             string `yaml:"org,omitempty"`
	WorkRoot        string `yaml:"workRoot,omitempty"`
	DeleteOnRelease *bool  `yaml:"deleteOnRelease,omitempty"`
}

type filePhalaConfig struct {
	CLIPath      string `yaml:"cli,omitempty"`
	InstanceType string `yaml:"instanceType,omitempty"`
	WorkRoot     string `yaml:"workRoot,omitempty"`
	NodeID       string `yaml:"nodeId,omitempty"`
	Compose      string `yaml:"compose,omitempty"`
	Attest       *bool  `yaml:"attest,omitempty"`
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

type fileCubeSandboxConfig struct {
	APIURL        string `yaml:"apiUrl,omitempty"`
	Domain        string `yaml:"domain,omitempty"`
	Template      string `yaml:"template,omitempty"`
	Workdir       string `yaml:"workdir,omitempty"`
	User          string `yaml:"user,omitempty"`
	ProxyNodeIP   string `yaml:"proxyNodeIp,omitempty"`
	ProxyPortHTTP int    `yaml:"proxyPortHttp,omitempty"`
	ProxyScheme   string `yaml:"proxyScheme,omitempty"`
}

type fileFreestyleConfig struct {
	APIURL   string `yaml:"apiUrl,omitempty"`
	Workdir  string `yaml:"workdir,omitempty"`
	VCPUs    int    `yaml:"vcpus,omitempty"`
	MemoryGB int    `yaml:"memoryGB,omitempty"`
}

type fileUnikraftCloudConfig struct {
	APIKey   string `yaml:"apiKey,omitempty"`
	APIURL   string `yaml:"apiUrl,omitempty"`
	Metro    string `yaml:"metro,omitempty"`
	Image    string `yaml:"image,omitempty"`
	MemoryMB int    `yaml:"memoryMB,omitempty"`
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

type fileNomadConfig struct {
	Address           string   `yaml:"address,omitempty"`
	Region            string   `yaml:"region,omitempty"`
	Namespace         string   `yaml:"namespace,omitempty"`
	TokenEnv          string   `yaml:"tokenEnv,omitempty"`
	CACert            string   `yaml:"caCert,omitempty"`
	CAPath            string   `yaml:"caPath,omitempty"`
	ClientCert        string   `yaml:"clientCert,omitempty"`
	ClientKey         string   `yaml:"clientKey,omitempty"`
	TLSServerName     string   `yaml:"tlsServerName,omitempty"`
	SkipVerify        *bool    `yaml:"skipVerify,omitempty"`
	Task              *string  `yaml:"task,omitempty"`
	Driver            *string  `yaml:"driver,omitempty"`
	Image             *string  `yaml:"image,omitempty"`
	Workdir           *string  `yaml:"workdir,omitempty"`
	JobSpecTemplate   string   `yaml:"jobspecTemplate,omitempty"`
	NodePool          string   `yaml:"nodePool,omitempty"`
	Datacenters       []string `yaml:"datacenters,omitempty"`
	CPU               *int     `yaml:"cpu,omitempty"`
	MemoryMB          *int     `yaml:"memoryMB,omitempty"`
	DiskMB            *int     `yaml:"diskMB,omitempty"`
	AllocReadyTimeout string   `yaml:"allocReadyTimeout,omitempty"`
	EvalTimeout       string   `yaml:"evalTimeout,omitempty"`
	ExecTimeoutSecs   *int     `yaml:"execTimeoutSecs,omitempty"`
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

type fileCrownestConfig struct {
	APIURL        string  `yaml:"apiUrl,omitempty"`
	ProjectID     *string `yaml:"projectId,omitempty"`
	Template      *string `yaml:"template,omitempty"`
	TimeoutSecs   *int    `yaml:"timeoutSecs,omitempty"`
	ForgetMissing *bool   `yaml:"forgetMissing,omitempty"`
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

type fileAsciiBoxConfig struct {
	BaseURL string `yaml:"baseUrl,omitempty"`
	CLIPath string `yaml:"cliPath,omitempty"`
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

func applyOptional[T any](target, value *T) {
	if value != nil {
		*target = *value
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

type fileSpritesConfig struct {
	APIURL   string `yaml:"apiUrl,omitempty"`
	WorkRoot string `yaml:"workRoot,omitempty"`
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

type fileMachine0Config struct {
	CLIPath       string `yaml:"cliPath,omitempty"`
	Image         string `yaml:"image,omitempty"`
	ImageVersion  *int   `yaml:"imageVersion,omitempty"`
	DesktopImage  string `yaml:"desktopImage,omitempty"`
	Size          string `yaml:"size,omitempty"`
	Region        string `yaml:"region,omitempty"`
	Key           string `yaml:"key,omitempty"`
	WorkRoot      string `yaml:"workRoot,omitempty"`
	ReleasePolicy string `yaml:"releasePolicy,omitempty"`
	CreateTimeout string `yaml:"createTimeout,omitempty"`
	PollInterval  string `yaml:"pollInterval,omitempty"`
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
	JUnit          []string `yaml:"junit,omitempty"`
	Auto           *bool    `yaml:"auto,omitempty"`
	FailOnFailures *bool    `yaml:"failOnFailures,omitempty"`
}

type fileShardConfig struct {
	MaxCount *int `yaml:"maxCount,omitempty"`
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
	if err := writeUserFileConfigAtomic(path, data, replaceClaimFile, fsyncDir); err != nil {
		return "", exit(2, "write config %s: %v", path, err)
	}
	return path, nil
}

func writeUserFileConfigAtomic(path string, data []byte, replaceFile func(string, string) error, syncDirectory func(string)) error {
	writePath, err := resolveConfigWritePath(path)
	if err != nil {
		return err
	}
	dir := filepath.Dir(writePath)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := replaceFile(tmpPath, writePath); err != nil {
		return err
	}
	removeTemp = false
	syncDirectory(dir)
	return nil
}

func resolveConfigWritePath(path string) (string, error) {
	writePath := path
	for i := 0; i < 255; i++ {
		info, err := os.Lstat(writePath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return writePath, nil
			}
			return "", err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return writePath, nil
		}
		target, err := os.Readlink(writePath)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(writePath), target)
		}
		writePath = target
	}
	return "", fmt.Errorf("resolve config path %s: too many symbolic links", path)
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

type configPathTrust struct {
	trusted        bool
	repositoryRoot string
}

func applyConfigFile(cfg *Config, path string, trust configPathTrust) error {
	file, err := readFileConfig(path)
	if err != nil {
		return err
	}
	if !trust.trusted && trust.repositoryRoot != "" {
		cfg.credentialProvenance.repositoryRoot = trust.repositoryRoot
	}
	return applyFileConfigWithTrustAndProviderSource(cfg, file, trust.trusted, providerSelectionSourceForConfigPath(trust))
}

func applyFileConfig(cfg *Config, file fileConfig) error {
	return applyFileConfigWithTrustAndProviderSource(cfg, file, true, providerSelectionUserConfig)
}

func classifyConfigPath(path string) configPathTrust {
	if sameConfigPath(path, userConfigPath()) {
		return configPathTrust{trusted: true}
	}
	boundary, _ := findRepositoryBoundary()
	root, _ := filepath.Abs(boundary.root)
	if explicit := strings.TrimSpace(os.Getenv("CRABBOX_CONFIG")); explicit != "" &&
		sameConfigPath(path, explicit) && !configPathWithinRoot(path, root) {
		return configPathTrust{trusted: true}
	}
	return configPathTrust{repositoryRoot: root}
}

func sameConfigPath(left, right string) bool {
	return left != "" && right != "" && filepath.Clean(left) == filepath.Clean(right)
}

func configPathWithinRoot(path, root string) bool {
	if path == "" || root == "" {
		return false
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	if pathWithinRoot(pathAbs, rootAbs) {
		return true
	}
	resolvedPath, pathErr := filepath.EvalSymlinks(pathAbs)
	resolvedRoot, rootErr := filepath.EvalSymlinks(rootAbs)
	return pathErr == nil && rootErr == nil && pathWithinRoot(resolvedPath, resolvedRoot)
}

func pathWithinRoot(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func inlineSSHPublicKey(value string) bool {
	fields := strings.Fields(value)
	if len(fields) < 2 {
		return false
	}
	switch fields[0] {
	case "ssh-ed25519", "ssh-rsa", "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521", "sk-ssh-ed25519@openssh.com", "sk-ecdsa-sha2-nistp256@openssh.com":
		return true
	default:
		return false
	}
}

func applyFileConfigWithTrust(cfg *Config, file fileConfig, trusted bool) error {
	source := providerSelectionRepoConfig
	if trusted {
		source = providerSelectionUserConfig
	}
	return applyFileConfigWithTrustAndProviderSource(cfg, file, trusted, source)
}

func applyFileConfigWithTrustAndProviderSource(cfg *Config, file fileConfig, trusted bool, providerSource providerSelectionSource) error {
	credentialSource := credentialSourceForFile(trusted)
	if !trusted && cfg.credentialProvenance.repositoryRoot == "" {
		if root, err := os.Getwd(); err == nil {
			cfg.credentialProvenance.repositoryRoot = root
		}
	}
	if file.Profile != "" {
		cfg.Profile = file.Profile
	}
	if file.Provider != "" {
		setProviderSelection(cfg, file.Provider, providerSource)
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
	applyOptional(&cfg.Desktop, file.Desktop)
	if file.DesktopEnv != "" {
		cfg.DesktopEnv = file.DesktopEnv
	}
	applyOptional(&cfg.Browser, file.Browser)
	applyOptional(&cfg.Code, file.Code)
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
		applyOptional(&cfg.BrokerAutoWebVNC, file.Broker.AutoWebVNC)
		if trusted && len(file.Broker.LoginRedirectOrigins) > 0 {
			cfg.BrokerLoginRedirectOrigins = normalizeList(file.Broker.LoginRedirectOrigins)
		}
		if file.Broker.AdminToken != "" {
			cfg.CoordAdminToken = file.Broker.AdminToken
			cfg.credentialProvenance.coordAdminToken = credentialSource
		}
		if file.Broker.Provider != "" {
			setProviderSelection(cfg, file.Broker.Provider, providerSource)
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
	{
		applied, err := cfg.DigitalOcean.applyFile(file.DigitalOcean)
		if applied.Image {
			cfg.digitalOceanImageExplicit = true
		}
		if err != nil {
			return err
		}
	}
	if err := cfg.Vultr.applyFile(file.Vultr); err != nil {
		return err
	}
	{
		applied, err := cfg.Linode.applyFile(file.Linode)
		if applied.Image {
			cfg.linodeImageExplicit = true
		}
		if applied.Type {
			cfg.linodeTypeExplicit = true
		}
		if err != nil {
			return err
		}
	}
	if file.GitHubCodespaces != nil {
		if trusted && file.GitHubCodespaces.APIURL != "" {
			cfg.GitHubCodespaces.APIURL = file.GitHubCodespaces.APIURL
		}
		if trusted && file.GitHubCodespaces.GHPath != "" {
			cfg.GitHubCodespaces.GHPath = expandUserPath(file.GitHubCodespaces.GHPath)
		}
		if trusted && file.GitHubCodespaces.Repo != "" {
			cfg.GitHubCodespaces.Repo = file.GitHubCodespaces.Repo
		}
		if file.GitHubCodespaces.Ref != "" {
			cfg.GitHubCodespaces.Ref = file.GitHubCodespaces.Ref
		}
		if file.GitHubCodespaces.Machine != "" {
			cfg.GitHubCodespaces.Machine = file.GitHubCodespaces.Machine
		}
		if file.GitHubCodespaces.DevcontainerPath != "" {
			cfg.GitHubCodespaces.DevcontainerPath = file.GitHubCodespaces.DevcontainerPath
		}
		if file.GitHubCodespaces.WorkingDirectory != "" {
			cfg.GitHubCodespaces.WorkingDirectory = file.GitHubCodespaces.WorkingDirectory
		}
		if file.GitHubCodespaces.Geo != "" {
			cfg.GitHubCodespaces.Geo = file.GitHubCodespaces.Geo
		}
		if trusted {
			applyLeaseDuration(&cfg.GitHubCodespaces.IdleTimeout, file.GitHubCodespaces.IdleTimeout)
			if applyNonNegativeLeaseDuration(&cfg.GitHubCodespaces.RetentionPeriod, file.GitHubCodespaces.RetentionPeriod) {
				MarkGitHubCodespacesRetentionExplicit(cfg)
			}
		}
		if trusted && file.GitHubCodespaces.DeleteOnRelease != nil {
			cfg.GitHubCodespaces.DeleteOnRelease = *file.GitHubCodespaces.DeleteOnRelease
			MarkDeleteOnReleaseExplicit(cfg, "github-codespaces")
		}
		if file.GitHubCodespaces.WorkRoot != "" {
			cfg.GitHubCodespaces.WorkRoot = file.GitHubCodespaces.WorkRoot
		}
	}
	{
		applied := cfg.Lambda.applyFile(file.Lambda)
		if applied.Type {
			cfg.lambdaTypeExplicit = true
		}
		if applied.Image {
			cfg.lambdaImageExplicit = true
		}
		if applied.ImageFamily {
			cfg.lambdaImageFamilyExplicit = true
		}
	}
	if file.Nebius != nil {
		if trusted && file.Nebius.CLI != "" {
			cfg.Nebius.CLI = file.Nebius.CLI
		}
		if trusted && file.Nebius.Profile != "" {
			cfg.Nebius.Profile = file.Nebius.Profile
		}
		if file.Nebius.ParentID != "" {
			cfg.Nebius.ParentID = file.Nebius.ParentID
		}
		if file.Nebius.SubnetID != "" {
			cfg.Nebius.SubnetID = file.Nebius.SubnetID
		}
		if file.Nebius.Platform != "" {
			cfg.Nebius.Platform = file.Nebius.Platform
		}
		if file.Nebius.Preset != "" {
			cfg.Nebius.Preset = file.Nebius.Preset
		}
		if file.Nebius.ImageFamily != "" {
			cfg.Nebius.ImageFamily = file.Nebius.ImageFamily
		}
		if file.Nebius.DiskType != "" {
			cfg.Nebius.DiskType = file.Nebius.DiskType
		}
		if file.Nebius.DiskSizeGiB > 0 {
			cfg.Nebius.DiskSizeGiB = file.Nebius.DiskSizeGiB
		}
		if file.Nebius.User != "" {
			cfg.Nebius.User = file.Nebius.User
		}
		if file.Nebius.PublicIP != "" {
			cfg.Nebius.PublicIP = file.Nebius.PublicIP
		}
		if len(file.Nebius.SecurityGroupIDs) > 0 {
			cfg.Nebius.SecurityGroupIDs = file.Nebius.SecurityGroupIDs
		}
		if trusted && file.Nebius.ServiceAccountID != "" {
			cfg.Nebius.ServiceAccountID = file.Nebius.ServiceAccountID
		}
		if file.Nebius.RecoveryPolicy != "" {
			cfg.Nebius.RecoveryPolicy = file.Nebius.RecoveryPolicy
		}
	}
	{
		applied, err := cfg.OVH.applyFile(file.OVH, trusted)
		if applied.Image {
			cfg.ovhImageExplicit = true
		}
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.Scaleway.applyFile(file.Scaleway)
		MarkScalewayConfigApplied(cfg, applied)
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.TencentCloud.applyFile(file.TencentCloud, trusted)
		MarkTencentCloudConfigApplied(cfg, applied)
		if err != nil {
			return err
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
	if _, err := cfg.AWSLambdaMicroVM.applyFile(file.AWSLambdaMicroVM); err != nil {
		return err
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
		if file.Azure.SnapshotSKU != "" {
			cfg.AzureSnapshotSKU = file.Azure.SnapshotSKU
		}
		if file.Azure.OSDiskSKU != "" {
			cfg.AzureOSDiskSKU = file.Azure.OSDiskSKU
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
	{
		applied, err := cfg.AzureDynamicSessions.applyFile(file.AzureDynamicSessions)
		if applied.Endpoint {
			cfg.credentialProvenance.azSessionsEndpoint = credentialSource
		}
		if err != nil {
			return err
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
		applyOptional(&cfg.Incus.InsecureTLS, file.Incus.InsecureTLS)
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
		applyOptional(&cfg.Proxmox.FullClone, file.Proxmox.FullClone)
		if file.Proxmox.InsecureTLS != nil {
			cfg.Proxmox.InsecureTLS = *file.Proxmox.InsecureTLS
			cfg.credentialProvenance.proxmoxInsecureTLS = credentialSource
		}
	}
	if file.Firecracker != nil {
		if trusted && file.Firecracker.Binary != "" {
			cfg.Firecracker.Binary = expandUserPath(file.Firecracker.Binary)
		}
		if trusted && file.Firecracker.Jailer != "" {
			cfg.Firecracker.Jailer = expandUserPath(file.Firecracker.Jailer)
		}
		if trusted && file.Firecracker.Kernel != "" {
			cfg.Firecracker.Kernel = expandUserPath(file.Firecracker.Kernel)
		}
		if trusted && file.Firecracker.RootFS != "" {
			cfg.Firecracker.RootFS = expandUserPath(file.Firecracker.RootFS)
		}
		if file.Firecracker.User != "" {
			cfg.Firecracker.User = file.Firecracker.User
		}
		if file.Firecracker.WorkRoot != "" {
			cfg.Firecracker.WorkRoot = file.Firecracker.WorkRoot
		}
		applyOptional(&cfg.Firecracker.CPUs, file.Firecracker.CPUs)
		applyOptional(&cfg.Firecracker.MemoryMiB, file.Firecracker.MemoryMiB)
		applyOptional(&cfg.Firecracker.DiskMiB, file.Firecracker.DiskMiB)
		if trusted && file.Firecracker.Network != "" {
			cfg.Firecracker.Network = file.Firecracker.Network
		}
		if trusted && file.Firecracker.CNINetwork != "" {
			cfg.Firecracker.CNINetwork = file.Firecracker.CNINetwork
		}
		if trusted && file.Firecracker.CNIConfDir != "" {
			cfg.Firecracker.CNIConfDir = expandUserPath(file.Firecracker.CNIConfDir)
		}
		if trusted && file.Firecracker.CNIBinDir != "" {
			cfg.Firecracker.CNIBinDir = expandUserPath(file.Firecracker.CNIBinDir)
		}
		if file.Firecracker.LaunchTimeout != "" {
			applyLeaseDuration(&cfg.Firecracker.LaunchTimeout, file.Firecracker.LaunchTimeout)
		}
		if file.Firecracker.DeleteOnRelease != nil {
			cfg.Firecracker.DeleteOnRelease = *file.Firecracker.DeleteOnRelease
			MarkDeleteOnReleaseExplicit(cfg, "firecracker")
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
		applyXCPNgNameUUIDPair(&cfg.XCPNg.Template, &cfg.XCPNg.TemplateUUID, file.XCPNg.Template, file.XCPNg.TemplateUUID)
		applyXCPNgNameUUIDPair(&cfg.XCPNg.SR, &cfg.XCPNg.SRUUID, file.XCPNg.SR, file.XCPNg.SRUUID)
		applyXCPNgNameUUIDPair(&cfg.XCPNg.Network, &cfg.XCPNg.NetworkUUID, file.XCPNg.Network, file.XCPNg.NetworkUUID)
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
			cfg.credentialProvenance.parallelsHost = credentialSource
		}
		if file.Parallels.HostUser != "" {
			cfg.Parallels.HostUser = file.Parallels.HostUser
		}
		if file.Parallels.HostKey != "" {
			cfg.Parallels.HostKey = expandUserPath(file.Parallels.HostKey)
			cfg.credentialProvenance.parallelsHostKey = credentialSource
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
				merged := applyFileParallelsTemplateConfig(cfg.Parallels.Templates[name], template)
				if template.Host != "" {
					merged.hostSource = credentialSource
				}
				if template.HostKey != "" {
					merged.hostKeySource = credentialSource
				}
				cfg.Parallels.Templates[name] = merged
			}
		}
		if len(file.Parallels.Hosts) > 0 {
			cfg.Parallels.Hosts = cfg.Parallels.Hosts[:0]
			for _, host := range file.Parallels.Hosts {
				merged := applyFileParallelsHostConfig(host)
				if host.Host != "" {
					merged.hostSource = credentialSource
				}
				if host.Key != "" {
					merged.keySource = credentialSource
				}
				cfg.Parallels.Hosts = append(cfg.Parallels.Hosts, merged)
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
			cfg.credentialProvenance.sshKey = credentialSource
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
		cfg.Sync.Excludes = appendOrderedStrings(cfg.Sync.Excludes, file.Sync.Exclude...)
		cfg.Sync.Excludes = appendOrderedStrings(cfg.Sync.Excludes, file.Sync.Excludes...)
		cfg.Sync.Includes = appendUniqueStrings(cfg.Sync.Includes, file.Sync.Include...)
		cfg.Sync.Includes = appendUniqueStrings(cfg.Sync.Includes, file.Sync.Includes...)
		applyOptional(&cfg.Sync.Delete, file.Sync.Delete)
		applyOptional(&cfg.Sync.Checksum, file.Sync.Checksum)
		applyOptional(&cfg.Sync.GitSeed, file.Sync.GitSeed)
		applyOptional(&cfg.Sync.GitOverlay, file.Sync.GitOverlay)
		applyOptional(&cfg.Sync.Fingerprint, file.Sync.Fingerprint)
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
		applyOptional(&cfg.Sync.AllowLarge, file.Sync.AllowLarge)
	}
	if file.Run != nil && file.Run.PreflightTools != nil {
		cfg.Run.PreflightTools = normalizePreflightToolNames(file.Run.PreflightTools)
	}
	if file.Env != nil && file.Env.Allow != nil {
		cfg.EnvAllow = appendUniqueStrings(nil, file.Env.Allow...)
	}
	if file.Capacity != nil {
		if file.Capacity.Market != "" {
			cfg.Capacity.Market = file.Capacity.Market
			MarkCapacityMarketExplicit(cfg)
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
		applyOptional(&cfg.Capacity.Hints, file.Capacity.Hints)
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
		applyOptional(&cfg.Actions.Ephemeral, file.Actions.Ephemeral)
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
		applyOptional(&cfg.Blacksmith.Debug, file.Blacksmith.Debug)
	}
	if err := applyKubeVirtFileConfig(cfg, file.KubeVirt, trusted); err != nil {
		return err
	}
	{
		applied, err := cfg.SealosDevbox.applyFile(file.SealosDevbox, trusted)
		cfg.SealosDevbox.ExpandAppliedLocalPaths(applied)
		if applied.WorkRoot {
			MarkSealosDevboxWorkRootExplicit(cfg)
		}
		if applied.DeleteOnRelease {
			MarkDeleteOnReleaseExplicit(cfg, "sealos-devbox")
		}
		if err != nil {
			return err
		}
	}
	if err := applyAgentSandboxFileConfig(cfg, file.AgentSandbox, trusted); err != nil {
		return err
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
			cfg.credentialProvenance.externalConfig = credentialSource
		}
		applyOptional(&cfg.External.Capabilities, file.External.Capabilities)
		if file.External.Lifecycle != nil {
			cfg.External.Lifecycle = *file.External.Lifecycle
			cfg.credentialProvenance.externalLifecycle = credentialSource
		}
		if file.External.Connection != nil {
			PreserveExternalDesktopChildEnvironmentBoundary(cfg)
			cfg.External.Connection = *file.External.Connection
			ssh := cfg.External.Connection.SSH
			cfg.credentialProvenance.externalConnection = credentialSource
			cfg.credentialProvenance.externalSSHConnection = credentialSource
			if trusted {
				targetOS, windowsMode := normalizedExternalDesktopTarget(*cfg)
				outputContract, outputContractOK := externalProviderOutputContract(cfg.External)
				if ssh.TrustProviderOutput && !outputContractOK {
					return exit(2, "external provider-output contract must be JSON encodable")
				}
				cfg.credentialProvenance.externalApproved = externalCredentialApproval{
					resource:           cfg.External.Connection.ResourceName,
					host:               ssh.Host,
					proxy:              ssh.ProxyCommand,
					allowEnv:           ssh.AllowEnv,
					envSSH:             ssh,
					providerOutput:     ssh.TrustProviderOutput,
					desktopUsername:    strings.TrimSpace(cfg.External.Connection.Desktop.Username),
					desktopEnv:         cfg.External.Connection.Desktop.PasswordEnv,
					desktopTarget:      targetOS,
					desktopWindowsMode: windowsMode,
					outputContract:     outputContract,
				}
				cfg.credentialProvenance.externalApproved.envSSH.FallbackPorts = append([]string(nil), ssh.FallbackPorts...)
			}
			cfg.credentialProvenance.externalResource = credentialDestinationSource(
				cfg.External.Connection.ResourceName, cfg.credentialProvenance.externalApproved.resource, credentialSource,
			)
			cfg.credentialProvenance.externalSSHHost = credentialDestinationSource(
				ssh.Host, cfg.credentialProvenance.externalApproved.host, credentialSource,
			)
			cfg.credentialProvenance.externalSSHProxy = credentialDestinationSource(
				ssh.ProxyCommand, cfg.credentialProvenance.externalApproved.proxy, credentialSource,
			)
			cfg.credentialProvenance.externalSSHAllowEnv = credentialSourceForBool(ssh.AllowEnv, credentialSource)
			cfg.credentialProvenance.externalDesktopUser = credentialDestinationSource(
				cfg.External.Connection.Desktop.Username,
				cfg.credentialProvenance.externalApproved.desktopUsername,
				credentialSource,
			)
			cfg.credentialProvenance.externalDesktopEnv = credentialDestinationSource(
				cfg.External.Connection.Desktop.PasswordEnv,
				cfg.credentialProvenance.externalApproved.desktopEnv,
				credentialSource,
			)
			if !trusted && ssh.AllowEnv && cfg.credentialProvenance.externalApproved.allowEnv &&
				externalSSHEnvApprovalMatches(cfg.External.Connection, cfg.credentialProvenance.externalApproved) {
				cfg.credentialProvenance.externalSSHAllowEnv = credentialSourceTrustedFile
			}
		}
		if file.External.WorkRoot != "" {
			cfg.External.WorkRoot = file.External.WorkRoot
		}
		if file.External.RoutingFile != "" {
			cfg.External.RoutingFile = file.External.RoutingFile
			cfg.credentialProvenance.externalRouting = credentialSource
		}
		if cfg.External.Connection.SSH.TrustProviderOutput {
			outputContract, outputContractOK := externalProviderOutputContract(cfg.External)
			if trusted {
				if !outputContractOK {
					return exit(2, "external provider-output contract must be JSON encodable")
				}
				cfg.credentialProvenance.externalApproved.providerOutput = true
				cfg.credentialProvenance.externalApproved.outputContract = outputContract
				cfg.credentialProvenance.externalSSHOutput = credentialSourceTrustedFile
			} else {
				cfg.credentialProvenance.externalSSHOutput = credentialSourceRepository
				if cfg.credentialProvenance.externalApproved.providerOutput &&
					outputContractOK && outputContract == cfg.credentialProvenance.externalApproved.outputContract {
					cfg.credentialProvenance.externalSSHOutput = credentialSourceTrustedFile
				}
			}
		}
		if trusted && (file.External.Lifecycle != nil || file.External.Connection != nil) {
			cfg.credentialProvenance.externalArgvApproval = externalLifecycleCredentialApproval{}
			if externalLifecycleAllowsConfigArgv(cfg.External.Lifecycle) {
				contract, ok := externalLifecycleContract(cfg.External)
				if !ok {
					return exit(2, "external lifecycle config-argv contract must be JSON encodable")
				}
				cfg.credentialProvenance.externalArgvApproval = externalLifecycleCredentialApproval{
					configArgv: true,
					contract:   contract,
				}
			}
		}
	}
	{
		applied, err := cfg.Namespace.applyFile(file.Namespace)
		if applied.DeleteOnRelease {
			MarkDeleteOnReleaseExplicit(cfg, "namespace-devbox")
		}
		if err != nil {
			return err
		}
	}
	if err := applyNamespaceInstanceFileConfig(cfg, file.NamespaceInstance, trusted); err != nil {
		return err
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
	if file.Boxd != nil {
		// Only trusted config can redirect credentials or organization billing.
		if trusted {
			if file.Boxd.APIURL != "" {
				cfg.Boxd.APIURL = file.Boxd.APIURL
			}
			if file.Boxd.Org != "" {
				cfg.Boxd.Org = file.Boxd.Org
			}
		}
		if file.Boxd.WorkRoot != "" {
			cfg.Boxd.WorkRoot = file.Boxd.WorkRoot
			MarkBoxdWorkRootExplicit(cfg)
		}
		if file.Boxd.DeleteOnRelease != nil {
			cfg.Boxd.DeleteOnRelease = *file.Boxd.DeleteOnRelease
			MarkDeleteOnReleaseExplicit(cfg, "boxd")
		}
	}
	if err := applyCoderFileConfig(cfg, file.Coder); err != nil {
		return err
	}
	{
		applied, err := cfg.Morph.applyFile(file.Morph)
		if applied.APIKey {
			cfg.credentialProvenance.morphAPIKey = credentialSource
		}
		if applied.APIURL {
			cfg.credentialProvenance.morphAPIURL = credentialSource
		}
		if applied.SSHGatewayHost {
			cfg.credentialProvenance.morphSSHGatewayHost = credentialSource
		}
		if applied.DeleteOnRelease {
			MarkDeleteOnReleaseExplicit(cfg, "morph")
		}
		if err != nil {
			return err
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
	{
		applied, err := cfg.E2B.applyFile(file.E2B)
		if applied.APIURL {
			cfg.credentialProvenance.e2bAPIURL = credentialSource
		}
		if applied.Domain {
			cfg.credentialProvenance.e2bDomain = credentialSource
		}
		if err != nil {
			return err
		}
	}
	if file.CubeSandbox != nil {
		if file.CubeSandbox.APIURL != "" {
			cfg.CubeSandbox.APIURL = file.CubeSandbox.APIURL
			cfg.credentialProvenance.cubeSandboxAPIURL = credentialSource
		}
		if file.CubeSandbox.Domain != "" {
			cfg.CubeSandbox.Domain = file.CubeSandbox.Domain
			cfg.credentialProvenance.cubeSandboxDomain = credentialSource
		}
		if file.CubeSandbox.Template != "" {
			cfg.CubeSandbox.Template = file.CubeSandbox.Template
		}
		if file.CubeSandbox.Workdir != "" {
			cfg.CubeSandbox.Workdir = file.CubeSandbox.Workdir
		}
		if file.CubeSandbox.User != "" {
			cfg.CubeSandbox.User = file.CubeSandbox.User
		}
		if file.CubeSandbox.ProxyNodeIP != "" {
			cfg.CubeSandbox.ProxyNodeIP = file.CubeSandbox.ProxyNodeIP
			cfg.credentialProvenance.cubeSandboxProxyNode = credentialSource
		}
		if file.CubeSandbox.ProxyPortHTTP > 0 {
			cfg.CubeSandbox.ProxyPortHTTP = file.CubeSandbox.ProxyPortHTTP
			cfg.credentialProvenance.cubeSandboxProxyPort = credentialSource
		}
		if file.CubeSandbox.ProxyScheme != "" {
			cfg.CubeSandbox.ProxyScheme = file.CubeSandbox.ProxyScheme
			cfg.credentialProvenance.cubeSandboxProxyProto = credentialSource
		}
	}
	{
		applied, err := cfg.ExeDev.applyFile(file.ExeDev)
		if applied.ControlHost {
			cfg.credentialProvenance.exeDevControlHost = credentialSource
		}
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.Railway.applyFile(file.Railway)
		if applied.APIURL {
			cfg.credentialProvenance.railwayAPIURL = credentialSource
		}
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.FastAPICloud.applyFile(file.FastAPICloud)
		if applied.APIURL {
			cfg.credentialProvenance.fastAPICloudAPIURL = credentialSource
		}
		if err != nil {
			return err
		}
	}
	if file.UnikraftCloud != nil {
		if file.UnikraftCloud.APIKey != "" {
			cfg.UnikraftCloud.APIKey = file.UnikraftCloud.APIKey
			cfg.credentialProvenance.unikraftCloudAPIKey = credentialSource
		}
		if file.UnikraftCloud.APIURL != "" {
			cfg.UnikraftCloud.APIURL = file.UnikraftCloud.APIURL
			cfg.credentialProvenance.unikraftCloudAPIURL = credentialSource
		}
		if file.UnikraftCloud.Metro != "" {
			cfg.UnikraftCloud.Metro = file.UnikraftCloud.Metro
		}
		if file.UnikraftCloud.Image != "" {
			cfg.UnikraftCloud.Image = file.UnikraftCloud.Image
		}
		if file.UnikraftCloud.MemoryMB > 0 {
			cfg.UnikraftCloud.MemoryMB = file.UnikraftCloud.MemoryMB
		}
	}
	{
		applied, err := cfg.Runpod.applyFile(file.Runpod)
		if applied.APIURL {
			cfg.credentialProvenance.runpodAPIURL = credentialSource
		}
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.Vast.applyFile(file.Vast)
		if applied.APIURL {
			cfg.credentialProvenance.vastAPIURL = credentialSource
		}
		if applied.WorkRoot {
			MarkVastWorkRootExplicit(cfg)
		}
		if applied.ReleaseAction {
			MarkDeleteOnReleaseExplicit(cfg, "vast")
		}
		if err != nil {
			return err
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
	if err := cfg.Wandb.applyFile(file.Wandb); err != nil {
		return err
	}
	{
		applied, err := cfg.Orgo.applyFile(file.Orgo, trusted)
		if applied.APIKey {
			cfg.credentialProvenance.orgoAPIKey = credentialSource
		}
		if applied.APIBase {
			cfg.credentialProvenance.orgoAPIBase = credentialSource
		}
		if err != nil {
			return err
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
	{
		applied, err := cfg.Tensorlake.applyFile(file.Tensorlake)
		if applied.APIURL {
			cfg.credentialProvenance.tensorlakeAPIURL = credentialSource
		}
		if err != nil {
			return err
		}
	}
	if err := cfg.Cua.applyFile(file.Cua, trusted); err != nil {
		return err
	}
	if err := cfg.OpenComputer.applyFile(file.OpenComputer); err != nil {
		return err
	}
	if err := cfg.CodeSandbox.applyFile(file.CodeSandbox, trusted); err != nil {
		return err
	}
	if err := cfg.OpenSandbox.applyFile(file.OpenSandbox); err != nil {
		return err
	}
	if file.Nomad != nil {
		if trusted && file.Nomad.Address != "" {
			cfg.Nomad.Address = file.Nomad.Address
			cfg.credentialProvenance.nomadAddress = credentialSource
		}
		if trusted && file.Nomad.TokenEnv != "" {
			cfg.Nomad.TokenEnv = file.Nomad.TokenEnv
			cfg.credentialProvenance.nomadTokenEnv = credentialSource
		}
		if trusted && file.Nomad.CACert != "" {
			cfg.Nomad.CACert = expandUserPath(file.Nomad.CACert)
		}
		if trusted && file.Nomad.CAPath != "" {
			cfg.Nomad.CAPath = expandUserPath(file.Nomad.CAPath)
		}
		if trusted && file.Nomad.ClientCert != "" {
			cfg.Nomad.ClientCert = expandUserPath(file.Nomad.ClientCert)
		}
		if trusted && file.Nomad.ClientKey != "" {
			cfg.Nomad.ClientKey = expandUserPath(file.Nomad.ClientKey)
		}
		if trusted && file.Nomad.TLSServerName != "" {
			cfg.Nomad.TLSServerName = file.Nomad.TLSServerName
		}
		if trusted && file.Nomad.SkipVerify != nil {
			cfg.Nomad.SkipVerify = *file.Nomad.SkipVerify
		}
		if trusted {
			if file.Nomad.Region != "" {
				cfg.Nomad.Region = file.Nomad.Region
			}
			if file.Nomad.Namespace != "" {
				cfg.Nomad.Namespace = file.Nomad.Namespace
			}
			applyOptional(&cfg.Nomad.Task, file.Nomad.Task)
			applyOptional(&cfg.Nomad.Driver, file.Nomad.Driver)
			applyOptional(&cfg.Nomad.Image, file.Nomad.Image)
			applyOptional(&cfg.Nomad.Workdir, file.Nomad.Workdir)
			if file.Nomad.JobSpecTemplate != "" {
				cfg.Nomad.JobSpecTemplate = expandUserPath(file.Nomad.JobSpecTemplate)
			}
			if file.Nomad.NodePool != "" {
				cfg.Nomad.NodePool = file.Nomad.NodePool
			}
			if len(file.Nomad.Datacenters) > 0 {
				cfg.Nomad.Datacenters = normalizeList(file.Nomad.Datacenters)
			}
			if file.Nomad.CPU != nil {
				if *file.Nomad.CPU < 0 {
					return exit(2, "nomad cpu must be non-negative")
				}
				cfg.Nomad.CPU = *file.Nomad.CPU
			}
			if file.Nomad.MemoryMB != nil {
				if *file.Nomad.MemoryMB < 0 {
					return exit(2, "nomad memoryMB must be non-negative")
				}
				cfg.Nomad.MemoryMB = *file.Nomad.MemoryMB
			}
			if file.Nomad.DiskMB != nil {
				if *file.Nomad.DiskMB < 0 {
					return exit(2, "nomad diskMB must be non-negative")
				}
				cfg.Nomad.DiskMB = *file.Nomad.DiskMB
			}
			if file.Nomad.AllocReadyTimeout != "" {
				applyLeaseDuration(&cfg.Nomad.AllocReadyTimeout, file.Nomad.AllocReadyTimeout)
			}
			if file.Nomad.EvalTimeout != "" {
				applyLeaseDuration(&cfg.Nomad.EvalTimeout, file.Nomad.EvalTimeout)
			}
			if file.Nomad.ExecTimeoutSecs != nil {
				if *file.Nomad.ExecTimeoutSecs < 0 {
					return exit(2, "nomad execTimeoutSecs must be non-negative")
				}
				cfg.Nomad.ExecTimeoutSecs = *file.Nomad.ExecTimeoutSecs
			}
		}
	}
	if err := cfg.Blaxel.applyFile(file.Blaxel, trusted); err != nil {
		return err
	}
	if err := cfg.VercelSandbox.applyFile(file.VercelSandbox); err != nil {
		return err
	}
	if file.Superserve != nil {
		if trusted && strings.TrimSpace(file.Superserve.BaseURL) != "" {
			cfg.Superserve.BaseURL = file.Superserve.BaseURL
		}
		applyOptional(&cfg.Superserve.Template, file.Superserve.Template)
		applyOptional(&cfg.Superserve.Snapshot, file.Superserve.Snapshot)
		applyOptional(&cfg.Superserve.Workdir, file.Superserve.Workdir)
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
		applyOptional(&cfg.Superserve.ForgetMissing, file.Superserve.ForgetMissing)
	}
	if file.Crownest != nil {
		if trusted && strings.TrimSpace(file.Crownest.APIURL) != "" {
			cfg.Crownest.APIURL = file.Crownest.APIURL
		}
		applyOptional(&cfg.Crownest.ProjectID, file.Crownest.ProjectID)
		applyOptional(&cfg.Crownest.Template, file.Crownest.Template)
		if file.Crownest.TimeoutSecs != nil {
			if *file.Crownest.TimeoutSecs < 0 {
				return exit(2, "crownest timeoutSecs must be non-negative")
			}
			cfg.Crownest.TimeoutSecs = *file.Crownest.TimeoutSecs
		}
		applyOptional(&cfg.Crownest.ForgetMissing, file.Crownest.ForgetMissing)
	}
	if file.DockerSandbox != nil {
		if file.DockerSandbox.CLIPath != "" {
			cfg.DockerSandbox.CLIPath = file.DockerSandbox.CLIPath
		}
		if file.DockerSandbox.Agent != "" {
			cfg.DockerSandbox.Agent = file.DockerSandbox.Agent
		}
		applyOptional(&cfg.DockerSandbox.Template, file.DockerSandbox.Template)
		if file.DockerSandbox.CPUs != nil {
			if *file.DockerSandbox.CPUs < 0 {
				return exit(2, "docker-sandbox cpus must be non-negative")
			}
			cfg.DockerSandbox.CPUs = *file.DockerSandbox.CPUs
		}
		applyOptional(&cfg.DockerSandbox.Memory, file.DockerSandbox.Memory)
		applyOptional(&cfg.DockerSandbox.Clone, file.DockerSandbox.Clone)
		applyOptional(&cfg.DockerSandbox.Workdir, file.DockerSandbox.Workdir)
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
	if err := cfg.AnthropicSRT.applyFile(file.AnthropicSRT); err != nil {
		return err
	}
	if err := cfg.CloudRunSandbox.applyFile(file.CloudRunSandbox); err != nil {
		return err
	}
	if err := cfg.Modal.applyFile(file.Modal, trusted); err != nil {
		return err
	}
	{
		applied, err := cfg.UpstashBox.applyFile(file.UpstashBox)
		if applied.BaseURL {
			cfg.credentialProvenance.upstashBoxBaseURL = credentialSource
		}
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.Smolvm.applyFile(file.Smolvm)
		if applied.BaseURL {
			cfg.credentialProvenance.smolvmBaseURL = credentialSource
		}
		if err != nil {
			return err
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
	{
		applied, err := cfg.Cloudflare.applyFile(file.Cloudflare)
		if applied.APIURL {
			cfg.credentialProvenance.cloudflareAPIURL = credentialSource
		}
		if applied.Token {
			cfg.credentialProvenance.cloudflareToken = credentialSource
		}
		if err != nil {
			return err
		}
	}
	if err := cfg.CloudflareSandbox.applyFile(file.CloudflareSandbox, trusted); err != nil {
		return err
	}
	applyCloudflareDynamicWorkersFileConfig(cfg, file.CloudflareDynamicWorkers, trusted)
	{
		applied, err := cfg.Semaphore.applyFile(file.Semaphore)
		if applied.Host {
			cfg.credentialProvenance.semaphoreHost = credentialSource
		}
		if applied.Token {
			cfg.credentialProvenance.semaphoreToken = credentialSource
		}
		if err != nil {
			return err
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
	applyLocalContainerFile(cfg, file.LocalContainer)
	if cfg.AppleContainer.applyFile(file.AppleContainer) {
		MarkAppleContainerImageExplicit(cfg)
	}
	if file.AppleVM == nil {
		// Deprecated pre-rename key; appleVM wins when both are present.
		file.AppleVM = file.AppleVZLegacy
	}
	applyAppleVMFile(cfg, file.AppleVM)
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
		applyOptional(&cfg.MXC.AllowDACLMutation, file.MXC.AllowDACLMutation)
		applyOptional(&cfg.MXC.AllowWindowsUI, file.MXC.AllowWindowsUI)
		applyOptional(&cfg.MXC.Experimental, file.MXC.Experimental)
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
	if file.Machine0 != nil {
		if file.Machine0.CLIPath != "" {
			cfg.Machine0.CLIPath = file.Machine0.CLIPath
		}
		if file.Machine0.Image != "" {
			cfg.Machine0.Image = file.Machine0.Image
		}
		applyOptional(&cfg.Machine0.ImageVersion, file.Machine0.ImageVersion)
		if file.Machine0.DesktopImage != "" {
			cfg.Machine0.DesktopImage = file.Machine0.DesktopImage
		}
		if file.Machine0.Size != "" {
			cfg.Machine0.Size = file.Machine0.Size
			cfg.Machine0.SizeExplicit = true
		}
		if file.Machine0.Region != "" {
			cfg.Machine0.Region = file.Machine0.Region
		}
		if file.Machine0.Key != "" {
			cfg.Machine0.Key = file.Machine0.Key
		}
		if file.Machine0.WorkRoot != "" {
			cfg.Machine0.WorkRoot = file.Machine0.WorkRoot
		}
		if file.Machine0.ReleasePolicy != "" {
			cfg.Machine0.ReleasePolicy = file.Machine0.ReleasePolicy
		}
		if file.Machine0.CreateTimeout != "" {
			applyLeaseDuration(&cfg.Machine0.CreateTimeout, file.Machine0.CreateTimeout)
		}
		if file.Machine0.PollInterval != "" {
			applyLeaseDuration(&cfg.Machine0.PollInterval, file.Machine0.PollInterval)
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
	if err := cfg.Lume.applyFile(file.Lume, trusted); err != nil {
		return err
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
		applyOptional(&cfg.HyperV.InitPassword, file.HyperV.InitPassword)
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
		applyOptional(&cfg.Tailscale.Enabled, file.Tailscale.Enabled)
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
		applyOptional(&cfg.Tailscale.ExitNodeAllowLANAccess, file.Tailscale.ExitNodeAllowLANAccess)
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
			cfg.credentialProvenance.staticHost = credentialSource
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
		if file.Results.JUnit != nil {
			cfg.Results.JUnit = appendUniqueStrings(nil, file.Results.JUnit...)
		}
		applyOptional(&cfg.Results.Auto, file.Results.Auto)
		applyOptional(&cfg.Results.FailOnFailures, file.Results.FailOnFailures)
	}
	if file.Shard != nil && file.Shard.MaxCount != nil {
		cfg.Shard.MaxCount = *file.Shard.MaxCount
	}
	if file.Cache != nil {
		applyOptional(&cfg.Cache.Pnpm, file.Cache.Pnpm)
		applyOptional(&cfg.Cache.Npm, file.Cache.Npm)
		applyOptional(&cfg.Cache.Docker, file.Cache.Docker)
		applyOptional(&cfg.Cache.Git, file.Cache.Git)
		if file.Cache.MaxGB > 0 {
			cfg.Cache.MaxGB = file.Cache.MaxGB
		}
		applyOptional(&cfg.Cache.PurgeOnRelease, file.Cache.PurgeOnRelease)
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
	applyOptional(&doctor.Enabled, file.Enabled)
	if len(file.Tools) > 0 {
		doctor.Tools = normalizePreflightToolNames(file.Tools)
	}
	if file.NodeMajor > 0 {
		doctor.NodeMajor = file.NodeMajor
	}
	if file.MinDiskGB > 0 {
		doctor.MinDiskGB = file.MinDiskGB
	}
	applyOptional(&doctor.RequireDocker, file.RequireDocker)
	applyOptional(&doctor.RequireCompose, file.RequireCompose)
	return doctor
}

func applyFilePresetConfig(preset PresetConfig, file filePresetConfig) PresetConfig {
	if file.Command != "" {
		preset.Command = file.Command
	}
	applyOptional(&preset.Shell, file.Shell)
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
	applyOptional(&preset.Preflight, file.Preflight)
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
		applyOptional(&job.Hydrate.Actions, file.Hydrate.Actions)
		applyOptional(&job.Hydrate.GitHubRunner, file.Hydrate.GitHubRunner)
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
	applyOptional(&job.Shell, file.Shell)
	if file.Command != "" {
		job.Command = file.Command
	}
	applyOptional(&job.NoSync, file.NoSync)
	applyOptional(&job.SyncOnly, file.SyncOnly)
	if file.Checksum != nil {
		value := *file.Checksum
		job.Checksum = &value
	}
	applyOptional(&job.ForceSyncLarge, file.ForceSyncLarge)
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
	// File and environment overlays intentionally ignore invalid durations.
	_ = ApplyLeaseDuration(target, value)
}

func applyNonNegativeLeaseDuration(target *time.Duration, value string) bool {
	if value == "" {
		return false
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed < 0 {
		return false
	}
	*target = parsed
	return true
}

func applyEnv(cfg *Config) error {
	cfg.Profile = getenv("CRABBOX_PROFILE", cfg.Profile)
	if provider := os.Getenv("CRABBOX_PROVIDER"); provider != "" {
		setProviderSelection(cfg, provider, providerSelectionEnvironment)
		cfg.brokerProvider = ""
	}
	if t := os.Getenv("CRABBOX_TARGET"); t != "" {
		cfg.TargetOS = t
		cfg.targetExplicit = true
		cfg.credentialProvenance.externalDesktopTarget = credentialSourceEnvironment
	} else if t := os.Getenv("CRABBOX_TARGET_OS"); t != "" {
		cfg.TargetOS = t
		cfg.targetExplicit = true
		cfg.credentialProvenance.externalDesktopTarget = credentialSourceEnvironment
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
		cfg.credentialProvenance.externalDesktopMode = credentialSourceEnvironment
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
	if value := os.Getenv("CRABBOX_BROKER_LOGIN_REDIRECT_ORIGINS"); value != "" {
		cfg.BrokerLoginRedirectOrigins = splitCommaList(value)
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
	if _, err := cfg.AWSLambdaMicroVM.applyEnv(); err != nil {
		return err
	}
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
	cfg.AzureSnapshotSKU = getenv("CRABBOX_AZURE_SNAPSHOT_SKU", cfg.AzureSnapshotSKU)
	cfg.AzureOSDiskSKU = getenv("CRABBOX_AZURE_OS_DISK_SKU", cfg.AzureOSDiskSKU)
	cfg.AzureVNet = getenv("CRABBOX_AZURE_VNET", cfg.AzureVNet)
	cfg.AzureSubnet = getenv("CRABBOX_AZURE_SUBNET", cfg.AzureSubnet)
	cfg.AzureNSG = getenv("CRABBOX_AZURE_NSG", cfg.AzureNSG)
	if cidrs := os.Getenv("CRABBOX_AZURE_SSH_CIDRS"); cidrs != "" {
		cfg.AzureSSHCIDRs = splitCommaList(cidrs)
	}
	cfg.AzureNetwork = getenv("CRABBOX_AZURE_NETWORK", cfg.AzureNetwork)
	{
		applied, err := cfg.AzureDynamicSessions.applyEnv()
		if applied.Endpoint {
			cfg.credentialProvenance.azSessionsEndpoint = credentialSourceEnvironment
		}
		if err != nil {
			return err
		}
	}
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
	{
		applied, err := cfg.DigitalOcean.applyEnv()
		if applied.Image {
			cfg.digitalOceanImageExplicit = true
		}
		if err != nil {
			return err
		}
	}
	if err := cfg.Vultr.applyEnv(); err != nil {
		return err
	}
	{
		applied, err := cfg.Linode.applyEnv()
		if applied.Image {
			cfg.linodeImageExplicit = true
		}
		if applied.Type {
			cfg.linodeTypeExplicit = true
		}
		if err != nil {
			return err
		}
	}
	cfg.GitHubCodespaces.APIURL = getenv("CRABBOX_GITHUB_CODESPACES_API_URL", cfg.GitHubCodespaces.APIURL)
	cfg.GitHubCodespaces.GHPath = expandUserPath(getenv("CRABBOX_GITHUB_CODESPACES_GH_PATH", cfg.GitHubCodespaces.GHPath))
	cfg.GitHubCodespaces.Repo = getenv("CRABBOX_GITHUB_CODESPACES_REPO", cfg.GitHubCodespaces.Repo)
	cfg.GitHubCodespaces.Ref = getenv("CRABBOX_GITHUB_CODESPACES_REF", cfg.GitHubCodespaces.Ref)
	cfg.GitHubCodespaces.Machine = getenv("CRABBOX_GITHUB_CODESPACES_MACHINE", cfg.GitHubCodespaces.Machine)
	cfg.GitHubCodespaces.DevcontainerPath = getenv("CRABBOX_GITHUB_CODESPACES_DEVCONTAINER_PATH", cfg.GitHubCodespaces.DevcontainerPath)
	cfg.GitHubCodespaces.WorkingDirectory = getenv("CRABBOX_GITHUB_CODESPACES_WORKING_DIRECTORY", cfg.GitHubCodespaces.WorkingDirectory)
	cfg.GitHubCodespaces.Geo = getenv("CRABBOX_GITHUB_CODESPACES_GEO", cfg.GitHubCodespaces.Geo)
	if idleTimeout := os.Getenv("CRABBOX_GITHUB_CODESPACES_IDLE_TIMEOUT"); idleTimeout != "" {
		applyLeaseDuration(&cfg.GitHubCodespaces.IdleTimeout, idleTimeout)
	}
	if retentionPeriod := os.Getenv("CRABBOX_GITHUB_CODESPACES_RETENTION_PERIOD"); retentionPeriod != "" {
		if applyNonNegativeLeaseDuration(&cfg.GitHubCodespaces.RetentionPeriod, retentionPeriod) {
			MarkGitHubCodespacesRetentionExplicit(cfg)
		}
	}
	if value, ok := getenvBool("CRABBOX_GITHUB_CODESPACES_DELETE_ON_RELEASE"); ok {
		cfg.GitHubCodespaces.DeleteOnRelease = value
		MarkDeleteOnReleaseExplicit(cfg, "github-codespaces")
	}
	cfg.GitHubCodespaces.WorkRoot = getenv("CRABBOX_GITHUB_CODESPACES_WORK_ROOT", cfg.GitHubCodespaces.WorkRoot)
	{
		applied := cfg.Lambda.applyEnv()
		if applied.Type {
			cfg.lambdaTypeExplicit = true
		}
		if applied.Image {
			cfg.lambdaImageExplicit = true
		}
		if applied.ImageFamily {
			cfg.lambdaImageFamilyExplicit = true
		}
	}
	cfg.Nebius.CLI = getenv("CRABBOX_NEBIUS_CLI", cfg.Nebius.CLI)
	cfg.Nebius.Profile = getenv("CRABBOX_NEBIUS_PROFILE", cfg.Nebius.Profile)
	cfg.Nebius.ParentID = getenv("CRABBOX_NEBIUS_PARENT_ID", cfg.Nebius.ParentID)
	cfg.Nebius.SubnetID = getenv("CRABBOX_NEBIUS_SUBNET_ID", cfg.Nebius.SubnetID)
	cfg.Nebius.Platform = getenv("CRABBOX_NEBIUS_PLATFORM", cfg.Nebius.Platform)
	cfg.Nebius.Preset = getenv("CRABBOX_NEBIUS_PRESET", cfg.Nebius.Preset)
	cfg.Nebius.ImageFamily = getenv("CRABBOX_NEBIUS_IMAGE_FAMILY", cfg.Nebius.ImageFamily)
	cfg.Nebius.DiskType = getenv("CRABBOX_NEBIUS_DISK_TYPE", cfg.Nebius.DiskType)
	cfg.Nebius.DiskSizeGiB = getenvInt("CRABBOX_NEBIUS_DISK_SIZE_GIB", cfg.Nebius.DiskSizeGiB)
	cfg.Nebius.User = getenv("CRABBOX_NEBIUS_USER", cfg.Nebius.User)
	cfg.Nebius.PublicIP = getenv("CRABBOX_NEBIUS_PUBLIC_IP", cfg.Nebius.PublicIP)
	if groups := os.Getenv("CRABBOX_NEBIUS_SECURITY_GROUP_IDS"); groups != "" {
		cfg.Nebius.SecurityGroupIDs = splitCommaList(groups)
	}
	cfg.Nebius.ServiceAccountID = getenv("CRABBOX_NEBIUS_SERVICE_ACCOUNT_ID", cfg.Nebius.ServiceAccountID)
	cfg.Nebius.RecoveryPolicy = getenv("CRABBOX_NEBIUS_RECOVERY_POLICY", cfg.Nebius.RecoveryPolicy)
	{
		applied, err := cfg.OVH.applyEnv()
		if applied.Image {
			cfg.ovhImageExplicit = true
		}
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.Scaleway.applyEnv()
		MarkScalewayConfigApplied(cfg, applied)
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.TencentCloud.applyEnv()
		MarkTencentCloudConfigApplied(cfg, applied)
		if err != nil {
			return err
		}
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
	cfg.Firecracker.Binary = expandUserPath(getenv("CRABBOX_FIRECRACKER_BINARY", cfg.Firecracker.Binary))
	cfg.Firecracker.Jailer = expandUserPath(getenv("CRABBOX_FIRECRACKER_JAILER", cfg.Firecracker.Jailer))
	cfg.Firecracker.Kernel = expandUserPath(getenv("CRABBOX_FIRECRACKER_KERNEL", cfg.Firecracker.Kernel))
	cfg.Firecracker.RootFS = expandUserPath(getenv("CRABBOX_FIRECRACKER_ROOTFS", cfg.Firecracker.RootFS))
	cfg.Firecracker.User = getenv("CRABBOX_FIRECRACKER_USER", cfg.Firecracker.User)
	cfg.Firecracker.WorkRoot = getenv("CRABBOX_FIRECRACKER_WORK_ROOT", cfg.Firecracker.WorkRoot)
	cfg.Firecracker.CPUs = getenvInt("CRABBOX_FIRECRACKER_CPUS", cfg.Firecracker.CPUs)
	cfg.Firecracker.MemoryMiB = getenvInt("CRABBOX_FIRECRACKER_MEMORY_MIB", cfg.Firecracker.MemoryMiB)
	cfg.Firecracker.DiskMiB = getenvInt("CRABBOX_FIRECRACKER_DISK_MIB", cfg.Firecracker.DiskMiB)
	cfg.Firecracker.Network = getenv("CRABBOX_FIRECRACKER_NETWORK", cfg.Firecracker.Network)
	cfg.Firecracker.CNINetwork = getenv("CRABBOX_FIRECRACKER_CNI_NETWORK", cfg.Firecracker.CNINetwork)
	cfg.Firecracker.CNIConfDir = expandUserPath(getenv("CRABBOX_FIRECRACKER_CNI_CONF_DIR", cfg.Firecracker.CNIConfDir))
	cfg.Firecracker.CNIBinDir = expandUserPath(getenv("CRABBOX_FIRECRACKER_CNI_BIN_DIR", cfg.Firecracker.CNIBinDir))
	if timeout := os.Getenv("CRABBOX_FIRECRACKER_LAUNCH_TIMEOUT"); timeout != "" {
		applyLeaseDuration(&cfg.Firecracker.LaunchTimeout, timeout)
	}
	if value, ok := getenvBool("CRABBOX_FIRECRACKER_DELETE_ON_RELEASE"); ok {
		cfg.Firecracker.DeleteOnRelease = value
		MarkDeleteOnReleaseExplicit(cfg, "firecracker")
	}
	cfg.XCPNg.APIURL = getenv("CRABBOX_XCP_NG_API_URL", cfg.XCPNg.APIURL)
	cfg.XCPNg.Username = getenv("CRABBOX_XCP_NG_USERNAME", cfg.XCPNg.Username)
	cfg.XCPNg.Password = getenv("CRABBOX_XCP_NG_PASSWORD", cfg.XCPNg.Password)
	xcpNgTemplate, xcpNgTemplateUUID := os.Getenv("CRABBOX_XCP_NG_TEMPLATE"), os.Getenv("CRABBOX_XCP_NG_TEMPLATE_UUID")
	applyXCPNgNameUUIDPair(&cfg.XCPNg.Template, &cfg.XCPNg.TemplateUUID, xcpNgTemplate, xcpNgTemplateUUID)
	xcpNgSR, xcpNgSRUUID := os.Getenv("CRABBOX_XCP_NG_SR"), os.Getenv("CRABBOX_XCP_NG_SR_UUID")
	applyXCPNgNameUUIDPair(&cfg.XCPNg.SR, &cfg.XCPNg.SRUUID, xcpNgSR, xcpNgSRUUID)
	xcpNgNetwork, xcpNgNetworkUUID := os.Getenv("CRABBOX_XCP_NG_NETWORK"), os.Getenv("CRABBOX_XCP_NG_NETWORK_UUID")
	applyXCPNgNameUUIDPair(&cfg.XCPNg.Network, &cfg.XCPNg.NetworkUUID, xcpNgNetwork, xcpNgNetworkUUID)
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
	if value := os.Getenv("CRABBOX_PARALLELS_HOST"); value != "" {
		cfg.Parallels.Host = value
		cfg.Parallels.Hosts = nil
		cfg.Parallels.SelectedHost = ""
		cfg.credentialProvenance.parallelsHost = credentialSourceEnvironment
	}
	cfg.Parallels.HostUser = getenv("CRABBOX_PARALLELS_HOST_USER", cfg.Parallels.HostUser)
	if value := os.Getenv("CRABBOX_PARALLELS_HOST_KEY"); value != "" {
		cfg.Parallels.HostKey = expandUserPath(value)
		cfg.credentialProvenance.parallelsHostKey = credentialSourceEnvironment
	}
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
		cfg.credentialProvenance.sshKey = credentialSourceEnvironment
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
	if market := os.Getenv("CRABBOX_CAPACITY_MARKET"); market != "" {
		cfg.Capacity.Market = market
		MarkCapacityMarketExplicit(cfg)
	}
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
	{
		applied, err := cfg.KubeVirt.applyEnv()
		cfg.KubeVirt.Kubectl = expandUserPath(cfg.KubeVirt.Kubectl)
		cfg.KubeVirt.Virtctl = expandUserPath(cfg.KubeVirt.Virtctl)
		cfg.KubeVirt.Kubeconfig = expandUserPath(cfg.KubeVirt.Kubeconfig)
		cfg.KubeVirt.Template = expandUserPath(cfg.KubeVirt.Template)
		cfg.KubeVirt.SSHKey = expandUserPath(cfg.KubeVirt.SSHKey)
		cfg.KubeVirt.SSHPublicKey = expandUserPath(cfg.KubeVirt.SSHPublicKey)
		if applied.DeleteOnRelease {
			MarkDeleteOnReleaseExplicit(cfg, "kubevirt")
		}
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.SealosDevbox.applyEnv()
		cfg.SealosDevbox.Kubectl = expandUserPath(cfg.SealosDevbox.Kubectl)
		cfg.SealosDevbox.Kubeconfig = expandUserPath(cfg.SealosDevbox.Kubeconfig)
		if applied.WorkRoot {
			MarkSealosDevboxWorkRootExplicit(cfg)
		}
		if applied.DeleteOnRelease {
			MarkDeleteOnReleaseExplicit(cfg, "sealos-devbox")
		}
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.AgentSandbox.applyEnv()
		cfg.AgentSandbox.Kubeconfig = expandUserPath(cfg.AgentSandbox.Kubeconfig)
		if applied.DeleteOnRelease {
			MarkDeleteOnReleaseExplicit(cfg, "agent-sandbox")
		}
		if err != nil {
			return err
		}
	}
	externalProviderOutputExplicit := false
	if value := os.Getenv("CRABBOX_EXTERNAL_COMMAND"); value != "" {
		cfg.External.Command = value
		externalProviderOutputExplicit = true
	}
	if arg := os.Getenv("CRABBOX_EXTERNAL_ARG"); arg != "" {
		cfg.External.Args = []string{arg}
		externalProviderOutputExplicit = true
	}
	if externalProviderOutputExplicit {
		markExternalProviderOutputExplicit(cfg, credentialSourceEnvironment)
	}
	cfg.External.WorkRoot = getenv("CRABBOX_EXTERNAL_WORK_ROOT", cfg.External.WorkRoot)
	if value := os.Getenv("CRABBOX_EXTERNAL_ROUTING_FILE"); value != "" {
		cfg.External.RoutingFile = value
		cfg.credentialProvenance.externalRouting = credentialSourceEnvironment
	}
	ApplyExternalDesktopEnvironmentOverrides(cfg)
	if value, ok := getenvBool("CRABBOX_EXTERNAL_IDEMPOTENT_LEASE_ID"); ok {
		cfg.External.Capabilities.IdempotentLeaseID = value
	}
	{
		applied, err := cfg.Namespace.applyEnv()
		if applied.DeleteOnRelease {
			MarkDeleteOnReleaseExplicit(cfg, "namespace-devbox")
		}
		if err != nil {
			return err
		}
	}
	if _, err := cfg.NamespaceInstance.applyEnv(); err != nil {
		return err
	}
	cfg.NamespaceInstance.CLIPath = expandUserPath(cfg.NamespaceInstance.CLIPath)
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
	{
		applied, err := cfg.Morph.applyEnv()
		if applied.APIKey {
			cfg.credentialProvenance.morphAPIKey = credentialSourceEnvironment
		}
		if applied.APIURL {
			cfg.credentialProvenance.morphAPIURL = credentialSourceEnvironment
		}
		if applied.SSHGatewayHost {
			cfg.credentialProvenance.morphSSHGatewayHost = credentialSourceEnvironment
		}
		if applied.DeleteOnRelease {
			MarkDeleteOnReleaseExplicit(cfg, "morph")
		}
		if err != nil {
			return err
		}
	}
	if value, ok := os.LookupEnv("CRABBOX_BOXD_API_URL"); ok {
		cfg.Boxd.APIURL = value
	}
	if value, ok := os.LookupEnv("CRABBOX_BOXD_ORG"); ok {
		cfg.Boxd.Org = value
	}
	if value := os.Getenv("CRABBOX_BOXD_WORK_ROOT"); value != "" {
		cfg.Boxd.WorkRoot = value
		MarkBoxdWorkRootExplicit(cfg)
	}
	if value, ok := getenvBool("CRABBOX_BOXD_DELETE_ON_RELEASE"); ok {
		cfg.Boxd.DeleteOnRelease = value
		MarkDeleteOnReleaseExplicit(cfg, "boxd")
	}
	if _, err := cfg.Coder.applyEnv(); err != nil {
		return err
	}
	cfg.Coder.CLIPath = expandUserPath(cfg.Coder.CLIPath)
	cfg.Coder.RichParameterFile = expandUserPath(cfg.Coder.RichParameterFile)
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
	{
		applied, err := cfg.E2B.applyEnv()
		if applied.APIKey {
			cfg.credentialProvenance.e2bAPIKey = credentialSourceEnvironment
		}
		if applied.APIURL {
			cfg.credentialProvenance.e2bAPIURL = credentialSourceEnvironment
		}
		if applied.Domain {
			cfg.credentialProvenance.e2bDomain = credentialSourceEnvironment
		}
		if err != nil {
			return err
		}
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_CUBESANDBOX_API_KEY", "CUBE_API_KEY", "E2B_API_KEY"); ok {
		cfg.CubeSandbox.APIKey = value
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_CUBESANDBOX_API_URL", "CUBE_API_URL", "E2B_API_URL"); ok {
		cfg.CubeSandbox.APIURL = value
		cfg.credentialProvenance.cubeSandboxAPIURL = credentialSourceEnvironment
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_CUBESANDBOX_DOMAIN", "CUBE_SANDBOX_DOMAIN"); ok {
		cfg.CubeSandbox.Domain = value
		cfg.credentialProvenance.cubeSandboxDomain = credentialSourceEnvironment
	}
	cfg.CubeSandbox.Template = getenv("CRABBOX_CUBESANDBOX_TEMPLATE", getenv("CUBE_TEMPLATE_ID", cfg.CubeSandbox.Template))
	cfg.CubeSandbox.Workdir = getenv("CRABBOX_CUBESANDBOX_WORKDIR", cfg.CubeSandbox.Workdir)
	cfg.CubeSandbox.User = getenv("CRABBOX_CUBESANDBOX_USER", cfg.CubeSandbox.User)
	if value, ok := firstNonEmptyEnv("CRABBOX_CUBESANDBOX_PROXY_NODE_IP", "CUBE_PROXY_NODE_IP"); ok {
		cfg.CubeSandbox.ProxyNodeIP = value
		cfg.credentialProvenance.cubeSandboxProxyNode = credentialSourceEnvironment
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_CUBESANDBOX_PROXY_PORT_HTTP", "CUBE_PROXY_PORT_HTTP"); ok {
		port, err := strconv.Atoi(value)
		if err != nil {
			return exit(2, "invalid cubesandbox proxy HTTP port %q", value)
		}
		cfg.CubeSandbox.ProxyPortHTTP = port
		cfg.credentialProvenance.cubeSandboxProxyPort = credentialSourceEnvironment
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_CUBESANDBOX_PROXY_SCHEME", "CUBE_PROXY_SCHEME"); ok {
		cfg.CubeSandbox.ProxyScheme = value
		cfg.credentialProvenance.cubeSandboxProxyProto = credentialSourceEnvironment
	}
	{
		applied, err := cfg.ExeDev.applyEnv()
		if applied.ControlHost {
			cfg.credentialProvenance.exeDevControlHost = credentialSourceEnvironment
		}
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.Railway.applyEnv()
		if applied.APIToken {
			cfg.credentialProvenance.railwayAPIToken = credentialSourceEnvironment
		}
		if applied.APIURL {
			cfg.credentialProvenance.railwayAPIURL = credentialSourceEnvironment
		}
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.FastAPICloud.applyEnv()
		if applied.Token {
			cfg.credentialProvenance.fastAPICloudToken = credentialSourceEnvironment
		}
		if applied.APIURL {
			cfg.credentialProvenance.fastAPICloudAPIURL = credentialSourceEnvironment
		}
		if err != nil {
			return err
		}
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_UNIKRAFT_CLOUD_API_KEY", "UNIKRAFT_CLOUD_API_KEY", "UKC_API_KEY", "UKC_TOKEN"); ok {
		cfg.UnikraftCloud.APIKey = value
		cfg.credentialProvenance.unikraftCloudAPIKey = credentialSourceEnvironment
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_UNIKRAFT_CLOUD_API_URL", "UNIKRAFT_CLOUD_API_URL"); ok {
		cfg.UnikraftCloud.APIURL = value
		cfg.credentialProvenance.unikraftCloudAPIURL = credentialSourceEnvironment
	}
	cfg.UnikraftCloud.Metro = getenv("CRABBOX_UNIKRAFT_CLOUD_METRO", getenv("UNIKRAFT_CLOUD_METRO", getenv("UKC_METRO", cfg.UnikraftCloud.Metro)))
	cfg.UnikraftCloud.Image = getenv("CRABBOX_UNIKRAFT_CLOUD_IMAGE", getenv("UNIKRAFT_CLOUD_IMAGE", cfg.UnikraftCloud.Image))
	{
		applied, err := cfg.Runpod.applyEnv()
		if applied.APIKey {
			cfg.credentialProvenance.runpodAPIKey = credentialSourceEnvironment
		}
		if applied.APIURL {
			cfg.credentialProvenance.runpodAPIURL = credentialSourceEnvironment
		}
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.Vast.applyEnv()
		if applied.APIKey {
			cfg.credentialProvenance.vastAPIKey = credentialSourceEnvironment
		}
		if applied.APIURL {
			cfg.credentialProvenance.vastAPIURL = credentialSourceEnvironment
		}
		if applied.WorkRoot {
			MarkVastWorkRootExplicit(cfg)
		}
		if applied.ReleaseAction {
			MarkDeleteOnReleaseExplicit(cfg, "vast")
		}
		if err != nil {
			return err
		}
	}
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
	if err := cfg.Wandb.applyEnv(); err != nil {
		return err
	}
	{
		applied, err := cfg.Orgo.applyEnv()
		if applied.APIKey {
			cfg.credentialProvenance.orgoAPIKey = credentialSourceEnvironment
		}
		if applied.APIBase {
			cfg.credentialProvenance.orgoAPIBase = credentialSourceEnvironment
		}
		if err != nil {
			return err
		}
	}
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
	{
		applied, err := cfg.Tensorlake.applyEnv()
		if applied.APIKey {
			cfg.credentialProvenance.tensorlakeAPIKey = credentialSourceEnvironment
		}
		if applied.APIURL {
			cfg.credentialProvenance.tensorlakeAPIURL = credentialSourceEnvironment
		}
		if err != nil {
			return err
		}
	}
	var err error
	if err := cfg.Cua.applyEnv(); err != nil {
		return err
	}
	if err := cfg.OpenComputer.applyEnv(); err != nil {
		return err
	}
	if err := cfg.CodeSandbox.applyEnv(); err != nil {
		return err
	}
	if err := cfg.OpenSandbox.applyEnv(); err != nil {
		return err
	}
	if value := os.Getenv("CRABBOX_NOMAD_ADDR"); value != "" {
		cfg.Nomad.Address = value
		cfg.credentialProvenance.nomadAddress = credentialSourceEnvironment
	} else if value := os.Getenv("NOMAD_ADDR"); value != "" {
		cfg.Nomad.Address = value
		cfg.credentialProvenance.nomadAddress = credentialSourceEnvironment
	}
	cfg.Nomad.Region = getenv("CRABBOX_NOMAD_REGION", getenv("NOMAD_REGION", cfg.Nomad.Region))
	cfg.Nomad.Namespace = getenv("CRABBOX_NOMAD_NAMESPACE", getenv("NOMAD_NAMESPACE", cfg.Nomad.Namespace))
	if value := os.Getenv("CRABBOX_NOMAD_TOKEN_ENV"); value != "" {
		cfg.Nomad.TokenEnv = value
		cfg.credentialProvenance.nomadTokenEnv = credentialSourceEnvironment
	}
	cfg.Nomad.CACert = expandUserPath(getenv("CRABBOX_NOMAD_CA_CERT", getenv("NOMAD_CACERT", cfg.Nomad.CACert)))
	cfg.Nomad.CAPath = expandUserPath(getenv("CRABBOX_NOMAD_CA_PATH", getenv("NOMAD_CAPATH", cfg.Nomad.CAPath)))
	cfg.Nomad.ClientCert = expandUserPath(getenv("CRABBOX_NOMAD_CLIENT_CERT", getenv("NOMAD_CLIENT_CERT", cfg.Nomad.ClientCert)))
	cfg.Nomad.ClientKey = expandUserPath(getenv("CRABBOX_NOMAD_CLIENT_KEY", getenv("NOMAD_CLIENT_KEY", cfg.Nomad.ClientKey)))
	cfg.Nomad.TLSServerName = getenv("CRABBOX_NOMAD_TLS_SERVER_NAME", getenv("NOMAD_TLS_SERVER_NAME", cfg.Nomad.TLSServerName))
	if v, ok := getenvBool("CRABBOX_NOMAD_SKIP_VERIFY"); ok {
		cfg.Nomad.SkipVerify = v
	} else if v, ok := getenvBool("NOMAD_SKIP_VERIFY"); ok {
		cfg.Nomad.SkipVerify = v
	}
	cfg.Nomad.Task = getenv("CRABBOX_NOMAD_TASK", cfg.Nomad.Task)
	cfg.Nomad.Driver = getenv("CRABBOX_NOMAD_DRIVER", cfg.Nomad.Driver)
	cfg.Nomad.Image = getenv("CRABBOX_NOMAD_IMAGE", cfg.Nomad.Image)
	cfg.Nomad.Workdir = getenv("CRABBOX_NOMAD_WORKDIR", cfg.Nomad.Workdir)
	cfg.Nomad.JobSpecTemplate = expandUserPath(getenv("CRABBOX_NOMAD_JOBSPEC_TEMPLATE", cfg.Nomad.JobSpecTemplate))
	cfg.Nomad.NodePool = getenv("CRABBOX_NOMAD_NODE_POOL", cfg.Nomad.NodePool)
	if datacenters, ok := getenvList("CRABBOX_NOMAD_DATACENTERS"); ok {
		cfg.Nomad.Datacenters = datacenters
	}
	cfg.Nomad.CPU = getenvInt("CRABBOX_NOMAD_CPU", cfg.Nomad.CPU)
	cfg.Nomad.MemoryMB = getenvInt("CRABBOX_NOMAD_MEMORY_MB", cfg.Nomad.MemoryMB)
	cfg.Nomad.DiskMB = getenvInt("CRABBOX_NOMAD_DISK_MB", cfg.Nomad.DiskMB)
	if timeout := os.Getenv("CRABBOX_NOMAD_ALLOC_READY_TIMEOUT"); timeout != "" {
		applyLeaseDuration(&cfg.Nomad.AllocReadyTimeout, timeout)
	}
	if timeout := os.Getenv("CRABBOX_NOMAD_EVAL_TIMEOUT"); timeout != "" {
		applyLeaseDuration(&cfg.Nomad.EvalTimeout, timeout)
	}
	cfg.Nomad.ExecTimeoutSecs, err = getenvNonNegativeInt("CRABBOX_NOMAD_EXEC_TIMEOUT_SECS", cfg.Nomad.ExecTimeoutSecs)
	if err != nil {
		return err
	}
	if err := cfg.Blaxel.applyEnv(); err != nil {
		return err
	}
	if err := cfg.VercelSandbox.applyEnv(); err != nil {
		return err
	}
	if err := cfg.CloudflareSandbox.applyEnv(); err != nil {
		return err
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
	if err := cfg.AnthropicSRT.applyEnv(); err != nil {
		return err
	}
	if err := cfg.CloudRunSandbox.applyEnv(); err != nil {
		return err
	}
	if err := cfg.Modal.applyEnv(); err != nil {
		return err
	}
	{
		applied, err := cfg.UpstashBox.applyEnv()
		if applied.APIKey {
			cfg.credentialProvenance.upstashBoxAPIKey = credentialSourceEnvironment
		}
		if applied.BaseURL {
			cfg.credentialProvenance.upstashBoxBaseURL = credentialSourceEnvironment
		}
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.Smolvm.applyEnv()
		if applied.APIKey {
			cfg.credentialProvenance.smolvmAPIKey = credentialSourceEnvironment
		}
		if applied.BaseURL {
			cfg.credentialProvenance.smolvmBaseURL = credentialSourceEnvironment
		}
		if err != nil {
			return err
		}
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
	{
		applied, err := cfg.Cloudflare.applyEnv()
		if applied.APIURL {
			cfg.credentialProvenance.cloudflareAPIURL = credentialSourceEnvironment
		}
		if applied.Token {
			cfg.credentialProvenance.cloudflareToken = credentialSourceEnvironment
		}
		if err != nil {
			return err
		}
	}
	cfg.Crownest.APIURL = getenv("CRABBOX_CROWNEST_API_URL", getenv("CROWNEST_API_URL", cfg.Crownest.APIURL))
	cfg.Crownest.ProjectID = getenv("CRABBOX_CROWNEST_PROJECT_ID", getenv("CROWNEST_PROJECT_ID", cfg.Crownest.ProjectID))
	cfg.Crownest.Template = getenv("CRABBOX_CROWNEST_TEMPLATE", getenv("CROWNEST_TEMPLATE", cfg.Crownest.Template))
	crownestTimeoutEnv := "CRABBOX_CROWNEST_TIMEOUT_SECS"
	crownestTimeoutValue := os.Getenv(crownestTimeoutEnv)
	if crownestTimeoutValue == "" {
		crownestTimeoutEnv = "CROWNEST_TIMEOUT_SECS"
		crownestTimeoutValue = os.Getenv(crownestTimeoutEnv)
	}
	if crownestTimeoutValue != "" {
		parsed, parseErr := strconv.Atoi(crownestTimeoutValue)
		if parseErr != nil {
			return exit(2, "%s must be an integer", crownestTimeoutEnv)
		}
		if parsed < 0 {
			return exit(2, "%s must be non-negative", crownestTimeoutEnv)
		}
		cfg.Crownest.TimeoutSecs = parsed
	}
	if v, ok := getenvBool("CRABBOX_CROWNEST_FORGET_MISSING"); ok {
		cfg.Crownest.ForgetMissing = v
	} else if v, ok := getenvBool("CROWNEST_FORGET_MISSING"); ok {
		cfg.Crownest.ForgetMissing = v
	}
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
	{
		applied, err := cfg.Semaphore.applyEnv()
		if applied.Host {
			cfg.credentialProvenance.semaphoreHost = credentialSourceEnvironment
		}
		if applied.Token {
			cfg.credentialProvenance.semaphoreToken = credentialSourceEnvironment
		}
		if err != nil {
			return err
		}
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_SPRITES_TOKEN", "SPRITES_TOKEN", "SPRITE_TOKEN", "SETUP_SPRITE_TOKEN"); ok {
		cfg.Sprites.Token = value
		cfg.credentialProvenance.spritesToken = credentialSourceEnvironment
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_SPRITES_API_URL", "SPRITES_API_URL"); ok {
		cfg.Sprites.APIURL = value
		cfg.credentialProvenance.spritesAPIURL = credentialSourceEnvironment
	}
	cfg.Sprites.WorkRoot = getenv("CRABBOX_SPRITES_WORK_ROOT", cfg.Sprites.WorkRoot)
	applyLocalContainerEnv(cfg)
	if cfg.AppleContainer.applyEnv() {
		MarkAppleContainerImageExplicit(cfg)
	}
	if err := applyAppleVMEnv(cfg); err != nil {
		return err
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
	cfg.Machine0.CLIPath = getenv("CRABBOX_MACHINE0_CLI", cfg.Machine0.CLIPath)
	cfg.Machine0.Image = getenv("CRABBOX_MACHINE0_IMAGE", cfg.Machine0.Image)
	cfg.Machine0.ImageVersion = getenvInt("CRABBOX_MACHINE0_IMAGE_VERSION", cfg.Machine0.ImageVersion)
	cfg.Machine0.DesktopImage = getenv("CRABBOX_MACHINE0_DESKTOP_IMAGE", cfg.Machine0.DesktopImage)
	if size := os.Getenv("CRABBOX_MACHINE0_SIZE"); size != "" {
		cfg.Machine0.Size = size
		cfg.Machine0.SizeExplicit = true
	}
	cfg.Machine0.Region = getenv("CRABBOX_MACHINE0_REGION", cfg.Machine0.Region)
	cfg.Machine0.Key = getenv("CRABBOX_MACHINE0_KEY", cfg.Machine0.Key)
	cfg.Machine0.WorkRoot = getenv("CRABBOX_MACHINE0_WORK_ROOT", cfg.Machine0.WorkRoot)
	cfg.Machine0.ReleasePolicy = getenv("CRABBOX_MACHINE0_RELEASE_POLICY", cfg.Machine0.ReleasePolicy)
	if timeout := os.Getenv("CRABBOX_MACHINE0_CREATE_TIMEOUT"); timeout != "" {
		applyLeaseDuration(&cfg.Machine0.CreateTimeout, timeout)
	}
	if interval := os.Getenv("CRABBOX_MACHINE0_POLL_INTERVAL"); interval != "" {
		applyLeaseDuration(&cfg.Machine0.PollInterval, interval)
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
	if err := cfg.Lume.applyEnv(); err != nil {
		return err
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
	if value := os.Getenv("CRABBOX_STATIC_HOST"); value != "" {
		cfg.Static.Host = value
		cfg.credentialProvenance.staticHost = credentialSourceEnvironment
	}
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
	if value, ok := getenvBool("CRABBOX_RESULTS_FAIL_ON_FAILURES"); ok {
		cfg.Results.FailOnFailures = value
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
	if value, ok := getenvBool("CRABBOX_SYNC_GIT_OVERLAY"); ok {
		cfg.Sync.GitOverlay = value
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
		cfg.envAllowOverriddenByEnv = true
	}
	if tools := os.Getenv("CRABBOX_PREFLIGHT_TOOLS"); tools != "" {
		cfg.Run.PreflightTools = parsePreflightToolsOverride(tools)
	}
	return nil
}

// ApplyExternalDesktopEnvironmentOverrides reapplies process-local desktop
// credential references after persisted routing replaces External config.
func ApplyExternalDesktopEnvironmentOverrides(cfg *Config) {
	if cfg == nil {
		return
	}
	PreserveExternalDesktopChildEnvironmentBoundary(cfg)
	configuredPasswordEnv := strings.TrimSpace(cfg.External.Connection.Desktop.PasswordEnv)
	if !strings.EqualFold(configuredPasswordEnv, "CRABBOX_EXTERNAL_DESKTOP_USERNAME") {
		cfg.External.Connection.Desktop.Username = getenv("CRABBOX_EXTERNAL_DESKTOP_USERNAME", cfg.External.Connection.Desktop.Username)
	}
	if os.Getenv("CRABBOX_EXTERNAL_DESKTOP_USERNAME") != "" && !strings.EqualFold(configuredPasswordEnv, "CRABBOX_EXTERNAL_DESKTOP_USERNAME") {
		cfg.credentialProvenance.externalDesktopUser = credentialSourceEnvironment
	}
	if value := os.Getenv("CRABBOX_EXTERNAL_DESKTOP_PASSWORD_ENV"); value != "" && !strings.EqualFold(configuredPasswordEnv, "CRABBOX_EXTERNAL_DESKTOP_PASSWORD_ENV") {
		cfg.External.Connection.Desktop.PasswordEnv = value
		cfg.credentialProvenance.externalDesktopEnv = credentialSourceEnvironment
	}
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

func serverTypeForConfig(cfg Config) string {
	if resolved, err := ProviderFor(cfg.Provider); err == nil {
		cfg.Provider = resolved.Name()
		if resolved.Spec().ClassDisposition == ProviderClassDispositionMapped {
			if cfg.ServerTypeExplicit && strings.TrimSpace(cfg.ServerType) != "" {
				return cfg.ServerType
			}
			if override, ok := resolved.(ProviderServerTypeOverrideProvider); ok {
				if serverType, selected := override.ServerTypeOverrideForConfig(cfg); selected {
					return serverType
				}
			}
		}
		if typer, ok := resolved.(ProviderServerTypeProvider); ok {
			return typer.ServerTypeForConfig(cfg)
		}
	}
	if isBlacksmithProvider(cfg.Provider) || isStaticProvider(cfg.Provider) || cfg.Provider == "islo" || cfg.Provider == "sprites" || cfg.Provider == "local-container" || cfg.Provider == "multipass" {
		return ""
	}
	if cfg.Provider == "e2b" {
		return blank(cfg.E2B.Template, E2BConfigDefaultTemplate)
	}
	if cfg.Provider == "exe-dev" || cfg.Provider == "exedev" || cfg.Provider == "exe" {
		return blank(cfg.ExeDev.Image, ExeDevDefaultImageLabel)
	}
	if cfg.Provider == "modal" {
		return blank(cfg.Modal.Image, ModalConfigDefaultImage)
	}
	if cfg.Provider == "upstash-box" || cfg.Provider == "upstash" {
		return blank(cfg.UpstashBox.Size, UpstashBoxConfigDefaultSize)
	}
	if cfg.Provider == "daytona" {
		return "snapshot"
	}
	if cfg.Provider == "proxmox" {
		return proxmoxServerTypeForConfig(cfg)
	}
	if cfg.Provider == "firecracker" {
		return firecrackerServerTypeForConfig(cfg)
	}
	if cfg.Provider == "incus" {
		return incusServerTypeForConfig(cfg)
	}
	if cfg.Provider == "parallels" {
		return parallelsServerTypeForConfig(cfg)
	}
	return ""
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
	if provider == "proxmox" {
		return "template"
	}
	if provider == "firecracker" {
		return "microvm"
	}
	if provider == "incus" {
		return "container"
	}
	if provider == "parallels" {
		return "template"
	}
	return ""
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

func firecrackerServerTypeForConfig(_ Config) string {
	return "microvm"
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
		cfg.credentialProvenance.parallelsHost = template.hostSource
	}
	if template.HostUser != "" {
		cfg.Parallels.HostUser = template.HostUser
	}
	if template.HostKey != "" {
		cfg.Parallels.HostKey = template.HostKey
		cfg.credentialProvenance.parallelsHostKey = template.hostKeySource
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
	provider, err := ProviderFor("cloudflare")
	if err == nil {
		if resolver, ok := provider.(ProviderServerTypeProvider); ok {
			return resolver.ServerTypeForClass(class)
		}
	}
	return strings.TrimSpace(class)
}

func CloudflareContainerInstanceTypeForClass(class string) string {
	return cloudflareContainerInstanceTypeForClass(class)
}

func serverTypeCandidatesForClass(class string) []string {
	cfg := Config{Provider: "hetzner", TargetOS: targetLinux, Architecture: ArchitectureAMD64, Class: class, architectureExplicit: true}
	return hetznerServerTypeCandidatesForConfig(cfg)
}

func hetznerServerTypeCandidatesForConfig(cfg Config) []string {
	if cfg.ServerTypeExplicit {
		if strings.TrimSpace(cfg.ServerType) != "" {
			return []string{cfg.ServerType}
		}
	}
	serverType := concreteStoredServerType(cfg)
	if candidates, matched := providerClassCandidatesForConfig(cfg); matched {
		return appendUniqueExactStrings([]string{serverType}, candidates...)
	}
	if IsCanonicalProviderClass(cfg.Class) {
		if serverType != "" {
			return []string{serverType}
		}
		return nil
	}
	candidates := []string{cfg.Class}
	if serverType == "" || serverType == cfg.Class {
		return candidates
	}
	return append([]string{serverType}, candidates...)
}

func awsInstanceTypeCandidatesForConfig(cfg Config) []string {
	candidates, _ := awsClassCandidatesForConfig(cfg)
	return candidates
}

func awsClassCandidatesForConfig(cfg Config) ([]string, bool) {
	if candidates, matched := providerClassCandidatesForConfig(cfg); matched {
		return candidates, true
	}
	if normalizeTargetOS(cfg.TargetOS) == targetMacOS {
		standard := cfg
		standard.Class = "standard"
		if candidates, matched := providerClassCandidatesForConfig(standard); matched {
			return appendUniqueExactStrings([]string{cfg.Class}, candidates...), false
		}
	}
	if IsCanonicalProviderClass(cfg.Class) {
		if storedType := concreteStoredServerType(cfg); storedType != "" {
			return []string{storedType}, true
		}
		return nil, false
	}
	return appendUniqueExactStrings([]string{concreteStoredServerType(cfg)}, cfg.Class), false
}

func awsInstanceTypeCandidatesForTargetModeArchitectureClass(target, windowsMode, architecture, class string) []string {
	cfg := Config{Provider: "aws", TargetOS: target, WindowsMode: windowsMode, Architecture: architecture, Class: class, architectureExplicit: true}
	return awsInstanceTypeCandidatesForConfig(cfg)
}

func awsMacOSInstanceTypeCandidates() []string {
	return awsInstanceTypeCandidatesForTargetModeArchitectureClass(targetMacOS, windowsModeNormal, ArchitectureAMD64, "standard")
}

func awsInstanceTypeCandidatesForClass(class string) []string {
	return awsInstanceTypeCandidatesForTargetModeArchitectureClass(targetLinux, windowsModeNormal, ArchitectureAMD64, class)
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

func getenvInt64(name string, fallback int64) int64 {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return fallback
	}
	return n
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
	return parseEnvListValue(value), true
}

func parseEnvListValue(value string) []string {
	if strings.EqualFold(strings.TrimSpace(value), "none") {
		return []string{}
	}
	return splitCommaList(value)
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

func appendOrderedStrings(values []string, extra ...string) []string {
	out := append([]string(nil), values...)
	for _, value := range extra {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}
