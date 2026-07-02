package parallels

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string      { return "parallels" }
func (Provider) Aliases() []string { return nil }

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:   "parallels",
		Family: "parallels",
		Kind:   core.ProviderKindSSHLease,
		Targets: []core.TargetSpec{
			{OS: core.TargetLinux},
			{OS: core.TargetMacOS},
			{OS: core.TargetWindows, WindowsMode: core.WindowsModeNormal},
			{OS: core.TargetWindows, WindowsMode: core.WindowsModeWSL2},
		},
		Features: core.FeatureSet{
			core.FeatureSSH,
			core.FeatureCrabboxSync,
			core.FeatureCleanup,
			core.FeatureDesktop,
			core.FeatureBrowser,
			core.FeatureCode,
			core.FeatureCheckpoint,
			core.FeatureFork,
			core.FeatureRestore,
			core.FeatureSnapshot,
		},
		Coordinator: core.CoordinatorNever,
	}
}

type flagValues struct {
	Template         *string
	Source           *string
	SourceID         *string
	SourceSnapshot   *string
	SourceSnapshotID *string
	CloneMode        *string
	Host             *string
	HostUser         *string
	HostKey          *string
	VMRoot           *string
	User             *string
	WorkRoot         *string
	StartupTimeout   *string
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return flagValues{
		Template:         fs.String("parallels-template", defaults.Parallels.Template, "Parallels template alias"),
		Source:           fs.String("parallels-source", defaults.Parallels.Source, "Parallels source VM name"),
		SourceID:         fs.String("parallels-source-id", defaults.Parallels.SourceID, "Parallels source VM UUID"),
		SourceSnapshot:   fs.String("parallels-source-snapshot", defaults.Parallels.SourceSnapshot, "Parallels source snapshot name"),
		SourceSnapshotID: fs.String("parallels-source-snapshot-id", defaults.Parallels.SourceSnapshotID, "Parallels source snapshot ID"),
		CloneMode:        fs.String("parallels-clone-mode", defaults.Parallels.CloneMode, "Parallels clone mode: linked, full, or unlink"),
		Host:             fs.String("parallels-host", defaults.Parallels.Host, "remote Mac host running Parallels"),
		HostUser:         fs.String("parallels-host-user", defaults.Parallels.HostUser, "remote Mac SSH user"),
		HostKey:          fs.String("parallels-host-key", defaults.Parallels.HostKey, "remote Mac SSH key"),
		VMRoot:           fs.String("parallels-vm-root", defaults.Parallels.VMRoot, "destination directory for cloned .pvm bundles"),
		User:             fs.String("parallels-user", defaults.Parallels.User, "guest SSH user"),
		WorkRoot:         fs.String("parallels-work-root", defaults.Parallels.WorkRoot, "remote work root inside Parallels guests"),
		StartupTimeout:   fs.String("parallels-startup-timeout", defaults.Parallels.StartupTimeout.String(), "Parallels VM startup timeout"),
	}
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	targetOverride := cfg.TargetOS
	windowsModeOverride := cfg.WindowsMode
	if core.FlagWasSet(fs, "parallels-template") {
		cfg.Parallels.Template = *v.Template
		if err := core.ApplyParallelsTemplateConfig(cfg, *v.Template); err != nil {
			return err
		}
		if core.FlagWasSet(fs, "target") {
			cfg.TargetOS = targetOverride
		}
		if core.FlagWasSet(fs, "windows-mode") {
			cfg.WindowsMode = windowsModeOverride
		}
	}
	if core.FlagWasSet(fs, "parallels-source") {
		cfg.Parallels.Source = *v.Source
		cfg.Parallels.SourceID = ""
	}
	if core.FlagWasSet(fs, "parallels-source-id") {
		cfg.Parallels.SourceID = *v.SourceID
	}
	if core.FlagWasSet(fs, "parallels-source-snapshot") {
		cfg.Parallels.SourceSnapshot = *v.SourceSnapshot
		cfg.Parallels.SourceSnapshotID = ""
	}
	if core.FlagWasSet(fs, "parallels-source-snapshot-id") {
		cfg.Parallels.SourceSnapshotID = *v.SourceSnapshotID
	}
	if core.FlagWasSet(fs, "parallels-clone-mode") {
		cfg.Parallels.CloneMode = *v.CloneMode
	}
	if core.FlagWasSet(fs, "parallels-host") {
		cfg.Parallels.Host = *v.Host
		// An explicit host is a direct-host override, not another fleet hint.
		// Leaving configured fleet candidates here silently replaces the flag.
		cfg.Parallels.Hosts = nil
		cfg.Parallels.SelectedHost = ""
	}
	if core.FlagWasSet(fs, "parallels-host-user") {
		cfg.Parallels.HostUser = *v.HostUser
	}
	if core.FlagWasSet(fs, "parallels-host-key") {
		cfg.Parallels.HostKey = core.ExpandUserPath(*v.HostKey)
	}
	if core.FlagWasSet(fs, "parallels-vm-root") {
		cfg.Parallels.VMRoot = core.ExpandUserPath(*v.VMRoot)
	}
	if core.FlagWasSet(fs, "parallels-user") {
		cfg.Parallels.User = *v.User
		cfg.SSHUser = *v.User
	}
	if core.FlagWasSet(fs, "parallels-work-root") {
		cfg.Parallels.WorkRoot = *v.WorkRoot
		cfg.WorkRoot = *v.WorkRoot
	}
	if core.FlagWasSet(fs, "parallels-startup-timeout") {
		if err := core.ApplyLeaseDuration(&cfg.Parallels.StartupTimeout, *v.StartupTimeout); err != nil {
			return err
		}
	}
	return nil
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return NewBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	doctor, ok := backend.(core.DoctorBackend)
	if !ok {
		return nil, core.Exit(2, "parallels doctor backend unavailable")
	}
	return doctor, nil
}

func (Provider) NativeCheckpointCapability(req core.NativeCheckpointRequest) (core.NativeCheckpointCapability, bool) {
	if req.Server.CloudID == "" {
		return core.NativeCheckpointCapability{}, false
	}
	if core.NormalizeCheckpointStrategy(req.Strategy) == core.CheckpointStrategyImage {
		return core.NativeCheckpointCapability{}, false
	}
	return core.NativeCheckpointCapability{Kind: core.CheckpointKindParallels, Direct: true}, true
}

func (Provider) ApplyNativeCheckpointForkConfig(req core.NativeCheckpointForkRequest) error {
	if req.Record.Kind != core.CheckpointKindParallels {
		return core.Exit(2, "provider=parallels does not support checkpoint kind=%s", req.Record.Kind)
	}
	cfg := req.Config
	cfg.Provider = "parallels"
	cfg.Coordinator = ""
	cfg.CoordToken = ""
	cfg.Parallels.SourceID = req.Record.Resource
	cfg.Parallels.SourceSnapshotID = req.Record.ImageID
	core.ApplyParallelsHostRefConfig(cfg, req.Record.Region)
	return nil
}
