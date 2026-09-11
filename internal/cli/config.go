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
	inputProvenance               configInputLedger
	synthesizedFlagInputs         bool
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
func applyXCPNgNameUUIDPair(dstName, dstUUID *string, incomingName, incomingUUID string) bool {
	if incomingName == "" && incomingUUID == "" {
		return false
	}
	*dstName = incomingName
	*dstUUID = incomingUUID
	return true
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
	completeCanonicalConfigInputs(&cfg)
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
		cfg.Nebius = cfg.Nebius.WithRuntimeDefaults()
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
	return resolveExplicitProviderWorkRoot(cfg.NvidiaBrev.WorkRoot, NvidiaBrevConfigDefaultWorkRoot, cfg.explicitWorkRoot, IsNvidiaBrevWorkRootExplicit(&cfg))
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
		Lambda:           initialLambdaConfig(),
		OVH:              defaultOVHConfig(),
		Scaleway:         defaultScalewayConfig(),
		TencentCloud:     defaultTencentCloudConfig(),
		Incus:            initialIncusConfig(),
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
		Phala:             defaultPhalaConfig(),
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
		Runpod:     defaultRunpodConfig(),
		Vast:       defaultVastConfig(),
		Blacksmith: defaultBlacksmithConfig(),
		NvidiaBrev: defaultNvidiaBrevConfig(),
		Nebius:     (NebiusConfig{}).WithRuntimeDefaults(),
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
		Crownest: defaultCrownestConfig(),
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
		Firecracker: initialFirecrackerConfig(),
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
		Multipass: initialMultipassConfig(multipassImage),
		Machine0:  defaultMachine0Config(),
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

func applyOptional[T any](target, value *T) bool {
	if value != nil {
		*target = *value
		return true
	}
	return false
}

func applyCloudflareDynamicWorkersFileConfig(cfg *Config, file *fileCloudflareDynamicWorkersConfig, trusted bool) (accepted bool) {
	if file == nil {
		return false
	}
	if trusted {
		if file.LoaderURL != "" {
			cfg.CloudflareDynamicWorkers.LoaderURL = file.LoaderURL
			accepted = true
		}
		if file.URL != "" {
			cfg.CloudflareDynamicWorkers.LoaderURL = file.URL
			accepted = true
		}
		if file.Token != "" {
			cfg.CloudflareDynamicWorkers.Token = file.Token
			accepted = true
		}
	}
	if file.CompatibilityDate != "" {
		cfg.CloudflareDynamicWorkers.CompatibilityDate = file.CompatibilityDate
		accepted = true
	}
	if len(file.CompatibilityFlags) > 0 {
		cfg.CloudflareDynamicWorkers.CompatibilityFlags = append([]string(nil), file.CompatibilityFlags...)
		accepted = true
	}
	if file.CacheMode != "" {
		cfg.CloudflareDynamicWorkers.CacheMode = file.CacheMode
		accepted = true
	}
	if file.Egress != "" && (trusted || strings.EqualFold(strings.TrimSpace(file.Egress), "blocked")) {
		cfg.CloudflareDynamicWorkers.Egress = file.Egress
		accepted = true
	}
	if trusted {
		if file.CPUMs > 0 {
			cfg.CloudflareDynamicWorkers.CPUMs = file.CPUMs
			accepted = true
		}
		if file.Subrequests > 0 {
			cfg.CloudflareDynamicWorkers.Subrequests = file.Subrequests
			accepted = true
		}
		if file.TimeoutSecs > 0 {
			cfg.CloudflareDynamicWorkers.TimeoutSecs = file.TimeoutSecs
			accepted = true
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
		accepted = accepted || file.CPUMs > 0 || file.Subrequests > 0 || file.TimeoutSecs > 0
	}
	if len(file.Metadata) > 0 {
		cfg.CloudflareDynamicWorkers.Metadata = map[string]string{}
		accepted = true
		for key, value := range file.Metadata {
			key = strings.TrimSpace(key)
			if key != "" {
				cfg.CloudflareDynamicWorkers.Metadata[key] = value
			}
		}
	}
	return accepted
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
	inputSource := configInputSourceForFile(providerSource)
	if !trusted && cfg.credentialProvenance.repositoryRoot == "" {
		if root, err := os.Getwd(); err == nil {
			cfg.credentialProvenance.repositoryRoot = root
		}
	}
	if file.Profile != "" {
		cfg.Profile = file.Profile
		recordConfigInput(cfg, configInputGeneric, inputSource, true)
	}
	if file.Provider != "" {
		setProviderSelection(cfg, file.Provider, providerSource)
		cfg.brokerProvider = ""
	}
	if file.Target != "" {
		cfg.TargetOS = file.Target
		cfg.targetExplicit = true
		recordConfigInput(cfg, configInputGeneric, inputSource, true)
	}
	if file.TargetOS != "" {
		cfg.TargetOS = file.TargetOS
		cfg.targetExplicit = true
		recordConfigInput(cfg, configInputGeneric, inputSource, true)
	}
	if file.Architecture != "" {
		cfg.Architecture = file.Architecture
		cfg.architectureExplicit = true
		recordConfigInput(cfg, configInputGeneric, inputSource, true)
	}
	if file.OSImage != "" {
		cfg.OSImage = file.OSImage
		cfg.osImageExplicit = true
		recordConfigInput(cfg, configInputGeneric, inputSource, true)
		if normalized, err := normalizeOSImage(file.OSImage); err == nil {
			cfg.OSImage = normalized
			applyOSImageProviderDefaults(cfg, false)
		}
	}
	if file.Windows != nil && file.Windows.Mode != "" {
		cfg.WindowsMode = file.Windows.Mode
		cfg.explicitWindowsMode = file.Windows.Mode
		recordConfigInput(cfg, configInputGeneric, inputSource, true)
	}
	recordConfigInput(cfg, configInputGeneric, inputSource, applyOptional(&cfg.Desktop, file.Desktop))
	if file.DesktopEnv != "" {
		cfg.DesktopEnv = file.DesktopEnv
		recordConfigInput(cfg, configInputGeneric, inputSource, true)
	}
	recordConfigInput(cfg, configInputGeneric, inputSource, applyOptional(&cfg.Browser, file.Browser))
	recordConfigInput(cfg, configInputGeneric, inputSource, applyOptional(&cfg.Code, file.Code))
	if file.Network != "" {
		cfg.Network = NetworkMode(strings.ToLower(strings.TrimSpace(file.Network)))
		recordConfigInput(cfg, configInputGeneric, inputSource, true)
	}
	if file.Class != "" {
		cfg.Class = file.Class
		MarkClassExplicit(cfg)
		recordConfigInput(cfg, configInputGeneric, inputSource, true)
	}
	if file.ServerType != "" {
		cfg.ServerType = file.ServerType
		cfg.ServerTypeExplicit = true
		recordConfigInput(cfg, configInputGeneric, inputSource, true)
	}
	if file.Coordinator != "" {
		cfg.Coordinator = file.Coordinator
		recordConfigInput(cfg, configInputGeneric, inputSource, true)
		cfg.credentialProvenance.coordinator = credentialSource
	}
	if file.CoordinatorToken != "" {
		cfg.CoordToken = file.CoordinatorToken
		recordConfigInput(cfg, configInputGeneric, inputSource, true)
		cfg.credentialProvenance.coordToken = credentialSource
	}
	if file.HostID != "" {
		cfg.HostID = file.HostID
		recordConfigInput(cfg, configInputGeneric, inputSource, true)
	}
	if file.Broker != nil {
		if file.Broker.URL != "" {
			cfg.Coordinator = file.Broker.URL
			recordConfigInput(cfg, configInputGeneric, inputSource, true)
			cfg.credentialProvenance.coordinator = credentialSource
		}
		if file.Broker.Token != "" {
			cfg.CoordToken = file.Broker.Token
			recordConfigInput(cfg, configInputGeneric, inputSource, true)
			cfg.credentialProvenance.coordToken = credentialSource
		}
		if file.Broker.Mode != "" {
			cfg.BrokerMode = BrokerMode(file.Broker.Mode)
			recordConfigInput(cfg, configInputGeneric, inputSource, true)
		}
		recordConfigInput(cfg, configInputGeneric, inputSource, applyOptional(&cfg.BrokerAutoWebVNC, file.Broker.AutoWebVNC))
		if trusted && len(file.Broker.LoginRedirectOrigins) > 0 {
			cfg.BrokerLoginRedirectOrigins = normalizeList(file.Broker.LoginRedirectOrigins)
			recordConfigInput(cfg, configInputGeneric, inputSource, true)
		}
		if file.Broker.AdminToken != "" {
			cfg.CoordAdminToken = file.Broker.AdminToken
			recordConfigInput(cfg, configInputGeneric, inputSource, true)
			cfg.credentialProvenance.coordAdminToken = credentialSource
		}
		if file.Broker.Provider != "" {
			setProviderSelection(cfg, file.Broker.Provider, providerSource)
			cfg.brokerProvider = file.Broker.Provider
		}
		if file.Broker.Access != nil {
			if file.Broker.Access.ClientID != "" {
				cfg.Access.ClientID = file.Broker.Access.ClientID
				recordConfigInput(cfg, configInputGeneric, inputSource, true)
				cfg.credentialProvenance.accessClientID = credentialSource
			}
			if file.Broker.Access.ClientSecret != "" {
				cfg.Access.ClientSecret = file.Broker.Access.ClientSecret
				recordConfigInput(cfg, configInputGeneric, inputSource, true)
				cfg.credentialProvenance.accessClientSecret = credentialSource
			}
			if file.Broker.Access.Token != "" {
				cfg.Access.Token = file.Broker.Access.Token
				recordConfigInput(cfg, configInputGeneric, inputSource, true)
				cfg.credentialProvenance.accessToken = credentialSource
			}
		}
	}
	if file.Hetzner != nil {
		if file.Hetzner.Location != "" {
			cfg.Location = file.Hetzner.Location
			recordConfigInput(cfg, "hetzner", inputSource, true)
			cfg.locationExplicit = true
		}
		if file.Hetzner.Image != "" {
			cfg.Image = file.Hetzner.Image
			recordConfigInput(cfg, "hetzner", inputSource, true)
			cfg.imageExplicit = true
		}
		if file.Hetzner.SSHKey != "" {
			cfg.ProviderKey = file.Hetzner.SSHKey
			recordConfigInput(cfg, "hetzner", inputSource, true)
		}
	}
	{
		applied, err := cfg.DigitalOcean.applyFile(file.DigitalOcean)
		recordConfigInput(cfg, "digitalocean", inputSource, applied.InputAccepted)
		if applied.Image {
			cfg.digitalOceanImageExplicit = true
		}
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.Vultr.applyFile(file.Vultr)
		recordConfigInput(cfg, "vultr", inputSource, applied.InputAccepted)
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.Linode.applyFile(file.Linode)
		recordConfigInput(cfg, "linode", inputSource, applied.InputAccepted)
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
			recordConfigInput(cfg, "github-codespaces", inputSource, true)
		}
		if trusted && file.GitHubCodespaces.GHPath != "" {
			cfg.GitHubCodespaces.GHPath = expandUserPath(file.GitHubCodespaces.GHPath)
			recordConfigInput(cfg, "github-codespaces", inputSource, true)
		}
		if trusted && file.GitHubCodespaces.Repo != "" {
			cfg.GitHubCodespaces.Repo = file.GitHubCodespaces.Repo
			recordConfigInput(cfg, "github-codespaces", inputSource, true)
		}
		if file.GitHubCodespaces.Ref != "" {
			cfg.GitHubCodespaces.Ref = file.GitHubCodespaces.Ref
			recordConfigInput(cfg, "github-codespaces", inputSource, true)
		}
		if file.GitHubCodespaces.Machine != "" {
			cfg.GitHubCodespaces.Machine = file.GitHubCodespaces.Machine
			recordConfigInput(cfg, "github-codespaces", inputSource, true)
		}
		if file.GitHubCodespaces.DevcontainerPath != "" {
			cfg.GitHubCodespaces.DevcontainerPath = file.GitHubCodespaces.DevcontainerPath
			recordConfigInput(cfg, "github-codespaces", inputSource, true)
		}
		if file.GitHubCodespaces.WorkingDirectory != "" {
			cfg.GitHubCodespaces.WorkingDirectory = file.GitHubCodespaces.WorkingDirectory
			recordConfigInput(cfg, "github-codespaces", inputSource, true)
		}
		if file.GitHubCodespaces.Geo != "" {
			cfg.GitHubCodespaces.Geo = file.GitHubCodespaces.Geo
			recordConfigInput(cfg, "github-codespaces", inputSource, true)
		}
		if trusted {
			recordConfigInput(cfg, "github-codespaces", inputSource, applyLeaseDuration(&cfg.GitHubCodespaces.IdleTimeout, file.GitHubCodespaces.IdleTimeout))
			if applyNonNegativeLeaseDuration(&cfg.GitHubCodespaces.RetentionPeriod, file.GitHubCodespaces.RetentionPeriod) {
				recordConfigInput(cfg, "github-codespaces", inputSource, true)
				MarkGitHubCodespacesRetentionExplicit(cfg)
			}
		}
		if trusted && file.GitHubCodespaces.DeleteOnRelease != nil {
			cfg.GitHubCodespaces.DeleteOnRelease = *file.GitHubCodespaces.DeleteOnRelease
			recordConfigInput(cfg, "github-codespaces", inputSource, true)
			MarkDeleteOnReleaseExplicit(cfg, "github-codespaces")
		}
		if file.GitHubCodespaces.WorkRoot != "" {
			cfg.GitHubCodespaces.WorkRoot = file.GitHubCodespaces.WorkRoot
			recordConfigInput(cfg, "github-codespaces", inputSource, true)
		}
	}
	{
		applied := cfg.Lambda.applyFile(file.Lambda)
		recordConfigInput(cfg, "lambda", inputSource, applied.InputAccepted)
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
	{
		applied, err := cfg.Nebius.applyFile(file.Nebius, trusted)
		recordConfigInput(cfg, "nebius", inputSource, applied.InputAccepted)
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.OVH.applyFile(file.OVH, trusted)
		recordConfigInput(cfg, "ovh", inputSource, applied.InputAccepted)
		if applied.Image {
			cfg.ovhImageExplicit = true
		}
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.Scaleway.applyFile(file.Scaleway)
		recordConfigInput(cfg, "scaleway", inputSource, applied.InputAccepted)
		MarkScalewayConfigApplied(cfg, applied)
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.TencentCloud.applyFile(file.TencentCloud, trusted)
		recordConfigInput(cfg, "tencentcloud", inputSource, applied.InputAccepted)
		MarkTencentCloudConfigApplied(cfg, applied)
		if err != nil {
			return err
		}
	}
	if file.AWS != nil {
		if file.AWS.Region != "" {
			cfg.AWSRegion = file.AWS.Region
			recordConfigInput(cfg, "aws", inputSource, true)
			recordConfigInput(cfg, "aws-lambda-microvm", inputSource, true)
		}
		if file.AWS.AMI != "" {
			cfg.AWSAMI = file.AWS.AMI
			recordConfigInput(cfg, "aws", inputSource, true)
		}
		if file.AWS.SecurityGroupID != "" {
			cfg.AWSSGID = file.AWS.SecurityGroupID
			recordConfigInput(cfg, "aws", inputSource, true)
		}
		if file.AWS.SubnetID != "" {
			cfg.AWSSubnetID = file.AWS.SubnetID
			recordConfigInput(cfg, "aws", inputSource, true)
		}
		if file.AWS.InstanceProfile != "" {
			cfg.AWSProfile = file.AWS.InstanceProfile
			recordConfigInput(cfg, "aws", inputSource, true)
		}
		if file.AWS.RootGB > 0 {
			cfg.AWSRootGB = file.AWS.RootGB
			recordConfigInput(cfg, "aws", inputSource, true)
		}
		if len(file.AWS.SSHCIDRs) > 0 {
			cfg.AWSSSHCIDRs = file.AWS.SSHCIDRs
			recordConfigInput(cfg, "aws", inputSource, true)
		}
		if file.AWS.MacHostID != "" {
			cfg.AWSMacHostID = file.AWS.MacHostID
			recordConfigInput(cfg, "aws", inputSource, true)
			if cfg.HostID == "" {
				cfg.HostID = file.AWS.MacHostID
			}
		}
	}
	{
		applied, err := cfg.AWSLambdaMicroVM.applyFile(file.AWSLambdaMicroVM)
		recordConfigInput(cfg, "aws-lambda-microvm", inputSource, applied.InputAccepted)
		if err != nil {
			return err
		}
	}
	if file.Azure != nil {
		if file.Azure.Backend != "" {
			cfg.AzureBackend = file.Azure.Backend
			recordConfigInput(cfg, "azure", inputSource, true)
		}
		if file.Azure.SubscriptionID != "" {
			cfg.AzureSubscription = file.Azure.SubscriptionID
			recordConfigInput(cfg, "azure", inputSource, true)
			recordConfigInput(cfg, "azure-dynamic-sessions", inputSource, true)
		}
		if file.Azure.TenantID != "" {
			cfg.AzureTenant = file.Azure.TenantID
			recordConfigInput(cfg, "azure", inputSource, true)
			recordConfigInput(cfg, "azure-dynamic-sessions", inputSource, true)
		}
		if file.Azure.ClientID != "" {
			cfg.AzureClientID = file.Azure.ClientID
			recordConfigInput(cfg, "azure", inputSource, true)
		}
		if file.Azure.Location != "" {
			cfg.AzureLocation = file.Azure.Location
			recordConfigInput(cfg, "azure", inputSource, true)
		}
		if file.Azure.ResourceGroup != "" {
			cfg.AzureResourceGroup = file.Azure.ResourceGroup
			recordConfigInput(cfg, "azure", inputSource, true)
		}
		if file.Azure.Image != "" {
			cfg.AzureImage = file.Azure.Image
			recordConfigInput(cfg, "azure", inputSource, true)
			cfg.azureImageExplicit = true
		}
		if file.Azure.OSDisk != "" {
			cfg.AzureOSDisk = file.Azure.OSDisk
			recordConfigInput(cfg, "azure", inputSource, true)
			cfg.AzureOSDiskExplicit = true
		}
		if file.Azure.SnapshotSKU != "" {
			cfg.AzureSnapshotSKU = file.Azure.SnapshotSKU
			recordConfigInput(cfg, "azure", inputSource, true)
		}
		if file.Azure.OSDiskSKU != "" {
			cfg.AzureOSDiskSKU = file.Azure.OSDiskSKU
			recordConfigInput(cfg, "azure", inputSource, true)
		}
		if file.Azure.VNet != "" {
			cfg.AzureVNet = file.Azure.VNet
			recordConfigInput(cfg, "azure", inputSource, true)
		}
		if file.Azure.Subnet != "" {
			cfg.AzureSubnet = file.Azure.Subnet
			recordConfigInput(cfg, "azure", inputSource, true)
		}
		if file.Azure.NSG != "" {
			cfg.AzureNSG = file.Azure.NSG
			recordConfigInput(cfg, "azure", inputSource, true)
		}
		if len(file.Azure.SSHCIDRs) > 0 {
			cfg.AzureSSHCIDRs = file.Azure.SSHCIDRs
			recordConfigInput(cfg, "azure", inputSource, true)
		}
		if file.Azure.Network != "" {
			cfg.AzureNetwork = file.Azure.Network
			recordConfigInput(cfg, "azure", inputSource, true)
		}
	}
	{
		applied, err := cfg.AzureDynamicSessions.applyFile(file.AzureDynamicSessions)
		recordConfigInput(cfg, "azure-dynamic-sessions", inputSource, applied.InputAccepted)
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
			recordConfigInput(cfg, "gcp", inputSource, true)
			cfg.gcpProjectExplicit = true
		}
		if file.GCP.Zone != "" {
			cfg.GCPZone = file.GCP.Zone
			recordConfigInput(cfg, "gcp", inputSource, true)
			cfg.gcpZoneExplicit = true
		}
		if file.GCP.Image != "" {
			cfg.GCPImage = file.GCP.Image
			recordConfigInput(cfg, "gcp", inputSource, true)
			cfg.gcpImageExplicit = true
		}
		if file.GCP.Network != "" {
			cfg.GCPNetwork = file.GCP.Network
			recordConfigInput(cfg, "gcp", inputSource, true)
			cfg.gcpNetworkExplicit = true
		}
		if file.GCP.Subnet != "" {
			cfg.GCPSubnet = file.GCP.Subnet
			recordConfigInput(cfg, "gcp", inputSource, true)
		}
		if len(file.GCP.Tags) > 0 {
			cfg.GCPTags = file.GCP.Tags
			recordConfigInput(cfg, "gcp", inputSource, true)
			cfg.gcpTagsExplicit = true
		}
		if len(file.GCP.SSHCIDRs) > 0 {
			cfg.GCPSSHCIDRs = file.GCP.SSHCIDRs
			recordConfigInput(cfg, "gcp", inputSource, true)
		}
		if file.GCP.RootGB > 0 {
			cfg.GCPRootGB = file.GCP.RootGB
			recordConfigInput(cfg, "gcp", inputSource, true)
			cfg.gcpRootGBExplicit = true
		}
		if file.GCP.ServiceAccount != "" {
			cfg.GCPServiceAccount = file.GCP.ServiceAccount
			recordConfigInput(cfg, "gcp", inputSource, true)
		}
	}
	{
		applied, err := cfg.Incus.applyFile(file.Incus)
		recordConfigInput(cfg, "incus", inputSource, applied.InputAccepted)
		cfg.Incus.ExpandAppliedLocalPaths(applied)
		if applied.DeleteOnRelease {
			MarkDeleteOnReleaseExplicit(cfg, "incus")
		}
		if err != nil {
			return err
		}
	}
	if file.Proxmox != nil {
		if file.Proxmox.APIURL != "" {
			cfg.Proxmox.APIURL = file.Proxmox.APIURL
			recordConfigInput(cfg, "proxmox", inputSource, true)
			cfg.credentialProvenance.proxmoxAPIURL = credentialSource
		}
		if file.Proxmox.TokenID != "" {
			cfg.Proxmox.TokenID = file.Proxmox.TokenID
			recordConfigInput(cfg, "proxmox", inputSource, true)
			cfg.credentialProvenance.proxmoxTokenID = credentialSource
		}
		if file.Proxmox.TokenSecret != "" {
			cfg.Proxmox.TokenSecret = file.Proxmox.TokenSecret
			recordConfigInput(cfg, "proxmox", inputSource, true)
			cfg.credentialProvenance.proxmoxTokenSecret = credentialSource
		}
		if file.Proxmox.Node != "" {
			cfg.Proxmox.Node = file.Proxmox.Node
			recordConfigInput(cfg, "proxmox", inputSource, true)
		}
		if file.Proxmox.TemplateID > 0 {
			cfg.Proxmox.TemplateID = file.Proxmox.TemplateID
			recordConfigInput(cfg, "proxmox", inputSource, true)
		}
		if file.Proxmox.Storage != "" {
			cfg.Proxmox.Storage = file.Proxmox.Storage
			recordConfigInput(cfg, "proxmox", inputSource, true)
		}
		if file.Proxmox.Pool != "" {
			cfg.Proxmox.Pool = file.Proxmox.Pool
			recordConfigInput(cfg, "proxmox", inputSource, true)
		}
		if file.Proxmox.Bridge != "" {
			cfg.Proxmox.Bridge = file.Proxmox.Bridge
			recordConfigInput(cfg, "proxmox", inputSource, true)
		}
		if file.Proxmox.User != "" {
			cfg.Proxmox.User = file.Proxmox.User
			recordConfigInput(cfg, "proxmox", inputSource, true)
		}
		if file.Proxmox.WorkRoot != "" {
			cfg.Proxmox.WorkRoot = file.Proxmox.WorkRoot
			recordConfigInput(cfg, "proxmox", inputSource, true)
		}
		recordConfigInput(cfg, "proxmox", inputSource, applyOptional(&cfg.Proxmox.FullClone, file.Proxmox.FullClone))
		if file.Proxmox.InsecureTLS != nil {
			cfg.Proxmox.InsecureTLS = *file.Proxmox.InsecureTLS
			recordConfigInput(cfg, "proxmox", inputSource, true)
			cfg.credentialProvenance.proxmoxInsecureTLS = credentialSource
		}
	}
	{
		applied, err := cfg.Firecracker.applyFile(file.Firecracker, trusted)
		recordConfigInput(cfg, "firecracker", inputSource, applied.InputAccepted)
		cfg.Firecracker.ExpandAppliedLocalPaths(applied)
		if applied.DeleteOnRelease {
			MarkDeleteOnReleaseExplicit(cfg, "firecracker")
			recordConfigInputIntent(cfg, "firecracker", inputSource, true)
		}
		if err != nil {
			return err
		}
	}
	if file.XCPNg != nil {
		// Project config is repository-controlled. Do not let it redirect
		// or replace inherited user or environment XAPI credentials.
		if trusted && file.XCPNg.APIURL != "" {
			cfg.XCPNg.APIURL = file.XCPNg.APIURL
			recordConfigInput(cfg, "xcp-ng", inputSource, true)
		}
		if trusted && file.XCPNg.Username != "" {
			cfg.XCPNg.Username = file.XCPNg.Username
			recordConfigInput(cfg, "xcp-ng", inputSource, true)
		}
		if trusted && file.XCPNg.Password != "" {
			cfg.XCPNg.Password = file.XCPNg.Password
			recordConfigInput(cfg, "xcp-ng", inputSource, true)
		}
		recordConfigInput(cfg, "xcp-ng", inputSource, applyXCPNgNameUUIDPair(&cfg.XCPNg.Template, &cfg.XCPNg.TemplateUUID, file.XCPNg.Template, file.XCPNg.TemplateUUID))
		recordConfigInput(cfg, "xcp-ng", inputSource, applyXCPNgNameUUIDPair(&cfg.XCPNg.SR, &cfg.XCPNg.SRUUID, file.XCPNg.SR, file.XCPNg.SRUUID))
		recordConfigInput(cfg, "xcp-ng", inputSource, applyXCPNgNameUUIDPair(&cfg.XCPNg.Network, &cfg.XCPNg.NetworkUUID, file.XCPNg.Network, file.XCPNg.NetworkUUID))
		if file.XCPNg.Host != "" {
			cfg.XCPNg.Host = file.XCPNg.Host
			recordConfigInput(cfg, "xcp-ng", inputSource, true)
		}
		if file.XCPNg.User != "" {
			cfg.XCPNg.User = file.XCPNg.User
			recordConfigInput(cfg, "xcp-ng", inputSource, true)
		}
		if file.XCPNg.WorkRoot != "" {
			cfg.XCPNg.WorkRoot = file.XCPNg.WorkRoot
			recordConfigInput(cfg, "xcp-ng", inputSource, true)
		}
		if trusted && file.XCPNg.InsecureTLS != nil {
			cfg.XCPNg.InsecureTLS = *file.XCPNg.InsecureTLS
			recordConfigInput(cfg, "xcp-ng", inputSource, true)
		}
	}
	if file.Parallels != nil {
		if file.Parallels.Template != "" {
			cfg.Parallels.Template = file.Parallels.Template
			recordConfigInput(cfg, "parallels", inputSource, true)
		}
		if file.Parallels.Source != "" {
			cfg.Parallels.Source = file.Parallels.Source
			recordConfigInput(cfg, "parallels", inputSource, true)
		}
		if file.Parallels.SourceID != "" {
			cfg.Parallels.SourceID = file.Parallels.SourceID
			recordConfigInput(cfg, "parallels", inputSource, true)
		}
		if file.Parallels.SourceSnapshot != "" {
			cfg.Parallels.SourceSnapshot = file.Parallels.SourceSnapshot
			recordConfigInput(cfg, "parallels", inputSource, true)
		}
		if file.Parallels.SourceSnapshotID != "" {
			cfg.Parallels.SourceSnapshotID = file.Parallels.SourceSnapshotID
			recordConfigInput(cfg, "parallels", inputSource, true)
		}
		if file.Parallels.CloneMode != "" {
			cfg.Parallels.CloneMode = file.Parallels.CloneMode
			recordConfigInput(cfg, "parallels", inputSource, true)
		}
		if file.Parallels.Host != "" {
			cfg.Parallels.Host = file.Parallels.Host
			recordConfigInput(cfg, "parallels", inputSource, true)
			cfg.credentialProvenance.parallelsHost = credentialSource
		}
		if file.Parallels.HostUser != "" {
			cfg.Parallels.HostUser = file.Parallels.HostUser
			recordConfigInput(cfg, "parallels", inputSource, true)
		}
		if file.Parallels.HostKey != "" {
			cfg.Parallels.HostKey = expandUserPath(file.Parallels.HostKey)
			recordConfigInput(cfg, "parallels", inputSource, true)
			cfg.credentialProvenance.parallelsHostKey = credentialSource
		}
		if file.Parallels.VMRoot != "" {
			cfg.Parallels.VMRoot = expandUserPath(file.Parallels.VMRoot)
			recordConfigInput(cfg, "parallels", inputSource, true)
		}
		if file.Parallels.User != "" {
			cfg.Parallels.User = file.Parallels.User
			recordConfigInput(cfg, "parallels", inputSource, true)
		}
		if file.Parallels.WorkRoot != "" {
			cfg.Parallels.WorkRoot = file.Parallels.WorkRoot
			recordConfigInput(cfg, "parallels", inputSource, true)
		}
		recordConfigInput(cfg, "parallels", inputSource, applyLeaseDuration(&cfg.Parallels.StartupTimeout, file.Parallels.StartupTimeout))
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
				recordConfigInput(cfg, "parallels", inputSource, true)
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
				recordConfigInput(cfg, "parallels", inputSource, true)
			}
		}
	}
	if file.SSH != nil {
		if file.SSH.User != "" {
			cfg.SSHUser = file.SSH.User
			recordConfigInput(cfg, configInputGeneric, inputSource, true)
			MarkSSHUserExplicit(cfg)
		}
		if file.SSH.Key != "" {
			cfg.SSHKey = expandUserPath(file.SSH.Key)
			recordConfigInput(cfg, configInputGeneric, inputSource, true)
			MarkSSHKeyExplicit(cfg)
			cfg.credentialProvenance.sshKey = credentialSource
		}
		if file.SSH.Port != "" {
			cfg.SSHPort = file.SSH.Port
			recordConfigInput(cfg, configInputGeneric, inputSource, true)
			MarkSSHPortExplicit(cfg)
		}
		if file.SSH.FallbackPorts != nil {
			cfg.SSHFallbackPorts = normalizeList(*file.SSH.FallbackPorts)
			recordConfigInput(cfg, configInputGeneric, inputSource, true)
			cfg.sshFallbackPortsExplicit = true
			cfg.explicitSSHFallbackPorts = append([]string(nil), cfg.SSHFallbackPorts...)
		}
	}
	if file.WorkRoot != "" {
		cfg.WorkRoot = file.WorkRoot
		recordConfigInput(cfg, configInputGeneric, inputSource, true)
		cfg.explicitWorkRoot = file.WorkRoot
	}
	recordConfigInput(cfg, configInputGeneric, inputSource, applyLeaseDuration(&cfg.TTL, file.TTL))
	recordConfigInput(cfg, configInputGeneric, inputSource, applyLeaseDuration(&cfg.IdleTimeout, file.IdleTimeout))
	if file.Lease != nil {
		recordConfigInput(cfg, configInputGeneric, inputSource, applyLeaseDuration(&cfg.TTL, file.Lease.TTL))
		recordConfigInput(cfg, configInputGeneric, inputSource, applyLeaseDuration(&cfg.IdleTimeout, file.Lease.IdleTimeout))
	}
	if file.Sync != nil {
		{
			var accepted bool
			cfg.Sync.Excludes, accepted = appendOrderedStringsAccepted(cfg.Sync.Excludes, file.Sync.Exclude...)
			recordConfigInput(cfg, configInputGeneric, inputSource, accepted)
		}
		{
			var accepted bool
			cfg.Sync.Excludes, accepted = appendOrderedStringsAccepted(cfg.Sync.Excludes, file.Sync.Excludes...)
			recordConfigInput(cfg, configInputGeneric, inputSource, accepted)
		}
		{
			var accepted bool
			cfg.Sync.Includes, accepted = appendUniqueStringsAccepted(cfg.Sync.Includes, file.Sync.Include...)
			recordConfigInput(cfg, configInputGeneric, inputSource, accepted)
		}
		{
			var accepted bool
			cfg.Sync.Includes, accepted = appendUniqueStringsAccepted(cfg.Sync.Includes, file.Sync.Includes...)
			recordConfigInput(cfg, configInputGeneric, inputSource, accepted)
		}
		recordConfigInput(cfg, configInputGeneric, inputSource, applyOptional(&cfg.Sync.Delete, file.Sync.Delete))
		recordConfigInput(cfg, configInputGeneric, inputSource, applyOptional(&cfg.Sync.Checksum, file.Sync.Checksum))
		recordConfigInput(cfg, configInputGeneric, inputSource, applyOptional(&cfg.Sync.GitSeed, file.Sync.GitSeed))
		recordConfigInput(cfg, configInputGeneric, inputSource, applyOptional(&cfg.Sync.GitOverlay, file.Sync.GitOverlay))
		recordConfigInput(cfg, configInputGeneric, inputSource, applyOptional(&cfg.Sync.Fingerprint, file.Sync.Fingerprint))
		if file.Sync.BaseRef != "" {
			cfg.Sync.BaseRef = file.Sync.BaseRef
			recordConfigInput(cfg, configInputGeneric, inputSource, true)
		}
		if file.Sync.Timeout != "" {
			if timeout, err := time.ParseDuration(file.Sync.Timeout); err == nil {
				cfg.Sync.Timeout = timeout
				recordConfigInput(cfg, configInputGeneric, inputSource, true)
			}
		}
		if file.Sync.WarnFiles > 0 {
			cfg.Sync.WarnFiles = file.Sync.WarnFiles
			recordConfigInput(cfg, configInputGeneric, inputSource, true)
		}
		if file.Sync.WarnBytes > 0 {
			cfg.Sync.WarnBytes = file.Sync.WarnBytes
			recordConfigInput(cfg, configInputGeneric, inputSource, true)
		}
		if file.Sync.FailFiles > 0 {
			cfg.Sync.FailFiles = file.Sync.FailFiles
			recordConfigInput(cfg, configInputGeneric, inputSource, true)
		}
		if file.Sync.FailBytes > 0 {
			cfg.Sync.FailBytes = file.Sync.FailBytes
			recordConfigInput(cfg, configInputGeneric, inputSource, true)
		}
		recordConfigInput(cfg, configInputGeneric, inputSource, applyOptional(&cfg.Sync.AllowLarge, file.Sync.AllowLarge))
	}
	if file.Run != nil && file.Run.PreflightTools != nil {
		cfg.Run.PreflightTools = normalizePreflightToolNames(file.Run.PreflightTools)
		recordConfigInput(cfg, configInputGeneric, inputSource, true)
	}
	if file.Env != nil && file.Env.Allow != nil {
		cfg.EnvAllow = appendUniqueStrings(nil, file.Env.Allow...)
		recordConfigInput(cfg, configInputGeneric, inputSource, true)
	}
	if file.Capacity != nil {
		if file.Capacity.Market != "" {
			cfg.Capacity.Market = file.Capacity.Market
			recordConfigInput(cfg, configInputGeneric, inputSource, true)
			MarkCapacityMarketExplicit(cfg)
		}
		if file.Capacity.Strategy != "" {
			cfg.Capacity.Strategy = file.Capacity.Strategy
			recordConfigInput(cfg, configInputGeneric, inputSource, true)
		}
		if file.Capacity.Fallback != "" {
			cfg.Capacity.Fallback = file.Capacity.Fallback
			recordConfigInput(cfg, configInputGeneric, inputSource, true)
		}
		if len(file.Capacity.Regions) > 0 {
			cfg.Capacity.Regions = appendUniqueStrings(nil, file.Capacity.Regions...)
			recordConfigInput(cfg, configInputGeneric, inputSource, true)
		}
		if len(file.Capacity.AvailabilityZones) > 0 {
			cfg.Capacity.AvailabilityZones = appendUniqueStrings(nil, file.Capacity.AvailabilityZones...)
			recordConfigInput(cfg, configInputGeneric, inputSource, true)
		}
		recordConfigInput(cfg, configInputGeneric, inputSource, applyOptional(&cfg.Capacity.Hints, file.Capacity.Hints))
	}
	if file.Actions != nil {
		if file.Actions.Repo != "" {
			cfg.Actions.Repo = file.Actions.Repo
			recordConfigInput(cfg, configInputGeneric, inputSource, true)
		}
		if file.Actions.Workflow != "" {
			cfg.Actions.Workflow = file.Actions.Workflow
			recordConfigInput(cfg, configInputGeneric, inputSource, true)
		}
		if file.Actions.Job != "" {
			cfg.Actions.Job = file.Actions.Job
			recordConfigInput(cfg, configInputGeneric, inputSource, true)
		}
		if file.Actions.Ref != "" {
			cfg.Actions.Ref = file.Actions.Ref
			recordConfigInput(cfg, configInputGeneric, inputSource, true)
		}
		if len(file.Actions.Fields) > 0 {
			cfg.Actions.Fields = appendUniqueStrings(nil, file.Actions.Fields...)
			recordConfigInput(cfg, configInputGeneric, inputSource, true)
		}
		if len(file.Actions.RunnerLabels) > 0 {
			cfg.Actions.RunnerLabels = appendUniqueStrings(nil, file.Actions.RunnerLabels...)
			recordConfigInput(cfg, configInputGeneric, inputSource, true)
		}
		if file.Actions.RunnerVersion != "" {
			cfg.Actions.RunnerVersion = file.Actions.RunnerVersion
			recordConfigInput(cfg, configInputGeneric, inputSource, true)
		}
		recordConfigInput(cfg, configInputGeneric, inputSource, applyOptional(&cfg.Actions.Ephemeral, file.Actions.Ephemeral))
	}
	{
		applied, err := cfg.Blacksmith.applyFile(file.Blacksmith)
		recordConfigInput(cfg, "blacksmith-testbox", inputSource, applied.InputAccepted)
		if err != nil {
			return err
		}
	}
	if err := applyKubeVirtFileConfig(cfg, file.KubeVirt, trusted, inputSource); err != nil {
		return err
	}
	{
		applied, err := cfg.SealosDevbox.applyFile(file.SealosDevbox, trusted)
		recordConfigInput(cfg, "sealos-devbox", inputSource, applied.InputAccepted)
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
	if err := applyAgentSandboxFileConfig(cfg, file.AgentSandbox, trusted, inputSource); err != nil {
		return err
	}
	if file.External != nil {
		if file.External.Command != "" {
			cfg.External.Command = file.External.Command
			recordConfigInput(cfg, "external", inputSource, true)
		}
		if len(file.External.Args) > 0 {
			cfg.External.Args = append([]string(nil), file.External.Args...)
			recordConfigInput(cfg, "external", inputSource, true)
		}
		if file.External.Config != nil {
			cfg.External.Config = file.External.Config
			recordConfigInput(cfg, "external", inputSource, true)
			cfg.credentialProvenance.externalConfig = credentialSource
		}
		if applyOptional(&cfg.External.Capabilities, file.External.Capabilities) {
			recordConfigInput(cfg, "external", inputSource, true)
		}
		if file.External.Lifecycle != nil {
			cfg.External.Lifecycle = *file.External.Lifecycle
			recordConfigInput(cfg, "external", inputSource, true)
			cfg.credentialProvenance.externalLifecycle = credentialSource
		}
		if file.External.Connection != nil {
			PreserveExternalDesktopChildEnvironmentBoundary(cfg)
			cfg.External.Connection = *file.External.Connection
			recordConfigInput(cfg, "external", inputSource, true)
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
			recordConfigInput(cfg, "external", inputSource, true)
		}
		if file.External.RoutingFile != "" {
			cfg.External.RoutingFile = file.External.RoutingFile
			recordConfigInput(cfg, "external", inputSource, true)
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
		recordConfigInput(cfg, "namespace-devbox", inputSource, applied.InputAccepted)
		if applied.DeleteOnRelease {
			MarkDeleteOnReleaseExplicit(cfg, "namespace-devbox")
		}
		if err != nil {
			return err
		}
	}
	if err := applyNamespaceInstanceFileConfig(cfg, file.NamespaceInstance, trusted, inputSource); err != nil {
		return err
	}
	if err := applyPhalaFileConfig(cfg, file.Phala, trusted, inputSource); err != nil {
		return err
	}
	if file.Boxd != nil {
		// Only trusted config can redirect credentials or organization billing.
		if trusted {
			if file.Boxd.APIURL != "" {
				cfg.Boxd.APIURL = file.Boxd.APIURL
				recordConfigInput(cfg, "boxd", inputSource, true)
			}
			if file.Boxd.Org != "" {
				cfg.Boxd.Org = file.Boxd.Org
				recordConfigInput(cfg, "boxd", inputSource, true)
			}
		}
		if file.Boxd.WorkRoot != "" {
			cfg.Boxd.WorkRoot = file.Boxd.WorkRoot
			recordConfigInput(cfg, "boxd", inputSource, true)
			MarkBoxdWorkRootExplicit(cfg)
			recordConfigInputIntent(cfg, "boxd", inputSource, true)
		}
		if file.Boxd.DeleteOnRelease != nil {
			cfg.Boxd.DeleteOnRelease = *file.Boxd.DeleteOnRelease
			recordConfigInput(cfg, "boxd", inputSource, true)
			MarkDeleteOnReleaseExplicit(cfg, "boxd")
			recordConfigInputIntent(cfg, "boxd", inputSource, true)
		}
	}
	if err := applyCoderFileConfig(cfg, file.Coder, inputSource); err != nil {
		return err
	}
	{
		applied, err := cfg.Morph.applyFile(file.Morph)
		recordConfigInput(cfg, "morph", inputSource, applied.InputAccepted)
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
			recordConfigInput(cfg, "daytona", inputSource, true)
			cfg.credentialProvenance.daytonaAPIURL = credentialSource
		}
		if file.Daytona.Snapshot != "" {
			cfg.Daytona.Snapshot = file.Daytona.Snapshot
			recordConfigInput(cfg, "daytona", inputSource, true)
		}
		if file.Daytona.Target != "" {
			cfg.Daytona.Target = file.Daytona.Target
			recordConfigInput(cfg, "daytona", inputSource, true)
		}
		if file.Daytona.User != "" {
			cfg.Daytona.User = file.Daytona.User
			recordConfigInput(cfg, "daytona", inputSource, true)
		}
		if file.Daytona.WorkRoot != "" {
			cfg.Daytona.WorkRoot = file.Daytona.WorkRoot
			recordConfigInput(cfg, "daytona", inputSource, true)
		}
		if file.Daytona.SSHGatewayHost != "" {
			cfg.Daytona.SSHGatewayHost = file.Daytona.SSHGatewayHost
			recordConfigInput(cfg, "daytona", inputSource, true)
			cfg.credentialProvenance.daytonaSSHGateway = credentialSource
		}
		if file.Daytona.SSHAccessMinutes > 0 {
			cfg.Daytona.SSHAccessMinutes = file.Daytona.SSHAccessMinutes
			recordConfigInput(cfg, "daytona", inputSource, true)
		}
	}
	{
		applied, err := cfg.E2B.applyFile(file.E2B)
		recordConfigInput(cfg, "e2b", inputSource, applied.InputAccepted)
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
			recordConfigInput(cfg, "cubesandbox", inputSource, true)
			cfg.credentialProvenance.cubeSandboxAPIURL = credentialSource
		}
		if file.CubeSandbox.Domain != "" {
			cfg.CubeSandbox.Domain = file.CubeSandbox.Domain
			recordConfigInput(cfg, "cubesandbox", inputSource, true)
			cfg.credentialProvenance.cubeSandboxDomain = credentialSource
		}
		if file.CubeSandbox.Template != "" {
			cfg.CubeSandbox.Template = file.CubeSandbox.Template
			recordConfigInput(cfg, "cubesandbox", inputSource, true)
		}
		if file.CubeSandbox.Workdir != "" {
			cfg.CubeSandbox.Workdir = file.CubeSandbox.Workdir
			recordConfigInput(cfg, "cubesandbox", inputSource, true)
		}
		if file.CubeSandbox.User != "" {
			cfg.CubeSandbox.User = file.CubeSandbox.User
			recordConfigInput(cfg, "cubesandbox", inputSource, true)
		}
		if file.CubeSandbox.ProxyNodeIP != "" {
			cfg.CubeSandbox.ProxyNodeIP = file.CubeSandbox.ProxyNodeIP
			recordConfigInput(cfg, "cubesandbox", inputSource, true)
			cfg.credentialProvenance.cubeSandboxProxyNode = credentialSource
		}
		if file.CubeSandbox.ProxyPortHTTP > 0 {
			cfg.CubeSandbox.ProxyPortHTTP = file.CubeSandbox.ProxyPortHTTP
			recordConfigInput(cfg, "cubesandbox", inputSource, true)
			cfg.credentialProvenance.cubeSandboxProxyPort = credentialSource
		}
		if file.CubeSandbox.ProxyScheme != "" {
			cfg.CubeSandbox.ProxyScheme = file.CubeSandbox.ProxyScheme
			recordConfigInput(cfg, "cubesandbox", inputSource, true)
			cfg.credentialProvenance.cubeSandboxProxyProto = credentialSource
		}
	}
	{
		applied, err := cfg.ExeDev.applyFile(file.ExeDev)
		recordConfigInput(cfg, "exe-dev", inputSource, applied.InputAccepted)
		if applied.ControlHost {
			cfg.credentialProvenance.exeDevControlHost = credentialSource
		}
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.Railway.applyFile(file.Railway)
		recordConfigInput(cfg, "railway", inputSource, applied.InputAccepted)
		if applied.APIURL {
			cfg.credentialProvenance.railwayAPIURL = credentialSource
		}
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.FastAPICloud.applyFile(file.FastAPICloud)
		recordConfigInput(cfg, "fastapi-cloud", inputSource, applied.InputAccepted)
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
			recordConfigInput(cfg, "unikraft-cloud", inputSource, true)
			cfg.credentialProvenance.unikraftCloudAPIKey = credentialSource
		}
		if file.UnikraftCloud.APIURL != "" {
			cfg.UnikraftCloud.APIURL = file.UnikraftCloud.APIURL
			recordConfigInput(cfg, "unikraft-cloud", inputSource, true)
			cfg.credentialProvenance.unikraftCloudAPIURL = credentialSource
		}
		if file.UnikraftCloud.Metro != "" {
			cfg.UnikraftCloud.Metro = file.UnikraftCloud.Metro
			recordConfigInput(cfg, "unikraft-cloud", inputSource, true)
		}
		if file.UnikraftCloud.Image != "" {
			cfg.UnikraftCloud.Image = file.UnikraftCloud.Image
			recordConfigInput(cfg, "unikraft-cloud", inputSource, true)
		}
		if file.UnikraftCloud.MemoryMB > 0 {
			cfg.UnikraftCloud.MemoryMB = file.UnikraftCloud.MemoryMB
			recordConfigInput(cfg, "unikraft-cloud", inputSource, true)
		}
	}
	{
		applied, err := cfg.Runpod.applyFile(file.Runpod)
		recordConfigInput(cfg, "runpod", inputSource, applied.InputAccepted)
		if applied.APIURL {
			cfg.credentialProvenance.runpodAPIURL = credentialSource
		}
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.Vast.applyFile(file.Vast)
		recordConfigInput(cfg, "vast", inputSource, applied.InputAccepted)
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
	if err := applyNvidiaBrevFileConfig(cfg, file.NvidiaBrev, trusted, inputSource); err != nil {
		return err
	}
	if file.Hostinger != nil {
		if trusted && file.Hostinger.APIToken != "" {
			cfg.Hostinger.APIToken = file.Hostinger.APIToken
			recordConfigInput(cfg, "hostinger", inputSource, true)
		}
		if trusted && file.Hostinger.APIURL != "" {
			cfg.Hostinger.APIURL = file.Hostinger.APIURL
			recordConfigInput(cfg, "hostinger", inputSource, true)
		}
		if trusted && file.Hostinger.ItemID != "" {
			cfg.Hostinger.ItemID = file.Hostinger.ItemID
			recordConfigInput(cfg, "hostinger", inputSource, true)
		}
		if trusted && file.Hostinger.PaymentMethodID != "" {
			cfg.Hostinger.PaymentMethodID = file.Hostinger.PaymentMethodID
			recordConfigInput(cfg, "hostinger", inputSource, true)
		}
		if trusted && file.Hostinger.TemplateID != "" {
			cfg.Hostinger.TemplateID = file.Hostinger.TemplateID
			recordConfigInput(cfg, "hostinger", inputSource, true)
		}
		if trusted && file.Hostinger.DataCenterID != "" {
			cfg.Hostinger.DataCenterID = file.Hostinger.DataCenterID
			recordConfigInput(cfg, "hostinger", inputSource, true)
		}
		if file.Hostinger.HostnamePrefix != "" {
			cfg.Hostinger.HostnamePrefix = file.Hostinger.HostnamePrefix
			recordConfigInput(cfg, "hostinger", inputSource, true)
		}
		if file.Hostinger.User != "" {
			cfg.Hostinger.User = file.Hostinger.User
			recordConfigInput(cfg, "hostinger", inputSource, true)
			MarkHostingerUserExplicit(cfg)
		}
		if file.Hostinger.WorkRoot != "" {
			cfg.Hostinger.WorkRoot = file.Hostinger.WorkRoot
			recordConfigInput(cfg, "hostinger", inputSource, true)
			MarkHostingerWorkRootExplicit(cfg)
		}
		if file.Hostinger.AllowPurchase != nil && (trusted || !*file.Hostinger.AllowPurchase) {
			cfg.Hostinger.AllowPurchase = *file.Hostinger.AllowPurchase
			recordConfigInput(cfg, "hostinger", inputSource, true)
		}
		if file.Hostinger.ReleaseAction != "" {
			cfg.Hostinger.ReleaseAction = file.Hostinger.ReleaseAction
			recordConfigInput(cfg, "hostinger", inputSource, true)
		}
	}
	{
		applied, err := cfg.Wandb.applyFile(file.Wandb)
		recordConfigInput(cfg, "wandb", inputSource, applied.InputAccepted)
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.Orgo.applyFile(file.Orgo, trusted)
		recordConfigInput(cfg, "orgo", inputSource, applied.InputAccepted)
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
			recordConfigInput(cfg, "islo", inputSource, true)
			cfg.credentialProvenance.isloBaseURL = credentialSource
		}
		if file.Islo.Image != "" {
			cfg.Islo.Image = file.Islo.Image
			recordConfigInput(cfg, "islo", inputSource, true)
			cfg.isloImageExplicit = true
		}
		if file.Islo.Workdir != "" {
			cfg.Islo.Workdir = file.Islo.Workdir
			recordConfigInput(cfg, "islo", inputSource, true)
		}
		if file.Islo.GatewayProfile != "" {
			cfg.Islo.GatewayProfile = file.Islo.GatewayProfile
			recordConfigInput(cfg, "islo", inputSource, true)
		}
		if file.Islo.SnapshotName != "" {
			cfg.Islo.SnapshotName = file.Islo.SnapshotName
			recordConfigInput(cfg, "islo", inputSource, true)
		}
		if file.Islo.VCPUs > 0 {
			cfg.Islo.VCPUs = file.Islo.VCPUs
			recordConfigInput(cfg, "islo", inputSource, true)
			cfg.isloVCPUsExplicit = true
		}
		if file.Islo.MemoryMB > 0 {
			cfg.Islo.MemoryMB = file.Islo.MemoryMB
			recordConfigInput(cfg, "islo", inputSource, true)
			cfg.isloMemoryMBExplicit = true
		}
		if file.Islo.DiskGB > 0 {
			cfg.Islo.DiskGB = file.Islo.DiskGB
			recordConfigInput(cfg, "islo", inputSource, true)
			cfg.isloDiskGBExplicit = true
		}
	}
	if file.Freestyle != nil {
		if file.Freestyle.APIURL != "" {
			cfg.Freestyle.APIURL = file.Freestyle.APIURL
			// The canonical loader restores repository URL input after this layer.
			recordConfigInput(cfg, "freestyle", inputSource, trusted)
		}
		if file.Freestyle.Workdir != "" {
			cfg.Freestyle.Workdir = file.Freestyle.Workdir
			recordConfigInput(cfg, "freestyle", inputSource, true)
		}
		if file.Freestyle.VCPUs > 0 {
			cfg.Freestyle.VCPUs = file.Freestyle.VCPUs
			recordConfigInput(cfg, "freestyle", inputSource, true)
		}
		if file.Freestyle.MemoryGB > 0 {
			cfg.Freestyle.MemoryGB = file.Freestyle.MemoryGB
			recordConfigInput(cfg, "freestyle", inputSource, true)
		}
	}
	if file.Tenki != nil {
		if file.Tenki.CLIPath != "" {
			cfg.Tenki.CLIPath = file.Tenki.CLIPath
			recordConfigInput(cfg, "tenki", inputSource, true)
		}
		if file.Tenki.Endpoint != "" {
			cfg.Tenki.Endpoint = file.Tenki.Endpoint
			recordConfigInput(cfg, "tenki", inputSource, true)
			cfg.credentialProvenance.tenkiEndpoint = credentialSource
		}
		if file.Tenki.Gateway != "" {
			cfg.Tenki.Gateway = file.Tenki.Gateway
			recordConfigInput(cfg, "tenki", inputSource, true)
			cfg.credentialProvenance.tenkiGateway = credentialSource
		}
		if file.Tenki.Workspace != "" {
			cfg.Tenki.Workspace = file.Tenki.Workspace
			recordConfigInput(cfg, "tenki", inputSource, true)
		}
		if file.Tenki.Project != "" {
			cfg.Tenki.Project = file.Tenki.Project
			recordConfigInput(cfg, "tenki", inputSource, true)
		}
		if file.Tenki.Image != "" {
			cfg.Tenki.Image = file.Tenki.Image
			recordConfigInput(cfg, "tenki", inputSource, true)
		}
		if file.Tenki.Snapshot != "" {
			cfg.Tenki.Snapshot = file.Tenki.Snapshot
			recordConfigInput(cfg, "tenki", inputSource, true)
		}
		if file.Tenki.WorkRoot != "" {
			cfg.Tenki.WorkRoot = file.Tenki.WorkRoot
			recordConfigInput(cfg, "tenki", inputSource, true)
		}
		if file.Tenki.CPUs > 0 {
			cfg.Tenki.CPUs = file.Tenki.CPUs
			recordConfigInput(cfg, "tenki", inputSource, true)
		}
		if file.Tenki.MemoryMB > 0 {
			cfg.Tenki.MemoryMB = file.Tenki.MemoryMB
			recordConfigInput(cfg, "tenki", inputSource, true)
		}
		if file.Tenki.DiskGB > 0 {
			cfg.Tenki.DiskGB = file.Tenki.DiskGB
			recordConfigInput(cfg, "tenki", inputSource, true)
		}
	}
	{
		applied, err := cfg.Tensorlake.applyFile(file.Tensorlake)
		recordConfigInput(cfg, "tensorlake", inputSource, applied.InputAccepted)
		if applied.APIURL {
			cfg.credentialProvenance.tensorlakeAPIURL = credentialSource
		}
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.Cua.applyFile(file.Cua, trusted)
		recordConfigInput(cfg, "cua", inputSource, applied.InputAccepted)
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.OpenComputer.applyFile(file.OpenComputer)
		recordConfigInput(cfg, "opencomputer", inputSource, applied.InputAccepted)
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.CodeSandbox.applyFile(file.CodeSandbox, trusted)
		recordConfigInput(cfg, "codesandbox", inputSource, applied.InputAccepted)
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.OpenSandbox.applyFile(file.OpenSandbox)
		recordConfigInput(cfg, "opensandbox", inputSource, applied.InputAccepted)
		if err != nil {
			return err
		}
	}
	if file.Nomad != nil {
		if trusted && file.Nomad.Address != "" {
			cfg.Nomad.Address = file.Nomad.Address
			recordConfigInput(cfg, "nomad", inputSource, true)
			cfg.credentialProvenance.nomadAddress = credentialSource
		}
		if trusted && file.Nomad.TokenEnv != "" {
			cfg.Nomad.TokenEnv = file.Nomad.TokenEnv
			recordConfigInput(cfg, "nomad", inputSource, true)
			cfg.credentialProvenance.nomadTokenEnv = credentialSource
		}
		if trusted && file.Nomad.CACert != "" {
			cfg.Nomad.CACert = expandUserPath(file.Nomad.CACert)
			recordConfigInput(cfg, "nomad", inputSource, true)
		}
		if trusted && file.Nomad.CAPath != "" {
			cfg.Nomad.CAPath = expandUserPath(file.Nomad.CAPath)
			recordConfigInput(cfg, "nomad", inputSource, true)
		}
		if trusted && file.Nomad.ClientCert != "" {
			cfg.Nomad.ClientCert = expandUserPath(file.Nomad.ClientCert)
			recordConfigInput(cfg, "nomad", inputSource, true)
		}
		if trusted && file.Nomad.ClientKey != "" {
			cfg.Nomad.ClientKey = expandUserPath(file.Nomad.ClientKey)
			recordConfigInput(cfg, "nomad", inputSource, true)
		}
		if trusted && file.Nomad.TLSServerName != "" {
			cfg.Nomad.TLSServerName = file.Nomad.TLSServerName
			recordConfigInput(cfg, "nomad", inputSource, true)
		}
		if trusted && file.Nomad.SkipVerify != nil {
			cfg.Nomad.SkipVerify = *file.Nomad.SkipVerify
			recordConfigInput(cfg, "nomad", inputSource, true)
		}
		if trusted {
			if file.Nomad.Region != "" {
				cfg.Nomad.Region = file.Nomad.Region
				recordConfigInput(cfg, "nomad", inputSource, true)
			}
			if file.Nomad.Namespace != "" {
				cfg.Nomad.Namespace = file.Nomad.Namespace
				recordConfigInput(cfg, "nomad", inputSource, true)
			}
			if file.Nomad.Task != nil {
				cfg.Nomad.Task = *file.Nomad.Task
				recordConfigInput(cfg, "nomad", inputSource, true)
			}
			if file.Nomad.Driver != nil {
				cfg.Nomad.Driver = *file.Nomad.Driver
				recordConfigInput(cfg, "nomad", inputSource, true)
			}
			if file.Nomad.Image != nil {
				cfg.Nomad.Image = *file.Nomad.Image
				recordConfigInput(cfg, "nomad", inputSource, true)
			}
			if file.Nomad.Workdir != nil {
				cfg.Nomad.Workdir = *file.Nomad.Workdir
				recordConfigInput(cfg, "nomad", inputSource, true)
			}
			if file.Nomad.JobSpecTemplate != "" {
				cfg.Nomad.JobSpecTemplate = expandUserPath(file.Nomad.JobSpecTemplate)
				recordConfigInput(cfg, "nomad", inputSource, true)
			}
			if file.Nomad.NodePool != "" {
				cfg.Nomad.NodePool = file.Nomad.NodePool
				recordConfigInput(cfg, "nomad", inputSource, true)
			}
			if len(file.Nomad.Datacenters) > 0 {
				cfg.Nomad.Datacenters = normalizeList(file.Nomad.Datacenters)
				recordConfigInput(cfg, "nomad", inputSource, true)
			}
			if file.Nomad.CPU != nil {
				if *file.Nomad.CPU < 0 {
					return exit(2, "nomad cpu must be non-negative")
				}
				cfg.Nomad.CPU = *file.Nomad.CPU
				recordConfigInput(cfg, "nomad", inputSource, true)
			}
			if file.Nomad.MemoryMB != nil {
				if *file.Nomad.MemoryMB < 0 {
					return exit(2, "nomad memoryMB must be non-negative")
				}
				cfg.Nomad.MemoryMB = *file.Nomad.MemoryMB
				recordConfigInput(cfg, "nomad", inputSource, true)
			}
			if file.Nomad.DiskMB != nil {
				if *file.Nomad.DiskMB < 0 {
					return exit(2, "nomad diskMB must be non-negative")
				}
				cfg.Nomad.DiskMB = *file.Nomad.DiskMB
				recordConfigInput(cfg, "nomad", inputSource, true)
			}
			if file.Nomad.AllocReadyTimeout != "" {
				recordConfigInput(cfg, "nomad", inputSource, applyLeaseDuration(&cfg.Nomad.AllocReadyTimeout, file.Nomad.AllocReadyTimeout))
			}
			if file.Nomad.EvalTimeout != "" {
				recordConfigInput(cfg, "nomad", inputSource, applyLeaseDuration(&cfg.Nomad.EvalTimeout, file.Nomad.EvalTimeout))
			}
			if file.Nomad.ExecTimeoutSecs != nil {
				if *file.Nomad.ExecTimeoutSecs < 0 {
					return exit(2, "nomad execTimeoutSecs must be non-negative")
				}
				cfg.Nomad.ExecTimeoutSecs = *file.Nomad.ExecTimeoutSecs
				recordConfigInput(cfg, "nomad", inputSource, true)
			}
		}
	}
	{
		applied, err := cfg.Blaxel.applyFile(file.Blaxel, trusted)
		recordConfigInput(cfg, "blaxel", inputSource, applied.InputAccepted)
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.VercelSandbox.applyFile(file.VercelSandbox)
		recordConfigInput(cfg, "vercel-sandbox", inputSource, applied.InputAccepted)
		if err != nil {
			return err
		}
	}
	if file.Superserve != nil {
		if trusted && strings.TrimSpace(file.Superserve.BaseURL) != "" {
			cfg.Superserve.BaseURL = file.Superserve.BaseURL
			recordConfigInput(cfg, "superserve", inputSource, true)
		}
		recordConfigInput(cfg, "superserve", inputSource, applyOptional(&cfg.Superserve.Template, file.Superserve.Template))
		recordConfigInput(cfg, "superserve", inputSource, applyOptional(&cfg.Superserve.Snapshot, file.Superserve.Snapshot))
		recordConfigInput(cfg, "superserve", inputSource, applyOptional(&cfg.Superserve.Workdir, file.Superserve.Workdir))
		if file.Superserve.TimeoutSecs != nil {
			if *file.Superserve.TimeoutSecs < 0 {
				return exit(2, "superserve timeoutSecs must be non-negative")
			}
			cfg.Superserve.TimeoutSecs = *file.Superserve.TimeoutSecs
			recordConfigInput(cfg, "superserve", inputSource, true)
		}
		if file.Superserve.ExecTimeoutSecs != nil {
			if *file.Superserve.ExecTimeoutSecs < 0 {
				return exit(2, "superserve execTimeoutSecs must be non-negative")
			}
			cfg.Superserve.ExecTimeoutSecs = *file.Superserve.ExecTimeoutSecs
			recordConfigInput(cfg, "superserve", inputSource, true)
		}
		if file.Superserve.NetworkAllowOut != nil {
			cfg.Superserve.NetworkAllowOut = normalizeList(file.Superserve.NetworkAllowOut)
			recordConfigInput(cfg, "superserve", inputSource, true)
		}
		if file.Superserve.NetworkDenyOut != nil {
			cfg.Superserve.NetworkDenyOut = normalizeList(file.Superserve.NetworkDenyOut)
			recordConfigInput(cfg, "superserve", inputSource, true)
		}
		recordConfigInput(cfg, "superserve", inputSource, applyOptional(&cfg.Superserve.ForgetMissing, file.Superserve.ForgetMissing))
	}
	if err := applyCrownestFileConfig(cfg, file.Crownest, trusted, inputSource); err != nil {
		return err
	}
	if file.DockerSandbox != nil {
		if file.DockerSandbox.CLIPath != "" {
			cfg.DockerSandbox.CLIPath = file.DockerSandbox.CLIPath
			recordConfigInput(cfg, "docker-sandbox", inputSource, true)
		}
		if file.DockerSandbox.Agent != "" {
			cfg.DockerSandbox.Agent = file.DockerSandbox.Agent
			recordConfigInput(cfg, "docker-sandbox", inputSource, true)
		}
		if file.DockerSandbox.Template != nil {
			applyOptional(&cfg.DockerSandbox.Template, file.DockerSandbox.Template)
			recordConfigInput(cfg, "docker-sandbox", inputSource, true)
		}
		if file.DockerSandbox.CPUs != nil {
			if *file.DockerSandbox.CPUs < 0 {
				return exit(2, "docker-sandbox cpus must be non-negative")
			}
			cfg.DockerSandbox.CPUs = *file.DockerSandbox.CPUs
			recordConfigInput(cfg, "docker-sandbox", inputSource, true)
		}
		if file.DockerSandbox.Memory != nil {
			applyOptional(&cfg.DockerSandbox.Memory, file.DockerSandbox.Memory)
			recordConfigInput(cfg, "docker-sandbox", inputSource, true)
		}
		if file.DockerSandbox.Clone != nil {
			applyOptional(&cfg.DockerSandbox.Clone, file.DockerSandbox.Clone)
			recordConfigInput(cfg, "docker-sandbox", inputSource, true)
		}
		if file.DockerSandbox.Workdir != nil {
			applyOptional(&cfg.DockerSandbox.Workdir, file.DockerSandbox.Workdir)
			recordConfigInput(cfg, "docker-sandbox", inputSource, true)
		}
		if file.DockerSandbox.ExtraWorkspaces != nil {
			cfg.DockerSandbox.ExtraWorkspaces = append([]string(nil), (*file.DockerSandbox.ExtraWorkspaces)...)
			recordConfigInput(cfg, "docker-sandbox", inputSource, true)
		}
		if file.DockerSandbox.MCP != nil {
			cfg.DockerSandbox.MCP = append([]string(nil), (*file.DockerSandbox.MCP)...)
			recordConfigInput(cfg, "docker-sandbox", inputSource, true)
		}
		if file.DockerSandbox.Kit != nil {
			cfg.DockerSandbox.Kit = append([]string(nil), (*file.DockerSandbox.Kit)...)
			recordConfigInput(cfg, "docker-sandbox", inputSource, true)
		}
	}
	{
		applied, err := cfg.AnthropicSRT.applyFile(file.AnthropicSRT)
		recordConfigInput(cfg, "anthropic-sandbox-runtime", inputSource, applied.InputAccepted)
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.CloudRunSandbox.applyFile(file.CloudRunSandbox)
		recordConfigInput(cfg, "cloud-run-sandbox", inputSource, applied.InputAccepted)
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.Modal.applyFile(file.Modal, trusted)
		recordConfigInput(cfg, "modal", inputSource, applied.InputAccepted)
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.UpstashBox.applyFile(file.UpstashBox)
		recordConfigInput(cfg, "upstash-box", inputSource, applied.InputAccepted)
		if applied.BaseURL {
			cfg.credentialProvenance.upstashBoxBaseURL = credentialSource
		}
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.Smolvm.applyFile(file.Smolvm)
		recordConfigInput(cfg, "smolvm", inputSource, applied.InputAccepted)
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
			recordConfigInput(cfg, "ascii-box", inputSource, true)
			cfg.credentialProvenance.asciiBoxBaseURL = credentialSource
		}
		if file.AsciiBox.CLIPath != "" {
			cfg.AsciiBox.CLIPath = file.AsciiBox.CLIPath
			recordConfigInput(cfg, "ascii-box", inputSource, true)
		}
		if file.AsciiBox.Workdir != "" {
			cfg.AsciiBox.Workdir = file.AsciiBox.Workdir
			recordConfigInput(cfg, "ascii-box", inputSource, true)
		}
	}
	{
		applied, err := cfg.Cloudflare.applyFile(file.Cloudflare)
		recordConfigInput(cfg, "cloudflare", inputSource, applied.InputAccepted)
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
	{
		applied, err := cfg.CloudflareSandbox.applyFile(file.CloudflareSandbox, trusted)
		recordConfigInput(cfg, "cloudflare-sandbox", inputSource, applied.InputAccepted)
		if err != nil {
			return err
		}
	}
	recordConfigInput(cfg, "cloudflare-dynamic-workers", inputSource, applyCloudflareDynamicWorkersFileConfig(cfg, file.CloudflareDynamicWorkers, trusted))
	{
		applied, err := cfg.Semaphore.applyFile(file.Semaphore)
		recordConfigInput(cfg, "semaphore", inputSource, applied.InputAccepted)
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
			recordConfigInput(cfg, "sprites", inputSource, true)
			cfg.credentialProvenance.spritesAPIURL = credentialSource
		}
		if file.Sprites.WorkRoot != "" {
			cfg.Sprites.WorkRoot = file.Sprites.WorkRoot
			recordConfigInput(cfg, "sprites", inputSource, true)
		}
	}
	{
		applied := applyLocalContainerFile(cfg, file.LocalContainer)
		recordConfigInput(cfg, "local-container", inputSource, applied.InputAccepted)
	}
	{
		applied := cfg.AppleContainer.applyFile(file.AppleContainer)
		recordConfigInput(cfg, "apple-container", inputSource, applied.InputAccepted)
		recordConfigInput(cfg, "apple-machine", inputSource, applied.InputAccepted)
		if applied.Image {
			MarkAppleContainerImageExplicit(cfg)
		}
	}
	if file.AppleVM == nil {
		// Deprecated pre-rename key; appleVM wins when both are present.
		file.AppleVM = file.AppleVZLegacy
	}
	{
		applied := applyAppleVMFile(cfg, file.AppleVM)
		recordConfigInput(cfg, "apple-vm", inputSource, applied.InputAccepted)
	}
	if file.MXC != nil {
		if file.MXC.CLIPath != "" {
			cfg.MXC.CLIPath = file.MXC.CLIPath
			recordConfigInput(cfg, "mxc", inputSource, true)
		}
		if file.MXC.Version != "" {
			cfg.MXC.Version = file.MXC.Version
			recordConfigInput(cfg, "mxc", inputSource, true)
		}
		if file.MXC.Containment != "" {
			cfg.MXC.Containment = file.MXC.Containment
			recordConfigInput(cfg, "mxc", inputSource, true)
		}
		if file.MXC.Network != "" {
			cfg.MXC.Network = file.MXC.Network
			recordConfigInput(cfg, "mxc", inputSource, true)
		}
		if file.MXC.ReadOnlyPaths != nil {
			cfg.MXC.ReadOnlyPaths = append([]string(nil), file.MXC.ReadOnlyPaths...)
			recordConfigInput(cfg, "mxc", inputSource, true)
		}
		if file.MXC.ReadWritePaths != nil {
			cfg.MXC.ReadWritePaths = append([]string(nil), file.MXC.ReadWritePaths...)
			recordConfigInput(cfg, "mxc", inputSource, true)
		}
		if file.MXC.AllowedHosts != nil {
			cfg.MXC.AllowedHosts = append([]string(nil), file.MXC.AllowedHosts...)
			recordConfigInput(cfg, "mxc", inputSource, true)
		}
		if file.MXC.BlockedHosts != nil {
			cfg.MXC.BlockedHosts = append([]string(nil), file.MXC.BlockedHosts...)
			recordConfigInput(cfg, "mxc", inputSource, true)
		}
		if file.MXC.AllowDACLMutation != nil {
			cfg.MXC.AllowDACLMutation = *file.MXC.AllowDACLMutation
			recordConfigInput(cfg, "mxc", inputSource, true)
		}
		if file.MXC.AllowWindowsUI != nil {
			cfg.MXC.AllowWindowsUI = *file.MXC.AllowWindowsUI
			recordConfigInput(cfg, "mxc", inputSource, true)
		}
		if file.MXC.Experimental != nil {
			cfg.MXC.Experimental = *file.MXC.Experimental
			recordConfigInput(cfg, "mxc", inputSource, true)
		}
	}
	{
		applied, err := cfg.Multipass.applyFile(file.Multipass)
		recordConfigInput(cfg, "multipass", inputSource, applied.InputAccepted)
		if applied.Image {
			MarkMultipassImageExplicit(cfg)
		}
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.Machine0.applyFile(file.Machine0)
		recordConfigInput(cfg, "machine0", inputSource, applied.InputAccepted)
		if applied.Size {
			cfg.Machine0.SizeExplicit = true
		}
		if err != nil {
			return err
		}
	}
	if file.Tart != nil {
		if file.Tart.Image != "" {
			cfg.Tart.Image = file.Tart.Image
			cfg.tartImageExplicit = true
			recordConfigInput(cfg, "tart", inputSource, true)
		}
		if file.Tart.User != "" {
			cfg.Tart.User = file.Tart.User
			recordConfigInput(cfg, "tart", inputSource, true)
		}
		if file.Tart.Password != "" {
			cfg.Tart.Password = file.Tart.Password
			recordConfigInput(cfg, "tart", inputSource, true)
		}
		if file.Tart.WorkRoot != "" {
			cfg.Tart.WorkRoot = file.Tart.WorkRoot
			recordConfigInput(cfg, "tart", inputSource, true)
		}
		if file.Tart.CPUs != nil {
			cfg.Tart.CPUs = *file.Tart.CPUs
			cfg.tartCPUsExplicit = true
			recordConfigInput(cfg, "tart", inputSource, true)
		}
		if file.Tart.Memory != nil {
			cfg.Tart.Memory = *file.Tart.Memory
			cfg.tartMemoryExplicit = true
			recordConfigInput(cfg, "tart", inputSource, true)
		}
		if file.Tart.Disk != nil {
			cfg.Tart.Disk = *file.Tart.Disk
			cfg.tartDiskExplicit = true
			recordConfigInput(cfg, "tart", inputSource, true)
		}
	}
	{
		applied, err := cfg.Lume.applyFile(file.Lume, trusted)
		recordConfigInput(cfg, "lume", inputSource, applied.InputAccepted)
		if err != nil {
			return err
		}
	}
	if file.HyperV != nil {
		if file.HyperV.Image != "" {
			cfg.HyperV.Image = file.HyperV.Image
			recordConfigInput(cfg, "hyperv", inputSource, true)
		}
		if file.HyperV.User != "" {
			cfg.HyperV.User = file.HyperV.User
			recordConfigInput(cfg, "hyperv", inputSource, true)
		}
		if file.HyperV.WorkRoot != "" {
			cfg.HyperV.WorkRoot = file.HyperV.WorkRoot
			recordConfigInput(cfg, "hyperv", inputSource, true)
		}
		if file.HyperV.CPUs > 0 {
			cfg.HyperV.CPUs = file.HyperV.CPUs
			recordConfigInput(cfg, "hyperv", inputSource, true)
		}
		if file.HyperV.Memory > 0 {
			cfg.HyperV.Memory = file.HyperV.Memory
			recordConfigInput(cfg, "hyperv", inputSource, true)
		}
		if file.HyperV.Switch != "" {
			cfg.HyperV.Switch = file.HyperV.Switch
			recordConfigInput(cfg, "hyperv", inputSource, true)
		}
		if file.HyperV.GuestPassword != "" {
			cfg.HyperV.GuestPassword = file.HyperV.GuestPassword
			recordConfigInput(cfg, "hyperv", inputSource, true)
		}
		if file.HyperV.InitPassword != nil {
			cfg.HyperV.InitPassword = *file.HyperV.InitPassword
			recordConfigInput(cfg, "hyperv", inputSource, true)
		}
	}
	if file.WindowsSandbox != nil {
		if file.WindowsSandbox.Workdir != "" {
			cfg.WindowsSandbox.Workdir = file.WindowsSandbox.Workdir
			recordConfigInput(cfg, "windows-sandbox", inputSource, true)
		}
		if trusted {
			if file.WindowsSandbox.TempRoot != "" {
				cfg.WindowsSandbox.TempRoot = expandUserPath(file.WindowsSandbox.TempRoot)
				recordConfigInput(cfg, "windows-sandbox", inputSource, true)
			}
			if file.WindowsSandbox.Networking != "" {
				cfg.WindowsSandbox.Networking = file.WindowsSandbox.Networking
				recordConfigInput(cfg, "windows-sandbox", inputSource, true)
			}
			if file.WindowsSandbox.VGPU != "" {
				cfg.WindowsSandbox.VGPU = file.WindowsSandbox.VGPU
				recordConfigInput(cfg, "windows-sandbox", inputSource, true)
			}
			if file.WindowsSandbox.Clipboard != "" {
				cfg.WindowsSandbox.Clipboard = file.WindowsSandbox.Clipboard
				recordConfigInput(cfg, "windows-sandbox", inputSource, true)
			}
			if file.WindowsSandbox.ProtectedClient != "" {
				cfg.WindowsSandbox.ProtectedClient = file.WindowsSandbox.ProtectedClient
				recordConfigInput(cfg, "windows-sandbox", inputSource, true)
			}
			if file.WindowsSandbox.AudioInput != "" {
				cfg.WindowsSandbox.AudioInput = file.WindowsSandbox.AudioInput
				recordConfigInput(cfg, "windows-sandbox", inputSource, true)
			}
			if file.WindowsSandbox.VideoInput != "" {
				cfg.WindowsSandbox.VideoInput = file.WindowsSandbox.VideoInput
				recordConfigInput(cfg, "windows-sandbox", inputSource, true)
			}
			if file.WindowsSandbox.PrinterRedirection != "" {
				cfg.WindowsSandbox.PrinterRedirection = file.WindowsSandbox.PrinterRedirection
				recordConfigInput(cfg, "windows-sandbox", inputSource, true)
			}
			if file.WindowsSandbox.MemoryMB > 0 {
				cfg.WindowsSandbox.MemoryMB = file.WindowsSandbox.MemoryMB
				recordConfigInput(cfg, "windows-sandbox", inputSource, true)
			}
		}
	}
	if file.Tailscale != nil {
		recordConfigInput(cfg, configInputGeneric, inputSource, applyOptional(&cfg.Tailscale.Enabled, file.Tailscale.Enabled))
		if file.Tailscale.Network != "" {
			cfg.Network = NetworkMode(strings.ToLower(strings.TrimSpace(file.Tailscale.Network)))
			recordConfigInput(cfg, configInputGeneric, inputSource, true)
		}
		if len(file.Tailscale.Tags) > 0 {
			cfg.Tailscale.Tags = normalizeTailscaleTags(file.Tailscale.Tags)
			recordConfigInput(cfg, configInputGeneric, inputSource, true)
		}
		if file.Tailscale.HostnameTemplate != "" {
			cfg.Tailscale.HostnameTemplate = file.Tailscale.HostnameTemplate
			recordConfigInput(cfg, configInputGeneric, inputSource, true)
		}
		if file.Tailscale.AuthKeyEnv != "" {
			cfg.Tailscale.AuthKeyEnv = file.Tailscale.AuthKeyEnv
			recordConfigInput(cfg, configInputGeneric, inputSource, true)
		}
		if file.Tailscale.ExitNode != "" {
			cfg.Tailscale.ExitNode = strings.TrimSpace(file.Tailscale.ExitNode)
			recordConfigInput(cfg, configInputGeneric, inputSource, true)
		}
		recordConfigInput(cfg, configInputGeneric, inputSource, applyOptional(&cfg.Tailscale.ExitNodeAllowLANAccess, file.Tailscale.ExitNodeAllowLANAccess))
	}
	if file.Static != nil {
		if file.Static.ID != "" {
			cfg.Static.ID = file.Static.ID
			recordConfigInput(cfg, "ssh", inputSource, true)
		}
		if file.Static.Name != "" {
			cfg.Static.Name = file.Static.Name
			recordConfigInput(cfg, "ssh", inputSource, true)
		}
		if file.Static.Host != "" {
			cfg.Static.Host = file.Static.Host
			recordConfigInput(cfg, "ssh", inputSource, true)
			cfg.credentialProvenance.staticHost = credentialSource
		}
		if file.Static.User != "" {
			cfg.Static.User = file.Static.User
			recordConfigInput(cfg, "ssh", inputSource, true)
		}
		if file.Static.Port != "" {
			cfg.Static.Port = file.Static.Port
			recordConfigInput(cfg, "ssh", inputSource, true)
		}
		if file.Static.WorkRoot != "" {
			cfg.Static.WorkRoot = file.Static.WorkRoot
			recordConfigInput(cfg, "ssh", inputSource, true)
		}
	}
	if file.Results != nil {
		if file.Results.JUnit != nil {
			cfg.Results.JUnit = appendUniqueStrings(nil, file.Results.JUnit...)
			recordConfigInput(cfg, configInputGeneric, inputSource, true)
		}
		recordConfigInput(cfg, configInputGeneric, inputSource, applyOptional(&cfg.Results.Auto, file.Results.Auto))
		recordConfigInput(cfg, configInputGeneric, inputSource, applyOptional(&cfg.Results.FailOnFailures, file.Results.FailOnFailures))
	}
	if file.Shard != nil && file.Shard.MaxCount != nil {
		cfg.Shard.MaxCount = *file.Shard.MaxCount
		recordConfigInput(cfg, configInputGeneric, inputSource, true)
	}
	if file.Cache != nil {
		recordConfigInput(cfg, configInputGeneric, inputSource, applyOptional(&cfg.Cache.Pnpm, file.Cache.Pnpm))
		recordConfigInput(cfg, configInputGeneric, inputSource, applyOptional(&cfg.Cache.Npm, file.Cache.Npm))
		recordConfigInput(cfg, configInputGeneric, inputSource, applyOptional(&cfg.Cache.Docker, file.Cache.Docker))
		recordConfigInput(cfg, configInputGeneric, inputSource, applyOptional(&cfg.Cache.Git, file.Cache.Git))
		if file.Cache.MaxGB > 0 {
			cfg.Cache.MaxGB = file.Cache.MaxGB
			recordConfigInput(cfg, configInputGeneric, inputSource, true)
		}
		recordConfigInput(cfg, configInputGeneric, inputSource, applyOptional(&cfg.Cache.PurgeOnRelease, file.Cache.PurgeOnRelease))
		if file.Cache.Volumes != nil {
			volumes, err := normalizeFileCacheVolumes(*file.Cache.Volumes)
			if err != nil {
				return err
			}
			cfg.Cache.Volumes = volumes
			recordConfigInput(cfg, configInputGeneric, inputSource, true)
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
				recordConfigInput(cfg, configInputGeneric, inputSource, true)
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
				recordConfigInput(cfg, configInputGeneric, inputSource, true)
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
				recordConfigInput(cfg, configInputGeneric, inputSource, true)
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
			recordConfigInput(cfg, configInputGeneric, inputSource, true)
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

func applyLeaseDuration(target *time.Duration, value string) bool {
	// File and environment overlays intentionally ignore invalid durations.
	return value != "" && ApplyLeaseDuration(target, value) == nil
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
	cfg.Profile = configInputEnvString(cfg, configInputGeneric, cfg.Profile, "CRABBOX_PROFILE")
	if provider := os.Getenv("CRABBOX_PROVIDER"); provider != "" {
		setProviderSelection(cfg, provider, providerSelectionEnvironment)
		cfg.brokerProvider = ""
	}
	if t := os.Getenv("CRABBOX_TARGET"); t != "" {
		cfg.TargetOS = t
		cfg.targetExplicit = true
		cfg.credentialProvenance.externalDesktopTarget = credentialSourceEnvironment
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
	} else if t := os.Getenv("CRABBOX_TARGET_OS"); t != "" {
		cfg.TargetOS = t
		cfg.targetExplicit = true
		cfg.credentialProvenance.externalDesktopTarget = credentialSourceEnvironment
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
	}
	if arch := os.Getenv("CRABBOX_ARCH"); arch != "" {
		cfg.Architecture = arch
		cfg.architectureExplicit = true
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
	}
	if osImage := os.Getenv("CRABBOX_OS"); osImage != "" {
		cfg.OSImage = osImage
		cfg.osImageExplicit = true
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
		if normalized, err := normalizeOSImage(osImage); err == nil {
			cfg.OSImage = normalized
			applyOSImageProviderDefaults(cfg, false)
		}
	}
	if windowsMode := os.Getenv("CRABBOX_WINDOWS_MODE"); windowsMode != "" {
		cfg.WindowsMode = windowsMode
		cfg.explicitWindowsMode = windowsMode
		cfg.credentialProvenance.externalDesktopMode = credentialSourceEnvironment
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
	}
	if value, ok := getenvBool("CRABBOX_DESKTOP"); ok {
		cfg.Desktop = value
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
	}
	cfg.DesktopEnv = configInputEnvString(cfg, configInputGeneric, cfg.DesktopEnv, "CRABBOX_DESKTOP_ENV")
	if value, ok := getenvBool("CRABBOX_BROWSER"); ok {
		cfg.Browser = value
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
	}
	if value, ok := getenvBool("CRABBOX_CODE"); ok {
		cfg.Code = value
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
	}
	if network := os.Getenv("CRABBOX_NETWORK"); network != "" {
		cfg.Network = NetworkMode(strings.ToLower(strings.TrimSpace(network)))
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
	}
	if value := os.Getenv("CRABBOX_DEFAULT_CLASS"); value != "" {
		cfg.Class = value
		MarkClassExplicit(cfg)
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
	}
	if os.Getenv("CRABBOX_SERVER_TYPE") != "" {
		cfg.ServerTypeExplicit = true
	}
	cfg.ServerType = configInputEnvString(cfg, configInputGeneric, cfg.ServerType, "CRABBOX_SERVER_TYPE")
	if value := os.Getenv("CRABBOX_COORDINATOR"); value != "" {
		cfg.Coordinator = value
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
		cfg.credentialProvenance.coordinator = credentialSourceEnvironment
	}
	cfg.BrokerMode = BrokerMode(configInputEnvString(cfg, configInputGeneric, string(cfg.BrokerMode), "CRABBOX_COORDINATOR_MODE"))
	if value, ok := getenvBool("CRABBOX_COORDINATOR_AUTO_WEBVNC"); ok {
		cfg.BrokerAutoWebVNC = value
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
	}
	if value := os.Getenv("CRABBOX_BROKER_LOGIN_REDIRECT_ORIGINS"); value != "" {
		cfg.BrokerLoginRedirectOrigins = splitCommaList(value)
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
	}
	if value := os.Getenv("CRABBOX_COORDINATOR_TOKEN"); value != "" {
		cfg.CoordToken = value
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
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
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
		cfg.credentialProvenance.coordTokenCommand = credentialSourceEnvironment
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_COORDINATOR_ADMIN_TOKEN", "CRABBOX_ADMIN_TOKEN"); ok {
		cfg.CoordAdminToken = value
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
		cfg.credentialProvenance.coordAdminToken = credentialSourceEnvironment
	}
	cfg.HostID = configInputEnvString(cfg, configInputGeneric, cfg.HostID, "CRABBOX_HOST_ID")
	if value, ok := firstNonEmptyEnv("CRABBOX_ACCESS_CLIENT_ID", "CF_ACCESS_CLIENT_ID"); ok {
		cfg.Access.ClientID = value
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
		cfg.credentialProvenance.accessClientID = credentialSourceEnvironment
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_ACCESS_CLIENT_SECRET", "CF_ACCESS_CLIENT_SECRET"); ok {
		cfg.Access.ClientSecret = value
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
		cfg.credentialProvenance.accessClientSecret = credentialSourceEnvironment
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_ACCESS_TOKEN", "CF_ACCESS_TOKEN"); ok {
		cfg.Access.Token = value
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
		cfg.credentialProvenance.accessToken = credentialSourceEnvironment
	}
	if location := os.Getenv("CRABBOX_HETZNER_LOCATION"); location != "" {
		cfg.Location = location
		recordConfigInput(cfg, "hetzner", configInputEnvironment, true)
		cfg.locationExplicit = true
	}
	if image := os.Getenv("CRABBOX_HETZNER_IMAGE"); image != "" {
		cfg.Image = image
		recordConfigInput(cfg, "hetzner", configInputEnvironment, true)
		cfg.imageExplicit = true
	}
	if region, accepted := firstNonEmptyEnv("CRABBOX_AWS_REGION", "AWS_REGION"); accepted {
		cfg.AWSRegion = region
		recordConfigInput(cfg, "aws", configInputEnvironment, true)
		recordConfigInput(cfg, "aws-lambda-microvm", configInputEnvironment, true)
	}
	cfg.AWSAMI = configInputEnvString(cfg, "aws", cfg.AWSAMI, "CRABBOX_AWS_AMI")
	cfg.AWSSGID = configInputEnvString(cfg, "aws", cfg.AWSSGID, "CRABBOX_AWS_SECURITY_GROUP_ID")
	cfg.AWSSubnetID = configInputEnvString(cfg, "aws", cfg.AWSSubnetID, "CRABBOX_AWS_SUBNET_ID")
	cfg.AWSProfile = configInputEnvString(cfg, "aws", cfg.AWSProfile, "CRABBOX_AWS_INSTANCE_PROFILE")
	cfg.AWSRootGB = configInputEnvInt32(cfg, "aws", cfg.AWSRootGB, "CRABBOX_AWS_ROOT_GB")
	cfg.AWSMacHostID = configInputEnvString(cfg, "aws", cfg.AWSMacHostID, "CRABBOX_AWS_MAC_HOST_ID")
	{
		applied, err := cfg.AWSLambdaMicroVM.applyEnv()
		recordConfigInput(cfg, "aws-lambda-microvm", configInputEnvironment, applied.InputAccepted)
		if err != nil {
			return err
		}
	}
	if cfg.HostID == "" && cfg.AWSMacHostID != "" {
		cfg.HostID = cfg.AWSMacHostID
	}
	if cfg.AWSMacHostID == "" && cfg.Provider == "aws" && cfg.TargetOS == targetMacOS {
		cfg.AWSMacHostID = cfg.HostID
	}
	if cidrs := os.Getenv("CRABBOX_AWS_SSH_CIDRS"); cidrs != "" {
		cfg.AWSSSHCIDRs = splitCommaList(cidrs)
		recordConfigInput(cfg, "aws", configInputEnvironment, true)
	}
	if value, accepted := firstNonEmptyEnv("CRABBOX_AZURE_SUBSCRIPTION_ID", "AZURE_SUBSCRIPTION_ID"); accepted {
		cfg.AzureSubscription = value
		recordConfigInput(cfg, "azure", configInputEnvironment, true)
		recordConfigInput(cfg, "azure-dynamic-sessions", configInputEnvironment, true)
	}
	if value, accepted := firstNonEmptyEnv("CRABBOX_AZURE_TENANT_ID", "AZURE_TENANT_ID"); accepted {
		cfg.AzureTenant = value
		recordConfigInput(cfg, "azure", configInputEnvironment, true)
		recordConfigInput(cfg, "azure-dynamic-sessions", configInputEnvironment, true)
	}
	cfg.AzureClientID = configInputEnvString(cfg, "azure", cfg.AzureClientID, "CRABBOX_AZURE_CLIENT_ID", "AZURE_CLIENT_ID")
	cfg.AzureBackend = configInputEnvString(cfg, "azure", cfg.AzureBackend, "CRABBOX_AZURE_BACKEND")
	cfg.AzureLocation = configInputEnvString(cfg, "azure", cfg.AzureLocation, "CRABBOX_AZURE_LOCATION")
	cfg.AzureResourceGroup = configInputEnvString(cfg, "azure", cfg.AzureResourceGroup, "CRABBOX_AZURE_RESOURCE_GROUP")
	if image := os.Getenv("CRABBOX_AZURE_IMAGE"); image != "" {
		cfg.AzureImage = image
		recordConfigInput(cfg, "azure", configInputEnvironment, true)
		cfg.azureImageExplicit = true
	}
	if value := os.Getenv("CRABBOX_AZURE_OS_DISK"); value != "" {
		cfg.AzureOSDisk = value
		recordConfigInput(cfg, "azure", configInputEnvironment, true)
		cfg.AzureOSDiskExplicit = true
	}
	cfg.AzureSnapshotSKU = configInputEnvString(cfg, "azure", cfg.AzureSnapshotSKU, "CRABBOX_AZURE_SNAPSHOT_SKU")
	cfg.AzureOSDiskSKU = configInputEnvString(cfg, "azure", cfg.AzureOSDiskSKU, "CRABBOX_AZURE_OS_DISK_SKU")
	cfg.AzureVNet = configInputEnvString(cfg, "azure", cfg.AzureVNet, "CRABBOX_AZURE_VNET")
	cfg.AzureSubnet = configInputEnvString(cfg, "azure", cfg.AzureSubnet, "CRABBOX_AZURE_SUBNET")
	cfg.AzureNSG = configInputEnvString(cfg, "azure", cfg.AzureNSG, "CRABBOX_AZURE_NSG")
	if cidrs := os.Getenv("CRABBOX_AZURE_SSH_CIDRS"); cidrs != "" {
		cfg.AzureSSHCIDRs = splitCommaList(cidrs)
		recordConfigInput(cfg, "azure", configInputEnvironment, true)
	}
	cfg.AzureNetwork = configInputEnvString(cfg, "azure", cfg.AzureNetwork, "CRABBOX_AZURE_NETWORK")
	{
		applied, err := cfg.AzureDynamicSessions.applyEnv()
		recordConfigInput(cfg, "azure-dynamic-sessions", configInputEnvironment, applied.InputAccepted)
		if applied.Endpoint {
			cfg.credentialProvenance.azSessionsEndpoint = credentialSourceEnvironment
		}
		if err != nil {
			return err
		}
	}
	if project := os.Getenv("CRABBOX_GCP_PROJECT"); project != "" {
		cfg.GCPProject = project
		recordConfigInput(cfg, "gcp", configInputEnvironment, true)
		cfg.gcpProjectExplicit = true
	} else if cfg.GCPProject == "" {
		if project := os.Getenv("GOOGLE_CLOUD_PROJECT"); project != "" {
			cfg.GCPProject = project
			recordConfigInput(cfg, "gcp", configInputEnvironment, true)
			cfg.gcpProjectExplicit = false
		} else if project := os.Getenv("GCP_PROJECT_ID"); project != "" {
			cfg.GCPProject = project
			recordConfigInput(cfg, "gcp", configInputEnvironment, true)
			cfg.gcpProjectExplicit = false
		}
	}
	if zone := os.Getenv("CRABBOX_GCP_ZONE"); zone != "" {
		cfg.GCPZone = zone
		recordConfigInput(cfg, "gcp", configInputEnvironment, true)
		cfg.gcpZoneExplicit = true
	}
	if image := os.Getenv("CRABBOX_GCP_IMAGE"); image != "" {
		cfg.GCPImage = image
		recordConfigInput(cfg, "gcp", configInputEnvironment, true)
		cfg.gcpImageExplicit = true
	}
	if network := os.Getenv("CRABBOX_GCP_NETWORK"); network != "" {
		cfg.GCPNetwork = network
		recordConfigInput(cfg, "gcp", configInputEnvironment, true)
		cfg.gcpNetworkExplicit = true
	}
	cfg.GCPSubnet = configInputEnvString(cfg, "gcp", cfg.GCPSubnet, "CRABBOX_GCP_SUBNET")
	if rootGB := os.Getenv("CRABBOX_GCP_ROOT_GB"); rootGB != "" {
		cfg.GCPRootGB = int64(configInputEnvInt(cfg, "gcp", int(cfg.GCPRootGB), "CRABBOX_GCP_ROOT_GB"))
		cfg.gcpRootGBExplicit = true
		recordConfigInputIntent(cfg, "gcp", configInputEnvironment, true)
	}
	cfg.GCPServiceAccount = configInputEnvString(cfg, "gcp", cfg.GCPServiceAccount, "CRABBOX_GCP_SERVICE_ACCOUNT")
	{
		applied, err := cfg.Incus.applyEnv()
		recordConfigInput(cfg, "incus", configInputEnvironment, applied.InputAccepted)
		cfg.Incus.Socket = expandUserPath(cfg.Incus.Socket)
		cfg.Incus.TLSServerCert = expandUserPath(cfg.Incus.TLSServerCert)
		if applied.DeleteOnRelease {
			MarkDeleteOnReleaseExplicit(cfg, "incus")
		}
		if err != nil {
			return err
		}
	}
	if tags := os.Getenv("CRABBOX_GCP_TAGS"); tags != "" {
		cfg.GCPTags = splitCommaList(tags)
		recordConfigInput(cfg, "gcp", configInputEnvironment, true)
		cfg.gcpTagsExplicit = true
	}
	if cidrs := os.Getenv("CRABBOX_GCP_SSH_CIDRS"); cidrs != "" {
		cfg.GCPSSHCIDRs = splitCommaList(cidrs)
		recordConfigInput(cfg, "gcp", configInputEnvironment, true)
	}
	{
		applied, err := cfg.DigitalOcean.applyEnv()
		recordConfigInput(cfg, "digitalocean", configInputEnvironment, applied.InputAccepted)
		if applied.Image {
			cfg.digitalOceanImageExplicit = true
		}
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.Vultr.applyEnv()
		recordConfigInput(cfg, "vultr", configInputEnvironment, applied.InputAccepted)
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.Linode.applyEnv()
		recordConfigInput(cfg, "linode", configInputEnvironment, applied.InputAccepted)
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
	cfg.GitHubCodespaces.APIURL = configInputEnvString(cfg, "github-codespaces", cfg.GitHubCodespaces.APIURL, "CRABBOX_GITHUB_CODESPACES_API_URL")
	cfg.GitHubCodespaces.GHPath = expandUserPath(configInputEnvString(cfg, "github-codespaces", cfg.GitHubCodespaces.GHPath, "CRABBOX_GITHUB_CODESPACES_GH_PATH"))
	cfg.GitHubCodespaces.Repo = configInputEnvString(cfg, "github-codespaces", cfg.GitHubCodespaces.Repo, "CRABBOX_GITHUB_CODESPACES_REPO")
	cfg.GitHubCodespaces.Ref = configInputEnvString(cfg, "github-codespaces", cfg.GitHubCodespaces.Ref, "CRABBOX_GITHUB_CODESPACES_REF")
	cfg.GitHubCodespaces.Machine = configInputEnvString(cfg, "github-codespaces", cfg.GitHubCodespaces.Machine, "CRABBOX_GITHUB_CODESPACES_MACHINE")
	cfg.GitHubCodespaces.DevcontainerPath = configInputEnvString(cfg, "github-codespaces", cfg.GitHubCodespaces.DevcontainerPath, "CRABBOX_GITHUB_CODESPACES_DEVCONTAINER_PATH")
	cfg.GitHubCodespaces.WorkingDirectory = configInputEnvString(cfg, "github-codespaces", cfg.GitHubCodespaces.WorkingDirectory, "CRABBOX_GITHUB_CODESPACES_WORKING_DIRECTORY")
	cfg.GitHubCodespaces.Geo = configInputEnvString(cfg, "github-codespaces", cfg.GitHubCodespaces.Geo, "CRABBOX_GITHUB_CODESPACES_GEO")
	if idleTimeout := os.Getenv("CRABBOX_GITHUB_CODESPACES_IDLE_TIMEOUT"); idleTimeout != "" {
		recordConfigInput(cfg, "github-codespaces", configInputEnvironment, applyLeaseDuration(&cfg.GitHubCodespaces.IdleTimeout, idleTimeout))
	}
	if retentionPeriod := os.Getenv("CRABBOX_GITHUB_CODESPACES_RETENTION_PERIOD"); retentionPeriod != "" {
		if applyNonNegativeLeaseDuration(&cfg.GitHubCodespaces.RetentionPeriod, retentionPeriod) {
			recordConfigInput(cfg, "github-codespaces", configInputEnvironment, true)
			MarkGitHubCodespacesRetentionExplicit(cfg)
		}
	}
	if value, ok := getenvBool("CRABBOX_GITHUB_CODESPACES_DELETE_ON_RELEASE"); ok {
		cfg.GitHubCodespaces.DeleteOnRelease = value
		recordConfigInput(cfg, "github-codespaces", configInputEnvironment, true)
		MarkDeleteOnReleaseExplicit(cfg, "github-codespaces")
	}
	cfg.GitHubCodespaces.WorkRoot = configInputEnvString(cfg, "github-codespaces", cfg.GitHubCodespaces.WorkRoot, "CRABBOX_GITHUB_CODESPACES_WORK_ROOT")
	{
		applied := cfg.Lambda.applyEnv()
		recordConfigInput(cfg, "lambda", configInputEnvironment, applied.InputAccepted)
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
	{
		applied, err := cfg.Nebius.applyEnv()
		recordConfigInput(cfg, "nebius", configInputEnvironment, applied.InputAccepted)
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.OVH.applyEnv()
		recordConfigInput(cfg, "ovh", configInputEnvironment, applied.InputAccepted)
		if applied.Image {
			cfg.ovhImageExplicit = true
		}
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.Scaleway.applyEnv()
		recordConfigInput(cfg, "scaleway", configInputEnvironment, applied.InputAccepted)
		MarkScalewayConfigApplied(cfg, applied)
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.TencentCloud.applyEnv()
		recordConfigInput(cfg, "tencentcloud", configInputEnvironment, applied.InputAccepted)
		MarkTencentCloudConfigApplied(cfg, applied)
		if err != nil {
			return err
		}
	}
	if value := os.Getenv("CRABBOX_PROXMOX_API_URL"); value != "" {
		cfg.Proxmox.APIURL = value
		recordConfigInput(cfg, "proxmox", configInputEnvironment, true)
		cfg.credentialProvenance.proxmoxAPIURL = credentialSourceEnvironment
	}
	if value := os.Getenv("CRABBOX_PROXMOX_TOKEN_ID"); value != "" {
		cfg.Proxmox.TokenID = value
		recordConfigInput(cfg, "proxmox", configInputEnvironment, true)
		cfg.credentialProvenance.proxmoxTokenID = credentialSourceEnvironment
	}
	if value := os.Getenv("CRABBOX_PROXMOX_TOKEN_SECRET"); value != "" {
		cfg.Proxmox.TokenSecret = value
		recordConfigInput(cfg, "proxmox", configInputEnvironment, true)
		cfg.credentialProvenance.proxmoxTokenSecret = credentialSourceEnvironment
	}
	cfg.Proxmox.Node = configInputEnvString(cfg, "proxmox", cfg.Proxmox.Node, "CRABBOX_PROXMOX_NODE")
	cfg.Proxmox.TemplateID = configInputEnvInt(cfg, "proxmox", cfg.Proxmox.TemplateID, "CRABBOX_PROXMOX_TEMPLATE_ID")
	cfg.Proxmox.Storage = configInputEnvString(cfg, "proxmox", cfg.Proxmox.Storage, "CRABBOX_PROXMOX_STORAGE")
	cfg.Proxmox.Pool = configInputEnvString(cfg, "proxmox", cfg.Proxmox.Pool, "CRABBOX_PROXMOX_POOL")
	cfg.Proxmox.Bridge = configInputEnvString(cfg, "proxmox", cfg.Proxmox.Bridge, "CRABBOX_PROXMOX_BRIDGE")
	cfg.Proxmox.User = configInputEnvString(cfg, "proxmox", cfg.Proxmox.User, "CRABBOX_PROXMOX_USER")
	cfg.Proxmox.WorkRoot = configInputEnvString(cfg, "proxmox", cfg.Proxmox.WorkRoot, "CRABBOX_PROXMOX_WORK_ROOT")
	if value, ok := getenvBool("CRABBOX_PROXMOX_FULL_CLONE"); ok {
		cfg.Proxmox.FullClone = value
		recordConfigInput(cfg, "proxmox", configInputEnvironment, true)
	}
	if value, ok := getenvBool("CRABBOX_PROXMOX_INSECURE_TLS"); ok {
		cfg.Proxmox.InsecureTLS = value
		recordConfigInput(cfg, "proxmox", configInputEnvironment, true)
		cfg.credentialProvenance.proxmoxInsecureTLS = credentialSourceEnvironment
	}
	{
		applied, err := cfg.Firecracker.applyEnv()
		recordConfigInput(cfg, "firecracker", configInputEnvironment, applied.InputAccepted)
		cfg.Firecracker.Binary = expandUserPath(cfg.Firecracker.Binary)
		cfg.Firecracker.Jailer = expandUserPath(cfg.Firecracker.Jailer)
		cfg.Firecracker.Kernel = expandUserPath(cfg.Firecracker.Kernel)
		cfg.Firecracker.RootFS = expandUserPath(cfg.Firecracker.RootFS)
		cfg.Firecracker.CNIConfDir = expandUserPath(cfg.Firecracker.CNIConfDir)
		cfg.Firecracker.CNIBinDir = expandUserPath(cfg.Firecracker.CNIBinDir)
		if applied.DeleteOnRelease {
			MarkDeleteOnReleaseExplicit(cfg, "firecracker")
			recordConfigInputIntent(cfg, "firecracker", configInputEnvironment, true)
		}
		if err != nil {
			return err
		}
	}
	cfg.XCPNg.APIURL = configInputEnvString(cfg, "xcp-ng", cfg.XCPNg.APIURL, "CRABBOX_XCP_NG_API_URL")
	cfg.XCPNg.Username = configInputEnvString(cfg, "xcp-ng", cfg.XCPNg.Username, "CRABBOX_XCP_NG_USERNAME")
	cfg.XCPNg.Password = configInputEnvString(cfg, "xcp-ng", cfg.XCPNg.Password, "CRABBOX_XCP_NG_PASSWORD")
	xcpNgTemplate, xcpNgTemplateUUID := os.Getenv("CRABBOX_XCP_NG_TEMPLATE"), os.Getenv("CRABBOX_XCP_NG_TEMPLATE_UUID")
	recordConfigInput(cfg, "xcp-ng", configInputEnvironment, applyXCPNgNameUUIDPair(&cfg.XCPNg.Template, &cfg.XCPNg.TemplateUUID, xcpNgTemplate, xcpNgTemplateUUID))
	xcpNgSR, xcpNgSRUUID := os.Getenv("CRABBOX_XCP_NG_SR"), os.Getenv("CRABBOX_XCP_NG_SR_UUID")
	recordConfigInput(cfg, "xcp-ng", configInputEnvironment, applyXCPNgNameUUIDPair(&cfg.XCPNg.SR, &cfg.XCPNg.SRUUID, xcpNgSR, xcpNgSRUUID))
	xcpNgNetwork, xcpNgNetworkUUID := os.Getenv("CRABBOX_XCP_NG_NETWORK"), os.Getenv("CRABBOX_XCP_NG_NETWORK_UUID")
	recordConfigInput(cfg, "xcp-ng", configInputEnvironment, applyXCPNgNameUUIDPair(&cfg.XCPNg.Network, &cfg.XCPNg.NetworkUUID, xcpNgNetwork, xcpNgNetworkUUID))
	cfg.XCPNg.Host = configInputEnvString(cfg, "xcp-ng", cfg.XCPNg.Host, "CRABBOX_XCP_NG_HOST")
	cfg.XCPNg.User = configInputEnvString(cfg, "xcp-ng", cfg.XCPNg.User, "CRABBOX_XCP_NG_USER")
	cfg.XCPNg.WorkRoot = configInputEnvString(cfg, "xcp-ng", cfg.XCPNg.WorkRoot, "CRABBOX_XCP_NG_WORK_ROOT")
	if value, ok := getenvBool("CRABBOX_XCP_NG_INSECURE_TLS"); ok {
		cfg.XCPNg.InsecureTLS = value
		recordConfigInput(cfg, "xcp-ng", configInputEnvironment, true)
	}
	cfg.Parallels.Source = configInputEnvString(cfg, "parallels", cfg.Parallels.Source, "CRABBOX_PARALLELS_SOURCE")
	cfg.Parallels.SourceID = configInputEnvString(cfg, "parallels", cfg.Parallels.SourceID, "CRABBOX_PARALLELS_SOURCE_ID")
	cfg.Parallels.SourceSnapshot = configInputEnvString(cfg, "parallels", cfg.Parallels.SourceSnapshot, "CRABBOX_PARALLELS_SOURCE_SNAPSHOT")
	cfg.Parallels.SourceSnapshotID = configInputEnvString(cfg, "parallels", cfg.Parallels.SourceSnapshotID, "CRABBOX_PARALLELS_SOURCE_SNAPSHOT_ID")
	cfg.Parallels.Template = configInputEnvString(cfg, "parallels", cfg.Parallels.Template, "CRABBOX_PARALLELS_TEMPLATE")
	cfg.Parallels.CloneMode = configInputEnvString(cfg, "parallels", cfg.Parallels.CloneMode, "CRABBOX_PARALLELS_CLONE_MODE")
	if value := os.Getenv("CRABBOX_PARALLELS_HOST"); value != "" {
		cfg.Parallels.Host = value
		recordConfigInput(cfg, "parallels", configInputEnvironment, true)
		cfg.Parallels.Hosts = nil
		recordConfigInput(cfg, "parallels", configInputEnvironment, true)
		cfg.Parallels.SelectedHost = ""
		recordConfigInput(cfg, "parallels", configInputEnvironment, true)
		cfg.credentialProvenance.parallelsHost = credentialSourceEnvironment
	}
	cfg.Parallels.HostUser = configInputEnvString(cfg, "parallels", cfg.Parallels.HostUser, "CRABBOX_PARALLELS_HOST_USER")
	if value := os.Getenv("CRABBOX_PARALLELS_HOST_KEY"); value != "" {
		cfg.Parallels.HostKey = expandUserPath(value)
		recordConfigInput(cfg, "parallels", configInputEnvironment, true)
		cfg.credentialProvenance.parallelsHostKey = credentialSourceEnvironment
	}
	cfg.Parallels.VMRoot = expandUserPath(configInputEnvString(cfg, "parallels", cfg.Parallels.VMRoot, "CRABBOX_PARALLELS_VM_ROOT"))
	cfg.Parallels.User = configInputEnvString(cfg, "parallels", cfg.Parallels.User, "CRABBOX_PARALLELS_USER")
	cfg.Parallels.WorkRoot = configInputEnvString(cfg, "parallels", cfg.Parallels.WorkRoot, "CRABBOX_PARALLELS_WORK_ROOT")
	if startupTimeout := os.Getenv("CRABBOX_PARALLELS_STARTUP_TIMEOUT"); startupTimeout != "" {
		recordConfigInput(cfg, "parallels", configInputEnvironment, applyLeaseDuration(&cfg.Parallels.StartupTimeout, startupTimeout))
	}
	if sshUser := os.Getenv("CRABBOX_SSH_USER"); sshUser != "" {
		cfg.SSHUser = sshUser
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
		MarkSSHUserExplicit(cfg)
	}
	if sshKey := os.Getenv("CRABBOX_SSH_KEY"); sshKey != "" {
		cfg.SSHKey = sshKey
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
		MarkSSHKeyExplicit(cfg)
		cfg.credentialProvenance.sshKey = credentialSourceEnvironment
	}
	if sshPort := os.Getenv("CRABBOX_SSH_PORT"); sshPort != "" {
		cfg.SSHPort = sshPort
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
		MarkSSHPortExplicit(cfg)
	}
	if ports, ok := getenvList("CRABBOX_SSH_FALLBACK_PORTS"); ok {
		cfg.SSHFallbackPorts = ports
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
		cfg.sshFallbackPortsExplicit = true
		cfg.explicitSSHFallbackPorts = append([]string(nil), ports...)
	}
	cfg.ProviderKey = configInputEnvString(cfg, "hetzner", cfg.ProviderKey, "CRABBOX_HETZNER_SSH_KEY")
	if workRoot := os.Getenv("CRABBOX_WORK_ROOT"); workRoot != "" {
		cfg.WorkRoot = workRoot
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
		cfg.explicitWorkRoot = workRoot
	}
	if ttl := os.Getenv("CRABBOX_TTL"); ttl != "" {
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, applyLeaseDuration(&cfg.TTL, ttl))
	}
	if idleTimeout := os.Getenv("CRABBOX_IDLE_TIMEOUT"); idleTimeout != "" {
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, applyLeaseDuration(&cfg.IdleTimeout, idleTimeout))
	}
	if market := os.Getenv("CRABBOX_CAPACITY_MARKET"); market != "" {
		cfg.Capacity.Market = market
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
		MarkCapacityMarketExplicit(cfg)
	}
	cfg.Capacity.Strategy = configInputEnvString(cfg, configInputGeneric, cfg.Capacity.Strategy, "CRABBOX_CAPACITY_STRATEGY")
	cfg.Capacity.Fallback = configInputEnvString(cfg, configInputGeneric, cfg.Capacity.Fallback, "CRABBOX_CAPACITY_FALLBACK")
	if value, ok := getenvBool("CRABBOX_CAPACITY_HINTS"); ok {
		cfg.Capacity.Hints = value
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
	}
	cfg.Actions.Workflow = configInputEnvString(cfg, configInputGeneric, cfg.Actions.Workflow, "CRABBOX_ACTIONS_WORKFLOW")
	cfg.Actions.Job = configInputEnvString(cfg, configInputGeneric, cfg.Actions.Job, "CRABBOX_ACTIONS_JOB")
	cfg.Actions.Ref = configInputEnvString(cfg, configInputGeneric, cfg.Actions.Ref, "CRABBOX_ACTIONS_REF")
	cfg.Actions.Repo = configInputEnvString(cfg, configInputGeneric, cfg.Actions.Repo, "CRABBOX_ACTIONS_REPO")
	cfg.Actions.RunnerVersion = configInputEnvString(cfg, configInputGeneric, cfg.Actions.RunnerVersion, "CRABBOX_ACTIONS_RUNNER_VERSION")
	{
		applied, err := cfg.Blacksmith.applyEnvPrefix()
		recordConfigInput(cfg, "blacksmith-testbox", configInputEnvironment, applied.InputAccepted)
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.KubeVirt.applyEnv()
		recordConfigInput(cfg, "kubevirt", configInputEnvironment, applied.InputAccepted)
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
		recordConfigInput(cfg, "sealos-devbox", configInputEnvironment, applied.InputAccepted)
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
		recordConfigInput(cfg, "agent-sandbox", configInputEnvironment, applied.InputAccepted)
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
		recordConfigInput(cfg, "external", configInputEnvironment, true)
		externalProviderOutputExplicit = true
	}
	if arg := os.Getenv("CRABBOX_EXTERNAL_ARG"); arg != "" {
		cfg.External.Args = []string{arg}
		recordConfigInput(cfg, "external", configInputEnvironment, true)
		externalProviderOutputExplicit = true
	}
	if externalProviderOutputExplicit {
		markExternalProviderOutputExplicit(cfg, credentialSourceEnvironment)
	}
	cfg.External.WorkRoot = configInputEnvString(cfg, "external", cfg.External.WorkRoot, "CRABBOX_EXTERNAL_WORK_ROOT")
	if value := os.Getenv("CRABBOX_EXTERNAL_ROUTING_FILE"); value != "" {
		cfg.External.RoutingFile = value
		recordConfigInput(cfg, "external", configInputEnvironment, true)
		cfg.credentialProvenance.externalRouting = credentialSourceEnvironment
	}
	ApplyExternalDesktopEnvironmentOverrides(cfg)
	if value, ok := getenvBool("CRABBOX_EXTERNAL_IDEMPOTENT_LEASE_ID"); ok {
		cfg.External.Capabilities.IdempotentLeaseID = value
		recordConfigInput(cfg, "external", configInputEnvironment, true)
	}
	{
		applied, err := cfg.Namespace.applyEnv()
		recordConfigInput(cfg, "namespace-devbox", configInputEnvironment, applied.InputAccepted)
		if applied.DeleteOnRelease {
			MarkDeleteOnReleaseExplicit(cfg, "namespace-devbox")
		}
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.NamespaceInstance.applyEnv()
		recordConfigInput(cfg, "namespace-instance", configInputEnvironment, applied.InputAccepted)
		if err != nil {
			return err
		}
	}
	cfg.NamespaceInstance.CLIPath = expandUserPath(cfg.NamespaceInstance.CLIPath)
	{
		applied, err := cfg.Phala.applyEnv()
		recordConfigInput(cfg, "phala", configInputEnvironment, applied.InputAccepted)
		cfg.Phala.CLIPath = expandUserPath(cfg.Phala.CLIPath)
		cfg.Phala.Compose = expandUserPath(cfg.Phala.Compose)
		if applied.InstanceType {
			MarkPhalaInstanceTypeExplicit(cfg)
		}
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.Morph.applyEnv()
		recordConfigInput(cfg, "morph", configInputEnvironment, applied.InputAccepted)
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
		recordConfigInput(cfg, "boxd", configInputEnvironment, true)
	}
	if value, ok := os.LookupEnv("CRABBOX_BOXD_ORG"); ok {
		cfg.Boxd.Org = value
		recordConfigInput(cfg, "boxd", configInputEnvironment, true)
	}
	if value := os.Getenv("CRABBOX_BOXD_WORK_ROOT"); value != "" {
		cfg.Boxd.WorkRoot = value
		recordConfigInput(cfg, "boxd", configInputEnvironment, true)
		MarkBoxdWorkRootExplicit(cfg)
		recordConfigInputIntent(cfg, "boxd", configInputEnvironment, true)
	}
	if value, ok := getenvBool("CRABBOX_BOXD_DELETE_ON_RELEASE"); ok {
		cfg.Boxd.DeleteOnRelease = value
		recordConfigInput(cfg, "boxd", configInputEnvironment, true)
		MarkDeleteOnReleaseExplicit(cfg, "boxd")
		recordConfigInputIntent(cfg, "boxd", configInputEnvironment, true)
	}
	{
		applied, err := cfg.Coder.applyEnv()
		recordConfigInput(cfg, "coder", configInputEnvironment, applied.InputAccepted)
		if err != nil {
			return err
		}
	}
	cfg.Coder.CLIPath = expandUserPath(cfg.Coder.CLIPath)
	cfg.Coder.RichParameterFile = expandUserPath(cfg.Coder.RichParameterFile)
	if value, ok := firstNonEmptyEnv("CRABBOX_DAYTONA_API_KEY", "DAYTONA_API_KEY"); ok {
		cfg.Daytona.APIKey = value
		recordConfigInput(cfg, "daytona", configInputEnvironment, true)
		cfg.credentialProvenance.daytonaAPIKey = credentialSourceEnvironment
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_DAYTONA_JWT_TOKEN", "DAYTONA_JWT_TOKEN"); ok {
		cfg.Daytona.JWTToken = value
		recordConfigInput(cfg, "daytona", configInputEnvironment, true)
		cfg.credentialProvenance.daytonaJWTToken = credentialSourceEnvironment
	}
	cfg.Daytona.OrganizationID = configInputEnvString(cfg, "daytona", cfg.Daytona.OrganizationID, "CRABBOX_DAYTONA_ORGANIZATION_ID", "DAYTONA_ORGANIZATION_ID")
	if value, ok := firstNonEmptyEnv("CRABBOX_DAYTONA_API_URL", "DAYTONA_API_URL"); ok {
		cfg.Daytona.APIURL = value
		recordConfigInput(cfg, "daytona", configInputEnvironment, true)
		cfg.credentialProvenance.daytonaAPIURL = credentialSourceEnvironment
	}
	cfg.Daytona.Snapshot = configInputEnvString(cfg, "daytona", cfg.Daytona.Snapshot, "CRABBOX_DAYTONA_SNAPSHOT", "DAYTONA_SNAPSHOT")
	cfg.Daytona.Target = configInputEnvString(cfg, "daytona", cfg.Daytona.Target, "CRABBOX_DAYTONA_TARGET", "DAYTONA_TARGET")
	cfg.Daytona.User = configInputEnvString(cfg, "daytona", cfg.Daytona.User, "CRABBOX_DAYTONA_USER")
	cfg.Daytona.WorkRoot = configInputEnvString(cfg, "daytona", cfg.Daytona.WorkRoot, "CRABBOX_DAYTONA_WORK_ROOT")
	if value := os.Getenv("CRABBOX_DAYTONA_SSH_GATEWAY_HOST"); value != "" {
		cfg.Daytona.SSHGatewayHost = value
		recordConfigInput(cfg, "daytona", configInputEnvironment, true)
		cfg.credentialProvenance.daytonaSSHGateway = credentialSourceEnvironment
	}
	cfg.Daytona.SSHAccessMinutes = configInputEnvInt(cfg, "daytona", cfg.Daytona.SSHAccessMinutes, "CRABBOX_DAYTONA_SSH_ACCESS_MINUTES")
	{
		applied, err := cfg.E2B.applyEnv()
		recordConfigInput(cfg, "e2b", configInputEnvironment, applied.InputAccepted)
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
		recordConfigInput(cfg, "cubesandbox", configInputEnvironment, true)
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_CUBESANDBOX_API_URL", "CUBE_API_URL", "E2B_API_URL"); ok {
		cfg.CubeSandbox.APIURL = value
		recordConfigInput(cfg, "cubesandbox", configInputEnvironment, true)
		cfg.credentialProvenance.cubeSandboxAPIURL = credentialSourceEnvironment
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_CUBESANDBOX_DOMAIN", "CUBE_SANDBOX_DOMAIN"); ok {
		cfg.CubeSandbox.Domain = value
		recordConfigInput(cfg, "cubesandbox", configInputEnvironment, true)
		cfg.credentialProvenance.cubeSandboxDomain = credentialSourceEnvironment
	}
	cfg.CubeSandbox.Template = configInputEnvString(cfg, "cubesandbox", cfg.CubeSandbox.Template, "CRABBOX_CUBESANDBOX_TEMPLATE", "CUBE_TEMPLATE_ID")
	cfg.CubeSandbox.Workdir = configInputEnvString(cfg, "cubesandbox", cfg.CubeSandbox.Workdir, "CRABBOX_CUBESANDBOX_WORKDIR")
	cfg.CubeSandbox.User = configInputEnvString(cfg, "cubesandbox", cfg.CubeSandbox.User, "CRABBOX_CUBESANDBOX_USER")
	if value, ok := firstNonEmptyEnv("CRABBOX_CUBESANDBOX_PROXY_NODE_IP", "CUBE_PROXY_NODE_IP"); ok {
		cfg.CubeSandbox.ProxyNodeIP = value
		recordConfigInput(cfg, "cubesandbox", configInputEnvironment, true)
		cfg.credentialProvenance.cubeSandboxProxyNode = credentialSourceEnvironment
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_CUBESANDBOX_PROXY_PORT_HTTP", "CUBE_PROXY_PORT_HTTP"); ok {
		port, err := strconv.Atoi(value)
		if err != nil {
			return exit(2, "invalid cubesandbox proxy HTTP port %q", value)
		}
		cfg.CubeSandbox.ProxyPortHTTP = port
		recordConfigInput(cfg, "cubesandbox", configInputEnvironment, true)
		cfg.credentialProvenance.cubeSandboxProxyPort = credentialSourceEnvironment
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_CUBESANDBOX_PROXY_SCHEME", "CUBE_PROXY_SCHEME"); ok {
		cfg.CubeSandbox.ProxyScheme = value
		recordConfigInput(cfg, "cubesandbox", configInputEnvironment, true)
		cfg.credentialProvenance.cubeSandboxProxyProto = credentialSourceEnvironment
	}
	{
		applied, err := cfg.ExeDev.applyEnv()
		recordConfigInput(cfg, "exe-dev", configInputEnvironment, applied.InputAccepted)
		if applied.ControlHost {
			cfg.credentialProvenance.exeDevControlHost = credentialSourceEnvironment
		}
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.Railway.applyEnv()
		recordConfigInput(cfg, "railway", configInputEnvironment, applied.InputAccepted)
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
		recordConfigInput(cfg, "fastapi-cloud", configInputEnvironment, applied.InputAccepted)
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
		recordConfigInput(cfg, "unikraft-cloud", configInputEnvironment, true)
		cfg.credentialProvenance.unikraftCloudAPIKey = credentialSourceEnvironment
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_UNIKRAFT_CLOUD_API_URL", "UNIKRAFT_CLOUD_API_URL"); ok {
		cfg.UnikraftCloud.APIURL = value
		recordConfigInput(cfg, "unikraft-cloud", configInputEnvironment, true)
		cfg.credentialProvenance.unikraftCloudAPIURL = credentialSourceEnvironment
	}
	cfg.UnikraftCloud.Metro = configInputEnvString(cfg, "unikraft-cloud", cfg.UnikraftCloud.Metro, "CRABBOX_UNIKRAFT_CLOUD_METRO", "UNIKRAFT_CLOUD_METRO", "UKC_METRO")
	cfg.UnikraftCloud.Image = configInputEnvString(cfg, "unikraft-cloud", cfg.UnikraftCloud.Image, "CRABBOX_UNIKRAFT_CLOUD_IMAGE", "UNIKRAFT_CLOUD_IMAGE")
	{
		applied, err := cfg.Runpod.applyEnv()
		recordConfigInput(cfg, "runpod", configInputEnvironment, applied.InputAccepted)
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
		recordConfigInput(cfg, "vast", configInputEnvironment, applied.InputAccepted)
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
	{
		applied, err := cfg.NvidiaBrev.applyEnv()
		recordConfigInput(cfg, "nvidia-brev", configInputEnvironment, applied.InputAccepted)
		if applied.ReleaseAction {
			MarkDeleteOnReleaseExplicit(cfg, "nvidia-brev")
		}
		if applied.WorkRoot {
			MarkNvidiaBrevWorkRootExplicit(cfg)
		}
		if err != nil {
			return err
		}
	}
	cfg.Hostinger.APIToken = configInputEnvString(cfg, "hostinger", cfg.Hostinger.APIToken, "CRABBOX_HOSTINGER_API_TOKEN", "HOSTINGER_API_TOKEN")
	cfg.Hostinger.APIURL = configInputEnvString(cfg, "hostinger", cfg.Hostinger.APIURL, "CRABBOX_HOSTINGER_API_URL", "HOSTINGER_API_URL")
	cfg.Hostinger.ItemID = configInputEnvString(cfg, "hostinger", cfg.Hostinger.ItemID, "CRABBOX_HOSTINGER_ITEM_ID")
	cfg.Hostinger.PaymentMethodID = configInputEnvString(cfg, "hostinger", cfg.Hostinger.PaymentMethodID, "CRABBOX_HOSTINGER_PAYMENT_METHOD_ID")
	cfg.Hostinger.TemplateID = configInputEnvString(cfg, "hostinger", cfg.Hostinger.TemplateID, "CRABBOX_HOSTINGER_TEMPLATE_ID")
	cfg.Hostinger.DataCenterID = configInputEnvString(cfg, "hostinger", cfg.Hostinger.DataCenterID, "CRABBOX_HOSTINGER_DATA_CENTER_ID")
	cfg.Hostinger.HostnamePrefix = configInputEnvString(cfg, "hostinger", cfg.Hostinger.HostnamePrefix, "CRABBOX_HOSTINGER_HOSTNAME_PREFIX")
	if user := os.Getenv("CRABBOX_HOSTINGER_USER"); user != "" {
		cfg.Hostinger.User = user
		recordConfigInput(cfg, "hostinger", configInputEnvironment, true)
		MarkHostingerUserExplicit(cfg)
	}
	if workRoot := os.Getenv("CRABBOX_HOSTINGER_WORK_ROOT"); workRoot != "" {
		cfg.Hostinger.WorkRoot = workRoot
		recordConfigInput(cfg, "hostinger", configInputEnvironment, true)
		MarkHostingerWorkRootExplicit(cfg)
	}
	if value, ok := getenvBool("CRABBOX_HOSTINGER_ALLOW_PURCHASE"); ok {
		cfg.Hostinger.AllowPurchase = value
		recordConfigInput(cfg, "hostinger", configInputEnvironment, true)
	}
	cfg.Hostinger.ReleaseAction = configInputEnvString(cfg, "hostinger", cfg.Hostinger.ReleaseAction, "CRABBOX_HOSTINGER_RELEASE_ACTION")
	{
		applied, err := cfg.Wandb.applyEnv()
		recordConfigInput(cfg, "wandb", configInputEnvironment, applied.InputAccepted)
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.Orgo.applyEnv()
		recordConfigInput(cfg, "orgo", configInputEnvironment, applied.InputAccepted)
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
		recordConfigInput(cfg, "islo", configInputEnvironment, true)
		cfg.credentialProvenance.isloAPIKey = credentialSourceEnvironment
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_ISLO_BASE_URL", "ISLO_BASE_URL"); ok {
		cfg.Islo.BaseURL = value
		recordConfigInput(cfg, "islo", configInputEnvironment, true)
		cfg.credentialProvenance.isloBaseURL = credentialSourceEnvironment
	}
	if image := os.Getenv("CRABBOX_ISLO_IMAGE"); image != "" {
		cfg.Islo.Image = image
		recordConfigInput(cfg, "islo", configInputEnvironment, true)
		cfg.isloImageExplicit = true
	}
	cfg.Islo.Workdir = configInputEnvString(cfg, "islo", cfg.Islo.Workdir, "CRABBOX_ISLO_WORKDIR")
	cfg.Islo.GatewayProfile = configInputEnvString(cfg, "islo", cfg.Islo.GatewayProfile, "CRABBOX_ISLO_GATEWAY_PROFILE")
	cfg.Islo.SnapshotName = configInputEnvString(cfg, "islo", cfg.Islo.SnapshotName, "CRABBOX_ISLO_SNAPSHOT_NAME")
	if raw := os.Getenv("CRABBOX_ISLO_VCPUS"); raw != "" {
		cfg.Islo.VCPUs = configInputEnvInt(cfg, "islo", cfg.Islo.VCPUs, "CRABBOX_ISLO_VCPUS")
		if _, err := strconv.Atoi(raw); err == nil {
			cfg.isloVCPUsExplicit = true
		}
	}
	if raw := os.Getenv("CRABBOX_ISLO_MEMORY_MB"); raw != "" {
		cfg.Islo.MemoryMB = configInputEnvInt(cfg, "islo", cfg.Islo.MemoryMB, "CRABBOX_ISLO_MEMORY_MB")
		if _, err := strconv.Atoi(raw); err == nil {
			cfg.isloMemoryMBExplicit = true
		}
	}
	if raw := os.Getenv("CRABBOX_ISLO_DISK_GB"); raw != "" {
		cfg.Islo.DiskGB = configInputEnvInt(cfg, "islo", cfg.Islo.DiskGB, "CRABBOX_ISLO_DISK_GB")
		if _, err := strconv.Atoi(raw); err == nil {
			cfg.isloDiskGBExplicit = true
		}
	}
	cfg.Freestyle.APIKey = configInputEnvString(cfg, "freestyle", cfg.Freestyle.APIKey, "CRABBOX_FREESTYLE_API_KEY", "FREESTYLE_API_KEY")
	cfg.Freestyle.APIURL = configInputEnvString(cfg, "freestyle", cfg.Freestyle.APIURL, "CRABBOX_FREESTYLE_API_URL", "FREESTYLE_API_URL")
	cfg.Freestyle.Workdir = configInputEnvString(cfg, "freestyle", cfg.Freestyle.Workdir, "CRABBOX_FREESTYLE_WORKDIR")
	cfg.Freestyle.VCPUs = configInputEnvInt(cfg, "freestyle", cfg.Freestyle.VCPUs, "CRABBOX_FREESTYLE_VCPUS")
	cfg.Freestyle.MemoryGB = configInputEnvInt(cfg, "freestyle", cfg.Freestyle.MemoryGB, "CRABBOX_FREESTYLE_MEMORY_GB")
	cfg.Tenki.CLIPath = configInputEnvString(cfg, "tenki", cfg.Tenki.CLIPath, "CRABBOX_TENKI_CLI", "TENKI_CLI")
	if value, ok := firstNonEmptyEnv("CRABBOX_TENKI_ENDPOINT", "TENKI_ENDPOINT"); ok {
		cfg.Tenki.Endpoint = value
		recordConfigInput(cfg, "tenki", configInputEnvironment, true)
		cfg.credentialProvenance.tenkiEndpoint = credentialSourceEnvironment
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_TENKI_GATEWAY", "TENKI_GATEWAY"); ok {
		cfg.Tenki.Gateway = value
		recordConfigInput(cfg, "tenki", configInputEnvironment, true)
		cfg.credentialProvenance.tenkiGateway = credentialSourceEnvironment
	}
	cfg.Tenki.Workspace = configInputEnvString(cfg, "tenki", cfg.Tenki.Workspace, "CRABBOX_TENKI_WORKSPACE")
	cfg.Tenki.Project = configInputEnvString(cfg, "tenki", cfg.Tenki.Project, "CRABBOX_TENKI_PROJECT")
	cfg.Tenki.Image = configInputEnvString(cfg, "tenki", cfg.Tenki.Image, "CRABBOX_TENKI_IMAGE")
	cfg.Tenki.Snapshot = configInputEnvString(cfg, "tenki", cfg.Tenki.Snapshot, "CRABBOX_TENKI_SNAPSHOT")
	cfg.Tenki.WorkRoot = configInputEnvString(cfg, "tenki", cfg.Tenki.WorkRoot, "CRABBOX_TENKI_WORK_ROOT")
	cfg.Tenki.CPUs = configInputEnvInt(cfg, "tenki", cfg.Tenki.CPUs, "CRABBOX_TENKI_CPUS")
	cfg.Tenki.MemoryMB = configInputEnvInt(cfg, "tenki", cfg.Tenki.MemoryMB, "CRABBOX_TENKI_MEMORY_MB")
	cfg.Tenki.DiskGB = configInputEnvInt(cfg, "tenki", cfg.Tenki.DiskGB, "CRABBOX_TENKI_DISK_GB")
	{
		applied, err := cfg.Tensorlake.applyEnv()
		recordConfigInput(cfg, "tensorlake", configInputEnvironment, applied.InputAccepted)
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
	{
		applied, err := cfg.Cua.applyEnv()
		recordConfigInput(cfg, "cua", configInputEnvironment, applied.InputAccepted)
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.OpenComputer.applyEnv()
		recordConfigInput(cfg, "opencomputer", configInputEnvironment, applied.InputAccepted)
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.CodeSandbox.applyEnv()
		recordConfigInput(cfg, "codesandbox", configInputEnvironment, applied.InputAccepted)
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.OpenSandbox.applyEnv()
		recordConfigInput(cfg, "opensandbox", configInputEnvironment, applied.InputAccepted)
		if err != nil {
			return err
		}
	}
	if value := os.Getenv("CRABBOX_NOMAD_ADDR"); value != "" {
		cfg.Nomad.Address = value
		cfg.credentialProvenance.nomadAddress = credentialSourceEnvironment
		recordConfigInput(cfg, "nomad", configInputEnvironment, true)
	} else if value := os.Getenv("NOMAD_ADDR"); value != "" {
		cfg.Nomad.Address = value
		cfg.credentialProvenance.nomadAddress = credentialSourceEnvironment
		recordConfigInput(cfg, "nomad", configInputEnvironment, true)
	}
	cfg.Nomad.Region = configInputEnvString(cfg, "nomad", cfg.Nomad.Region, "CRABBOX_NOMAD_REGION", "NOMAD_REGION")
	cfg.Nomad.Namespace = configInputEnvString(cfg, "nomad", cfg.Nomad.Namespace, "CRABBOX_NOMAD_NAMESPACE", "NOMAD_NAMESPACE")
	if value := os.Getenv("CRABBOX_NOMAD_TOKEN_ENV"); value != "" {
		cfg.Nomad.TokenEnv = value
		cfg.credentialProvenance.nomadTokenEnv = credentialSourceEnvironment
		recordConfigInput(cfg, "nomad", configInputEnvironment, true)
	}
	cfg.Nomad.CACert = expandUserPath(configInputEnvString(cfg, "nomad", cfg.Nomad.CACert, "CRABBOX_NOMAD_CA_CERT", "NOMAD_CACERT"))
	cfg.Nomad.CAPath = expandUserPath(configInputEnvString(cfg, "nomad", cfg.Nomad.CAPath, "CRABBOX_NOMAD_CA_PATH", "NOMAD_CAPATH"))
	cfg.Nomad.ClientCert = expandUserPath(configInputEnvString(cfg, "nomad", cfg.Nomad.ClientCert, "CRABBOX_NOMAD_CLIENT_CERT", "NOMAD_CLIENT_CERT"))
	cfg.Nomad.ClientKey = expandUserPath(configInputEnvString(cfg, "nomad", cfg.Nomad.ClientKey, "CRABBOX_NOMAD_CLIENT_KEY", "NOMAD_CLIENT_KEY"))
	cfg.Nomad.TLSServerName = configInputEnvString(cfg, "nomad", cfg.Nomad.TLSServerName, "CRABBOX_NOMAD_TLS_SERVER_NAME", "NOMAD_TLS_SERVER_NAME")
	if v, ok := getenvBool("CRABBOX_NOMAD_SKIP_VERIFY"); ok {
		cfg.Nomad.SkipVerify = v
		recordConfigInput(cfg, "nomad", configInputEnvironment, true)
	} else if v, ok := getenvBool("NOMAD_SKIP_VERIFY"); ok {
		cfg.Nomad.SkipVerify = v
		recordConfigInput(cfg, "nomad", configInputEnvironment, true)
	}
	cfg.Nomad.Task = configInputEnvString(cfg, "nomad", cfg.Nomad.Task, "CRABBOX_NOMAD_TASK")
	cfg.Nomad.Driver = configInputEnvString(cfg, "nomad", cfg.Nomad.Driver, "CRABBOX_NOMAD_DRIVER")
	cfg.Nomad.Image = configInputEnvString(cfg, "nomad", cfg.Nomad.Image, "CRABBOX_NOMAD_IMAGE")
	cfg.Nomad.Workdir = configInputEnvString(cfg, "nomad", cfg.Nomad.Workdir, "CRABBOX_NOMAD_WORKDIR")
	cfg.Nomad.JobSpecTemplate = expandUserPath(configInputEnvString(cfg, "nomad", cfg.Nomad.JobSpecTemplate, "CRABBOX_NOMAD_JOBSPEC_TEMPLATE"))
	cfg.Nomad.NodePool = configInputEnvString(cfg, "nomad", cfg.Nomad.NodePool, "CRABBOX_NOMAD_NODE_POOL")
	if datacenters, ok := getenvList("CRABBOX_NOMAD_DATACENTERS"); ok {
		cfg.Nomad.Datacenters = datacenters
		recordConfigInput(cfg, "nomad", configInputEnvironment, true)
	}
	cfg.Nomad.CPU = configInputEnvInt(cfg, "nomad", cfg.Nomad.CPU, "CRABBOX_NOMAD_CPU")
	cfg.Nomad.MemoryMB = configInputEnvInt(cfg, "nomad", cfg.Nomad.MemoryMB, "CRABBOX_NOMAD_MEMORY_MB")
	cfg.Nomad.DiskMB = configInputEnvInt(cfg, "nomad", cfg.Nomad.DiskMB, "CRABBOX_NOMAD_DISK_MB")
	if timeout := os.Getenv("CRABBOX_NOMAD_ALLOC_READY_TIMEOUT"); timeout != "" {
		recordConfigInput(cfg, "nomad", configInputEnvironment, applyLeaseDuration(&cfg.Nomad.AllocReadyTimeout, timeout))
	}
	if timeout := os.Getenv("CRABBOX_NOMAD_EVAL_TIMEOUT"); timeout != "" {
		recordConfigInput(cfg, "nomad", configInputEnvironment, applyLeaseDuration(&cfg.Nomad.EvalTimeout, timeout))
	}
	var nomadExecAccepted bool
	cfg.Nomad.ExecTimeoutSecs, nomadExecAccepted, err = getenvNonNegativeIntAccepted("CRABBOX_NOMAD_EXEC_TIMEOUT_SECS", cfg.Nomad.ExecTimeoutSecs)
	recordConfigInput(cfg, "nomad", configInputEnvironment, nomadExecAccepted)
	if err != nil {
		return err
	}
	{
		applied, err := cfg.Blaxel.applyEnv()
		recordConfigInput(cfg, "blaxel", configInputEnvironment, applied.InputAccepted)
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.VercelSandbox.applyEnv()
		recordConfigInput(cfg, "vercel-sandbox", configInputEnvironment, applied.InputAccepted)
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.CloudflareSandbox.applyEnv()
		recordConfigInput(cfg, "cloudflare-sandbox", configInputEnvironment, applied.InputAccepted)
		if err != nil {
			return err
		}
	}
	cfg.Superserve.BaseURL = configInputEnvString(cfg, "superserve", cfg.Superserve.BaseURL, "CRABBOX_SUPERSERVE_BASE_URL", "SUPERSERVE_BASE_URL")
	cfg.Superserve.Template = configInputEnvString(cfg, "superserve", cfg.Superserve.Template, "CRABBOX_SUPERSERVE_TEMPLATE")
	cfg.Superserve.Snapshot = configInputEnvString(cfg, "superserve", cfg.Superserve.Snapshot, "CRABBOX_SUPERSERVE_SNAPSHOT")
	cfg.Superserve.Workdir = configInputEnvString(cfg, "superserve", cfg.Superserve.Workdir, "CRABBOX_SUPERSERVE_WORKDIR")
	{
		var accepted bool
		cfg.Superserve.TimeoutSecs, accepted, err = getenvNonNegativeIntAccepted("CRABBOX_SUPERSERVE_TIMEOUT_SECS", cfg.Superserve.TimeoutSecs)
		recordConfigInput(cfg, "superserve", configInputEnvironment, accepted)
	}
	if err != nil {
		return err
	}
	{
		var accepted bool
		cfg.Superserve.ExecTimeoutSecs, accepted, err = getenvNonNegativeIntAccepted("CRABBOX_SUPERSERVE_EXEC_TIMEOUT_SECS", cfg.Superserve.ExecTimeoutSecs)
		recordConfigInput(cfg, "superserve", configInputEnvironment, accepted)
	}
	if err != nil {
		return err
	}
	if allowOut := os.Getenv("CRABBOX_SUPERSERVE_NETWORK_ALLOW_OUT"); allowOut != "" {
		cfg.Superserve.NetworkAllowOut = splitCommaList(allowOut)
		recordConfigInput(cfg, "superserve", configInputEnvironment, true)
	}
	if denyOut := os.Getenv("CRABBOX_SUPERSERVE_NETWORK_DENY_OUT"); denyOut != "" {
		cfg.Superserve.NetworkDenyOut = splitCommaList(denyOut)
		recordConfigInput(cfg, "superserve", configInputEnvironment, true)
	}
	if v, ok := getenvBool("CRABBOX_SUPERSERVE_FORGET_MISSING"); ok {
		cfg.Superserve.ForgetMissing = v
		recordConfigInput(cfg, "superserve", configInputEnvironment, true)
	}
	cfg.DockerSandbox.CLIPath = configInputEnvString(cfg, "docker-sandbox", cfg.DockerSandbox.CLIPath, "CRABBOX_DOCKER_SANDBOX_CLI")
	cfg.DockerSandbox.Agent = configInputEnvString(cfg, "docker-sandbox", cfg.DockerSandbox.Agent, "CRABBOX_DOCKER_SANDBOX_AGENT")
	cfg.DockerSandbox.Template = configInputEnvString(cfg, "docker-sandbox", cfg.DockerSandbox.Template, "CRABBOX_DOCKER_SANDBOX_TEMPLATE")
	if cpus := os.Getenv("CRABBOX_DOCKER_SANDBOX_CPUS"); cpus != "" {
		parsed, err := strconv.ParseFloat(cpus, 64)
		if err != nil {
			return fmt.Errorf("parse CRABBOX_DOCKER_SANDBOX_CPUS: %w", err)
		}
		cfg.DockerSandbox.CPUs = parsed
		recordConfigInput(cfg, "docker-sandbox", configInputEnvironment, true)
	}
	cfg.DockerSandbox.Memory = configInputEnvString(cfg, "docker-sandbox", cfg.DockerSandbox.Memory, "CRABBOX_DOCKER_SANDBOX_MEMORY")
	if v, ok := getenvBool("CRABBOX_DOCKER_SANDBOX_CLONE"); ok {
		cfg.DockerSandbox.Clone = v
		recordConfigInput(cfg, "docker-sandbox", configInputEnvironment, true)
	}
	cfg.DockerSandbox.Workdir = configInputEnvString(cfg, "docker-sandbox", cfg.DockerSandbox.Workdir, "CRABBOX_DOCKER_SANDBOX_WORKDIR")
	if values, ok := getenvList("CRABBOX_DOCKER_SANDBOX_EXTRA_WORKSPACES"); ok {
		cfg.DockerSandbox.ExtraWorkspaces = values
		recordConfigInput(cfg, "docker-sandbox", configInputEnvironment, true)
	}
	if values, ok := getenvList("CRABBOX_DOCKER_SANDBOX_MCP"); ok {
		cfg.DockerSandbox.MCP = values
		recordConfigInput(cfg, "docker-sandbox", configInputEnvironment, true)
	}
	if values, ok := getenvList("CRABBOX_DOCKER_SANDBOX_KIT"); ok {
		cfg.DockerSandbox.Kit = values
		recordConfigInput(cfg, "docker-sandbox", configInputEnvironment, true)
	}
	{
		applied, err := cfg.AnthropicSRT.applyEnv()
		recordConfigInput(cfg, "anthropic-sandbox-runtime", configInputEnvironment, applied.InputAccepted)
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.CloudRunSandbox.applyEnv()
		recordConfigInput(cfg, "cloud-run-sandbox", configInputEnvironment, applied.InputAccepted)
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.Modal.applyEnv()
		recordConfigInput(cfg, "modal", configInputEnvironment, applied.InputAccepted)
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.UpstashBox.applyEnv()
		recordConfigInput(cfg, "upstash-box", configInputEnvironment, applied.InputAccepted)
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
		recordConfigInput(cfg, "smolvm", configInputEnvironment, applied.InputAccepted)
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
		recordConfigInput(cfg, "ascii-box", configInputEnvironment, true)
		cfg.credentialProvenance.asciiBoxAPIKey = credentialSourceEnvironment
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_ASCII_BOX_BASE_URL", "ASCII_BOX_BASE_URL"); ok {
		cfg.AsciiBox.BaseURL = value
		recordConfigInput(cfg, "ascii-box", configInputEnvironment, true)
		cfg.credentialProvenance.asciiBoxBaseURL = credentialSourceEnvironment
	}
	cfg.AsciiBox.CLIPath = configInputEnvString(cfg, "ascii-box", cfg.AsciiBox.CLIPath, "CRABBOX_ASCII_BOX_CLI", "BOX_CLI")
	cfg.AsciiBox.Workdir = configInputEnvString(cfg, "ascii-box", cfg.AsciiBox.Workdir, "CRABBOX_ASCII_BOX_WORKDIR")
	{
		applied, err := cfg.Cloudflare.applyEnv()
		recordConfigInput(cfg, "cloudflare", configInputEnvironment, applied.InputAccepted)
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
	{
		applied, err := cfg.Crownest.applyEnv()
		recordConfigInput(cfg, "crownest", configInputEnvironment, applied.InputAccepted)
		if err != nil {
			return err
		}
	}
	cfg.CloudflareDynamicWorkers.LoaderURL = configInputEnvString(cfg, "cloudflare-dynamic-workers", cfg.CloudflareDynamicWorkers.LoaderURL, "CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_URL", "CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_LOADER_URL")
	cfg.CloudflareDynamicWorkers.Token = configInputEnvString(cfg, "cloudflare-dynamic-workers", cfg.CloudflareDynamicWorkers.Token, "CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN")
	cfg.CloudflareDynamicWorkers.CompatibilityDate = configInputEnvString(cfg, "cloudflare-dynamic-workers", cfg.CloudflareDynamicWorkers.CompatibilityDate, "CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_COMPATIBILITY_DATE")
	if flags, ok := getenvList("CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_COMPATIBILITY_FLAGS"); ok {
		cfg.CloudflareDynamicWorkers.CompatibilityFlags = flags
		recordConfigInput(cfg, "cloudflare-dynamic-workers", configInputEnvironment, true)
	}
	cfg.CloudflareDynamicWorkers.CacheMode = configInputEnvString(cfg, "cloudflare-dynamic-workers", cfg.CloudflareDynamicWorkers.CacheMode, "CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_CACHE_MODE")
	cfg.CloudflareDynamicWorkers.Egress = configInputEnvString(cfg, "cloudflare-dynamic-workers", cfg.CloudflareDynamicWorkers.Egress, "CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_EGRESS")
	cfg.CloudflareDynamicWorkers.CPUMs = configInputEnvInt(cfg, "cloudflare-dynamic-workers", cfg.CloudflareDynamicWorkers.CPUMs, "CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_CPU_MS")
	cfg.CloudflareDynamicWorkers.Subrequests = configInputEnvInt(cfg, "cloudflare-dynamic-workers", cfg.CloudflareDynamicWorkers.Subrequests, "CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_SUBREQUESTS")
	cfg.CloudflareDynamicWorkers.TimeoutSecs = configInputEnvInt(cfg, "cloudflare-dynamic-workers", cfg.CloudflareDynamicWorkers.TimeoutSecs, "CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TIMEOUT_SECS")
	{
		applied, err := cfg.Semaphore.applyEnv()
		recordConfigInput(cfg, "semaphore", configInputEnvironment, applied.InputAccepted)
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
		recordConfigInput(cfg, "sprites", configInputEnvironment, true)
		cfg.credentialProvenance.spritesToken = credentialSourceEnvironment
	}
	if value, ok := firstNonEmptyEnv("CRABBOX_SPRITES_API_URL", "SPRITES_API_URL"); ok {
		cfg.Sprites.APIURL = value
		recordConfigInput(cfg, "sprites", configInputEnvironment, true)
		cfg.credentialProvenance.spritesAPIURL = credentialSourceEnvironment
	}
	cfg.Sprites.WorkRoot = configInputEnvString(cfg, "sprites", cfg.Sprites.WorkRoot, "CRABBOX_SPRITES_WORK_ROOT")
	{
		applied := applyLocalContainerEnv(cfg)
		recordConfigInput(cfg, "local-container", configInputEnvironment, applied.InputAccepted)
	}
	{
		applied := cfg.AppleContainer.applyEnv()
		recordConfigInput(cfg, "apple-container", configInputEnvironment, applied.InputAccepted)
		recordConfigInput(cfg, "apple-machine", configInputEnvironment, applied.InputAccepted)
		if applied.Image {
			MarkAppleContainerImageExplicit(cfg)
		}
	}
	{
		applied, err := applyAppleVMEnv(cfg)
		recordConfigInput(cfg, "apple-vm", configInputEnvironment, applied.InputAccepted)
		if err != nil {
			return err
		}
	}
	cfg.MXC.CLIPath = configInputEnvString(cfg, "mxc", cfg.MXC.CLIPath, "CRABBOX_MXC_CLI")
	cfg.MXC.Version = configInputEnvString(cfg, "mxc", cfg.MXC.Version, "CRABBOX_MXC_VERSION")
	cfg.MXC.Containment = configInputEnvString(cfg, "mxc", cfg.MXC.Containment, "CRABBOX_MXC_CONTAINMENT")
	cfg.MXC.Network = configInputEnvString(cfg, "mxc", cfg.MXC.Network, "CRABBOX_MXC_NETWORK")
	if value := os.Getenv("CRABBOX_MXC_READONLY_PATHS"); value != "" {
		cfg.MXC.ReadOnlyPaths = splitCommaList(value)
		recordConfigInput(cfg, "mxc", configInputEnvironment, true)
	}
	if value := os.Getenv("CRABBOX_MXC_READWRITE_PATHS"); value != "" {
		cfg.MXC.ReadWritePaths = splitCommaList(value)
		recordConfigInput(cfg, "mxc", configInputEnvironment, true)
	}
	if value := os.Getenv("CRABBOX_MXC_ALLOWED_HOSTS"); value != "" {
		cfg.MXC.AllowedHosts = splitCommaList(value)
		recordConfigInput(cfg, "mxc", configInputEnvironment, true)
	}
	if value := os.Getenv("CRABBOX_MXC_BLOCKED_HOSTS"); value != "" {
		cfg.MXC.BlockedHosts = splitCommaList(value)
		recordConfigInput(cfg, "mxc", configInputEnvironment, true)
	}
	if value, ok := getenvBool("CRABBOX_MXC_ALLOW_DACL_MUTATION"); ok {
		cfg.MXC.AllowDACLMutation = value
		recordConfigInput(cfg, "mxc", configInputEnvironment, true)
	}
	if value, ok := getenvBool("CRABBOX_MXC_ALLOW_WINDOWS_UI"); ok {
		cfg.MXC.AllowWindowsUI = value
		recordConfigInput(cfg, "mxc", configInputEnvironment, true)
	}
	if value, ok := getenvBool("CRABBOX_MXC_EXPERIMENTAL"); ok {
		cfg.MXC.Experimental = value
		recordConfigInput(cfg, "mxc", configInputEnvironment, true)
	}
	{
		applied, err := cfg.Multipass.applyEnv()
		recordConfigInput(cfg, "multipass", configInputEnvironment, applied.InputAccepted)
		if applied.Image {
			MarkMultipassImageExplicit(cfg)
		}
		if err != nil {
			return err
		}
	}
	{
		applied, err := cfg.Machine0.applyEnv()
		recordConfigInput(cfg, "machine0", configInputEnvironment, applied.InputAccepted)
		if applied.Size {
			cfg.Machine0.SizeExplicit = true
		}
		if err != nil {
			return err
		}
	}
	if image := os.Getenv("CRABBOX_TART_IMAGE"); image != "" {
		cfg.Tart.Image = image
		cfg.tartImageExplicit = true
		recordConfigInput(cfg, "tart", configInputEnvironment, true)
	}
	cfg.Tart.User = configInputEnvString(cfg, "tart", cfg.Tart.User, "CRABBOX_TART_USER")
	cfg.Tart.Password = configInputEnvString(cfg, "tart", cfg.Tart.Password, "CRABBOX_TART_PASSWORD")
	cfg.Tart.WorkRoot = configInputEnvString(cfg, "tart", cfg.Tart.WorkRoot, "CRABBOX_TART_WORK_ROOT")
	if v := os.Getenv("CRABBOX_TART_CPUS"); v != "" {
		cfg.Tart.CPUs = configInputEnvInt(cfg, "tart", cfg.Tart.CPUs, "CRABBOX_TART_CPUS")
		cfg.tartCPUsExplicit = true
		recordConfigInputIntent(cfg, "tart", configInputEnvironment, true)
	}
	if v := os.Getenv("CRABBOX_TART_MEMORY"); v != "" {
		cfg.Tart.Memory = configInputEnvInt(cfg, "tart", cfg.Tart.Memory, "CRABBOX_TART_MEMORY")
		cfg.tartMemoryExplicit = true
		recordConfigInputIntent(cfg, "tart", configInputEnvironment, true)
	}
	if v := os.Getenv("CRABBOX_TART_DISK"); v != "" {
		cfg.Tart.Disk = configInputEnvInt(cfg, "tart", cfg.Tart.Disk, "CRABBOX_TART_DISK")
		cfg.tartDiskExplicit = cfg.Tart.Disk > 0
		recordConfigInputIntent(cfg, "tart", configInputEnvironment, true)
	}
	{
		applied, err := cfg.Lume.applyEnv()
		recordConfigInput(cfg, "lume", configInputEnvironment, applied.InputAccepted)
		if err != nil {
			return err
		}
	}
	cfg.HyperV.Image = configInputEnvString(cfg, "hyperv", cfg.HyperV.Image, "CRABBOX_HYPERV_IMAGE")
	cfg.HyperV.User = configInputEnvString(cfg, "hyperv", cfg.HyperV.User, "CRABBOX_HYPERV_USER")
	cfg.HyperV.WorkRoot = configInputEnvString(cfg, "hyperv", cfg.HyperV.WorkRoot, "CRABBOX_HYPERV_WORK_ROOT")
	cfg.HyperV.CPUs = configInputEnvInt(cfg, "hyperv", cfg.HyperV.CPUs, "CRABBOX_HYPERV_CPUS")
	cfg.HyperV.Memory = configInputEnvInt(cfg, "hyperv", cfg.HyperV.Memory, "CRABBOX_HYPERV_MEMORY")
	cfg.HyperV.Switch = configInputEnvString(cfg, "hyperv", cfg.HyperV.Switch, "CRABBOX_HYPERV_SWITCH")
	cfg.HyperV.GuestPassword = configInputEnvString(cfg, "hyperv", cfg.HyperV.GuestPassword, "CRABBOX_HYPERV_GUEST_PASSWORD")
	if value, ok := getenvBool("CRABBOX_HYPERV_INIT_PASSWORD"); ok {
		cfg.HyperV.InitPassword = value
		recordConfigInput(cfg, "hyperv", configInputEnvironment, true)
	}
	cfg.WindowsSandbox.Workdir = configInputEnvString(cfg, "windows-sandbox", cfg.WindowsSandbox.Workdir, "CRABBOX_WINDOWS_SANDBOX_WORKDIR")
	cfg.WindowsSandbox.TempRoot = expandUserPath(configInputEnvString(cfg, "windows-sandbox", cfg.WindowsSandbox.TempRoot, "CRABBOX_WINDOWS_SANDBOX_TEMP_ROOT"))
	cfg.WindowsSandbox.Networking = configInputEnvString(cfg, "windows-sandbox", cfg.WindowsSandbox.Networking, "CRABBOX_WINDOWS_SANDBOX_NETWORKING")
	cfg.WindowsSandbox.VGPU = configInputEnvString(cfg, "windows-sandbox", cfg.WindowsSandbox.VGPU, "CRABBOX_WINDOWS_SANDBOX_VGPU")
	cfg.WindowsSandbox.Clipboard = configInputEnvString(cfg, "windows-sandbox", cfg.WindowsSandbox.Clipboard, "CRABBOX_WINDOWS_SANDBOX_CLIPBOARD")
	cfg.WindowsSandbox.ProtectedClient = configInputEnvString(cfg, "windows-sandbox", cfg.WindowsSandbox.ProtectedClient, "CRABBOX_WINDOWS_SANDBOX_PROTECTED_CLIENT")
	cfg.WindowsSandbox.AudioInput = configInputEnvString(cfg, "windows-sandbox", cfg.WindowsSandbox.AudioInput, "CRABBOX_WINDOWS_SANDBOX_AUDIO_INPUT")
	cfg.WindowsSandbox.VideoInput = configInputEnvString(cfg, "windows-sandbox", cfg.WindowsSandbox.VideoInput, "CRABBOX_WINDOWS_SANDBOX_VIDEO_INPUT")
	cfg.WindowsSandbox.PrinterRedirection = configInputEnvString(cfg, "windows-sandbox", cfg.WindowsSandbox.PrinterRedirection, "CRABBOX_WINDOWS_SANDBOX_PRINTER_REDIRECTION")
	cfg.WindowsSandbox.MemoryMB = configInputEnvInt(cfg, "windows-sandbox", cfg.WindowsSandbox.MemoryMB, "CRABBOX_WINDOWS_SANDBOX_MEMORY_MB")
	if value, ok := getenvBool("CRABBOX_TAILSCALE"); ok {
		cfg.Tailscale.Enabled = value
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
	}
	if tags := os.Getenv("CRABBOX_TAILSCALE_TAGS"); tags != "" {
		cfg.Tailscale.Tags = normalizeTailscaleTags(splitCommaList(tags))
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
	}
	cfg.Tailscale.HostnameTemplate = configInputEnvString(cfg, configInputGeneric, cfg.Tailscale.HostnameTemplate, "CRABBOX_TAILSCALE_HOSTNAME_TEMPLATE")
	cfg.Tailscale.AuthKeyEnv = configInputEnvString(cfg, configInputGeneric, cfg.Tailscale.AuthKeyEnv, "CRABBOX_TAILSCALE_AUTH_KEY_ENV")
	cfg.Tailscale.ExitNode = configInputEnvString(cfg, configInputGeneric, cfg.Tailscale.ExitNode, "CRABBOX_TAILSCALE_EXIT_NODE")
	if value, ok := getenvBool("CRABBOX_TAILSCALE_EXIT_NODE_ALLOW_LAN_ACCESS"); ok {
		cfg.Tailscale.ExitNodeAllowLANAccess = value
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
	}
	if cfg.Tailscale.AuthKeyEnv != "" {
		cfg.Tailscale.AuthKey = configInputEnvString(cfg, configInputGeneric, "", cfg.Tailscale.AuthKeyEnv)
	}
	cfg.Static.ID = configInputEnvString(cfg, "ssh", cfg.Static.ID, "CRABBOX_STATIC_ID")
	cfg.Static.Name = configInputEnvString(cfg, "ssh", cfg.Static.Name, "CRABBOX_STATIC_NAME")
	if value := os.Getenv("CRABBOX_STATIC_HOST"); value != "" {
		cfg.Static.Host = value
		recordConfigInput(cfg, "ssh", configInputEnvironment, true)
		cfg.credentialProvenance.staticHost = credentialSourceEnvironment
	}
	cfg.Static.User = configInputEnvString(cfg, "ssh", cfg.Static.User, "CRABBOX_STATIC_USER")
	cfg.Static.Port = configInputEnvString(cfg, "ssh", cfg.Static.Port, "CRABBOX_STATIC_PORT")
	cfg.Static.WorkRoot = configInputEnvString(cfg, "ssh", cfg.Static.WorkRoot, "CRABBOX_STATIC_WORK_ROOT")
	{
		applied, err := cfg.Blacksmith.applyEnvSuffix()
		recordConfigInput(cfg, "blacksmith-testbox", configInputEnvironment, applied.InputAccepted)
		if err != nil {
			return err
		}
	}
	if labels := os.Getenv("CRABBOX_ACTIONS_RUNNER_LABELS"); labels != "" {
		cfg.Actions.RunnerLabels = splitCommaList(labels)
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
	}
	if value, ok := getenvBool("CRABBOX_ACTIONS_EPHEMERAL"); ok {
		cfg.Actions.Ephemeral = value
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
	}
	if junit := os.Getenv("CRABBOX_RESULTS_JUNIT"); junit != "" {
		cfg.Results.JUnit = splitCommaList(junit)
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
	}
	if value, ok := getenvBool("CRABBOX_RESULTS_AUTO"); ok {
		cfg.Results.Auto = value
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
	}
	if value, ok := getenvBool("CRABBOX_RESULTS_FAIL_ON_FAILURES"); ok {
		cfg.Results.FailOnFailures = value
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
	}
	if value, ok := getenvBool("CRABBOX_CACHE_PNPM"); ok {
		cfg.Cache.Pnpm = value
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
	}
	if value, ok := getenvBool("CRABBOX_CACHE_NPM"); ok {
		cfg.Cache.Npm = value
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
	}
	if value, ok := getenvBool("CRABBOX_CACHE_DOCKER"); ok {
		cfg.Cache.Docker = value
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
	}
	if value, ok := getenvBool("CRABBOX_CACHE_GIT"); ok {
		cfg.Cache.Git = value
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
	}
	cfg.Cache.MaxGB = configInputEnvInt(cfg, configInputGeneric, cfg.Cache.MaxGB, "CRABBOX_CACHE_MAX_GB")
	if value, ok := getenvBool("CRABBOX_CACHE_PURGE_ON_RELEASE"); ok {
		cfg.Cache.PurgeOnRelease = value
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
	}
	if volumes := os.Getenv("CRABBOX_CACHE_VOLUMES"); volumes != "" {
		parsed, err := ParseCacheVolumeSpecs(splitCommaList(volumes))
		if err != nil {
			return err
		}
		cfg.Cache.Volumes = parsed
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
	}
	if regions := os.Getenv("CRABBOX_CAPACITY_REGIONS"); regions != "" {
		cfg.Capacity.Regions = splitCommaList(regions)
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
	}
	if zones := os.Getenv("CRABBOX_CAPACITY_AVAILABILITY_ZONES"); zones != "" {
		cfg.Capacity.AvailabilityZones = splitCommaList(zones)
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
	}
	if value, ok := getenvBool("CRABBOX_SYNC_CHECKSUM"); ok {
		cfg.Sync.Checksum = value
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
	}
	if value, ok := getenvBool("CRABBOX_SYNC_DELETE"); ok {
		cfg.Sync.Delete = value
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
	}
	if value, ok := getenvBool("CRABBOX_SYNC_GIT_SEED"); ok {
		cfg.Sync.GitSeed = value
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
	}
	if value, ok := getenvBool("CRABBOX_SYNC_GIT_OVERLAY"); ok {
		cfg.Sync.GitOverlay = value
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
	}
	if value, ok := getenvBool("CRABBOX_SYNC_FINGERPRINT"); ok {
		cfg.Sync.Fingerprint = value
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
	}
	if timeout := os.Getenv("CRABBOX_SYNC_TIMEOUT"); timeout != "" {
		if parsed, err := time.ParseDuration(timeout); err == nil {
			cfg.Sync.Timeout = parsed
			recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
		}
	}
	cfg.Sync.WarnFiles = configInputEnvInt(cfg, configInputGeneric, cfg.Sync.WarnFiles, "CRABBOX_SYNC_WARN_FILES")
	cfg.Sync.WarnBytes = int64(configInputEnvInt(cfg, configInputGeneric, int(cfg.Sync.WarnBytes), "CRABBOX_SYNC_WARN_BYTES"))
	cfg.Sync.FailFiles = configInputEnvInt(cfg, configInputGeneric, cfg.Sync.FailFiles, "CRABBOX_SYNC_FAIL_FILES")
	cfg.Sync.FailBytes = int64(configInputEnvInt(cfg, configInputGeneric, int(cfg.Sync.FailBytes), "CRABBOX_SYNC_FAIL_BYTES"))
	if value, ok := getenvBool("CRABBOX_SYNC_ALLOW_LARGE"); ok {
		cfg.Sync.AllowLarge = value
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
	}
	cfg.Sync.BaseRef = configInputEnvString(cfg, configInputGeneric, cfg.Sync.BaseRef, "CRABBOX_SYNC_BASE_REF")
	if envAllow := os.Getenv("CRABBOX_ENV_ALLOW"); envAllow != "" {
		cfg.EnvAllow = splitCommaList(envAllow)
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
		cfg.envAllowOverriddenByEnv = true
	}
	if tools := os.Getenv("CRABBOX_PREFLIGHT_TOOLS"); tools != "" {
		cfg.Run.PreflightTools = parsePreflightToolsOverride(tools)
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
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
		cfg.External.Connection.Desktop.Username = configInputEnvString(cfg, "external", cfg.External.Connection.Desktop.Username, "CRABBOX_EXTERNAL_DESKTOP_USERNAME")
	}
	if os.Getenv("CRABBOX_EXTERNAL_DESKTOP_USERNAME") != "" && !strings.EqualFold(configuredPasswordEnv, "CRABBOX_EXTERNAL_DESKTOP_USERNAME") {
		cfg.credentialProvenance.externalDesktopUser = credentialSourceEnvironment
	}
	if value := os.Getenv("CRABBOX_EXTERNAL_DESKTOP_PASSWORD_ENV"); value != "" && !strings.EqualFold(configuredPasswordEnv, "CRABBOX_EXTERNAL_DESKTOP_PASSWORD_ENV") {
		cfg.External.Connection.Desktop.PasswordEnv = value
		cfg.credentialProvenance.externalDesktopEnv = credentialSourceEnvironment
		recordConfigInput(cfg, "external", configInputEnvironment, true)
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
	if value, ok := lookupEnvInteger(name, strconv.IntSize); ok {
		return int(value)
	}
	return fallback
}

func getenvNonNegativeInt(name string, fallback int) (int, error) {
	value, _, err := getenvNonNegativeIntAccepted(name, fallback)
	return value, err
}

func getenvNonNegativeIntAccepted(name string, fallback int) (int, bool, error) {
	return parseNonNegativeIntAccepted(name, os.Getenv(name), fallback)
}

func getenvNonNegativeIntAliasAccepted(name, alias string, fallback int) (int, bool, error) {
	value := os.Getenv(name)
	if value == "" {
		name = alias
		value = os.Getenv(name)
	}
	return parseNonNegativeIntAccepted(name, value, fallback)
}

func parseNonNegativeIntAccepted(name, value string, fallback int) (int, bool, error) {
	if value == "" {
		return fallback, false, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, false, exit(2, "%s must be an integer", name)
	}
	if parsed < 0 {
		return 0, false, exit(2, "%s must be non-negative", name)
	}
	return parsed, true, nil
}

func getenvInt32(name string, fallback int32) int32 {
	if value, ok := lookupEnvInteger(name, 32); ok {
		return int32(value)
	}
	return fallback
}

func getenvInt64(name string, fallback int64) int64 {
	if value, ok := lookupEnvInteger(name, 64); ok {
		return value
	}
	return fallback
}

// lookupEnvInteger reports accepted raw decimal input independently of fallback
// policy. An explicit zero or a value equal to the fallback still counts.
func lookupEnvInteger(name string, bitSize int) (int64, bool) {
	value := os.Getenv(name)
	if value == "" {
		return 0, false
	}
	parsed, err := strconv.ParseInt(value, 10, bitSize)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

func getenvFloat(name string, fallback float64) float64 {
	if value, ok := lookupEnvFloat(name); ok {
		return value
	}
	return fallback
}

func lookupEnvFloat(name string) (float64, bool) {
	value := os.Getenv(name)
	if value == "" {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
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
	result, _ := appendUniqueStringsAccepted(values, extra...)
	return result
}

func appendUniqueStringsAccepted(values []string, extra ...string) ([]string, bool) {
	seen := map[string]bool{}
	out := make([]string, 0, len(values)+len(extra))
	accepted := false
	for index, value := range append(values, extra...) {
		value = strings.TrimSpace(value)
		if index >= len(values) && value != "" {
			accepted = true
		}
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out, accepted
}

func appendOrderedStrings(values []string, extra ...string) []string {
	result, _ := appendOrderedStringsAccepted(values, extra...)
	return result
}

func appendOrderedStringsAccepted(values []string, extra ...string) ([]string, bool) {
	out := append([]string(nil), values...)
	accepted := false
	for _, value := range extra {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
			accepted = true
		}
	}
	return out, accepted
}
