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
	Tailscale          TailscaleConfig
	Static             StaticConfig
	Results            ResultsConfig
	Cache              CacheConfig
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

func baseConfig() Config {
	home, _ := os.UserHomeDir()
	sshKey := ""
	if home != "" {
		sshKey = filepath.Join(home, ".ssh", "id_ed25519")
	}

	class := "beast"
	provider := "hetzner"
	return Config{
		Profile:          "default",
		Provider:         provider,
		TargetOS:         "linux",
		WindowsMode:      "normal",
		Network:          NetworkAuto,
		Class:            class,
		ServerType:       "",
		Location:         "fsn1",
		Image:            "ubuntu-24.04",
		AWSRegion:        "eu-west-1",
		AWSRootGB:        400,
		SSHUser:          "crabbox",
		SSHKey:           sshKey,
		SSHPort:          "2222",
		SSHFallbackPorts: []string{"22"},
		ProviderKey:      "crabbox-steipete",
		WorkRoot:         "/work/crabbox",
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
		},
		Actions: ActionsConfig{
			RunnerVersion: "latest",
			Ephemeral:     true,
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
	Profile          string                `yaml:"profile,omitempty"`
	Provider         string                `yaml:"provider,omitempty"`
	Target           string                `yaml:"target,omitempty"`
	TargetOS         string                `yaml:"targetOS,omitempty"`
	Windows          *fileWindowsConfig    `yaml:"windows,omitempty"`
	Desktop          *bool                 `yaml:"desktop,omitempty"`
	Browser          *bool                 `yaml:"browser,omitempty"`
	Network          string                `yaml:"network,omitempty"`
	Class            string                `yaml:"class,omitempty"`
	ServerType       string                `yaml:"serverType,omitempty"`
	Coordinator      string                `yaml:"coordinator,omitempty"`
	CoordinatorToken string                `yaml:"coordinatorToken,omitempty"`
	Broker           *fileBrokerConfig     `yaml:"broker,omitempty"`
	Hetzner          *fileHetznerConfig    `yaml:"hetzner,omitempty"`
	AWS              *fileAWSConfig        `yaml:"aws,omitempty"`
	SSH              *fileSSHConfig        `yaml:"ssh,omitempty"`
	Sync             *fileSyncConfig       `yaml:"sync,omitempty"`
	Env              *fileEnvConfig        `yaml:"env,omitempty"`
	Capacity         *fileCapacityConfig   `yaml:"capacity,omitempty"`
	Actions          *fileActionsConfig    `yaml:"actions,omitempty"`
	Blacksmith       *fileBlacksmithConfig `yaml:"blacksmith,omitempty"`
	Tailscale        *fileTailscaleConfig  `yaml:"tailscale,omitempty"`
	Static           *fileStaticConfig     `yaml:"static,omitempty"`
	Results          *fileResultsConfig    `yaml:"results,omitempty"`
	Cache            *fileCacheConfig      `yaml:"cache,omitempty"`
	Lease            *fileLeaseConfig      `yaml:"lease,omitempty"`
	TTL              string                `yaml:"ttl,omitempty"`
	IdleTimeout      string                `yaml:"idleTimeout,omitempty"`
	WorkRoot         string                `yaml:"workRoot,omitempty"`
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

type fileTailscaleConfig struct {
	Enabled          *bool    `yaml:"enabled,omitempty"`
	Network          string   `yaml:"network,omitempty"`
	Tags             []string `yaml:"tags,omitempty"`
	HostnameTemplate string   `yaml:"hostnameTemplate,omitempty"`
	AuthKeyEnv       string   `yaml:"authKeyEnv,omitempty"`
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
	cfg.Actions.Workflow = getenv("CRABBOX_ACTIONS_WORKFLOW", cfg.Actions.Workflow)
	cfg.Actions.Job = getenv("CRABBOX_ACTIONS_JOB", cfg.Actions.Job)
	cfg.Actions.Ref = getenv("CRABBOX_ACTIONS_REF", cfg.Actions.Ref)
	cfg.Actions.Repo = getenv("CRABBOX_ACTIONS_REPO", cfg.Actions.Repo)
	cfg.Actions.RunnerVersion = getenv("CRABBOX_ACTIONS_RUNNER_VERSION", cfg.Actions.RunnerVersion)
	cfg.Blacksmith.Org = getenv("CRABBOX_BLACKSMITH_ORG", cfg.Blacksmith.Org)
	cfg.Blacksmith.Workflow = getenv("CRABBOX_BLACKSMITH_WORKFLOW", cfg.Blacksmith.Workflow)
	cfg.Blacksmith.Job = getenv("CRABBOX_BLACKSMITH_JOB", cfg.Blacksmith.Job)
	cfg.Blacksmith.Ref = getenv("CRABBOX_BLACKSMITH_REF", cfg.Blacksmith.Ref)
	if value, ok := getenvBool("CRABBOX_TAILSCALE"); ok {
		cfg.Tailscale.Enabled = value
	}
	if tags := os.Getenv("CRABBOX_TAILSCALE_TAGS"); tags != "" {
		cfg.Tailscale.Tags = normalizeTailscaleTags(splitCommaList(tags))
	}
	cfg.Tailscale.HostnameTemplate = getenv("CRABBOX_TAILSCALE_HOSTNAME_TEMPLATE", cfg.Tailscale.HostnameTemplate)
	cfg.Tailscale.AuthKeyEnv = getenv("CRABBOX_TAILSCALE_AUTH_KEY_ENV", cfg.Tailscale.AuthKeyEnv)
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
	if isBlacksmithProvider(cfg.Provider) || isStaticProvider(cfg.Provider) {
		return ""
	}
	if cfg.Provider == "aws" {
		return awsInstanceTypeCandidatesForTargetClass(cfg.TargetOS, cfg.Class)[0]
	}
	return serverTypeForClass(cfg.Class)
}

func serverTypeForProviderClass(provider, class string) string {
	if isBlacksmithProvider(provider) || isStaticProvider(provider) {
		return ""
	}
	if provider == "aws" {
		return awsInstanceTypeCandidatesForClass(class)[0]
	}
	return serverTypeForClass(class)
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
	switch target {
	case targetMacOS:
		return []string{"mac2.metal"}
	case targetWindows:
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
