package cli

import (
	"errors"
	"fmt"
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
	TargetOS                      string
	targetExplicit                bool
	Architecture                  string
	architectureExplicit          bool
	OSImage                       string
	osImageExplicit               bool
	osImageProviderDefaults       string
	WindowsMode                   string
	Desktop                       bool
	DesktopEnv                    string
	Browser                       bool
	Code                          bool
	Network                       NetworkMode
	Class                         string
	Pond                          string
	ExposedPorts                  []string
	ServerType                    string
	ServerTypeExplicit            bool
	Coordinator                   string
	CoordToken                    string
	CoordAdminToken               string
	HostID                        string
	Access                        AccessConfig
	Location                      string
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
	Incus                         IncusConfig
	Proxmox                       ProxmoxConfig
	Parallels                     ParallelsConfig
	parallelsTemplateApplied      bool
	SSHUser                       string
	SSHKey                        string
	SSHPort                       string
	SSHFallbackPorts              []string
	ProviderKey                   string
	WorkRoot                      string
	TTL                           time.Duration
	IdleTimeout                   time.Duration
	Sync                          SyncConfig
	Run                           RunConfig
	EnvAllow                      []string
	Capacity                      CapacityConfig
	Actions                       ActionsConfig
	Blacksmith                    BlacksmithConfig
	KubeVirt                      KubeVirtConfig
	External                      ExternalConfig
	Namespace                     NamespaceConfig
	Morph                         MorphConfig
	Daytona                       DaytonaConfig
	E2B                           E2BConfig
	ExeDev                        ExeDevConfig
	Railway                       RailwayConfig
	Runpod                        RunpodConfig
	Wandb                         WandbConfig
	Islo                          IsloConfig
	isloImageExplicit             bool
	Tenki                         TenkiConfig
	Tensorlake                    TensorlakeConfig
	OpenComputer                  OpenComputerConfig
	DockerSandbox                 DockerSandboxConfig
	Modal                         ModalConfig
	UpstashBox                    UpstashBoxConfig
	AsciiBox                      AsciiBoxConfig
	Cloudflare                    CloudflareConfig
	Semaphore                     SemaphoreConfig
	Sprites                       SpritesConfig
	LocalContainer                LocalContainerConfig
	localContainerRuntimeExplicit bool
	localContainerImageExplicit   bool
	AppleContainer                AppleContainerConfig
	appleContainerImageExplicit   bool
	MXC                           MXCConfig
	Multipass                     MultipassConfig
	multipassImageExplicit        bool
	Tart                          TartConfig
	tartImageExplicit             bool
	tartDiskExplicit              bool
	tartCPUsExplicit              bool
	tartMemoryExplicit            bool
	HyperV                        HyperVConfig
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

type ExternalConfig struct {
	Command     string
	Args        []string
	Config      map[string]any
	Lifecycle   ExternalLifecycleConfig
	Connection  ExternalConnectionConfig
	WorkRoot    string
	RoutingFile string
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
	Provider       string
	Target         string
	WindowsMode    string
	Profile        string
	Class          string
	Architecture   string
	ServerType     string
	Market         string
	TTL            time.Duration
	IdleTimeout    time.Duration
	Desktop        *bool
	DesktopEnv     string
	Browser        *bool
	Code           *bool
	Network        string
	Hydrate        JobHydrateConfig
	Actions        JobActionsConfig
	Shell          bool
	Command        string
	NoSync         bool
	SyncOnly       bool
	Checksum       *bool
	ForceSyncLarge bool
	JUnit          []string
	Downloads      []string
	Stop           string
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

func defaultConfig() Config {
	cfg, err := loadConfig()
	if err != nil {
		return baseConfig()
	}
	return cfg
}

func loadConfig() (Config, error) {
	cfg := baseConfig()
	for _, path := range configPaths() {
		if err := applyConfigFile(&cfg, path); err != nil {
			return Config{}, err
		}
	}
	if err := applyEnv(&cfg); err != nil {
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

func canonicalizeConfigProvider(cfg *Config) {
	provider, err := ProviderFor(cfg.Provider)
	if err == nil {
		cfg.Provider = provider.Name()
	}
}

func applyProviderConfigDefaults(cfg *Config) error {
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
	applyOSImageProviderDefaults(cfg, false)
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

func applyOSImageProviderDefaults(cfg *Config, force bool) {
	if normalizeTargetOS(cfg.TargetOS) != targetLinux {
		return
	}
	hetznerImage, azureImage, gcpImage, isloImage, containerImage, err := osImageDefaultProviderImagesForArchitecture(cfg.OSImage, effectiveArchitectureForConfig(*cfg))
	if err != nil {
		return
	}
	multipassImage, err := osImageDefaultMultipassImage(cfg.OSImage)
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
	if force || cfg.Islo.Image == "" || (!cfg.isloImageExplicit && (cfg.Islo.Image == base.Islo.Image || wasOSDefault)) {
		cfg.Islo.Image = isloImage
	}
	if force || cfg.LocalContainer.Image == "" || (!cfg.localContainerImageExplicit && (cfg.LocalContainer.Image == base.LocalContainer.Image || wasOSDefault)) {
		cfg.LocalContainer.Image = containerImage
	}
	if force || cfg.AppleContainer.Image == "" || (!cfg.appleContainerImageExplicit && (cfg.AppleContainer.Image == base.AppleContainer.Image || wasOSDefault)) {
		cfg.AppleContainer.Image = containerImage
	}
	if force || cfg.Multipass.Image == "" || (!cfg.multipassImageExplicit && (cfg.Multipass.Image == base.Multipass.Image || wasOSDefault)) {
		cfg.Multipass.Image = multipassImage
	}
	cfg.osImageProviderDefaults = cfg.OSImage
}

func MarkIsloImageExplicit(cfg *Config) {
	cfg.isloImageExplicit = true
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

func baseConfig() Config {
	home, _ := os.UserHomeDir()
	sshKey := ""
	if home != "" {
		sshKey = filepath.Join(home, ".ssh", "id_ed25519")
	}

	class := "beast"
	provider := "hetzner"
	osImage := defaultOSImage
	hetznerImage, azureImage, gcpImage, isloImage, containerImage, _ := osImageDefaultProviderImages(osImage)
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
		External: ExternalConfig{
			WorkRoot: defaultPOSIXWorkRoot,
		},
		Namespace: NamespaceConfig{
			Image:               "builtin:base",
			WorkRoot:            "/workspaces/crabbox",
			AutoStopIdleTimeout: 30 * time.Minute,
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
		Islo: IsloConfig{
			BaseURL:  "https://api.islo.dev",
			Image:    isloImage,
			Workdir:  "crabbox",
			VCPUs:    2,
			MemoryMB: 4096,
			DiskGB:   20,
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
		DockerSandbox: DockerSandboxConfig{
			CLIPath: "sbx",
			Agent:   "shell",
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
		AsciiBox: AsciiBoxConfig{
			BaseURL: "https://ascii.dev",
			CLIPath: "box",
			Workdir: "/home/user/crabbox",
		},
		Cloudflare: CloudflareConfig{
			Workdir: "/workspace/crabbox",
		},
		Proxmox: ProxmoxConfig{
			User:      "crabbox",
			WorkRoot:  defaultPOSIXWorkRoot,
			FullClone: true,
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
	Profile              string                             `yaml:"profile,omitempty"`
	Provider             string                             `yaml:"provider,omitempty"`
	Target               string                             `yaml:"target,omitempty"`
	TargetOS             string                             `yaml:"targetOS,omitempty"`
	Architecture         string                             `yaml:"architecture,omitempty"`
	OSImage              string                             `yaml:"os,omitempty"`
	Windows              *fileWindowsConfig                 `yaml:"windows,omitempty"`
	Desktop              *bool                              `yaml:"desktop,omitempty"`
	DesktopEnv           string                             `yaml:"desktopEnv,omitempty"`
	Browser              *bool                              `yaml:"browser,omitempty"`
	Code                 *bool                              `yaml:"code,omitempty"`
	Network              string                             `yaml:"network,omitempty"`
	Class                string                             `yaml:"class,omitempty"`
	ServerType           string                             `yaml:"serverType,omitempty"`
	Coordinator          string                             `yaml:"coordinator,omitempty"`
	CoordinatorToken     string                             `yaml:"coordinatorToken,omitempty"`
	HostID               string                             `yaml:"hostId,omitempty"`
	Broker               *fileBrokerConfig                  `yaml:"broker,omitempty"`
	Hetzner              *fileHetznerConfig                 `yaml:"hetzner,omitempty"`
	AWS                  *fileAWSConfig                     `yaml:"aws,omitempty"`
	Azure                *fileAzureConfig                   `yaml:"azure,omitempty"`
	AzureDynamicSessions *fileAzureDynamicSessionsConfig    `yaml:"azureDynamicSessions,omitempty"`
	GCP                  *fileGCPConfig                     `yaml:"gcp,omitempty"`
	Incus                *fileIncusConfig                   `yaml:"incus,omitempty"`
	Proxmox              *fileProxmoxConfig                 `yaml:"proxmox,omitempty"`
	Parallels            *fileParallelsConfig               `yaml:"parallels,omitempty"`
	SSH                  *fileSSHConfig                     `yaml:"ssh,omitempty"`
	Sync                 *fileSyncConfig                    `yaml:"sync,omitempty"`
	Run                  *fileRunConfig                     `yaml:"run,omitempty"`
	Env                  *fileEnvConfig                     `yaml:"env,omitempty"`
	Capacity             *fileCapacityConfig                `yaml:"capacity,omitempty"`
	Actions              *fileActionsConfig                 `yaml:"actions,omitempty"`
	Blacksmith           *fileBlacksmithConfig              `yaml:"blacksmith,omitempty"`
	KubeVirt             *fileKubeVirtConfig                `yaml:"kubevirt,omitempty"`
	External             *fileExternalConfig                `yaml:"external,omitempty"`
	Namespace            *fileNamespaceConfig               `yaml:"namespace,omitempty"`
	Morph                *fileMorphConfig                   `yaml:"morph,omitempty"`
	Daytona              *fileDaytonaConfig                 `yaml:"daytona,omitempty"`
	E2B                  *fileE2BConfig                     `yaml:"e2b,omitempty"`
	ExeDev               *fileExeDevConfig                  `yaml:"exeDev,omitempty"`
	Railway              *fileRailwayConfig                 `yaml:"railway,omitempty"`
	Runpod               *fileRunpodConfig                  `yaml:"runpod,omitempty"`
	Wandb                *fileWandbConfig                   `yaml:"wandb,omitempty"`
	Islo                 *fileIsloConfig                    `yaml:"islo,omitempty"`
	Tenki                *fileTenkiConfig                   `yaml:"tenki,omitempty"`
	Tensorlake           *fileTensorlakeConfig              `yaml:"tensorlake,omitempty"`
	OpenComputer         *fileOpenComputerConfig            `yaml:"openComputer,omitempty"`
	DockerSandbox        *fileDockerSandboxConfig           `yaml:"dockerSandbox,omitempty"`
	Modal                *fileModalConfig                   `yaml:"modal,omitempty"`
	UpstashBox           *fileUpstashBoxConfig              `yaml:"upstashBox,omitempty"`
	AsciiBox             *fileAsciiBoxConfig                `yaml:"asciiBox,omitempty"`
	Cloudflare           *fileCloudflareConfig              `yaml:"cloudflare,omitempty"`
	Semaphore            *fileSemaphoreConfig               `yaml:"semaphore,omitempty"`
	Sprites              *fileSpritesConfig                 `yaml:"sprites,omitempty"`
	LocalContainer       *fileLocalContainerConfig          `yaml:"localContainer,omitempty"`
	AppleContainer       *fileAppleContainerConfig          `yaml:"appleContainer,omitempty"`
	MXC                  *fileMXCConfig                     `yaml:"mxc,omitempty"`
	Multipass            *fileMultipassConfig               `yaml:"multipass,omitempty"`
	Tart                 *fileTartConfig                    `yaml:"tart,omitempty"`
	HyperV               *fileHyperVConfig                  `yaml:"hyperv,omitempty"`
	Tailscale            *fileTailscaleConfig               `yaml:"tailscale,omitempty"`
	Static               *fileStaticConfig                  `yaml:"static,omitempty"`
	Results              *fileResultsConfig                 `yaml:"results,omitempty"`
	Cache                *fileCacheConfig                   `yaml:"cache,omitempty"`
	Lease                *fileLeaseConfig                   `yaml:"lease,omitempty"`
	Profiles             map[string]fileProfileConfig       `yaml:"profiles,omitempty"`
	Presets              map[string]filePresetConfig        `yaml:"presets,omitempty"`
	ProofTemplates       map[string]fileProofTemplateConfig `yaml:"proofTemplates,omitempty"`
	Jobs                 map[string]fileJobConfig           `yaml:"jobs,omitempty"`
	TTL                  string                             `yaml:"ttl,omitempty"`
	IdleTimeout          string                             `yaml:"idleTimeout,omitempty"`
	WorkRoot             string                             `yaml:"workRoot,omitempty"`
}

type fileWindowsConfig struct {
	Mode string `yaml:"mode,omitempty"`
}

type fileBrokerConfig struct {
	URL        string            `yaml:"url,omitempty"`
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

type fileExternalConfig struct {
	Command     string                    `yaml:"command,omitempty"`
	Args        []string                  `yaml:"args,omitempty"`
	Config      map[string]any            `yaml:"config,omitempty"`
	Lifecycle   *ExternalLifecycleConfig  `yaml:"lifecycle,omitempty"`
	Connection  *ExternalConnectionConfig `yaml:"connection,omitempty"`
	WorkRoot    string                    `yaml:"workRoot,omitempty"`
	RoutingFile string                    `yaml:"routingFile,omitempty"`
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

func applyCloudflareFileConfig(cfg *Config, file *fileCloudflareConfig) {
	if file == nil {
		return
	}
	if file.APIURL != "" {
		cfg.Cloudflare.APIURL = file.APIURL
	}
	if file.Token != "" {
		cfg.Cloudflare.Token = file.Token
	}
	if file.Workdir != "" {
		cfg.Cloudflare.Workdir = file.Workdir
	}
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
	Provider       string                `yaml:"provider,omitempty"`
	Target         string                `yaml:"target,omitempty"`
	TargetOS       string                `yaml:"targetOS,omitempty"`
	Windows        *fileWindowsConfig    `yaml:"windows,omitempty"`
	Profile        string                `yaml:"profile,omitempty"`
	Class          string                `yaml:"class,omitempty"`
	Architecture   string                `yaml:"architecture,omitempty"`
	ServerType     string                `yaml:"serverType,omitempty"`
	Type           string                `yaml:"type,omitempty"`
	Capacity       *fileCapacityConfig   `yaml:"capacity,omitempty"`
	Market         string                `yaml:"market,omitempty"`
	TTL            string                `yaml:"ttl,omitempty"`
	IdleTimeout    string                `yaml:"idleTimeout,omitempty"`
	Desktop        *bool                 `yaml:"desktop,omitempty"`
	DesktopEnv     string                `yaml:"desktopEnv,omitempty"`
	Browser        *bool                 `yaml:"browser,omitempty"`
	Code           *bool                 `yaml:"code,omitempty"`
	Network        string                `yaml:"network,omitempty"`
	Hydrate        *fileJobHydrateConfig `yaml:"hydrate,omitempty"`
	Actions        *fileJobActionsConfig `yaml:"actions,omitempty"`
	Shell          *bool                 `yaml:"shell,omitempty"`
	Command        string                `yaml:"command,omitempty"`
	NoSync         *bool                 `yaml:"noSync,omitempty"`
	SyncOnly       *bool                 `yaml:"syncOnly,omitempty"`
	Checksum       *bool                 `yaml:"checksum,omitempty"`
	ForceSyncLarge *bool                 `yaml:"forceSyncLarge,omitempty"`
	JUnit          []string              `yaml:"junit,omitempty"`
	Downloads      []string              `yaml:"downloads,omitempty"`
	Stop           string                `yaml:"stop,omitempty"`
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
	return applyFileConfig(cfg, file)
}

func applyFileConfig(cfg *Config, file fileConfig) error {
	if file.Profile != "" {
		cfg.Profile = file.Profile
	}
	if file.Provider != "" {
		cfg.Provider = file.Provider
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
	}
	if file.ServerType != "" {
		cfg.ServerType = file.ServerType
		cfg.ServerTypeExplicit = true
	}
	if file.Coordinator != "" {
		cfg.Coordinator = file.Coordinator
	}
	if file.CoordinatorToken != "" {
		cfg.CoordToken = file.CoordinatorToken
	}
	if file.HostID != "" {
		cfg.HostID = file.HostID
	}
	if file.Broker != nil {
		if file.Broker.URL != "" {
			cfg.Coordinator = file.Broker.URL
		}
		if file.Broker.Token != "" {
			cfg.CoordToken = file.Broker.Token
		}
		if file.Broker.AdminToken != "" {
			cfg.CoordAdminToken = file.Broker.AdminToken
		}
		if file.Broker.Provider != "" {
			cfg.Provider = file.Broker.Provider
		}
		if file.Broker.Access != nil {
			if file.Broker.Access.ClientID != "" {
				cfg.Access.ClientID = file.Broker.Access.ClientID
			}
			if file.Broker.Access.ClientSecret != "" {
				cfg.Access.ClientSecret = file.Broker.Access.ClientSecret
			}
			if file.Broker.Access.Token != "" {
				cfg.Access.Token = file.Broker.Access.Token
			}
		}
	}
	if file.Hetzner != nil {
		if file.Hetzner.Location != "" {
			cfg.Location = file.Hetzner.Location
		}
		if file.Hetzner.Image != "" {
			cfg.Image = file.Hetzner.Image
			cfg.imageExplicit = true
		}
		if file.Hetzner.SSHKey != "" {
			cfg.ProviderKey = file.Hetzner.SSHKey
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
		}
		if file.Proxmox.TokenID != "" {
			cfg.Proxmox.TokenID = file.Proxmox.TokenID
		}
		if file.Proxmox.TokenSecret != "" {
			cfg.Proxmox.TokenSecret = file.Proxmox.TokenSecret
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
		}
		if file.SSH.Key != "" {
			cfg.SSHKey = expandUserPath(file.SSH.Key)
		}
		if file.SSH.Port != "" {
			cfg.SSHPort = file.SSH.Port
		}
		if file.SSH.FallbackPorts != nil {
			cfg.SSHFallbackPorts = normalizeList(*file.SSH.FallbackPorts)
		}
	}
	if file.WorkRoot != "" {
		cfg.WorkRoot = file.WorkRoot
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
		}
	}
	if file.Morph != nil {
		if file.Morph.APIKey != "" {
			cfg.Morph.APIKey = file.Morph.APIKey
		}
		if file.Morph.APIURL != "" {
			cfg.Morph.APIURL = file.Morph.APIURL
		}
		if file.Morph.Snapshot != "" {
			cfg.Morph.Snapshot = file.Morph.Snapshot
		}
		if file.Morph.SSHGatewayHost != "" {
			cfg.Morph.SSHGatewayHost = file.Morph.SSHGatewayHost
		}
		if file.Morph.WorkRoot != "" {
			cfg.Morph.WorkRoot = file.Morph.WorkRoot
		}
		if file.Morph.DeleteOnRelease != nil {
			cfg.Morph.DeleteOnRelease = *file.Morph.DeleteOnRelease
		}
		if file.Morph.WakeOnSSH != nil {
			cfg.Morph.WakeOnSSH = *file.Morph.WakeOnSSH
		}
	}
	if file.Daytona != nil {
		if file.Daytona.APIURL != "" {
			cfg.Daytona.APIURL = file.Daytona.APIURL
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
		}
		if file.Daytona.SSHAccessMinutes > 0 {
			cfg.Daytona.SSHAccessMinutes = file.Daytona.SSHAccessMinutes
		}
	}
	if file.E2B != nil {
		if file.E2B.APIURL != "" {
			cfg.E2B.APIURL = file.E2B.APIURL
		}
		if file.E2B.Domain != "" {
			cfg.E2B.Domain = file.E2B.Domain
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
		}
		if file.Islo.MemoryMB > 0 {
			cfg.Islo.MemoryMB = file.Islo.MemoryMB
		}
		if file.Islo.DiskGB > 0 {
			cfg.Islo.DiskGB = file.Islo.DiskGB
		}
	}
	if file.Tenki != nil {
		if file.Tenki.CLIPath != "" {
			cfg.Tenki.CLIPath = file.Tenki.CLIPath
		}
		if file.Tenki.Endpoint != "" {
			cfg.Tenki.Endpoint = file.Tenki.Endpoint
		}
		if file.Tenki.Gateway != "" {
			cfg.Tenki.Gateway = file.Tenki.Gateway
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
	if file.AsciiBox != nil {
		if file.AsciiBox.BaseURL != "" {
			cfg.AsciiBox.BaseURL = file.AsciiBox.BaseURL
		}
		if file.AsciiBox.CLIPath != "" {
			cfg.AsciiBox.CLIPath = file.AsciiBox.CLIPath
		}
		if file.AsciiBox.Workdir != "" {
			cfg.AsciiBox.Workdir = file.AsciiBox.Workdir
		}
	}
	applyCloudflareFileConfig(cfg, file.Cloudflare)
	if file.Semaphore != nil {
		if file.Semaphore.Host != "" {
			cfg.Semaphore.Host = file.Semaphore.Host
		}
		if file.Semaphore.Token != "" {
			cfg.Semaphore.Token = file.Semaphore.Token
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
	cfg.Provider = getenv("CRABBOX_PROVIDER", cfg.Provider)
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
	cfg.WindowsMode = getenv("CRABBOX_WINDOWS_MODE", cfg.WindowsMode)
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
	cfg.Class = getenv("CRABBOX_DEFAULT_CLASS", cfg.Class)
	if os.Getenv("CRABBOX_SERVER_TYPE") != "" {
		cfg.ServerTypeExplicit = true
	}
	cfg.ServerType = getenv("CRABBOX_SERVER_TYPE", cfg.ServerType)
	cfg.Coordinator = getenv("CRABBOX_COORDINATOR", cfg.Coordinator)
	cfg.CoordToken = getenv("CRABBOX_COORDINATOR_TOKEN", cfg.CoordToken)
	cfg.CoordAdminToken = getenv("CRABBOX_COORDINATOR_ADMIN_TOKEN", getenv("CRABBOX_ADMIN_TOKEN", cfg.CoordAdminToken))
	cfg.HostID = getenv("CRABBOX_HOST_ID", cfg.HostID)
	cfg.Access.ClientID = getenv("CRABBOX_ACCESS_CLIENT_ID", getenv("CF_ACCESS_CLIENT_ID", cfg.Access.ClientID))
	cfg.Access.ClientSecret = getenv("CRABBOX_ACCESS_CLIENT_SECRET", getenv("CF_ACCESS_CLIENT_SECRET", cfg.Access.ClientSecret))
	cfg.Access.Token = getenv("CRABBOX_ACCESS_TOKEN", getenv("CF_ACCESS_TOKEN", cfg.Access.Token))
	cfg.Location = getenv("CRABBOX_HETZNER_LOCATION", cfg.Location)
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
	cfg.AzureDynamicSessions.Endpoint = getenv("CRABBOX_AZURE_DYNAMIC_SESSIONS_ENDPOINT", cfg.AzureDynamicSessions.Endpoint)
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
	cfg.Proxmox.APIURL = getenv("CRABBOX_PROXMOX_API_URL", cfg.Proxmox.APIURL)
	cfg.Proxmox.TokenID = getenv("CRABBOX_PROXMOX_TOKEN_ID", cfg.Proxmox.TokenID)
	cfg.Proxmox.TokenSecret = getenv("CRABBOX_PROXMOX_TOKEN_SECRET", cfg.Proxmox.TokenSecret)
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
	cfg.SSHUser = getenv("CRABBOX_SSH_USER", cfg.SSHUser)
	cfg.SSHKey = getenv("CRABBOX_SSH_KEY", cfg.SSHKey)
	cfg.SSHPort = getenv("CRABBOX_SSH_PORT", cfg.SSHPort)
	if ports, ok := getenvList("CRABBOX_SSH_FALLBACK_PORTS"); ok {
		cfg.SSHFallbackPorts = ports
	}
	cfg.ProviderKey = getenv("CRABBOX_HETZNER_SSH_KEY", cfg.ProviderKey)
	cfg.WorkRoot = getenv("CRABBOX_WORK_ROOT", cfg.WorkRoot)
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
	}
	cfg.External.Command = getenv("CRABBOX_EXTERNAL_COMMAND", cfg.External.Command)
	if arg := os.Getenv("CRABBOX_EXTERNAL_ARG"); arg != "" {
		cfg.External.Args = []string{arg}
	}
	cfg.External.WorkRoot = getenv("CRABBOX_EXTERNAL_WORK_ROOT", cfg.External.WorkRoot)
	cfg.External.RoutingFile = getenv("CRABBOX_EXTERNAL_ROUTING_FILE", cfg.External.RoutingFile)
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
	}
	cfg.Morph.APIKey = getenv("CRABBOX_MORPH_API_KEY", getenv("MORPH_API_KEY", cfg.Morph.APIKey))
	cfg.Morph.APIURL = getenv("CRABBOX_MORPH_API_URL", cfg.Morph.APIURL)
	cfg.Morph.Snapshot = getenv("CRABBOX_MORPH_SNAPSHOT", cfg.Morph.Snapshot)
	cfg.Morph.SSHGatewayHost = getenv("CRABBOX_MORPH_SSH_GATEWAY_HOST", cfg.Morph.SSHGatewayHost)
	cfg.Morph.WorkRoot = getenv("CRABBOX_MORPH_WORK_ROOT", cfg.Morph.WorkRoot)
	if value, ok := getenvBool("CRABBOX_MORPH_DELETE_ON_RELEASE"); ok {
		cfg.Morph.DeleteOnRelease = value
	}
	if value, ok := getenvBool("CRABBOX_MORPH_WAKE_ON_SSH"); ok {
		cfg.Morph.WakeOnSSH = value
	}
	cfg.Daytona.APIKey = getenv("CRABBOX_DAYTONA_API_KEY", getenv("DAYTONA_API_KEY", cfg.Daytona.APIKey))
	cfg.Daytona.JWTToken = getenv("CRABBOX_DAYTONA_JWT_TOKEN", getenv("DAYTONA_JWT_TOKEN", cfg.Daytona.JWTToken))
	cfg.Daytona.OrganizationID = getenv("CRABBOX_DAYTONA_ORGANIZATION_ID", getenv("DAYTONA_ORGANIZATION_ID", cfg.Daytona.OrganizationID))
	cfg.Daytona.APIURL = getenv("CRABBOX_DAYTONA_API_URL", getenv("DAYTONA_API_URL", cfg.Daytona.APIURL))
	cfg.Daytona.Snapshot = getenv("CRABBOX_DAYTONA_SNAPSHOT", getenv("DAYTONA_SNAPSHOT", cfg.Daytona.Snapshot))
	cfg.Daytona.Target = getenv("CRABBOX_DAYTONA_TARGET", getenv("DAYTONA_TARGET", cfg.Daytona.Target))
	cfg.Daytona.User = getenv("CRABBOX_DAYTONA_USER", cfg.Daytona.User)
	cfg.Daytona.WorkRoot = getenv("CRABBOX_DAYTONA_WORK_ROOT", cfg.Daytona.WorkRoot)
	cfg.Daytona.SSHGatewayHost = getenv("CRABBOX_DAYTONA_SSH_GATEWAY_HOST", cfg.Daytona.SSHGatewayHost)
	cfg.Daytona.SSHAccessMinutes = getenvInt("CRABBOX_DAYTONA_SSH_ACCESS_MINUTES", cfg.Daytona.SSHAccessMinutes)
	cfg.E2B.APIKey = getenv("CRABBOX_E2B_API_KEY", getenv("E2B_API_KEY", cfg.E2B.APIKey))
	cfg.E2B.APIURL = getenv("CRABBOX_E2B_API_URL", getenv("E2B_API_URL", cfg.E2B.APIURL))
	cfg.E2B.Domain = getenv("CRABBOX_E2B_DOMAIN", getenv("E2B_DOMAIN", cfg.E2B.Domain))
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
	cfg.Railway.APIToken = getenv("CRABBOX_RAILWAY_API_TOKEN", getenv("RAILWAY_API_TOKEN", cfg.Railway.APIToken))
	cfg.Railway.APIURL = getenv("CRABBOX_RAILWAY_API_URL", getenv("RAILWAY_API_URL", cfg.Railway.APIURL))
	cfg.Railway.ProjectID = getenv("CRABBOX_RAILWAY_PROJECT_ID", getenv("RAILWAY_PROJECT_ID", cfg.Railway.ProjectID))
	cfg.Railway.EnvironmentID = getenv("CRABBOX_RAILWAY_ENVIRONMENT_ID", getenv("RAILWAY_ENVIRONMENT_ID", cfg.Railway.EnvironmentID))
	cfg.Runpod.APIKey = getenv("CRABBOX_RUNPOD_API_KEY", getenv("RUNPOD_API_KEY", cfg.Runpod.APIKey))
	cfg.Runpod.APIURL = getenv("CRABBOX_RUNPOD_API_URL", getenv("RUNPOD_API_URL", cfg.Runpod.APIURL))
	cfg.Runpod.CloudType = getenv("CRABBOX_RUNPOD_CLOUD_TYPE", getenv("RUNPOD_CLOUD_TYPE", cfg.Runpod.CloudType))
	cfg.Runpod.InstanceID = getenv("CRABBOX_RUNPOD_INSTANCE_ID", getenv("RUNPOD_INSTANCE_ID", cfg.Runpod.InstanceID))
	cfg.Runpod.Image = getenv("CRABBOX_RUNPOD_IMAGE", getenv("RUNPOD_IMAGE", cfg.Runpod.Image))
	cfg.Runpod.TemplateID = getenv("CRABBOX_RUNPOD_TEMPLATE_ID", getenv("RUNPOD_TEMPLATE_ID", cfg.Runpod.TemplateID))
	cfg.Runpod.DiskGB = getenvInt("CRABBOX_RUNPOD_DISK_GB", cfg.Runpod.DiskGB)
	cfg.Runpod.User = getenv("CRABBOX_RUNPOD_USER", cfg.Runpod.User)
	cfg.Runpod.WorkRoot = getenv("CRABBOX_RUNPOD_WORK_ROOT", cfg.Runpod.WorkRoot)
	// WANDB_API_KEY is resolved by the W&B client after file config so a
	// generic shell login cannot override an explicit wandb.apiKey value.
	cfg.Wandb.APIKey = getenv("CRABBOX_WANDB_API_KEY", cfg.Wandb.APIKey)
	cfg.Wandb.DefaultImage = getenv("CRABBOX_WANDB_DEFAULT_IMAGE", getenv("WANDB_DEFAULT_IMAGE", cfg.Wandb.DefaultImage))
	cfg.Wandb.MaxLifetimeSeconds = getenvInt("CRABBOX_WANDB_MAX_LIFETIME_SECONDS", getenvInt("WANDB_MAX_LIFETIME_SECONDS", cfg.Wandb.MaxLifetimeSeconds))
	cfg.Islo.APIKey = getenv("CRABBOX_ISLO_API_KEY", getenv("ISLO_API_KEY", cfg.Islo.APIKey))
	cfg.Islo.BaseURL = getenv("CRABBOX_ISLO_BASE_URL", getenv("ISLO_BASE_URL", cfg.Islo.BaseURL))
	if image := os.Getenv("CRABBOX_ISLO_IMAGE"); image != "" {
		cfg.Islo.Image = image
		cfg.isloImageExplicit = true
	}
	cfg.Islo.Workdir = getenv("CRABBOX_ISLO_WORKDIR", cfg.Islo.Workdir)
	cfg.Islo.GatewayProfile = getenv("CRABBOX_ISLO_GATEWAY_PROFILE", cfg.Islo.GatewayProfile)
	cfg.Islo.SnapshotName = getenv("CRABBOX_ISLO_SNAPSHOT_NAME", cfg.Islo.SnapshotName)
	cfg.Islo.VCPUs = getenvInt("CRABBOX_ISLO_VCPUS", cfg.Islo.VCPUs)
	cfg.Islo.MemoryMB = getenvInt("CRABBOX_ISLO_MEMORY_MB", cfg.Islo.MemoryMB)
	cfg.Islo.DiskGB = getenvInt("CRABBOX_ISLO_DISK_GB", cfg.Islo.DiskGB)
	cfg.Tenki.CLIPath = getenv("CRABBOX_TENKI_CLI", getenv("TENKI_CLI", cfg.Tenki.CLIPath))
	cfg.Tenki.Endpoint = getenv("CRABBOX_TENKI_ENDPOINT", getenv("TENKI_ENDPOINT", cfg.Tenki.Endpoint))
	cfg.Tenki.Gateway = getenv("CRABBOX_TENKI_GATEWAY", getenv("TENKI_GATEWAY", cfg.Tenki.Gateway))
	cfg.Tenki.Workspace = getenv("CRABBOX_TENKI_WORKSPACE", cfg.Tenki.Workspace)
	cfg.Tenki.Project = getenv("CRABBOX_TENKI_PROJECT", cfg.Tenki.Project)
	cfg.Tenki.Image = getenv("CRABBOX_TENKI_IMAGE", cfg.Tenki.Image)
	cfg.Tenki.Snapshot = getenv("CRABBOX_TENKI_SNAPSHOT", cfg.Tenki.Snapshot)
	cfg.Tenki.WorkRoot = getenv("CRABBOX_TENKI_WORK_ROOT", cfg.Tenki.WorkRoot)
	cfg.Tenki.CPUs = getenvInt("CRABBOX_TENKI_CPUS", cfg.Tenki.CPUs)
	cfg.Tenki.MemoryMB = getenvInt("CRABBOX_TENKI_MEMORY_MB", cfg.Tenki.MemoryMB)
	cfg.Tenki.DiskGB = getenvInt("CRABBOX_TENKI_DISK_GB", cfg.Tenki.DiskGB)
	cfg.Tensorlake.APIKey = getenv("CRABBOX_TENSORLAKE_API_KEY", getenv("TENSORLAKE_API_KEY", cfg.Tensorlake.APIKey))
	cfg.Tensorlake.APIURL = getenv("CRABBOX_TENSORLAKE_API_URL", getenv("TENSORLAKE_API_URL", cfg.Tensorlake.APIURL))
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
	cfg.Modal.App = getenv("CRABBOX_MODAL_APP", cfg.Modal.App)
	cfg.Modal.Image = getenv("CRABBOX_MODAL_IMAGE", cfg.Modal.Image)
	cfg.Modal.Workdir = getenv("CRABBOX_MODAL_WORKDIR", cfg.Modal.Workdir)
	cfg.Modal.Python = getenv("CRABBOX_MODAL_PYTHON", cfg.Modal.Python)
	cfg.UpstashBox.APIKey = getenv("CRABBOX_UPSTASH_BOX_API_KEY", getenv("UPSTASH_BOX_API_KEY", cfg.UpstashBox.APIKey))
	cfg.UpstashBox.BaseURL = getenv("CRABBOX_UPSTASH_BOX_BASE_URL", getenv("UPSTASH_BOX_BASE_URL", cfg.UpstashBox.BaseURL))
	cfg.UpstashBox.Runtime = getenv("CRABBOX_UPSTASH_BOX_RUNTIME", cfg.UpstashBox.Runtime)
	cfg.UpstashBox.Size = getenv("CRABBOX_UPSTASH_BOX_SIZE", cfg.UpstashBox.Size)
	cfg.UpstashBox.Workdir = getenv("CRABBOX_UPSTASH_BOX_WORKDIR", cfg.UpstashBox.Workdir)
	if value, ok := getenvBool("CRABBOX_UPSTASH_BOX_KEEP_ALIVE"); ok {
		cfg.UpstashBox.KeepAlive = value
	}
	cfg.AsciiBox.APIKey = getenv("CRABBOX_ASCII_BOX_API_KEY", getenv("ASCII_BOX_API_KEY", cfg.AsciiBox.APIKey))
	cfg.AsciiBox.BaseURL = getenv("CRABBOX_ASCII_BOX_BASE_URL", getenv("ASCII_BOX_BASE_URL", cfg.AsciiBox.BaseURL))
	cfg.AsciiBox.CLIPath = getenv("CRABBOX_ASCII_BOX_CLI", getenv("BOX_CLI", cfg.AsciiBox.CLIPath))
	cfg.AsciiBox.Workdir = getenv("CRABBOX_ASCII_BOX_WORKDIR", cfg.AsciiBox.Workdir)
	cfg.Cloudflare.APIURL = getenv("CRABBOX_CLOUDFLARE_RUNNER_URL", cfg.Cloudflare.APIURL)
	cfg.Cloudflare.Token = getenv("CRABBOX_CLOUDFLARE_RUNNER_TOKEN", cfg.Cloudflare.Token)
	cfg.Cloudflare.Workdir = getenv("CRABBOX_CLOUDFLARE_WORKDIR", cfg.Cloudflare.Workdir)
	cfg.Semaphore.Host = getenv("CRABBOX_SEMAPHORE_HOST", getenv("SEMAPHORE_HOST", cfg.Semaphore.Host))
	cfg.Semaphore.Token = getenv("CRABBOX_SEMAPHORE_TOKEN", getenv("SEMAPHORE_API_TOKEN", cfg.Semaphore.Token))
	cfg.Semaphore.Project = getenv("CRABBOX_SEMAPHORE_PROJECT", getenv("SEMAPHORE_PROJECT", cfg.Semaphore.Project))
	cfg.Semaphore.Machine = getenv("CRABBOX_SEMAPHORE_MACHINE", cfg.Semaphore.Machine)
	cfg.Semaphore.OSImage = getenv("CRABBOX_SEMAPHORE_OS_IMAGE", cfg.Semaphore.OSImage)
	cfg.Semaphore.IdleTimeout = getenv("CRABBOX_SEMAPHORE_IDLE_TIMEOUT", cfg.Semaphore.IdleTimeout)
	cfg.Sprites.Token = getenv("CRABBOX_SPRITES_TOKEN", getenv("SPRITES_TOKEN", getenv("SPRITE_TOKEN", getenv("SETUP_SPRITE_TOKEN", cfg.Sprites.Token))))
	cfg.Sprites.APIURL = getenv("CRABBOX_SPRITES_API_URL", getenv("SPRITES_API_URL", cfg.Sprites.APIURL))
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
