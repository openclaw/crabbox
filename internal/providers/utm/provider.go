// Package utm manages owned local UTM Linux and macOS guest leases.
package utm

import (
	"flag"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() { core.RegisterProvider(Provider{}) }

type Provider struct{}

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{Name: "utm", Family: "utm", Kind: core.ProviderKindSSHLease,
		Authentication: core.DirectProviderAuthentication(core.ProviderAuthenticationLocalContext),
		Targets:        []core.TargetSpec{{OS: core.TargetLinux}, {OS: core.TargetMacOS}}, Features: core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup, core.FeatureCheckpoint, core.FeatureRestore, core.FeatureClaimExec}, Coordinator: core.CoordinatorNever, ClassDisposition: core.ProviderClassDispositionUnmapped, ActionsRunnerUnsupported: true}
}
func (Provider) RegisterFlags(fs *flag.FlagSet, cfg core.Config) any {
	return core.RegisterUTMConfigFlags(fs, cfg.UTM)
}
func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(core.UTMConfigFlagValues)
	if !ok {
		return nil
	}
	applied, err := v.Apply(&cfg.UTM, fs)
	core.RecordProviderFlagInputs(cfg, applied.InputAccepted, "utm")
	return err
}
func (Provider) ApplyConfigDefaults(cfg *core.Config) error {
	if !core.IsArchitectureExplicit(*cfg) {
		cfg.Architecture = "arm64"
	}
	if cfg.TargetOS == "" || !core.IsTargetExplicit(cfg) {
		cfg.TargetOS = core.TargetLinux
		if cfg.UTM.Acquisition == "ipsw" {
			cfg.TargetOS = core.TargetMacOS
		}
	}
	if cfg.UTM.User != "" {
		cfg.SSHUser = cfg.UTM.User
	}
	if cfg.UTM.WorkRoot != "" {
		cfg.WorkRoot = cfg.UTM.WorkRoot
	}
	return nil
}
func (Provider) SupportsArchitecture(cfg core.Config, arch string) bool {
	return (cfg.TargetOS == core.TargetLinux || cfg.TargetOS == core.TargetMacOS) && arch == "arm64"
}
func (Provider) ClaimScope(cfg core.Config) string {
	return "utm:" + filepath.Clean(core.ExpandUserPath(cfg.UTM.AppPath))
}
func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	mode := cfg.UTM.Acquisition
	if mode == "" {
		mode = "template"
	}
	if mode != "template" && mode != "ipsw" {
		return nil, core.Exit(2, "utm.acquisition must be template or ipsw")
	}
	wantedOS := core.TargetLinux
	if mode == "ipsw" {
		wantedOS = core.TargetMacOS
	}
	if cfg.TargetOS != "" && cfg.TargetOS != wantedOS {
		return nil, core.Exit(2, "UTM acquisition=%s requires target OS %s", mode, wantedOS)
	}
	if cfg.Tailscale.Enabled || string(cfg.Network) == "tailscale" {
		return nil, core.Exit(2, "provider=utm requires its local VM SSH transport; Tailscale is unsupported")
	}
	if cfg.Desktop || cfg.Browser || cfg.Code {
		return nil, core.Exit(2, "provider=utm does not support desktop, browser or code capabilities")
	}
	c := cfg.UTM
	c.Acquisition = mode
	c.IPSW = core.ExpandUserPath(c.IPSW)
	if c.AppPath == "" {
		c.AppPath = core.UTMConfigDefaultAppPath
	}
	c.AppPath = core.ExpandUserPath(c.AppPath)
	c.StorageRoot = core.ExpandUserPath(c.StorageRoot)
	if c.StorageRoot != "" && !filepath.IsAbs(c.StorageRoot) {
		return nil, fmt.Errorf("utm.storageRoot must be an absolute local directory")
	}
	if !filepath.IsAbs(c.AppPath) || filepath.Ext(c.AppPath) != ".app" {
		return nil, fmt.Errorf("utm.appPath must be an absolute .app bundle path")
	}
	if c.User == "" {
		c.User = core.UTMConfigDefaultUser
	}
	if c.WorkRoot == "" {
		c.WorkRoot = core.UTMConfigDefaultWorkRoot
	}
	if c.InstallTimeout <= 0 {
		c.InstallTimeout = core.UTMConfigDefaultInstallTimeout
	}
	if c.StartupTimeout <= 0 {
		c.StartupTimeout = core.UTMConfigDefaultStartupTimeout
	}
	c.SSHKey = core.ExpandUserPath(c.SSHKey)
	if c.SSHKey == "" {
		c.SSHKey = core.ExpandUserPath(cfg.SSHKey)
	}
	if strings.ContainsAny(c.User, "\r\n\x00") || strings.HasPrefix(c.User, "-") {
		return nil, fmt.Errorf("invalid utm.user")
	}
	cfg.Provider = "utm"
	cfg.TargetOS = wantedOS
	cfg.UTM = c
	cfg.SSHUser = c.User
	cfg.SSHKey = c.SSHKey
	cfg.WorkRoot = c.WorkRoot
	cfg.SSHFallbackPorts = nil
	return &backend{cfg: cfg, rt: rt, hostOS: runtime.GOOS}, nil
}
func (Provider) ConfigShowSection(cfg core.Config) core.ProviderConfigShowSection {
	c := cfg.UTM
	return core.ProviderConfigShowSection{JSONKey: "utm", TextLabel: "utm", Providers: []string{"utm"}, Fields: []core.ProviderConfigShowField{
		{JSONName: "acquisition", JSONValue: c.Acquisition, TextName: "acquisition", TextValue: c.Acquisition},
		{JSONName: "ipsw", JSONValue: c.IPSW, TextName: "ipsw", TextValue: c.IPSW},
		{JSONName: "installTimeout", JSONValue: c.InstallTimeout.String(), TextName: "install_timeout", TextValue: c.InstallTimeout.String()},
		{JSONName: "appPath", JSONValue: c.AppPath, TextName: "app", TextValue: c.AppPath},
		{JSONName: "storageRoot", JSONValue: c.StorageRoot, TextName: "storage_root", TextValue: c.StorageRoot},
		{JSONName: "template", JSONValue: c.Template, TextName: "template", TextValue: c.Template},
		{JSONName: "user", JSONValue: c.User, TextName: "user", TextValue: c.User},
		{JSONName: "sshKey", JSONValue: c.SSHKey, TextName: "ssh_key", TextValue: c.SSHKey},
		{JSONName: "workRoot", JSONValue: c.WorkRoot, TextName: "work_root", TextValue: c.WorkRoot},
		{JSONName: "startupTimeout", JSONValue: c.StartupTimeout.String(), TextName: "startup_timeout", TextValue: c.StartupTimeout.String()}}}
}
