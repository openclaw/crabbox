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
	Profile                  string
	Provider                 string
	TargetOS                 string
	WindowsMode              string
	Desktop                  bool
	Browser                  bool
	Code                     bool
	Network                  NetworkMode
	Class                    string
	ServerType               string
	ServerTypeExplicit       bool
	Coordinator              string
	CoordToken               string
	CoordAdminToken          string
	HostID                   string
	Access                   AccessConfig
	Location                 string
	Image                    string
	AWSRegion                string
	AWSAMI                   string
	AWSSnapshot              string
	AWSSGID                  string
	AWSSubnetID              string
	AWSProfile               string
	AWSRootGB                int32
	AWSSSHCIDRs              []string
	AWSMacHostID             string
	AzureSubscription        string
	AzureTenant              string
	AzureClientID            string
	AzureLocation            string
	AzureResourceGroup       string
	AzureImage               string
	AzureSnapshot            string
	AzureOSDisk              string
	AzureOSDiskExplicit      bool
	AzureVNet                string
	AzureSubnet              string
	AzureNSG                 string
	AzureSSHCIDRs            []string
	AzureNetwork             string
	GCPProject               string
	gcpProjectExplicit       bool
	GCPZone                  string
	gcpZoneExplicit          bool
	GCPImage                 string
	gcpImageExplicit         bool
	GCPMachineImage          string
	GCPSnapshot              string
	GCPNetwork               string
	gcpNetworkExplicit       bool
	GCPSubnet                string
	GCPTags                  []string
	gcpTagsExplicit          bool
	GCPSSHCIDRs              []string
	GCPRootGB                int64
	gcpRootGBExplicit        bool
	GCPServiceAccount        string
	Proxmox                  ProxmoxConfig
	Parallels                ParallelsConfig
	parallelsTemplateApplied bool
	SSHUser                  string
	SSHKey                   string
	SSHPort                  string
	SSHFallbackPorts         []string
	ProviderKey              string
	WorkRoot                 string
	TTL                      time.Duration
	IdleTimeout              time.Duration
	Sync                     SyncConfig
	Run                      RunConfig
	EnvAllow                 []string
	Capacity                 CapacityConfig
	Actions                  ActionsConfig
	Blacksmith               BlacksmithConfig
	Namespace                NamespaceConfig
	Daytona                  DaytonaConfig
	E2B                      E2BConfig
	ExeDev                   ExeDevConfig
	Railway                  RailwayConfig
	Runpod                   RunpodConfig
	Islo                     IsloConfig
	Tensorlake               TensorlakeConfig
	Modal                    ModalConfig
	Cloudflare               CloudflareConfig
	Semaphore                SemaphoreConfig
	Sprites                  SpritesConfig
	LocalContainer           LocalContainerConfig
	Tailscale                TailscaleConfig
	Static                   StaticConfig
	Results                  ResultsConfig
	Cache                    CacheConfig
	Profiles                 map[string]ProfileConfig
	Presets                  map[string]PresetConfig
	ProofTemplates           map[string]ProofTemplateConfig
	Jobs                     map[string]JobConfig
}

type SyncConfig struct {
	Excludes    []string
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

type ModalConfig struct {
	App     string
	Image   string
	Workdir string
	Python  string
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
	Runtime      string
	Image        string
	User         string
	WorkRoot     string
	CPUs         int
	Memory       string
	Network      string
	DockerSocket bool
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
}

type CacheConfig struct {
	Pnpm           bool
	Npm            bool
	Docker         bool
	Git            bool
	MaxGB          int
	PurgeOnRelease bool
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
	ServerType     string
	Market         string
	TTL            time.Duration
	IdleTimeout    time.Duration
	Desktop        *bool
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
	applyEnv(&cfg)
	canonicalizeConfigProvider(&cfg)
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

func baseConfig() Config {
	home, _ := os.UserHomeDir()
	sshKey := ""
	if home != "" {
		sshKey = filepath.Join(home, ".ssh", "id_ed25519")
	}

	class := "beast"
	provider := "hetzner"
	return Config{
		Profile:            "default",
		Provider:           provider,
		TargetOS:           "linux",
		WindowsMode:        "normal",
		Network:            NetworkAuto,
		Class:              class,
		ServerType:         "",
		Location:           "fsn1",
		Image:              "ubuntu-24.04",
		AWSRegion:          "eu-west-1",
		AWSRootGB:          400,
		AzureLocation:      "eastus",
		AzureResourceGroup: "crabbox-leases",
		AzureImage:         defaultAzureLinuxImage,
		AzureOSDisk:        AzureOSDiskManaged,
		AzureVNet:          "crabbox-vnet",
		AzureSubnet:        "crabbox-subnet",
		AzureNSG:           "crabbox-nsg",
		GCPZone:            "europe-west2-a",
		GCPImage:           defaultGCPLinuxImage,
		GCPNetwork:         "default",
		GCPTags:            []string{"crabbox-ssh"},
		GCPRootGB:          400,
		SSHUser:            "crabbox",
		SSHKey:             sshKey,
		SSHPort:            "2222",
		SSHFallbackPorts:   []string{"22"},
		ProviderKey:        "crabbox-steipete",
		WorkRoot:           defaultPOSIXWorkRoot,
		TTL:                90 * time.Minute,
		IdleTimeout:        30 * time.Minute,
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
		Namespace: NamespaceConfig{
			Image:               "builtin:base",
			WorkRoot:            "/workspaces/crabbox",
			AutoStopIdleTimeout: 30 * time.Minute,
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
			Image:    "docker.io/library/ubuntu:24.04",
			Workdir:  "crabbox",
			VCPUs:    2,
			MemoryMB: 4096,
			DiskGB:   20,
		},
		Tensorlake: TensorlakeConfig{
			APIURL:   "https://api.tensorlake.ai",
			CLIPath:  "tensorlake",
			Workdir:  "/workspace/crabbox",
			CPUs:     1.0,
			MemoryMB: 1024,
			DiskMB:   10240,
		},
		Modal: ModalConfig{
			App:     "crabbox",
			Image:   "python:3.13-slim",
			Workdir: "/workspace/crabbox",
			Python:  "python3",
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
			Image:   "debian:bookworm",
			User:    "crabbox",
			Network: "bridge",
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
	Profile          string                             `yaml:"profile,omitempty"`
	Provider         string                             `yaml:"provider,omitempty"`
	Target           string                             `yaml:"target,omitempty"`
	TargetOS         string                             `yaml:"targetOS,omitempty"`
	Windows          *fileWindowsConfig                 `yaml:"windows,omitempty"`
	Desktop          *bool                              `yaml:"desktop,omitempty"`
	Browser          *bool                              `yaml:"browser,omitempty"`
	Code             *bool                              `yaml:"code,omitempty"`
	Network          string                             `yaml:"network,omitempty"`
	Class            string                             `yaml:"class,omitempty"`
	ServerType       string                             `yaml:"serverType,omitempty"`
	Coordinator      string                             `yaml:"coordinator,omitempty"`
	CoordinatorToken string                             `yaml:"coordinatorToken,omitempty"`
	HostID           string                             `yaml:"hostId,omitempty"`
	Broker           *fileBrokerConfig                  `yaml:"broker,omitempty"`
	Hetzner          *fileHetznerConfig                 `yaml:"hetzner,omitempty"`
	AWS              *fileAWSConfig                     `yaml:"aws,omitempty"`
	Azure            *fileAzureConfig                   `yaml:"azure,omitempty"`
	GCP              *fileGCPConfig                     `yaml:"gcp,omitempty"`
	Proxmox          *fileProxmoxConfig                 `yaml:"proxmox,omitempty"`
	Parallels        *fileParallelsConfig               `yaml:"parallels,omitempty"`
	SSH              *fileSSHConfig                     `yaml:"ssh,omitempty"`
	Sync             *fileSyncConfig                    `yaml:"sync,omitempty"`
	Run              *fileRunConfig                     `yaml:"run,omitempty"`
	Env              *fileEnvConfig                     `yaml:"env,omitempty"`
	Capacity         *fileCapacityConfig                `yaml:"capacity,omitempty"`
	Actions          *fileActionsConfig                 `yaml:"actions,omitempty"`
	Blacksmith       *fileBlacksmithConfig              `yaml:"blacksmith,omitempty"`
	Namespace        *fileNamespaceConfig               `yaml:"namespace,omitempty"`
	Daytona          *fileDaytonaConfig                 `yaml:"daytona,omitempty"`
	E2B              *fileE2BConfig                     `yaml:"e2b,omitempty"`
	ExeDev           *fileExeDevConfig                  `yaml:"exeDev,omitempty"`
	Railway          *fileRailwayConfig                 `yaml:"railway,omitempty"`
	Runpod           *fileRunpodConfig                  `yaml:"runpod,omitempty"`
	Islo             *fileIsloConfig                    `yaml:"islo,omitempty"`
	Tensorlake       *fileTensorlakeConfig              `yaml:"tensorlake,omitempty"`
	Modal            *fileModalConfig                   `yaml:"modal,omitempty"`
	Cloudflare       *fileCloudflareConfig              `yaml:"cloudflare,omitempty"`
	Semaphore        *fileSemaphoreConfig               `yaml:"semaphore,omitempty"`
	Sprites          *fileSpritesConfig                 `yaml:"sprites,omitempty"`
	LocalContainer   *fileLocalContainerConfig          `yaml:"localContainer,omitempty"`
	Tailscale        *fileTailscaleConfig               `yaml:"tailscale,omitempty"`
	Static           *fileStaticConfig                  `yaml:"static,omitempty"`
	Results          *fileResultsConfig                 `yaml:"results,omitempty"`
	Cache            *fileCacheConfig                   `yaml:"cache,omitempty"`
	Lease            *fileLeaseConfig                   `yaml:"lease,omitempty"`
	Profiles         map[string]fileProfileConfig       `yaml:"profiles,omitempty"`
	Presets          map[string]filePresetConfig        `yaml:"presets,omitempty"`
	ProofTemplates   map[string]fileProofTemplateConfig `yaml:"proofTemplates,omitempty"`
	Jobs             map[string]fileJobConfig           `yaml:"jobs,omitempty"`
	TTL              string                             `yaml:"ttl,omitempty"`
	IdleTimeout      string                             `yaml:"idleTimeout,omitempty"`
	WorkRoot         string                             `yaml:"workRoot,omitempty"`
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

type fileModalConfig struct {
	App     string `yaml:"app,omitempty"`
	Image   string `yaml:"image,omitempty"`
	Workdir string `yaml:"workdir,omitempty"`
	Python  string `yaml:"python,omitempty"`
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
}

type fileCacheConfig struct {
	Pnpm           *bool `yaml:"pnpm,omitempty"`
	Npm            *bool `yaml:"npm,omitempty"`
	Docker         *bool `yaml:"docker,omitempty"`
	Git            *bool `yaml:"git,omitempty"`
	MaxGB          int   `yaml:"maxGB,omitempty"`
	PurgeOnRelease *bool `yaml:"purgeOnRelease,omitempty"`
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
	ServerType     string                `yaml:"serverType,omitempty"`
	Type           string                `yaml:"type,omitempty"`
	Capacity       *fileCapacityConfig   `yaml:"capacity,omitempty"`
	Market         string                `yaml:"market,omitempty"`
	TTL            string                `yaml:"ttl,omitempty"`
	IdleTimeout    string                `yaml:"idleTimeout,omitempty"`
	Desktop        *bool                 `yaml:"desktop,omitempty"`
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
	applyFileConfig(cfg, file)
	return nil
}

func applyFileConfig(cfg *Config, file fileConfig) {
	if file.Profile != "" {
		cfg.Profile = file.Profile
	}
	if file.Provider != "" {
		cfg.Provider = file.Provider
	}
	if file.Target != "" {
		cfg.TargetOS = file.Target
	}
	if file.TargetOS != "" {
		cfg.TargetOS = file.TargetOS
	}
	if file.Windows != nil && file.Windows.Mode != "" {
		cfg.WindowsMode = file.Windows.Mode
	}
	if file.Desktop != nil {
		cfg.Desktop = *file.Desktop
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
	if file.Islo != nil {
		if file.Islo.BaseURL != "" {
			cfg.Islo.BaseURL = file.Islo.BaseURL
		}
		if file.Islo.Image != "" {
			cfg.Islo.Image = file.Islo.Image
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
		}
		if file.LocalContainer.Image != "" {
			cfg.LocalContainer.Image = file.LocalContainer.Image
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
	if file.Results != nil && len(file.Results.JUnit) > 0 {
		cfg.Results.JUnit = appendUniqueStrings(nil, file.Results.JUnit...)
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

func applyEnv(cfg *Config) {
	cfg.Profile = getenv("CRABBOX_PROFILE", cfg.Profile)
	cfg.Provider = getenv("CRABBOX_PROVIDER", cfg.Provider)
	cfg.TargetOS = getenv("CRABBOX_TARGET", getenv("CRABBOX_TARGET_OS", cfg.TargetOS))
	cfg.WindowsMode = getenv("CRABBOX_WINDOWS_MODE", cfg.WindowsMode)
	if value, ok := getenvBool("CRABBOX_DESKTOP"); ok {
		cfg.Desktop = value
	}
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
	cfg.Image = getenv("CRABBOX_HETZNER_IMAGE", cfg.Image)
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
	cfg.AzureLocation = getenv("CRABBOX_AZURE_LOCATION", cfg.AzureLocation)
	cfg.AzureResourceGroup = getenv("CRABBOX_AZURE_RESOURCE_GROUP", cfg.AzureResourceGroup)
	cfg.AzureImage = getenv("CRABBOX_AZURE_IMAGE", cfg.AzureImage)
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
	cfg.Islo.APIKey = getenv("CRABBOX_ISLO_API_KEY", getenv("ISLO_API_KEY", cfg.Islo.APIKey))
	cfg.Islo.BaseURL = getenv("CRABBOX_ISLO_BASE_URL", getenv("ISLO_BASE_URL", cfg.Islo.BaseURL))
	cfg.Islo.Image = getenv("CRABBOX_ISLO_IMAGE", cfg.Islo.Image)
	cfg.Islo.Workdir = getenv("CRABBOX_ISLO_WORKDIR", cfg.Islo.Workdir)
	cfg.Islo.GatewayProfile = getenv("CRABBOX_ISLO_GATEWAY_PROFILE", cfg.Islo.GatewayProfile)
	cfg.Islo.SnapshotName = getenv("CRABBOX_ISLO_SNAPSHOT_NAME", cfg.Islo.SnapshotName)
	cfg.Islo.VCPUs = getenvInt("CRABBOX_ISLO_VCPUS", cfg.Islo.VCPUs)
	cfg.Islo.MemoryMB = getenvInt("CRABBOX_ISLO_MEMORY_MB", cfg.Islo.MemoryMB)
	cfg.Islo.DiskGB = getenvInt("CRABBOX_ISLO_DISK_GB", cfg.Islo.DiskGB)
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
	cfg.Modal.App = getenv("CRABBOX_MODAL_APP", cfg.Modal.App)
	cfg.Modal.Image = getenv("CRABBOX_MODAL_IMAGE", cfg.Modal.Image)
	cfg.Modal.Workdir = getenv("CRABBOX_MODAL_WORKDIR", cfg.Modal.Workdir)
	cfg.Modal.Python = getenv("CRABBOX_MODAL_PYTHON", cfg.Modal.Python)
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
	cfg.LocalContainer.Runtime = getenv("CRABBOX_LOCAL_CONTAINER_RUNTIME", cfg.LocalContainer.Runtime)
	cfg.LocalContainer.Image = getenv("CRABBOX_LOCAL_CONTAINER_IMAGE", cfg.LocalContainer.Image)
	cfg.LocalContainer.User = getenv("CRABBOX_LOCAL_CONTAINER_USER", cfg.LocalContainer.User)
	cfg.LocalContainer.WorkRoot = getenv("CRABBOX_LOCAL_CONTAINER_WORK_ROOT", cfg.LocalContainer.WorkRoot)
	cfg.LocalContainer.CPUs = getenvInt("CRABBOX_LOCAL_CONTAINER_CPUS", cfg.LocalContainer.CPUs)
	cfg.LocalContainer.Memory = getenv("CRABBOX_LOCAL_CONTAINER_MEMORY", cfg.LocalContainer.Memory)
	cfg.LocalContainer.Network = getenv("CRABBOX_LOCAL_CONTAINER_NETWORK", cfg.LocalContainer.Network)
	if value, ok := getenvBool("CRABBOX_LOCAL_CONTAINER_DOCKER_SOCKET"); ok {
		cfg.LocalContainer.DockerSocket = value
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
	}
	if isBlacksmithProvider(cfg.Provider) || isStaticProvider(cfg.Provider) || cfg.Provider == "islo" || cfg.Provider == "sprites" || cfg.Provider == "local-container" {
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
	if cfg.Provider == "parallels" {
		return parallelsServerTypeForConfig(cfg)
	}
	return serverTypeForClass(cfg.Class)
}

func serverTypeForProviderClass(provider, class string) string {
	if resolved, err := ProviderFor(provider); err == nil {
		provider = resolved.Name()
	}
	if isBlacksmithProvider(provider) || isStaticProvider(provider) || provider == "islo" || provider == "sprites" || provider == "local-container" {
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
	if provider == "parallels" {
		return "template"
	}
	return serverTypeForClass(class)
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
	return awsInstanceTypeCandidatesForTargetModeClass(cfg.TargetOS, cfg.WindowsMode, cfg.Class)
}

func awsInstanceTypeCandidatesForTargetModeClass(target, windowsMode, class string) []string {
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
		return awsInstanceTypeCandidatesForClass(class)
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
