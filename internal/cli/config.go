package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Profile            string
	Provider           string
	TargetOS           string
	WindowsMode        string
	Desktop            bool
	Browser            bool
	Code               bool
	Network            NetworkMode
	Class              string
	ServerType         string
	ServerTypeExplicit bool
	Coordinator        string
	CoordToken         string
	CoordAdminToken    string
	Access             AccessConfig
	Location           string
	Image              string
	AWSRegion          string
	AWSAMI             string
	AWSSGID            string
	AWSSubnetID        string
	AWSProfile         string
	AWSRootGB          int32
	AWSSSHCIDRs        []string
	AWSMacHostID       string
	AzureSubscription  string
	AzureTenant        string
	AzureClientID      string
	AzureLocation      string
	AzureResourceGroup string
	AzureImage         string
	AzureVNet          string
	AzureSubnet        string
	AzureNSG           string
	AzureSSHCIDRs      []string
	AzureNetwork       string
	GCPProject         string
	gcpProjectExplicit bool
	GCPZone            string
	gcpZoneExplicit    bool
	GCPImage           string
	gcpImageExplicit   bool
	GCPNetwork         string
	gcpNetworkExplicit bool
	GCPSubnet          string
	GCPTags            []string
	gcpTagsExplicit    bool
	GCPSSHCIDRs        []string
	GCPRootGB          int64
	gcpRootGBExplicit  bool
	GCPServiceAccount  string
	SSHUser            string
	SSHKey             string
	SSHPort            string
	SSHFallbackPorts   []string
	ProviderKey        string
	WorkRoot           string
	TTL                time.Duration
	IdleTimeout        time.Duration
	Sync               SyncConfig
	EnvAllow           []string
	Capacity           CapacityConfig
	Actions            ActionsConfig
	Blacksmith         BlacksmithConfig
	Namespace          NamespaceConfig
	Daytona            DaytonaConfig
	E2B                E2BConfig
	Islo               IsloConfig
	Semaphore          SemaphoreConfig
	Sprites            SpritesConfig
	Tailscale          TailscaleConfig
	Static             StaticConfig
	Results            ResultsConfig
	Cache              CacheConfig
	Jobs               map[string]JobConfig
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
		Islo: IsloConfig{
			BaseURL:  "https://api.islo.dev",
			Image:    "docker.io/library/ubuntu:24.04",
			Workdir:  "crabbox",
			VCPUs:    2,
			MemoryMB: 4096,
			DiskGB:   20,
		},
		Sprites: SpritesConfig{
			APIURL:   "https://api.sprites.dev",
			WorkRoot: "/home/sprite/crabbox",
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
	Profile          string                   `yaml:"profile,omitempty"`
	Provider         string                   `yaml:"provider,omitempty"`
	Target           string                   `yaml:"target,omitempty"`
	TargetOS         string                   `yaml:"targetOS,omitempty"`
	Windows          *fileWindowsConfig       `yaml:"windows,omitempty"`
	Desktop          *bool                    `yaml:"desktop,omitempty"`
	Browser          *bool                    `yaml:"browser,omitempty"`
	Code             *bool                    `yaml:"code,omitempty"`
	Network          string                   `yaml:"network,omitempty"`
	Class            string                   `yaml:"class,omitempty"`
	ServerType       string                   `yaml:"serverType,omitempty"`
	Coordinator      string                   `yaml:"coordinator,omitempty"`
	CoordinatorToken string                   `yaml:"coordinatorToken,omitempty"`
	Broker           *fileBrokerConfig        `yaml:"broker,omitempty"`
	Hetzner          *fileHetznerConfig       `yaml:"hetzner,omitempty"`
	AWS              *fileAWSConfig           `yaml:"aws,omitempty"`
	Azure            *fileAzureConfig         `yaml:"azure,omitempty"`
	GCP              *fileGCPConfig           `yaml:"gcp,omitempty"`
	SSH              *fileSSHConfig           `yaml:"ssh,omitempty"`
	Sync             *fileSyncConfig          `yaml:"sync,omitempty"`
	Env              *fileEnvConfig           `yaml:"env,omitempty"`
	Capacity         *fileCapacityConfig      `yaml:"capacity,omitempty"`
	Actions          *fileActionsConfig       `yaml:"actions,omitempty"`
	Blacksmith       *fileBlacksmithConfig    `yaml:"blacksmith,omitempty"`
	Namespace        *fileNamespaceConfig     `yaml:"namespace,omitempty"`
	Daytona          *fileDaytonaConfig       `yaml:"daytona,omitempty"`
	E2B              *fileE2BConfig           `yaml:"e2b,omitempty"`
	Islo             *fileIsloConfig          `yaml:"islo,omitempty"`
	Semaphore        *fileSemaphoreConfig     `yaml:"semaphore,omitempty"`
	Sprites          *fileSpritesConfig       `yaml:"sprites,omitempty"`
	Tailscale        *fileTailscaleConfig     `yaml:"tailscale,omitempty"`
	Static           *fileStaticConfig        `yaml:"static,omitempty"`
	Results          *fileResultsConfig       `yaml:"results,omitempty"`
	Cache            *fileCacheConfig         `yaml:"cache,omitempty"`
	Lease            *fileLeaseConfig         `yaml:"lease,omitempty"`
	Jobs             map[string]fileJobConfig `yaml:"jobs,omitempty"`
	TTL              string                   `yaml:"ttl,omitempty"`
	IdleTimeout      string                   `yaml:"idleTimeout,omitempty"`
	WorkRoot         string                   `yaml:"workRoot,omitempty"`
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
	cfg.AWSRootGB = int32(getenvInt("CRABBOX_AWS_ROOT_GB", int(cfg.AWSRootGB)))
	cfg.AWSMacHostID = getenv("CRABBOX_AWS_MAC_HOST_ID", cfg.AWSMacHostID)
	if cidrs := os.Getenv("CRABBOX_AWS_SSH_CIDRS"); cidrs != "" {
		cfg.AWSSSHCIDRs = splitCommaList(cidrs)
	}
	cfg.AzureSubscription = getenv("CRABBOX_AZURE_SUBSCRIPTION_ID", getenv("AZURE_SUBSCRIPTION_ID", cfg.AzureSubscription))
	cfg.AzureTenant = getenv("CRABBOX_AZURE_TENANT_ID", getenv("AZURE_TENANT_ID", cfg.AzureTenant))
	cfg.AzureClientID = getenv("CRABBOX_AZURE_CLIENT_ID", getenv("AZURE_CLIENT_ID", cfg.AzureClientID))
	cfg.AzureLocation = getenv("CRABBOX_AZURE_LOCATION", cfg.AzureLocation)
	cfg.AzureResourceGroup = getenv("CRABBOX_AZURE_RESOURCE_GROUP", cfg.AzureResourceGroup)
	cfg.AzureImage = getenv("CRABBOX_AZURE_IMAGE", cfg.AzureImage)
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
	cfg.Islo.APIKey = getenv("CRABBOX_ISLO_API_KEY", getenv("ISLO_API_KEY", cfg.Islo.APIKey))
	cfg.Islo.BaseURL = getenv("CRABBOX_ISLO_BASE_URL", getenv("ISLO_BASE_URL", cfg.Islo.BaseURL))
	cfg.Islo.Image = getenv("CRABBOX_ISLO_IMAGE", cfg.Islo.Image)
	cfg.Islo.Workdir = getenv("CRABBOX_ISLO_WORKDIR", cfg.Islo.Workdir)
	cfg.Islo.GatewayProfile = getenv("CRABBOX_ISLO_GATEWAY_PROFILE", cfg.Islo.GatewayProfile)
	cfg.Islo.SnapshotName = getenv("CRABBOX_ISLO_SNAPSHOT_NAME", cfg.Islo.SnapshotName)
	cfg.Islo.VCPUs = getenvInt("CRABBOX_ISLO_VCPUS", cfg.Islo.VCPUs)
	cfg.Islo.MemoryMB = getenvInt("CRABBOX_ISLO_MEMORY_MB", cfg.Islo.MemoryMB)
	cfg.Islo.DiskGB = getenvInt("CRABBOX_ISLO_DISK_GB", cfg.Islo.DiskGB)
	cfg.Semaphore.Host = getenv("CRABBOX_SEMAPHORE_HOST", getenv("SEMAPHORE_HOST", cfg.Semaphore.Host))
	cfg.Semaphore.Token = getenv("CRABBOX_SEMAPHORE_TOKEN", getenv("SEMAPHORE_API_TOKEN", cfg.Semaphore.Token))
	cfg.Semaphore.Project = getenv("CRABBOX_SEMAPHORE_PROJECT", getenv("SEMAPHORE_PROJECT", cfg.Semaphore.Project))
	cfg.Semaphore.Machine = getenv("CRABBOX_SEMAPHORE_MACHINE", cfg.Semaphore.Machine)
	cfg.Semaphore.OSImage = getenv("CRABBOX_SEMAPHORE_OS_IMAGE", cfg.Semaphore.OSImage)
	cfg.Semaphore.IdleTimeout = getenv("CRABBOX_SEMAPHORE_IDLE_TIMEOUT", cfg.Semaphore.IdleTimeout)
	cfg.Sprites.Token = getenv("CRABBOX_SPRITES_TOKEN", getenv("SPRITES_TOKEN", getenv("SPRITE_TOKEN", getenv("SETUP_SPRITE_TOKEN", cfg.Sprites.Token))))
	cfg.Sprites.APIURL = getenv("CRABBOX_SPRITES_API_URL", getenv("SPRITES_API_URL", cfg.Sprites.APIURL))
	cfg.Sprites.WorkRoot = getenv("CRABBOX_SPRITES_WORK_ROOT", cfg.Sprites.WorkRoot)
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
	if isBlacksmithProvider(cfg.Provider) || isStaticProvider(cfg.Provider) || cfg.Provider == "islo" || cfg.Provider == "sprites" {
		return ""
	}
	if cfg.Provider == "namespace-devbox" || cfg.Provider == "namespace" {
		return namespaceDevboxSizeForConfig(cfg)
	}
	if cfg.Provider == "e2b" {
		return blank(cfg.E2B.Template, "base")
	}
	if cfg.Provider == "daytona" {
		return "snapshot"
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
	return serverTypeForClass(cfg.Class)
}

func serverTypeForProviderClass(provider, class string) string {
	if resolved, err := ProviderFor(provider); err == nil {
		provider = resolved.Name()
	}
	if isBlacksmithProvider(provider) || isStaticProvider(provider) || provider == "islo" || provider == "sprites" {
		return ""
	}
	if provider == "namespace-devbox" || provider == "namespace" {
		return namespaceDevboxSizeForClass(class)
	}
	if provider == "e2b" {
		return "base"
	}
	if provider == "daytona" {
		return "snapshot"
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
	return serverTypeForClass(class)
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
		return []string{"mac2.metal"}
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
