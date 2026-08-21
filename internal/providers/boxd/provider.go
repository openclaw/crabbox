package boxd

import (
	"encoding/json"
	"flag"
	"os"
	"path"
	"path/filepath"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

// Provider registers boxd (https://boxd.sh) — KVM microVMs with a public
// HTTPS/SSH edge — as an SSH-lease backend. Lifecycle goes through the
// external `boxd` CLI (`machine new/get/list/remove --json`); the CLI also
// manages auth (`boxd auth login` or BOXD_TOKEN), the per-device SSH key it
// links to the account, and the ssh-config/known_hosts entries the backend
// reads the SSH endpoint from.
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
	workRoot := strings.TrimSpace(cfg.Boxd.WorkRoot)
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

// ClaimScope fences claims to one boxd control plane and org context, so
// leases against different clusters (prod, staging, self-hosted) or orgs
// never alias each other.
func (Provider) ClaimScope(cfg core.Config) string {
	parts := make([]string, 0, 2)
	if url := strings.TrimSpace(cfg.Boxd.APIURL); url != "" {
		parts = append(parts, "endpoint:"+strings.ToLower(url))
	}
	if org := strings.TrimSpace(cfg.Boxd.Org); org != "" {
		parts = append(parts, "org:"+org)
	}
	return strings.Join(parts, "|")
}

// DiagnosticSecrets registers runtime-only credentials for diagnostic
// redaction: the BOXD_TOKEN environment variable and the boxd CLI's stored
// login token. The token itself never enters crabbox config or argv — the
// boxd CLI reads it from its own credential store or BOXD_TOKEN.
func (Provider) DiagnosticSecrets(cfg core.Config) []string {
	secrets := make([]string, 0, 2)
	if token := strings.TrimSpace(os.Getenv("BOXD_TOKEN")); token != "" {
		secrets = append(secrets, token)
	}
	if token := storedBoxdCLIToken(cfg.Boxd.CLIPath); token != "" {
		secrets = append(secrets, token)
	}
	return secrets
}

// storedBoxdCLIToken best-effort reads the boxd CLI's stored login token so it
// joins diagnostic redaction. The CLI keeps credentials under a config dir
// named after the binary (`boxd`, `boxd-stg`, ...), so the configured CLI path
// selects the matching store.
func storedBoxdCLIToken(cliPath string) string {
	app := strings.TrimSpace(filepath.Base(strings.TrimSpace(cliPath)))
	if app == "" || app == "." || app == string(filepath.Separator) {
		app = "boxd"
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(configDir, app, "credentials.json"))
	if err != nil {
		return ""
	}
	var credentials struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &credentials); err != nil {
		return ""
	}
	return strings.TrimSpace(credentials.Token)
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
	if strings.TrimSpace(cfg.Boxd.CLIPath) == "" {
		cfg.Boxd.CLIPath = "boxd"
	}
	if strings.TrimSpace(cfg.Boxd.WorkRoot) == "" {
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
