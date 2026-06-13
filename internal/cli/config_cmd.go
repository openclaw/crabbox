package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

func (a App) configShow(args []string) error {
	fs := newFlagSet("config show", a.Stderr)
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if err := validateProviderConfig(cfg); err != nil {
		return err
	}
	cfg = effectiveConfigForShow(cfg)
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(configShowView(cfg))
	}
	writeConfigShowText(a.Stdout, cfg)
	return nil
}

func effectiveConfigForShow(cfg Config) Config {
	cfg.Hostinger.WorkRoot = EffectiveHostingerWorkRoot(cfg)
	if cfg.Provider == "digitalocean" || cfg.Provider == "linode" {
		base := baseConfig()
		if !IsSSHUserExplicit(&cfg) && (cfg.SSHUser == "" || cfg.SSHUser == base.SSHUser) {
			cfg.SSHUser = "root"
		}
		if !IsSSHPortExplicit(&cfg) && (cfg.SSHPort == "" || cfg.SSHPort == base.SSHPort) {
			cfg.SSHPort = "22"
		}
		cfg.SSHFallbackPorts = nil
	}
	if cfg.Provider == "hostinger" {
		cfg.WorkRoot = cfg.Hostinger.WorkRoot
		cfg.SSHUser = cfg.Hostinger.User
		cfg.SSHPort = "22"
		cfg.SSHFallbackPorts = nil
	}
	return cfg
}

func configShowView(cfg Config) map[string]any {
	return map[string]any{
		"profile":            cfg.Profile,
		"provider":           cfg.Provider,
		"target":             cfg.TargetOS,
		"architecture":       effectiveArchitectureForConfig(cfg),
		"os":                 cfg.OSImage,
		"windowsMode":        cfg.WindowsMode,
		"class":              cfg.Class,
		"serverType":         cfg.ServerType,
		"serverTypeExplicit": cfg.ServerTypeExplicit,
		"coordinator":        cfg.Coordinator,
		"brokerMode":         cfg.BrokerMode,
		"brokerAutoWebVNC":   cfg.BrokerAutoWebVNC,
		"brokerAuth":         tokenState(cfg.CoordToken),
		"brokerAdminAuth":    tokenState(cfg.CoordAdminToken),
		"accessAuth":         accessAuthState(cfg.Access),
		"sshKey":             cfg.SSHKey,
		"sshUser":            cfg.SSHUser,
		"sshPort":            cfg.SSHPort,
		"sshFallbackPorts":   cfg.SSHFallbackPorts,
		"workRoot":           cfg.WorkRoot,
		"sync": map[string]any{
			"exclude":     configuredExcludes(cfg),
			"include":     syncIncludes(cfg),
			"delete":      cfg.Sync.Delete,
			"checksum":    cfg.Sync.Checksum,
			"gitSeed":     cfg.Sync.GitSeed,
			"fingerprint": cfg.Sync.Fingerprint,
			"baseRef":     cfg.Sync.BaseRef,
			"timeout":     cfg.Sync.Timeout.String(),
			"warnFiles":   cfg.Sync.WarnFiles,
			"warnBytes":   cfg.Sync.WarnBytes,
			"failFiles":   cfg.Sync.FailFiles,
			"failBytes":   cfg.Sync.FailBytes,
			"allowLarge":  cfg.Sync.AllowLarge,
		},
		"env": map[string]any{
			"allow": cfg.EnvAllow,
		},
		"run": map[string]any{
			"preflightTools": cfg.Run.PreflightTools,
		},
		"capacity": map[string]any{
			"market":            cfg.Capacity.Market,
			"strategy":          cfg.Capacity.Strategy,
			"fallback":          cfg.Capacity.Fallback,
			"regions":           cfg.Capacity.Regions,
			"availabilityZones": cfg.Capacity.AvailabilityZones,
			"hints":             cfg.Capacity.Hints,
		},
		"actions": map[string]any{
			"repo":          cfg.Actions.Repo,
			"workflow":      cfg.Actions.Workflow,
			"job":           cfg.Actions.Job,
			"ref":           cfg.Actions.Ref,
			"runnerLabels":  cfg.Actions.RunnerLabels,
			"runnerVersion": cfg.Actions.RunnerVersion,
			"ephemeral":     cfg.Actions.Ephemeral,
		},
		"azure": map[string]any{
			"location":      cfg.AzureLocation,
			"resourceGroup": cfg.AzureResourceGroup,
			"image":         cfg.AzureImage,
			"osDisk":        cfg.AzureOSDisk,
			"network":       cfg.AzureNetwork,
			"sshCIDRs":      cfg.AzureSSHCIDRs,
		},
		"digitalocean": map[string]any{
			"region":   cfg.DigitalOcean.Region,
			"image":    cfg.DigitalOcean.Image,
			"vpc":      cfg.DigitalOcean.VPCUUID,
			"sshCIDRs": cfg.DigitalOcean.SSHCIDRs,
		},
		"linode": map[string]any{
			"region":   cfg.Linode.Region,
			"image":    cfg.Linode.Image,
			"type":     cfg.Linode.Type,
			"firewall": cfg.Linode.FirewallID,
			"sshCIDRs": cfg.Linode.SSHCIDRs,
		},
		"hostinger": map[string]any{
			"apiUrl":          cfg.Hostinger.APIURL,
			"auth":            tokenState(cfg.Hostinger.APIToken),
			"itemId":          cfg.Hostinger.ItemID,
			"paymentMethodId": cfg.Hostinger.PaymentMethodID,
			"templateId":      cfg.Hostinger.TemplateID,
			"dataCenterId":    cfg.Hostinger.DataCenterID,
			"hostnamePrefix":  cfg.Hostinger.HostnamePrefix,
			"user":            cfg.Hostinger.User,
			"workRoot":        cfg.Hostinger.WorkRoot,
			"allowPurchase":   cfg.Hostinger.AllowPurchase,
			"releaseAction":   cfg.Hostinger.ReleaseAction,
		},
		"azureDynamicSessions": map[string]any{
			"endpoint":        cfg.AzureDynamicSessions.Endpoint,
			"unsupportedPool": cfg.AzureDynamicSessions.Pool,
			"apiVersion":      cfg.AzureDynamicSessions.APIVersion,
			"workdir":         cfg.AzureDynamicSessions.Workdir,
			"timeoutSecs":     cfg.AzureDynamicSessions.TimeoutSecs,
		},
		"blacksmith": map[string]any{
			"org":         cfg.Blacksmith.Org,
			"workflow":    cfg.Blacksmith.Workflow,
			"job":         cfg.Blacksmith.Job,
			"ref":         cfg.Blacksmith.Ref,
			"idleTimeout": cfg.Blacksmith.IdleTimeout.String(),
			"debug":       cfg.Blacksmith.Debug,
		},
		"namespace": map[string]any{
			"image":               cfg.Namespace.Image,
			"size":                cfg.Namespace.Size,
			"repository":          cfg.Namespace.Repository,
			"site":                cfg.Namespace.Site,
			"volumeSizeGB":        cfg.Namespace.VolumeSizeGB,
			"autoStopIdleTimeout": cfg.Namespace.AutoStopIdleTimeout.String(),
			"workRoot":            cfg.Namespace.WorkRoot,
			"deleteOnRelease":     cfg.Namespace.DeleteOnRelease,
		},
		"morph": map[string]any{
			"apiUrl":          cfg.Morph.APIURL,
			"auth":            tokenState(cfg.Morph.APIKey),
			"snapshot":        cfg.Morph.Snapshot,
			"sshGatewayHost":  cfg.Morph.SSHGatewayHost,
			"workRoot":        cfg.Morph.WorkRoot,
			"deleteOnRelease": cfg.Morph.DeleteOnRelease,
			"wakeOnSSH":       cfg.Morph.WakeOnSSH,
		},
		"e2b": map[string]any{
			"apiUrl":   cfg.E2B.APIURL,
			"domain":   cfg.E2B.Domain,
			"template": cfg.E2B.Template,
			"workdir":  cfg.E2B.Workdir,
			"user":     cfg.E2B.User,
		},
		"cloudflare": map[string]any{
			"apiUrl":  cfg.Cloudflare.APIURL,
			"auth":    tokenState(cfg.Cloudflare.Token),
			"workdir": cfg.Cloudflare.Workdir,
		},
		"upstashBox": map[string]any{
			"baseUrl":   cfg.UpstashBox.BaseURL,
			"auth":      tokenState(cfg.UpstashBox.APIKey),
			"runtime":   cfg.UpstashBox.Runtime,
			"size":      cfg.UpstashBox.Size,
			"workdir":   cfg.UpstashBox.Workdir,
			"keepAlive": cfg.UpstashBox.KeepAlive,
		},
		"smolvm": map[string]any{
			"baseUrl":  cfg.Smolvm.BaseURL,
			"auth":     tokenState(cfg.Smolvm.APIKey),
			"image":    cfg.Smolvm.Image,
			"workdir":  cfg.Smolvm.Workdir,
			"cpus":     cfg.Smolvm.CPUs,
			"memoryMB": cfg.Smolvm.MemoryMB,
			"network":  cfg.Smolvm.Network,
			"keep":     cfg.Smolvm.Keep,
		},
		"asciiBox": map[string]any{
			"baseUrl": cfg.AsciiBox.BaseURL,
			"auth":    tokenState(cfg.AsciiBox.APIKey),
			"cliPath": cfg.AsciiBox.CLIPath,
			"workdir": cfg.AsciiBox.Workdir,
		},
		"appleContainer": map[string]any{
			"cliPath":  cfg.AppleContainer.CLIPath,
			"image":    cfg.AppleContainer.Image,
			"user":     cfg.AppleContainer.User,
			"workRoot": cfg.AppleContainer.WorkRoot,
			"cpus":     cfg.AppleContainer.CPUs,
			"memory":   cfg.AppleContainer.Memory,
		},
		"mxc": map[string]any{
			"cliPath":           cfg.MXC.CLIPath,
			"version":           cfg.MXC.Version,
			"containment":       cfg.MXC.Containment,
			"network":           cfg.MXC.Network,
			"readOnlyPaths":     cfg.MXC.ReadOnlyPaths,
			"readWritePaths":    cfg.MXC.ReadWritePaths,
			"allowedHosts":      cfg.MXC.AllowedHosts,
			"blockedHosts":      cfg.MXC.BlockedHosts,
			"allowDaclMutation": cfg.MXC.AllowDACLMutation,
			"allowWindowsUI":    cfg.MXC.AllowWindowsUI,
			"experimental":      cfg.MXC.Experimental,
		},
		"dockerSandbox": map[string]any{
			"cliPath":         cfg.DockerSandbox.CLIPath,
			"agent":           cfg.DockerSandbox.Agent,
			"template":        cfg.DockerSandbox.Template,
			"cpus":            cfg.DockerSandbox.CPUs,
			"memory":          cfg.DockerSandbox.Memory,
			"clone":           cfg.DockerSandbox.Clone,
			"workdir":         cfg.DockerSandbox.Workdir,
			"extraWorkspaces": cfg.DockerSandbox.ExtraWorkspaces,
			"mcp":             cfg.DockerSandbox.MCP,
			"kit":             cfg.DockerSandbox.Kit,
		},
		"multipass": map[string]any{
			"cliPath":       cfg.Multipass.CLIPath,
			"image":         cfg.Multipass.Image,
			"user":          cfg.Multipass.User,
			"workRoot":      cfg.Multipass.WorkRoot,
			"cpus":          cfg.Multipass.CPUs,
			"memory":        cfg.Multipass.Memory,
			"disk":          cfg.Multipass.Disk,
			"launchTimeout": cfg.Multipass.LaunchTimeout.String(),
		},
		"tart": map[string]any{
			"image":    cfg.Tart.Image,
			"user":     cfg.Tart.User,
			"workRoot": cfg.Tart.WorkRoot,
			"cpus":     cfg.Tart.CPUs,
			"memory":   cfg.Tart.Memory,
			"disk":     cfg.Tart.Disk,
		},
		"static": map[string]any{
			"id":       cfg.Static.ID,
			"name":     cfg.Static.Name,
			"host":     cfg.Static.Host,
			"user":     cfg.Static.User,
			"port":     cfg.Static.Port,
			"workRoot": cfg.Static.WorkRoot,
		},
		"results": map[string]any{
			"junit": cfg.Results.JUnit,
			"auto":  cfg.Results.Auto,
		},
		"cache": map[string]any{
			"pnpm":           cfg.Cache.Pnpm,
			"npm":            cfg.Cache.Npm,
			"docker":         cfg.Cache.Docker,
			"git":            cfg.Cache.Git,
			"maxGB":          cfg.Cache.MaxGB,
			"purgeOnRelease": cfg.Cache.PurgeOnRelease,
			"volumes":        cfg.Cache.Volumes,
		},
		"jobs": jobConfigViews(cfg.Jobs),
		"hetzner": map[string]any{
			"location": cfg.Location,
			"image":    cfg.Image,
			"sshKey":   cfg.ProviderKey,
		},
		"aws": map[string]any{
			"region":          cfg.AWSRegion,
			"ami":             cfg.AWSAMI,
			"securityGroupId": cfg.AWSSGID,
			"subnetId":        cfg.AWSSubnetID,
			"instanceProfile": cfg.AWSProfile,
			"rootGB":          cfg.AWSRootGB,
			"sshCIDRs":        cfg.AWSSSHCIDRs,
		},
		"gcp": map[string]any{
			"project":        cfg.GCPProject,
			"zone":           cfg.GCPZone,
			"image":          cfg.GCPImage,
			"network":        cfg.GCPNetwork,
			"subnet":         cfg.GCPSubnet,
			"tags":           cfg.GCPTags,
			"rootGB":         cfg.GCPRootGB,
			"sshCIDRs":       cfg.GCPSSHCIDRs,
			"serviceAccount": cfg.GCPServiceAccount,
		},
		"proxmox": map[string]any{
			"apiUrl":      cfg.Proxmox.APIURL,
			"auth":        tokenState(cfg.Proxmox.TokenSecret),
			"tokenId":     cfg.Proxmox.TokenID,
			"node":        cfg.Proxmox.Node,
			"templateId":  cfg.Proxmox.TemplateID,
			"storage":     cfg.Proxmox.Storage,
			"pool":        cfg.Proxmox.Pool,
			"bridge":      cfg.Proxmox.Bridge,
			"user":        cfg.Proxmox.User,
			"workRoot":    cfg.Proxmox.WorkRoot,
			"fullClone":   cfg.Proxmox.FullClone,
			"insecureTLS": cfg.Proxmox.InsecureTLS,
		},
		"xcpNg": map[string]any{
			"apiUrl":       redactedConfigURL(cfg.XCPNg.APIURL),
			"username":     cfg.XCPNg.Username,
			"auth":         tokenState(cfg.XCPNg.Password),
			"template":     cfg.XCPNg.Template,
			"templateUuid": cfg.XCPNg.TemplateUUID,
			"sr":           cfg.XCPNg.SR,
			"srUuid":       cfg.XCPNg.SRUUID,
			"network":      cfg.XCPNg.Network,
			"networkUuid":  cfg.XCPNg.NetworkUUID,
			"host":         cfg.XCPNg.Host,
			"user":         cfg.XCPNg.User,
			"workRoot":     cfg.XCPNg.WorkRoot,
			"insecureTLS":  cfg.XCPNg.InsecureTLS,
		},
		"parallels": map[string]any{
			"template":         cfg.Parallels.Template,
			"source":           cfg.Parallels.Source,
			"sourceId":         cfg.Parallels.SourceID,
			"sourceSnapshot":   cfg.Parallels.SourceSnapshot,
			"sourceSnapshotId": cfg.Parallels.SourceSnapshotID,
			"cloneMode":        cfg.Parallels.CloneMode,
			"host":             cfg.Parallels.Host,
			"hostUser":         cfg.Parallels.HostUser,
			"hostKey":          cfg.Parallels.HostKey,
			"vmRoot":           cfg.Parallels.VMRoot,
			"user":             cfg.Parallels.User,
			"workRoot":         cfg.Parallels.WorkRoot,
			"startupTimeout":   cfg.Parallels.StartupTimeout.String(),
			"templates":        cfg.Parallels.Templates,
			"hosts":            cfg.Parallels.Hosts,
		},
	}
}

func writeConfigShowText(w io.Writer, cfg Config) {
	fmt.Fprintf(w, "config=%s\n", userConfigPath())
	fmt.Fprintf(w, "provider=%s target=%s arch=%s os=%s windows_mode=%s class=%s type=%s profile=%s\n", cfg.Provider, cfg.TargetOS, effectiveArchitectureForConfig(cfg), cfg.OSImage, cfg.WindowsMode, cfg.Class, cfg.ServerType, cfg.Profile)
	fmt.Fprintf(w, "broker=%s mode=%s auto_webvnc=%t auth=%s admin_auth=%s\n", blank(cfg.Coordinator, "-"), cfg.BrokerMode, cfg.BrokerAutoWebVNC, tokenState(cfg.CoordToken), tokenState(cfg.CoordAdminToken))
	fmt.Fprintf(w, "access_auth=%s\n", accessAuthState(cfg.Access))
	fmt.Fprintf(w, "ssh=%s@<host>:%s fallback_ports=%s key=%s\n", cfg.SSHUser, cfg.SSHPort, blank(strings.Join(cfg.SSHFallbackPorts, ","), "-"), cfg.SSHKey)
	fmt.Fprintf(w, "sync delete=%t checksum=%t git_seed=%t fingerprint=%t base_ref=%s excludes=%d includes=%d timeout=%s\n", cfg.Sync.Delete, cfg.Sync.Checksum, cfg.Sync.GitSeed, cfg.Sync.Fingerprint, blank(cfg.Sync.BaseRef, "-"), len(configuredExcludes(cfg)), len(syncIncludes(cfg)), cfg.Sync.Timeout)
	fmt.Fprintf(w, "env allow=%s\n", strings.Join(cfg.EnvAllow, ","))
	fmt.Fprintf(w, "run preflight_tools=%s\n", blank(strings.Join(cfg.Run.PreflightTools, ","), "-"))
	fmt.Fprintf(w, "capacity market=%s strategy=%s fallback=%s regions=%s hints=%t\n", cfg.Capacity.Market, cfg.Capacity.Strategy, cfg.Capacity.Fallback, blank(strings.Join(cfg.Capacity.Regions, ","), "-"), cfg.Capacity.Hints)
	fmt.Fprintf(w, "actions repo=%s workflow=%s job=%s ref=%s runner_version=%s ephemeral=%t labels=%s\n", blank(cfg.Actions.Repo, "-"), blank(cfg.Actions.Workflow, "-"), blank(cfg.Actions.Job, "-"), blank(cfg.Actions.Ref, "-"), cfg.Actions.RunnerVersion, cfg.Actions.Ephemeral, blank(strings.Join(cfg.Actions.RunnerLabels, ","), "-"))
	fmt.Fprintf(w, "blacksmith org=%s workflow=%s job=%s ref=%s idle_timeout=%s debug=%t\n", blank(cfg.Blacksmith.Org, "-"), blank(cfg.Blacksmith.Workflow, "-"), blank(cfg.Blacksmith.Job, "-"), blank(cfg.Blacksmith.Ref, "-"), cfg.Blacksmith.IdleTimeout, cfg.Blacksmith.Debug)
	fmt.Fprintf(w, "namespace image=%s size=%s repository=%s site=%s volume_size_gb=%d auto_stop_idle_timeout=%s work_root=%s delete_on_release=%t\n", cfg.Namespace.Image, blank(cfg.Namespace.Size, "-"), blank(cfg.Namespace.Repository, "-"), blank(cfg.Namespace.Site, "-"), cfg.Namespace.VolumeSizeGB, cfg.Namespace.AutoStopIdleTimeout, cfg.Namespace.WorkRoot, cfg.Namespace.DeleteOnRelease)
	fmt.Fprintf(w, "morph api_url=%s snapshot=%s ssh_gateway_host=%s work_root=%s delete_on_release=%t wake_on_ssh=%t auth=%s\n", blank(cfg.Morph.APIURL, "-"), blank(cfg.Morph.Snapshot, "-"), blank(cfg.Morph.SSHGatewayHost, "-"), blank(cfg.Morph.WorkRoot, "-"), cfg.Morph.DeleteOnRelease, cfg.Morph.WakeOnSSH, tokenState(cfg.Morph.APIKey))
	fmt.Fprintf(w, "e2b api_url=%s domain=%s template=%s workdir=%s user=%s\n", cfg.E2B.APIURL, cfg.E2B.Domain, cfg.E2B.Template, cfg.E2B.Workdir, blank(cfg.E2B.User, "-"))
	fmt.Fprintf(w, "upstash_box base_url=%s runtime=%s size=%s workdir=%s keep_alive=%t auth=%s\n", cfg.UpstashBox.BaseURL, cfg.UpstashBox.Runtime, cfg.UpstashBox.Size, cfg.UpstashBox.Workdir, cfg.UpstashBox.KeepAlive, tokenState(cfg.UpstashBox.APIKey))
	fmt.Fprintf(w, "smolvm base_url=%s image=%s workdir=%s cpus=%d memory_mb=%d network=%s keep=%t auth=%s\n", cfg.Smolvm.BaseURL, cfg.Smolvm.Image, cfg.Smolvm.Workdir, cfg.Smolvm.CPUs, cfg.Smolvm.MemoryMB, cfg.Smolvm.Network, cfg.Smolvm.Keep, tokenState(cfg.Smolvm.APIKey))
	fmt.Fprintf(w, "ascii_box base_url=%s cli=%s workdir=%s auth=%s\n", cfg.AsciiBox.BaseURL, cfg.AsciiBox.CLIPath, cfg.AsciiBox.Workdir, tokenState(cfg.AsciiBox.APIKey))
	fmt.Fprintf(w, "apple_container cli=%s image=%s user=%s work_root=%s cpus=%d memory=%s\n", cfg.AppleContainer.CLIPath, cfg.AppleContainer.Image, cfg.AppleContainer.User, cfg.AppleContainer.WorkRoot, cfg.AppleContainer.CPUs, blank(cfg.AppleContainer.Memory, "-"))
	fmt.Fprintf(w, "mxc cli=%s version=%s containment=%s network=%s readonly_paths=%d readwrite_paths=%d allowed_hosts=%d blocked_hosts=%d allow_dacl_mutation=%t allow_windows_ui=%t experimental=%t\n", cfg.MXC.CLIPath, cfg.MXC.Version, cfg.MXC.Containment, cfg.MXC.Network, len(cfg.MXC.ReadOnlyPaths), len(cfg.MXC.ReadWritePaths), len(cfg.MXC.AllowedHosts), len(cfg.MXC.BlockedHosts), cfg.MXC.AllowDACLMutation, cfg.MXC.AllowWindowsUI, cfg.MXC.Experimental)
	fmt.Fprintf(w, "docker_sandbox cli=%s agent=%s template=%s cpus=%g memory=%s clone=%t workdir=%s extra_workspaces=%s mcp=%s kit=%s\n", cfg.DockerSandbox.CLIPath, cfg.DockerSandbox.Agent, blank(cfg.DockerSandbox.Template, "-"), cfg.DockerSandbox.CPUs, blank(cfg.DockerSandbox.Memory, "-"), cfg.DockerSandbox.Clone, blank(cfg.DockerSandbox.Workdir, "-"), blank(strings.Join(cfg.DockerSandbox.ExtraWorkspaces, ","), "-"), blank(strings.Join(cfg.DockerSandbox.MCP, ","), "-"), blank(strings.Join(cfg.DockerSandbox.Kit, ","), "-"))
	fmt.Fprintf(w, "multipass cli=%s image=%s user=%s work_root=%s cpus=%d memory=%s disk=%s launch_timeout=%s\n", cfg.Multipass.CLIPath, cfg.Multipass.Image, cfg.Multipass.User, cfg.Multipass.WorkRoot, cfg.Multipass.CPUs, blank(cfg.Multipass.Memory, "-"), blank(cfg.Multipass.Disk, "-"), cfg.Multipass.LaunchTimeout)
	fmt.Fprintf(w, "tart image=%s user=%s work_root=%s cpus=%d memory=%d disk=%d\n", cfg.Tart.Image, cfg.Tart.User, cfg.Tart.WorkRoot, cfg.Tart.CPUs, cfg.Tart.Memory, cfg.Tart.Disk)
	fmt.Fprintf(w, "cloudflare api_url=%s workdir=%s auth=%s\n", blank(cfg.Cloudflare.APIURL, "-"), cfg.Cloudflare.Workdir, tokenState(cfg.Cloudflare.Token))
	fmt.Fprintf(w, "static id=%s name=%s host=%s user=%s port=%s work_root=%s\n", blank(cfg.Static.ID, "-"), blank(cfg.Static.Name, "-"), blank(cfg.Static.Host, "-"), blank(cfg.Static.User, "-"), blank(cfg.Static.Port, "-"), blank(cfg.Static.WorkRoot, "-"))
	fmt.Fprintf(w, "results junit=%s auto=%t\n", blank(strings.Join(cfg.Results.JUnit, ","), "-"), cfg.Results.Auto)
	fmt.Fprintf(w, "cache pnpm=%t npm=%t docker=%t git=%t max_gb=%d purge_on_release=%t volumes=%d\n", cfg.Cache.Pnpm, cfg.Cache.Npm, cfg.Cache.Docker, cfg.Cache.Git, cfg.Cache.MaxGB, cfg.Cache.PurgeOnRelease, len(cfg.Cache.Volumes))
	if len(cfg.Jobs) > 0 {
		names := make([]string, 0, len(cfg.Jobs))
		for name := range cfg.Jobs {
			names = append(names, name)
		}
		sort.Strings(names)
		fmt.Fprintf(w, "jobs=%s\n", strings.Join(names, ","))
	}
	fmt.Fprintf(w, "aws region=%s root_gb=%d ssh_cidrs=%s\n", cfg.AWSRegion, cfg.AWSRootGB, blank(strings.Join(cfg.AWSSSHCIDRs, ","), "-"))
	fmt.Fprintf(w, "azure location=%s resource_group=%s os_disk=%s network=%s ssh_cidrs=%s\n", cfg.AzureLocation, cfg.AzureResourceGroup, cfg.AzureOSDisk, blank(cfg.AzureNetwork, "-"), blank(strings.Join(cfg.AzureSSHCIDRs, ","), "-"))
	fmt.Fprintf(w, "digitalocean region=%s image=%s vpc=%s ssh_cidrs=%s\n", cfg.DigitalOcean.Region, cfg.DigitalOcean.Image, blank(cfg.DigitalOcean.VPCUUID, "-"), blank(strings.Join(cfg.DigitalOcean.SSHCIDRs, ","), "-"))
	fmt.Fprintf(w, "linode region=%s image=%s type=%s firewall=%s ssh_cidrs=%s\n", cfg.Linode.Region, cfg.Linode.Image, cfg.Linode.Type, blank(cfg.Linode.FirewallID, "-"), blank(strings.Join(cfg.Linode.SSHCIDRs, ","), "-"))
	fmt.Fprintf(w, "hostinger api_url=%s item_id=%s payment_method_id=%s template_id=%s data_center_id=%s hostname_prefix=%s user=%s work_root=%s allow_purchase=%t release_action=%s auth=%s\n", blank(cfg.Hostinger.APIURL, "-"), blank(cfg.Hostinger.ItemID, "-"), blank(cfg.Hostinger.PaymentMethodID, "-"), blank(cfg.Hostinger.TemplateID, "-"), blank(cfg.Hostinger.DataCenterID, "-"), blank(cfg.Hostinger.HostnamePrefix, "-"), blank(cfg.Hostinger.User, "-"), blank(cfg.Hostinger.WorkRoot, "-"), cfg.Hostinger.AllowPurchase, blank(cfg.Hostinger.ReleaseAction, "-"), tokenState(cfg.Hostinger.APIToken))
	fmt.Fprintf(w, "azure_dynamic_sessions endpoint=%s unsupported_pool=%s api_version=%s workdir=%s timeout_secs=%d\n", blank(cfg.AzureDynamicSessions.Endpoint, "-"), blank(cfg.AzureDynamicSessions.Pool, "-"), cfg.AzureDynamicSessions.APIVersion, cfg.AzureDynamicSessions.Workdir, cfg.AzureDynamicSessions.TimeoutSecs)
	fmt.Fprintf(w, "gcp project=%s zone=%s image=%s network=%s subnet=%s root_gb=%d ssh_cidrs=%s\n", blank(cfg.GCPProject, "-"), cfg.GCPZone, cfg.GCPImage, cfg.GCPNetwork, blank(cfg.GCPSubnet, "-"), cfg.GCPRootGB, blank(strings.Join(cfg.GCPSSHCIDRs, ","), "-"))
	fmt.Fprintf(w, "proxmox api_url=%s node=%s template_id=%d storage=%s pool=%s bridge=%s user=%s work_root=%s full_clone=%t auth=%s\n", blank(cfg.Proxmox.APIURL, "-"), blank(cfg.Proxmox.Node, "-"), cfg.Proxmox.TemplateID, blank(cfg.Proxmox.Storage, "-"), blank(cfg.Proxmox.Pool, "-"), blank(cfg.Proxmox.Bridge, "-"), cfg.Proxmox.User, cfg.Proxmox.WorkRoot, cfg.Proxmox.FullClone, tokenState(cfg.Proxmox.TokenSecret))
	fmt.Fprintf(w, "xcp_ng api_url=%s username=%s template=%s template_uuid=%s sr=%s sr_uuid=%s network=%s network_uuid=%s host=%s user=%s work_root=%s insecure_tls=%t auth=%s\n", blank(redactedConfigURL(cfg.XCPNg.APIURL), "-"), blank(cfg.XCPNg.Username, "-"), blank(cfg.XCPNg.Template, "-"), blank(cfg.XCPNg.TemplateUUID, "-"), blank(cfg.XCPNg.SR, "-"), blank(cfg.XCPNg.SRUUID, "-"), blank(cfg.XCPNg.Network, "-"), blank(cfg.XCPNg.NetworkUUID, "-"), blank(cfg.XCPNg.Host, "-"), cfg.XCPNg.User, cfg.XCPNg.WorkRoot, cfg.XCPNg.InsecureTLS, tokenState(cfg.XCPNg.Password))
	fmt.Fprintf(w, "parallels template=%s source=%s source_id=%s snapshot=%s snapshot_id=%s clone_mode=%s host=%s user=%s work_root=%s startup_timeout=%s templates=%d hosts=%d\n", blank(cfg.Parallels.Template, "-"), blank(cfg.Parallels.Source, "-"), blank(cfg.Parallels.SourceID, "-"), blank(cfg.Parallels.SourceSnapshot, "-"), blank(cfg.Parallels.SourceSnapshotID, "-"), cfg.Parallels.CloneMode, blank(cfg.Parallels.Host, "local"), cfg.Parallels.User, cfg.Parallels.WorkRoot, cfg.Parallels.StartupTimeout, len(cfg.Parallels.Templates), len(cfg.Parallels.Hosts))
}

func redactedConfigURL(value string) string {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return value
	}
	addedScheme := false
	parseValue := raw
	if !strings.Contains(parseValue, "://") {
		parseValue = "https://" + parseValue
		addedScheme = true
	}
	u, err := url.Parse(parseValue)
	if err != nil {
		return sanitizedMalformedConfigURL(parseValue, addedScheme)
	}
	if u.User == nil {
		return value
	}
	redacted := *u
	redacted.User = url.User("<redacted>")
	out := strings.ReplaceAll(redacted.String(), "%3Credacted%3E", "<redacted>")
	if addedScheme {
		out = strings.TrimPrefix(out, "https://")
	}
	return out
}

// sanitizedMalformedConfigURL strips any userinfo from a malformed URL so
// url.Parse error messages and downstream diagnostics cannot echo the
// original credentials.
func sanitizedMalformedConfigURL(parseValue string, addedScheme bool) string {
	sanitized := parseValue
	if i := strings.Index(sanitized, "://"); i >= 0 {
		rest := sanitized[i+3:]
		if at := strings.LastIndex(rest, "@"); at >= 0 {
			sanitized = sanitized[:i+3] + rest[at+1:]
		}
	} else {
		if at := strings.LastIndex(sanitized, "@"); at >= 0 {
			sanitized = sanitized[at+1:]
		}
	}
	if addedScheme {
		sanitized = strings.TrimPrefix(sanitized, "https://")
	}
	return sanitized
}

func jobConfigViews(jobs map[string]JobConfig) map[string]any {
	if len(jobs) == 0 {
		return nil
	}
	view := make(map[string]any, len(jobs))
	for name, job := range jobs {
		entry := map[string]any{
			"provider":       job.Provider,
			"target":         job.Target,
			"windowsMode":    job.WindowsMode,
			"profile":        job.Profile,
			"class":          job.Class,
			"architecture":   job.Architecture,
			"serverType":     job.ServerType,
			"market":         job.Market,
			"desktop":        job.Desktop,
			"desktopEnv":     job.DesktopEnv,
			"browser":        job.Browser,
			"code":           job.Code,
			"network":        job.Network,
			"shell":          job.Shell,
			"command":        job.Command,
			"noSync":         job.NoSync,
			"syncOnly":       job.SyncOnly,
			"checksum":       job.Checksum,
			"forceSyncLarge": job.ForceSyncLarge,
			"junit":          job.JUnit,
			"downloads":      job.Downloads,
			"stop":           job.Stop,
			"hydrate": map[string]any{
				"actions":          job.Hydrate.Actions,
				"githubRunner":     job.Hydrate.GitHubRunner,
				"waitTimeout":      durationString(job.Hydrate.WaitTimeout),
				"keepAliveMinutes": job.Hydrate.KeepAliveMinutes,
			},
			"actions": map[string]any{
				"repo":     job.Actions.Repo,
				"workflow": job.Actions.Workflow,
				"job":      job.Actions.Job,
				"ref":      job.Actions.Ref,
				"fields":   job.Actions.Fields,
			},
		}
		if job.TTL > 0 {
			entry["ttl"] = job.TTL.String()
		}
		if job.IdleTimeout > 0 {
			entry["idleTimeout"] = job.IdleTimeout.String()
		}
		view[name] = entry
	}
	return view
}

func durationString(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return d.String()
}

func (a App) configSetBroker(args []string) error {
	fs := newFlagSet("config set-broker", a.Stderr)
	url := fs.String("url", "", "broker URL")
	provider := fs.String("provider", "", "default provider (managed coordinator provider or registered direct provider)")
	mode := fs.String("mode", "", "lease mode: managed or registered")
	autoWebVNC := fs.Bool("auto-webvnc", true, "start a portal WebVNC bridge for kept registered desktop leases")
	tokenStdin := fs.Bool("token-stdin", false, "read broker token from stdin")
	adminTokenStdin := fs.Bool("admin-token-stdin", false, "read broker admin token from stdin")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *url == "" {
		return exit(2, "config set-broker requires --url")
	}
	if *mode != "" && *mode != string(BrokerModeManaged) && *mode != string(BrokerModeRegistered) {
		return exit(2, "--mode must be managed or registered")
	}
	path := writableConfigPath()
	if path == "" {
		return exit(2, "user config directory is unavailable")
	}
	file, err := readFileConfig(path)
	if err != nil {
		return err
	}
	if file.Broker == nil {
		file.Broker = &fileBrokerConfig{}
	}
	effectiveMode, err := normalizeBrokerMode(blank(*mode, file.Broker.Mode))
	if err != nil {
		return err
	}
	explicitProvider := strings.TrimSpace(*provider)
	validationProvider := explicitProvider
	if validationProvider == "" {
		validationProvider = strings.TrimSpace(file.Broker.Provider)
	}
	brokerProvider, err := validateBrokerProviderForMode(validationProvider, string(effectiveMode))
	if err != nil {
		return err
	}
	var token string
	if *tokenStdin {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return exit(2, "read broker token: %v", err)
		}
		token = strings.TrimSpace(string(data))
		if token == "" {
			return exit(2, "broker token from stdin is empty")
		}
	}
	var adminToken string
	if *adminTokenStdin {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return exit(2, "read broker admin token: %v", err)
		}
		adminToken = strings.TrimSpace(string(data))
		if adminToken == "" {
			return exit(2, "broker admin token from stdin is empty")
		}
	}
	file.Broker.URL = *url
	if *mode != "" {
		file.Broker.Mode = *mode
	}
	if flagWasSet(fs, "auto-webvnc") {
		file.Broker.AutoWebVNC = autoWebVNC
	}
	if token != "" {
		file.Broker.Token = token
	}
	if adminToken != "" {
		file.Broker.AdminToken = adminToken
	}
	if explicitProvider != "" && brokerProvider != "" {
		file.Broker.Provider = brokerProvider
		file.Provider = brokerProvider
	}
	written, err := writeUserFileConfig(file)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "wrote %s broker=%s mode=%s auth=%s admin_auth=%s\n", written, *url, blank(file.Broker.Mode, string(BrokerModeManaged)), tokenState(file.Broker.Token), tokenState(file.Broker.AdminToken))
	return nil
}

func validateBrokerProvider(provider string) (string, error) {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return "", nil
	}
	resolved, err := ProviderFor(provider)
	if err != nil {
		return "", err
	}
	spec := resolved.Spec()
	if spec.Coordinator != CoordinatorSupported {
		return "", exit(2, "provider %q cannot be used with a broker; supported broker providers are aws, azure, gcp, and hetzner", provider)
	}
	return resolved.Name(), nil
}

func validateBrokerProviderForMode(provider, mode string) (string, error) {
	provider = strings.TrimSpace(provider)
	if mode != string(BrokerModeRegistered) {
		return validateBrokerProvider(provider)
	}
	if provider == "" {
		return "", nil
	}
	if normalizeProviderName(provider) == "external" {
		return "external", nil
	}
	resolved, err := ProviderFor(provider)
	if err != nil {
		return "", err
	}
	return resolved.Name(), nil
}

func tokenState(token string) string {
	if token == "" {
		return "missing"
	}
	return "configured"
}

func accessAuthState(access AccessConfig) string {
	hasServiceToken := access.ClientID != "" && access.ClientSecret != ""
	hasToken := access.Token != ""
	if hasServiceToken && hasToken {
		return "service-token+token"
	}
	if hasServiceToken {
		return "service-token"
	}
	if hasToken {
		return "token"
	}
	if access.ClientID != "" || access.ClientSecret != "" {
		return "incomplete"
	}
	return "missing"
}

func blank(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func Blank(value, fallback string) string {
	return blank(value, fallback)
}
