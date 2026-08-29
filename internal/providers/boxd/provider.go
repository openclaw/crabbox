package boxd

import (
	"encoding/json"
	"flag"
	"os"
	"path"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

// Provider uses the HTTPS console for lifecycle and authenticated guest bootstrap.
type Provider struct{}

func (Provider) Name() string      { return providerName }
func (Provider) Aliases() []string { return nil }

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:             providerName,
		Family:           providerName,
		Kind:             core.ProviderKindSSHLease,
		Targets:          []core.TargetSpec{{OS: core.TargetLinux}},
		Features:         core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup},
		Coordinator:      core.CoordinatorNever,
		ClassDisposition: core.ProviderClassDispositionUnmapped,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return registerFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return applyFlags(cfg, fs, values)
}

func (Provider) ValidateConfig(cfg core.Config) error {
	if _, err := consoleURL(cfg.Boxd.APIURL); err != nil {
		return err
	}
	workRoot := cfg.Boxd.WorkRoot
	if workRoot != strings.TrimSpace(workRoot) {
		return core.Exit(2, "boxd.workRoot must be a canonical absolute Linux path")
	}
	cleanWorkRoot := path.Clean(workRoot)
	if workRoot == "" || !strings.HasPrefix(cleanWorkRoot, "/") || cleanWorkRoot != workRoot {
		return core.Exit(2, "boxd.workRoot must be a canonical absolute Linux path")
	}
	switch cleanWorkRoot {
	case "/", "/bin", "/dev", "/etc", "/home", "/lib", "/lib64", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var":
		return core.Exit(2, "boxd.workRoot %q is too broad; choose a dedicated subdirectory", cleanWorkRoot)
	}
	return nil
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if cfg.TargetOS != "" && cfg.TargetOS != core.TargetLinux {
		return nil, core.Exit(2, "provider=%s supports target=linux only", providerName)
	}
	if cfg.Tailscale.Enabled || string(cfg.Network) == "tailscale" {
		return nil, core.Exit(2, "--tailscale is not supported for provider=%s; boxd machines are reached through the boxd edge proxy", providerName)
	}
	if cfg.Network != "" && cfg.Network != core.NetworkAuto && cfg.Network != core.NetworkPublic {
		return nil, core.Exit(2, "provider=%s supports only public networking; refusing requested network=%s", providerName, cfg.Network)
	}
	applyDefaults(&cfg)
	if err := p.ValidateConfig(cfg); err != nil {
		return nil, err
	}
	return newBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	return backend.(core.DoctorBackend), nil
}

// ClaimScope binds routing; the authenticated user is independently fenced in each claim.
func (Provider) ClaimScope(cfg core.Config) string {
	u, err := consoleURL(cfg.Boxd.APIURL)
	if err != nil {
		return "invalid-boxd-origin"
	}
	data, _ := json.Marshal([]string{u.String(), cfg.Boxd.Org})
	return string(data)
}

func (Provider) DiagnosticSecrets(core.Config) []string {
	return []string{os.Getenv("CRABBOX_BOXD_TOKEN"), os.Getenv("BOXD_TOKEN")}
}

// ServerTypeForConfig: boxd machine sizing follows the account/org quota, not
// a caller-chosen instance type.
func (Provider) ServerTypeForConfig(cfg core.Config) string {
	if cfg.ServerTypeExplicit && cfg.ServerType != "" {
		return cfg.ServerType
	}
	return "machine"
}

func (Provider) ServerTypeForClass(string) string {
	return "machine"
}

func applyDefaults(cfg *core.Config) {
	cfg.Provider = providerName
	if strings.TrimSpace(cfg.TargetOS) == "" {
		cfg.TargetOS = core.TargetLinux
	}
	if cfg.Boxd.APIURL == "" {
		cfg.Boxd.APIURL = defaultConsoleURL
	}
	if !core.IsBoxdWorkRootExplicit(cfg) && core.IsWorkRootExplicit(cfg) {
		cfg.Boxd.WorkRoot = cfg.WorkRoot
	} else if strings.TrimSpace(cfg.Boxd.WorkRoot) == "" {
		if strings.TrimSpace(cfg.WorkRoot) == "" || core.IsDefaultWorkRoot(cfg.WorkRoot) {
			cfg.Boxd.WorkRoot = defaultBoxdWorkRoot
		} else {
			cfg.Boxd.WorkRoot = cfg.WorkRoot
		}
	}
	cfg.WorkRoot = cfg.Boxd.WorkRoot
	if cfg.Network == "" || cfg.Network == core.NetworkAuto || cfg.Network == core.NetworkPublic {
		cfg.Network = core.NetworkPublic
	}
	if !cfg.ServerTypeExplicit || strings.TrimSpace(cfg.ServerType) == "" {
		cfg.ServerType = "machine"
	}
}
