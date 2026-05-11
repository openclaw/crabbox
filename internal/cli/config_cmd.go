package cli

import (
	"encoding/json"
	"fmt"
	"io"
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
	view := map[string]any{
		"profile":            cfg.Profile,
		"provider":           cfg.Provider,
		"target":             cfg.TargetOS,
		"windowsMode":        cfg.WindowsMode,
		"class":              cfg.Class,
		"serverType":         cfg.ServerType,
		"serverTypeExplicit": cfg.ServerTypeExplicit,
		"coordinator":        cfg.Coordinator,
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
		"e2b": map[string]any{
			"apiUrl":   cfg.E2B.APIURL,
			"domain":   cfg.E2B.Domain,
			"template": cfg.E2B.Template,
			"workdir":  cfg.E2B.Workdir,
			"user":     cfg.E2B.User,
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
		},
		"cache": map[string]any{
			"pnpm":           cfg.Cache.Pnpm,
			"npm":            cfg.Cache.Npm,
			"docker":         cfg.Cache.Docker,
			"git":            cfg.Cache.Git,
			"maxGB":          cfg.Cache.MaxGB,
			"purgeOnRelease": cfg.Cache.PurgeOnRelease,
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
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(view)
	}
	fmt.Fprintf(a.Stdout, "config=%s\n", userConfigPath())
	fmt.Fprintf(a.Stdout, "provider=%s target=%s windows_mode=%s class=%s type=%s profile=%s\n", cfg.Provider, cfg.TargetOS, cfg.WindowsMode, cfg.Class, cfg.ServerType, cfg.Profile)
	fmt.Fprintf(a.Stdout, "broker=%s auth=%s admin_auth=%s\n", blank(cfg.Coordinator, "-"), tokenState(cfg.CoordToken), tokenState(cfg.CoordAdminToken))
	fmt.Fprintf(a.Stdout, "access_auth=%s\n", accessAuthState(cfg.Access))
	fmt.Fprintf(a.Stdout, "ssh=%s@<host>:%s fallback_ports=%s key=%s\n", cfg.SSHUser, cfg.SSHPort, blank(strings.Join(cfg.SSHFallbackPorts, ","), "-"), cfg.SSHKey)
	fmt.Fprintf(a.Stdout, "sync delete=%t checksum=%t git_seed=%t fingerprint=%t base_ref=%s excludes=%d timeout=%s\n", cfg.Sync.Delete, cfg.Sync.Checksum, cfg.Sync.GitSeed, cfg.Sync.Fingerprint, blank(cfg.Sync.BaseRef, "-"), len(configuredExcludes(cfg)), cfg.Sync.Timeout)
	fmt.Fprintf(a.Stdout, "env allow=%s\n", strings.Join(cfg.EnvAllow, ","))
	fmt.Fprintf(a.Stdout, "capacity market=%s strategy=%s fallback=%s regions=%s hints=%t\n", cfg.Capacity.Market, cfg.Capacity.Strategy, cfg.Capacity.Fallback, blank(strings.Join(cfg.Capacity.Regions, ","), "-"), cfg.Capacity.Hints)
	fmt.Fprintf(a.Stdout, "actions repo=%s workflow=%s job=%s ref=%s runner_version=%s ephemeral=%t labels=%s\n", blank(cfg.Actions.Repo, "-"), blank(cfg.Actions.Workflow, "-"), blank(cfg.Actions.Job, "-"), blank(cfg.Actions.Ref, "-"), cfg.Actions.RunnerVersion, cfg.Actions.Ephemeral, blank(strings.Join(cfg.Actions.RunnerLabels, ","), "-"))
	fmt.Fprintf(a.Stdout, "blacksmith org=%s workflow=%s job=%s ref=%s idle_timeout=%s debug=%t\n", blank(cfg.Blacksmith.Org, "-"), blank(cfg.Blacksmith.Workflow, "-"), blank(cfg.Blacksmith.Job, "-"), blank(cfg.Blacksmith.Ref, "-"), cfg.Blacksmith.IdleTimeout, cfg.Blacksmith.Debug)
	fmt.Fprintf(a.Stdout, "namespace image=%s size=%s repository=%s site=%s volume_size_gb=%d auto_stop_idle_timeout=%s work_root=%s delete_on_release=%t\n", cfg.Namespace.Image, blank(cfg.Namespace.Size, "-"), blank(cfg.Namespace.Repository, "-"), blank(cfg.Namespace.Site, "-"), cfg.Namespace.VolumeSizeGB, cfg.Namespace.AutoStopIdleTimeout, cfg.Namespace.WorkRoot, cfg.Namespace.DeleteOnRelease)
	fmt.Fprintf(a.Stdout, "e2b api_url=%s domain=%s template=%s workdir=%s user=%s\n", cfg.E2B.APIURL, cfg.E2B.Domain, cfg.E2B.Template, cfg.E2B.Workdir, blank(cfg.E2B.User, "-"))
	fmt.Fprintf(a.Stdout, "static id=%s name=%s host=%s user=%s port=%s work_root=%s\n", blank(cfg.Static.ID, "-"), blank(cfg.Static.Name, "-"), blank(cfg.Static.Host, "-"), blank(cfg.Static.User, "-"), blank(cfg.Static.Port, "-"), blank(cfg.Static.WorkRoot, "-"))
	fmt.Fprintf(a.Stdout, "results junit=%s\n", blank(strings.Join(cfg.Results.JUnit, ","), "-"))
	fmt.Fprintf(a.Stdout, "cache pnpm=%t npm=%t docker=%t git=%t max_gb=%d purge_on_release=%t\n", cfg.Cache.Pnpm, cfg.Cache.Npm, cfg.Cache.Docker, cfg.Cache.Git, cfg.Cache.MaxGB, cfg.Cache.PurgeOnRelease)
	if len(cfg.Jobs) > 0 {
		names := make([]string, 0, len(cfg.Jobs))
		for name := range cfg.Jobs {
			names = append(names, name)
		}
		sort.Strings(names)
		fmt.Fprintf(a.Stdout, "jobs=%s\n", strings.Join(names, ","))
	}
	fmt.Fprintf(a.Stdout, "aws region=%s root_gb=%d ssh_cidrs=%s\n", cfg.AWSRegion, cfg.AWSRootGB, blank(strings.Join(cfg.AWSSSHCIDRs, ","), "-"))
	fmt.Fprintf(a.Stdout, "gcp project=%s zone=%s image=%s network=%s subnet=%s root_gb=%d ssh_cidrs=%s\n", blank(cfg.GCPProject, "-"), cfg.GCPZone, cfg.GCPImage, cfg.GCPNetwork, blank(cfg.GCPSubnet, "-"), cfg.GCPRootGB, blank(strings.Join(cfg.GCPSSHCIDRs, ","), "-"))
	return nil
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
			"serverType":     job.ServerType,
			"market":         job.Market,
			"desktop":        job.Desktop,
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
	provider := fs.String("provider", "", "default provider: hetzner, aws, azure, or gcp")
	tokenStdin := fs.Bool("token-stdin", false, "read broker token from stdin")
	adminTokenStdin := fs.Bool("admin-token-stdin", false, "read broker admin token from stdin")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *url == "" {
		return exit(2, "config set-broker requires --url")
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
	file.Broker.URL = *url
	if token != "" {
		file.Broker.Token = token
	}
	if adminToken != "" {
		file.Broker.AdminToken = adminToken
	}
	if *provider != "" {
		file.Broker.Provider = *provider
		file.Provider = *provider
	}
	written, err := writeUserFileConfig(file)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "wrote %s broker=%s auth=%s admin_auth=%s\n", written, *url, tokenState(file.Broker.Token), tokenState(file.Broker.AdminToken))
	return nil
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
