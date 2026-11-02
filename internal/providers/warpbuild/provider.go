package warpbuild

import (
	"flag"
	"path"
	"path/filepath"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const providerName = "warpbuild"

func init() { core.RegisterProvider(Provider{}) }

type Provider struct{}

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name: providerName, Family: providerName, Kind: core.ProviderKindDelegatedRun,
		Targets:        []core.TargetSpec{{OS: core.TargetMacOS}},
		Authentication: core.DirectProviderAuthentication(core.ProviderAuthenticationAPIKey),
		Features:       core.FeatureSet{core.FeatureArchiveSync, core.FeatureRunSession},
		Coordinator:    core.CoordinatorNever, ClassDisposition: core.ProviderClassDispositionUnmapped,
		SyncGuardrailFullCandidate: true,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterWarpBuildConfigFlags(fs, defaults.WarpBuild)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	applied, err := core.ApplyProviderConfigFlags[core.WarpBuildConfigFlagValues](cfg, fs, values, &cfg.WarpBuild, providerName)
	if !applied || err != nil {
		return err
	}
	return shared.RejectExplicitMachineSizingFlags(fs, providerName, "use --warpbuild-runner-label to select a native sandbox size", "use --warpbuild-runner-label to select a native sandbox size")
}

func (Provider) ValidateConfig(cfg core.Config) error {
	c := cfg.WarpBuild
	if c.CLIPath == "" || (!filepath.IsAbs(c.CLIPath) && (strings.ContainsAny(c.CLIPath, `/\`) || c.CLIPath == "." || c.CLIPath == "..")) {
		return core.Exit(2, "warpbuild cliPath must be a bare executable name or an absolute path")
	}
	switch c.RunnerLabel {
	case "warp-macos-26-arm64-6x", "warp-macos-26-arm64-12x":
	default:
		return core.Exit(2, "unsupported WarpBuild sandbox runner label %q", c.RunnerLabel)
	}
	_, err := shared.CleanPOSIXWorkspacePath("warpbuild workdir", c.Workdir, "/Users", "/Users/runner", "/Applications", "/Library", "/System")
	return err
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if cfg.TargetOS != core.TargetMacOS {
		if core.IsTargetExplicit(&cfg) {
			return nil, core.Exit(2, "provider=warpbuild supports macOS only")
		}
		cfg.TargetOS = core.TargetMacOS
	}
	if cfg.Tailscale.Enabled {
		return nil, core.Exit(2, "provider=warpbuild does not support --tailscale")
	}
	if err := p.ValidateConfig(cfg); err != nil {
		return nil, err
	}
	cfg.WarpBuild.Workdir = path.Clean(strings.TrimSpace(cfg.WarpBuild.Workdir))
	return &backend{cfg: cfg, rt: rt}, nil
}

func (Provider) BackendCapabilities() core.Backend { return &backend{} }

func (Provider) ConfigShowSection(cfg core.Config) core.ProviderConfigShowSection {
	c := cfg.WarpBuild
	return core.ProviderConfigShowSection{JSONKey: providerName, TextLabel: providerName, Providers: []string{providerName}, Fields: []core.ProviderConfigShowField{
		{JSONName: "cliPath", JSONValue: c.CLIPath, TextName: "cli", TextValue: c.CLIPath},
		{JSONName: "runnerLabel", JSONValue: c.RunnerLabel, TextName: "runner_label", TextValue: c.RunnerLabel},
		{JSONName: "workdir", JSONValue: c.Workdir, TextName: "workdir", TextValue: c.Workdir},
		{JSONName: "token", JSONValue: core.ConfigShowSecretState(c.Token), TextName: "auth", TextValue: core.ConfigShowSecretState(c.Token)},
	}}
}
