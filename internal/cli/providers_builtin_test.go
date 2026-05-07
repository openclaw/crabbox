package cli

import (
	"context"
	"flag"
)

func init() {
	RegisterProvider(testHetznerProvider{})
	RegisterProvider(testAWSProvider{})
	RegisterProvider(testAzureProvider{})
	RegisterProvider(testStaticSSHProvider{})
	RegisterProvider(testBlacksmithProvider{})
	RegisterProvider(testDaytonaProvider{})
	RegisterProvider(testIsloProvider{})
}

type testAzureProvider struct{}

func (testAzureProvider) Name() string      { return "azure" }
func (testAzureProvider) Aliases() []string { return nil }
func (testAzureProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name: "azure",
		Kind: ProviderKindSSHLease,
		Targets: []TargetSpec{
			{OS: targetLinux},
			{OS: targetWindows, WindowsMode: windowsModeNormal},
		},
		Features:    FeatureSet{FeatureSSH, FeatureCrabboxSync, FeatureCleanup, FeatureDesktop, FeatureBrowser, FeatureCode, FeatureTailscale},
		Coordinator: CoordinatorSupported,
	}
}
func (testAzureProvider) RegisterFlags(*flag.FlagSet, Config) any { return noProviderFlags{} }
func (testAzureProvider) ApplyFlags(*Config, *flag.FlagSet, any) error {
	return nil
}
func (p testAzureProvider) Configure(cfg Config, rt Runtime) (Backend, error) {
	return testSSHBackend{spec: p.Spec()}, nil
}

type testHetznerProvider struct{}

func (testHetznerProvider) Name() string      { return "hetzner" }
func (testHetznerProvider) Aliases() []string { return nil }
func (testHetznerProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        "hetzner",
		Kind:        ProviderKindSSHLease,
		Targets:     []TargetSpec{{OS: targetLinux}},
		Features:    FeatureSet{FeatureSSH, FeatureCrabboxSync, FeatureCleanup, FeatureDesktop, FeatureBrowser, FeatureCode, FeatureTailscale},
		Coordinator: CoordinatorSupported,
	}
}
func (testHetznerProvider) RegisterFlags(*flag.FlagSet, Config) any { return noProviderFlags{} }
func (testHetznerProvider) ApplyFlags(*Config, *flag.FlagSet, any) error {
	return nil
}
func (p testHetznerProvider) Configure(cfg Config, rt Runtime) (Backend, error) {
	return testSSHBackend{spec: p.Spec()}, nil
}

type testAWSProvider struct{}

func (testAWSProvider) Name() string      { return "aws" }
func (testAWSProvider) Aliases() []string { return nil }
func (testAWSProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name: "aws",
		Kind: ProviderKindSSHLease,
		Targets: []TargetSpec{
			{OS: targetLinux},
			{OS: targetWindows, WindowsMode: windowsModeNormal},
			{OS: targetWindows, WindowsMode: windowsModeWSL2},
			{OS: targetMacOS},
		},
		Features:    FeatureSet{FeatureSSH, FeatureCrabboxSync, FeatureCleanup, FeatureDesktop, FeatureBrowser, FeatureCode},
		Coordinator: CoordinatorSupported,
	}
}
func (testAWSProvider) RegisterFlags(*flag.FlagSet, Config) any { return noProviderFlags{} }
func (testAWSProvider) ApplyFlags(*Config, *flag.FlagSet, any) error {
	return nil
}
func (p testAWSProvider) Configure(cfg Config, rt Runtime) (Backend, error) {
	return testSSHBackend{spec: p.Spec()}, nil
}

type testStaticSSHProvider struct{}

func (testStaticSSHProvider) Name() string { return staticProvider }
func (testStaticSSHProvider) Aliases() []string {
	return []string{"static", "static-ssh"}
}
func (testStaticSSHProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name: staticProvider,
		Kind: ProviderKindSSHLease,
		Targets: []TargetSpec{
			{OS: targetLinux},
			{OS: targetWindows, WindowsMode: windowsModeNormal},
			{OS: targetWindows, WindowsMode: windowsModeWSL2},
			{OS: targetMacOS},
		},
		Features:    FeatureSet{FeatureSSH, FeatureCrabboxSync, FeatureDesktop, FeatureBrowser, FeatureCode},
		Coordinator: CoordinatorNever,
	}
}
func (testStaticSSHProvider) RegisterFlags(*flag.FlagSet, Config) any { return noProviderFlags{} }
func (testStaticSSHProvider) ApplyFlags(*Config, *flag.FlagSet, any) error {
	return nil
}
func (p testStaticSSHProvider) Configure(cfg Config, rt Runtime) (Backend, error) {
	return testSSHBackend{spec: p.Spec()}, nil
}

type testBlacksmithProvider struct{}

func (testBlacksmithProvider) Name() string { return "blacksmith-testbox" }
func (testBlacksmithProvider) Aliases() []string {
	return []string{"blacksmith"}
}
func (testBlacksmithProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        "blacksmith-testbox",
		Kind:        ProviderKindDelegatedRun,
		Targets:     []TargetSpec{{OS: targetLinux}},
		Features:    nil,
		Coordinator: CoordinatorNever,
	}
}

type testBlacksmithFlagValues struct {
	Org      *string
	Workflow *string
	Job      *string
	Ref      *string
}

func (testBlacksmithProvider) RegisterFlags(fs *flag.FlagSet, defaults Config) any {
	return testBlacksmithFlagValues{
		Org:      fs.String("blacksmith-org", defaults.Blacksmith.Org, "Blacksmith organization"),
		Workflow: fs.String("blacksmith-workflow", defaults.Blacksmith.Workflow, "Blacksmith Testbox workflow file, name, or id"),
		Job:      fs.String("blacksmith-job", defaults.Blacksmith.Job, "Blacksmith Testbox workflow job"),
		Ref:      fs.String("blacksmith-ref", defaults.Blacksmith.Ref, "Blacksmith Testbox git ref"),
	}
}
func (testBlacksmithProvider) ApplyFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(testBlacksmithFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "blacksmith-org") {
		cfg.Blacksmith.Org = *v.Org
	}
	if flagWasSet(fs, "blacksmith-workflow") {
		cfg.Blacksmith.Workflow = *v.Workflow
	}
	if flagWasSet(fs, "blacksmith-job") {
		cfg.Blacksmith.Job = *v.Job
	}
	if flagWasSet(fs, "blacksmith-ref") {
		cfg.Blacksmith.Ref = *v.Ref
	}
	return nil
}
func (p testBlacksmithProvider) Configure(cfg Config, rt Runtime) (Backend, error) {
	return testDelegatedBackend{spec: p.Spec()}, nil
}

type testDaytonaProvider struct{}

func (testDaytonaProvider) Name() string      { return "daytona" }
func (testDaytonaProvider) Aliases() []string { return nil }
func (testDaytonaProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        "daytona",
		Kind:        ProviderKindSSHLease,
		Targets:     []TargetSpec{{OS: targetLinux}},
		Features:    FeatureSet{FeatureSSH, FeatureCrabboxSync},
		Coordinator: CoordinatorNever,
	}
}

type testDaytonaFlagValues struct {
	Snapshot *string
	Target   *string
	WorkRoot *string
}

func (testDaytonaProvider) RegisterFlags(fs *flag.FlagSet, defaults Config) any {
	return testDaytonaFlagValues{
		Snapshot: fs.String("daytona-snapshot", defaults.Daytona.Snapshot, "Daytona snapshot name"),
		Target:   fs.String("daytona-target", defaults.Daytona.Target, "Daytona compute target"),
		WorkRoot: fs.String("daytona-work-root", defaults.Daytona.WorkRoot, "Daytona sandbox work root"),
	}
}
func (testDaytonaProvider) ApplyFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if cfg.Provider == "daytona" {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=daytona")
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=daytona")
		}
	}
	v, ok := values.(testDaytonaFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "daytona-snapshot") {
		cfg.Daytona.Snapshot = *v.Snapshot
	}
	if flagWasSet(fs, "daytona-target") {
		cfg.Daytona.Target = *v.Target
	}
	if flagWasSet(fs, "daytona-work-root") {
		cfg.Daytona.WorkRoot = *v.WorkRoot
	}
	return nil
}
func (p testDaytonaProvider) Configure(cfg Config, rt Runtime) (Backend, error) {
	return testSSHBackend{spec: p.Spec()}, nil
}

type testIsloProvider struct{}

func (testIsloProvider) Name() string      { return "islo" }
func (testIsloProvider) Aliases() []string { return nil }
func (testIsloProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        "islo",
		Kind:        ProviderKindDelegatedRun,
		Targets:     []TargetSpec{{OS: targetLinux}},
		Features:    nil,
		Coordinator: CoordinatorNever,
	}
}

type testIsloFlagValues struct {
	Image    *string
	VCPUs    *int
	MemoryMB *int
}

func (testIsloProvider) RegisterFlags(fs *flag.FlagSet, defaults Config) any {
	return testIsloFlagValues{
		Image:    fs.String("islo-image", defaults.Islo.Image, "Islo sandbox image"),
		VCPUs:    fs.Int("islo-vcpus", defaults.Islo.VCPUs, "Islo sandbox vCPUs"),
		MemoryMB: fs.Int("islo-memory-mb", defaults.Islo.MemoryMB, "Islo sandbox memory in MB"),
	}
}
func (testIsloProvider) ApplyFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(testIsloFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "islo-image") {
		cfg.Islo.Image = *v.Image
	}
	if flagWasSet(fs, "islo-vcpus") {
		cfg.Islo.VCPUs = *v.VCPUs
	}
	if flagWasSet(fs, "islo-memory-mb") {
		cfg.Islo.MemoryMB = *v.MemoryMB
	}
	return nil
}
func (p testIsloProvider) Configure(cfg Config, rt Runtime) (Backend, error) {
	return testDelegatedBackend{spec: p.Spec()}, nil
}

type testDelegatedBackend struct {
	spec ProviderSpec
}

func (b testDelegatedBackend) Spec() ProviderSpec { return b.spec }
func (b testDelegatedBackend) Warmup(context.Context, WarmupRequest) error {
	return nil
}
func (b testDelegatedBackend) Run(context.Context, RunRequest) (RunResult, error) {
	return RunResult{}, nil
}
func (b testDelegatedBackend) List(context.Context, ListRequest) ([]LeaseView, error) {
	return nil, nil
}
func (b testDelegatedBackend) Status(context.Context, StatusRequest) (StatusView, error) {
	return StatusView{}, nil
}
func (b testDelegatedBackend) Stop(context.Context, StopRequest) error {
	return nil
}

type testSSHBackend struct {
	spec ProviderSpec
}

func (b testSSHBackend) Spec() ProviderSpec { return b.spec }
func (b testSSHBackend) Acquire(context.Context, AcquireRequest) (LeaseTarget, error) {
	return LeaseTarget{}, nil
}
func (b testSSHBackend) Resolve(context.Context, ResolveRequest) (LeaseTarget, error) {
	return LeaseTarget{}, nil
}
func (b testSSHBackend) List(context.Context, ListRequest) ([]LeaseView, error) {
	return nil, nil
}
func (b testSSHBackend) ReleaseLease(context.Context, ReleaseLeaseRequest) error {
	return nil
}
func (b testSSHBackend) Touch(context.Context, TouchRequest) (Server, error) {
	return Server{}, nil
}
