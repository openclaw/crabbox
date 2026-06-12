package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"strings"
)

func init() {
	RegisterProvider(testHetznerProvider{})
	RegisterProvider(testDigitalOceanProvider{})
	RegisterProvider(testAWSProvider{})
	RegisterProvider(testAzureProvider{})
	RegisterProvider(testAzureDynamicSessionsProvider{})
	RegisterProvider(testGCPProvider{})
	RegisterProvider(testIncusProvider{})
	RegisterProvider(testProxmoxProvider{})
	RegisterProvider(testXCPNgProvider{})
	RegisterProvider(testStaticSSHProvider{})
	RegisterProvider(testExternalProvider{})
	RegisterProvider(testExeDevProvider{})
	RegisterProvider(testRunPodProvider{})
	RegisterProvider(testBlacksmithProvider{})
	RegisterProvider(testNamespaceProvider{})
	RegisterProvider(testMorphProvider{})
	RegisterProvider(testDaytonaProvider{})
	RegisterProvider(testIsloProvider{})
	RegisterProvider(testFreestyleProvider{})
	RegisterProvider(testE2BProvider{})
	RegisterProvider(testModalProvider{})
	RegisterProvider(testCloudflareProvider{})
	RegisterProvider(testSpritesProvider{})
	RegisterProvider(testLocalContainerProvider{})
	RegisterProvider(testAppleVZProvider{})
	RegisterProvider(testDockerSandboxProvider{})
	RegisterProvider(testMultipassProvider{})
	RegisterProvider(testTartProvider{})
	RegisterProvider(testHyperVProvider{})
	RegisterProvider(testParallelsProvider{})
	RegisterProvider(testWandbProvider{})
	RegisterProvider(testServiceControlProvider{})
}

type testExternalProvider struct{}

func (testExternalProvider) Name() string      { return "external" }
func (testExternalProvider) Aliases() []string { return nil }
func (testExternalProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        "external",
		Family:      "external",
		Kind:        ProviderKindSSHLease,
		Targets:     []TargetSpec{{OS: targetLinux}},
		Features:    FeatureSet{FeatureSSH, FeatureCrabboxSync, FeatureCleanup, FeatureDesktop, FeatureBrowser, FeatureCode},
		Coordinator: CoordinatorNever,
	}
}
func (testExternalProvider) RegisterFlags(fs *flag.FlagSet, defaults Config) any {
	return fs.String("external-routing-file", defaults.External.RoutingFile, "")
}
func (testExternalProvider) ApplyFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if flagWasSet(fs, "external-routing-file") {
		path, _ := values.(*string)
		if path != nil {
			routing, err := LoadExternalRouting(*path)
			if err != nil {
				return err
			}
			cfg.External = routing
		}
	}
	if strings.TrimSpace(cfg.External.Command) == "" {
		return exit(2, "external command is required")
	}
	return nil
}
func (p testExternalProvider) Configure(cfg Config, _ Runtime) (Backend, error) {
	return testExternalSSHBackend{testSSHBackend: testSSHBackend{spec: p.Spec()}, cfg: cfg}, nil
}

type testExternalSSHBackend struct {
	testSSHBackend
	cfg Config
}

func (b testExternalSSHBackend) Resolve(_ context.Context, req ResolveRequest) (LeaseTarget, error) {
	return LeaseTarget{LeaseID: req.ID, Server: Server{Name: b.cfg.External.Command}}, nil
}

type testAzureProvider struct{}

func (testAzureProvider) Name() string      { return "azure" }
func (testAzureProvider) Aliases() []string { return nil }
func (testAzureProvider) RoutingFlagNames() []string {
	return []string{"azure-backend"}
}
func (testAzureProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:   "azure",
		Family: "azure",
		Kind:   ProviderKindSSHLease,
		Targets: []TargetSpec{
			{OS: targetLinux},
			{OS: targetWindows, WindowsMode: windowsModeNormal},
			{OS: targetWindows, WindowsMode: windowsModeWSL2},
		},
		Features:    FeatureSet{FeatureSSH, FeatureCrabboxSync, FeatureCleanup, FeatureDesktop, FeatureBrowser, FeatureCode, FeatureTailscale},
		Coordinator: CoordinatorSupported,
	}
}
func (testAzureProvider) RegisterFlags(fs *flag.FlagSet, defaults Config) any {
	return struct{ Backend *string }{
		Backend: fs.String("azure-backend", defaults.AzureBackend, ""),
	}
}
func (testAzureProvider) RouteConfig(cfg *Config, fs *flag.FlagSet, values any) error {
	backend := cfg.AzureBackend
	if fs != nil && flagWasSet(fs, "azure-backend") {
		flags, _ := values.(struct{ Backend *string })
		if flags.Backend != nil {
			backend = *flags.Backend
		}
	}
	normalized, err := NormalizeAzureBackend(backend)
	if err != nil {
		return exit(2, "%s", err)
	}
	cfg.AzureBackend = normalized
	if normalized == AzureBackendDynamicSessions {
		cfg.Provider = "azure-dynamic-sessions"
	} else {
		cfg.Provider = "azure"
	}
	return nil
}
func (p testAzureProvider) ApplyFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	return p.RouteConfig(cfg, fs, values)
}
func (testAzureProvider) ServerTypeForConfig(cfg Config) string {
	return azureVMSizeCandidatesForConfig(cfg)[0]
}
func (testAzureProvider) ServerTypeForClass(class string) string {
	return azureVMSizeCandidatesForClass(class)[0]
}
func (p testAzureProvider) Configure(cfg Config, rt Runtime) (Backend, error) {
	return testSSHBackend{spec: p.Spec()}, nil
}
func (testAzureProvider) NativeCheckpointCapability(req NativeCheckpointRequest) (NativeCheckpointCapability, bool) {
	if req.Config.Coordinator == "" || req.Server.CloudID == "" || firstNonBlank(req.Target.TargetOS, req.Config.TargetOS) != targetLinux {
		return NativeCheckpointCapability{}, false
	}
	if normalizeCheckpointStrategy(req.Strategy) == checkpointStrategyImage {
		return NativeCheckpointCapability{
			Kind:              checkpointKindAzure,
			CreateUnsupported: "Azure managed images require a stopped/generalized source VM; use --strategy disk-snapshot for active Azure leases",
		}, true
	}
	return NativeCheckpointCapability{Kind: checkpointKindAzureOS}, true
}
func (testAzureProvider) ApplyNativeCheckpointForkConfig(req NativeCheckpointForkRequest) error {
	switch req.Record.Kind {
	case checkpointKindAzure:
		req.Config.AzureImage = firstNonBlank(req.Record.Resource, req.Record.ImageID)
	case checkpointKindAzureOS:
		req.Config.AzureSnapshot = firstNonBlank(req.Record.Resource, req.Record.ImageID)
	default:
		return exit(2, "provider=azure does not support checkpoint kind=%s", req.Record.Kind)
	}
	if req.Record.Region != "" {
		req.Config.AzureLocation = req.Record.Region
	}
	if req.AzureOSDiskExplicit {
		mode, err := NormalizeAzureOSDiskMode(req.AzureOSDisk)
		if err != nil {
			return err
		}
		req.Config.AzureOSDisk = mode
		req.Config.AzureOSDiskExplicit = true
	}
	return nil
}

type testAzureDynamicSessionsProvider struct{}

func (testAzureDynamicSessionsProvider) Name() string      { return "azure-dynamic-sessions" }
func (testAzureDynamicSessionsProvider) Aliases() []string { return nil }
func (testAzureDynamicSessionsProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        "azure-dynamic-sessions",
		Family:      "azure",
		Kind:        ProviderKindDelegatedRun,
		Targets:     []TargetSpec{{OS: targetLinux}},
		Features:    FeatureSet{FeatureArchiveSync},
		Coordinator: CoordinatorNever,
	}
}
func (testAzureDynamicSessionsProvider) RouteConfig(cfg *Config, _ *flag.FlagSet, _ any) error {
	cfg.AzureBackend = AzureBackendDynamicSessions
	return nil
}
func (testAzureDynamicSessionsProvider) RegisterFlags(*flag.FlagSet, Config) any {
	return noProviderFlags{}
}
func (testAzureDynamicSessionsProvider) ApplyFlags(*Config, *flag.FlagSet, any) error {
	return nil
}
func (testAzureDynamicSessionsProvider) ServerTypeForConfig(Config) string { return "" }
func (testAzureDynamicSessionsProvider) ServerTypeForClass(string) string  { return "" }
func (p testAzureDynamicSessionsProvider) Configure(cfg Config, rt Runtime) (Backend, error) {
	return testDelegatedBackend{spec: p.Spec()}, nil
}

type testWandbProvider struct{}

func (testWandbProvider) Name() string      { return "wandb" }
func (testWandbProvider) Aliases() []string { return nil }
func (testWandbProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        "wandb",
		Family:      "wandb",
		Kind:        ProviderKindDelegatedRun,
		Targets:     []TargetSpec{{OS: targetLinux}},
		Features:    FeatureSet{FeatureArchiveSync},
		Coordinator: CoordinatorNever,
	}
}
func (testWandbProvider) RegisterFlags(*flag.FlagSet, Config) any { return noProviderFlags{} }
func (testWandbProvider) ApplyFlags(*Config, *flag.FlagSet, any) error {
	return nil
}
func (p testWandbProvider) Configure(cfg Config, rt Runtime) (Backend, error) {
	return testDelegatedBackend{spec: p.Spec()}, nil
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

type testDigitalOceanProvider struct{}

func (testDigitalOceanProvider) Name() string      { return "digitalocean" }
func (testDigitalOceanProvider) Aliases() []string { return nil }
func (testDigitalOceanProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        "digitalocean",
		Family:      "digitalocean",
		Kind:        ProviderKindSSHLease,
		Targets:     []TargetSpec{{OS: targetLinux}},
		Features:    FeatureSet{FeatureSSH, FeatureCrabboxSync, FeatureCleanup, FeatureTailscale},
		Coordinator: CoordinatorNever,
	}
}
func (testDigitalOceanProvider) RegisterFlags(*flag.FlagSet, Config) any { return noProviderFlags{} }
func (testDigitalOceanProvider) ApplyFlags(*Config, *flag.FlagSet, any) error {
	return nil
}
func (testDigitalOceanProvider) ServerTypeForConfig(Config) string { return "s-1vcpu-1gb" }
func (testDigitalOceanProvider) ServerTypeForClass(string) string  { return "s-1vcpu-1gb" }
func (p testDigitalOceanProvider) Configure(cfg Config, rt Runtime) (Backend, error) {
	return testSSHBackend{spec: p.Spec()}, nil
}

type testGCPProvider struct{}

func (testGCPProvider) Name() string { return "gcp" }
func (testGCPProvider) Aliases() []string {
	return []string{"google", "google-cloud"}
}
func (testGCPProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        "gcp",
		Kind:        ProviderKindSSHLease,
		Targets:     []TargetSpec{{OS: targetLinux}},
		Features:    FeatureSet{FeatureSSH, FeatureCrabboxSync, FeatureCleanup, FeatureTailscale},
		Coordinator: CoordinatorSupported,
	}
}
func (testGCPProvider) RegisterFlags(*flag.FlagSet, Config) any { return noProviderFlags{} }
func (testGCPProvider) ApplyFlags(*Config, *flag.FlagSet, any) error {
	return nil
}
func (p testGCPProvider) Configure(cfg Config, rt Runtime) (Backend, error) {
	return testSSHBackend{spec: p.Spec()}, nil
}
func (testGCPProvider) NativeCheckpointCapability(req NativeCheckpointRequest) (NativeCheckpointCapability, bool) {
	if req.Config.Coordinator == "" || req.Server.CloudID == "" || firstNonBlank(req.Target.TargetOS, req.Config.TargetOS) != targetLinux {
		return NativeCheckpointCapability{}, false
	}
	if normalizeCheckpointStrategy(req.Strategy) == checkpointStrategyImage {
		return NativeCheckpointCapability{Kind: checkpointKindGCP}, true
	}
	return NativeCheckpointCapability{Kind: checkpointKindGCPDisk}, true
}
func (testGCPProvider) ApplyNativeCheckpointForkConfig(req NativeCheckpointForkRequest) error {
	switch req.Record.Kind {
	case checkpointKindGCP:
		req.Config.GCPMachineImage = firstNonBlank(req.Record.Resource, req.Record.ImageID)
	case checkpointKindGCPDisk:
		req.Config.GCPSnapshot = firstNonBlank(req.Record.Resource, req.Record.ImageID)
	default:
		return exit(2, "provider=gcp does not support checkpoint kind=%s", req.Record.Kind)
	}
	if req.Record.Region != "" {
		req.Config.GCPZone = req.Record.Region
	}
	if req.Record.Project != "" {
		req.Config.GCPProject = req.Record.Project
		req.Config.gcpProjectExplicit = true
	}
	return nil
}

type testAWSProvider struct{}

var testAWSBackendOverride SSHLeaseBackend

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
	if testAWSBackendOverride != nil {
		return testAWSBackendOverride, nil
	}
	return testSSHBackend{spec: p.Spec()}, nil
}
func (testAWSProvider) NativeCheckpointCapability(req NativeCheckpointRequest) (NativeCheckpointCapability, bool) {
	if req.Server.CloudID == "" || isWindowsNativeTarget(req.Target) {
		return NativeCheckpointCapability{}, false
	}
	targetOS := firstNonBlank(req.Target.TargetOS, req.Config.TargetOS)
	if targetOS != targetLinux && targetOS != targetMacOS {
		return NativeCheckpointCapability{}, false
	}
	strategy := normalizeCheckpointStrategy(req.Strategy)
	if req.Config.Coordinator == "" {
		if targetOS != targetMacOS && strategy != checkpointStrategyImage {
			return NativeCheckpointCapability{}, false
		}
		return NativeCheckpointCapability{Kind: checkpointKindAWSAMI, Direct: true}, true
	}
	if targetOS == targetMacOS || strategy == checkpointStrategyImage {
		return NativeCheckpointCapability{Kind: checkpointKindAWSAMI}, true
	}
	return NativeCheckpointCapability{Kind: checkpointKindAWSEBS}, true
}
func (testAWSProvider) ApplyNativeCheckpointForkConfig(req NativeCheckpointForkRequest) error {
	switch req.Record.Kind {
	case checkpointKindAWSAMI:
		req.Config.AWSAMI = req.Record.ImageID
	case checkpointKindAWSEBS:
		req.Config.AWSSnapshot = req.Record.ImageID
	default:
		return exit(2, "provider=aws does not support checkpoint kind=%s", req.Record.Kind)
	}
	if req.Record.Region != "" {
		req.Config.AWSRegion = req.Record.Region
	}
	if req.Config.TargetOS == targetMacOS {
		if req.Record.Direct && req.Record.HostID != "" {
			req.Config.HostID = req.Record.HostID
			req.Config.AWSMacHostID = req.Record.HostID
		}
		if !req.MarketExplicit {
			req.Config.Capacity.Market = "on-demand"
		}
		normalizeTargetConfig(req.Config)
	}
	return nil
}

type testParallelsProvider struct{}

func (testParallelsProvider) Name() string      { return "parallels" }
func (testParallelsProvider) Aliases() []string { return nil }
func (testParallelsProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name: "parallels",
		Kind: ProviderKindSSHLease,
		Targets: []TargetSpec{
			{OS: targetLinux},
			{OS: targetMacOS},
			{OS: targetWindows, WindowsMode: windowsModeNormal},
			{OS: targetWindows, WindowsMode: windowsModeWSL2},
		},
		Features:    FeatureSet{FeatureSSH, FeatureCrabboxSync, FeatureCleanup, FeatureDesktop, FeatureBrowser, FeatureCode, FeatureCheckpoint, FeatureFork, FeatureRestore, FeatureSnapshot},
		Coordinator: CoordinatorNever,
	}
}
func (testParallelsProvider) RegisterFlags(*flag.FlagSet, Config) any { return noProviderFlags{} }
func (testParallelsProvider) ApplyFlags(*Config, *flag.FlagSet, any) error {
	return nil
}
func (p testParallelsProvider) Configure(cfg Config, rt Runtime) (Backend, error) {
	return testSSHBackend{spec: p.Spec()}, nil
}
func (testParallelsProvider) NativeCheckpointCapability(req NativeCheckpointRequest) (NativeCheckpointCapability, bool) {
	if req.Server.CloudID == "" || normalizeCheckpointStrategy(req.Strategy) == checkpointStrategyImage {
		return NativeCheckpointCapability{}, false
	}
	return NativeCheckpointCapability{Kind: checkpointKindParallels, Direct: true}, true
}
func (testParallelsProvider) ApplyNativeCheckpointForkConfig(req NativeCheckpointForkRequest) error {
	if req.Record.Kind != checkpointKindParallels {
		return exit(2, "provider=parallels does not support checkpoint kind=%s", req.Record.Kind)
	}
	req.Config.Provider = "parallels"
	req.Config.Coordinator = ""
	req.Config.CoordToken = ""
	req.Config.Parallels.SourceID = req.Record.Resource
	req.Config.Parallels.SourceSnapshotID = req.Record.ImageID
	applyParallelsHostRefConfig(req.Config, req.Record.Region)
	return nil
}

type testProxmoxProvider struct{}

type testIncusProvider struct{}

func (testIncusProvider) Name() string      { return "incus" }
func (testIncusProvider) Aliases() []string { return nil }
func (testIncusProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        "incus",
		Family:      "local-vm",
		Kind:        ProviderKindSSHLease,
		Targets:     []TargetSpec{{OS: targetLinux}},
		Features:    FeatureSet{FeatureSSH, FeatureCrabboxSync, FeatureCleanup},
		Coordinator: CoordinatorNever,
	}
}
func (testIncusProvider) RegisterFlags(fs *flag.FlagSet, defaults Config) any {
	return testIncusFlagValues{
		InstanceType:    fs.String("incus-instance-type", defaults.Incus.InstanceType, "Incus instance type"),
		Image:           fs.String("incus-image", defaults.Incus.Image, "Incus image"),
		User:            fs.String("incus-user", defaults.Incus.User, "Incus SSH user"),
		WorkRoot:        fs.String("incus-work-root", defaults.Incus.WorkRoot, "Incus work root"),
		ProxyListenPort: fs.String("incus-proxy-listen-port", defaults.Incus.ProxyListenPort, "Incus proxy listen port"),
	}
}
func (testIncusProvider) ApplyFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(testIncusFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "incus-instance-type") {
		switch strings.ToLower(strings.TrimSpace(*v.InstanceType)) {
		case "vm", "virtual-machine", "virtual_machine":
			cfg.Incus.InstanceType = "virtual-machine"
		default:
			cfg.Incus.InstanceType = strings.ToLower(strings.TrimSpace(*v.InstanceType))
		}
		cfg.ServerType = incusServerTypeForConfig(*cfg)
	}
	if flagWasSet(fs, "incus-image") {
		cfg.Incus.Image = *v.Image
		cfg.ServerType = incusServerTypeForConfig(*cfg)
	}
	if flagWasSet(fs, "incus-user") {
		cfg.Incus.User = *v.User
		cfg.SSHUser = *v.User
	}
	if flagWasSet(fs, "incus-work-root") {
		cfg.Incus.WorkRoot = *v.WorkRoot
		cfg.WorkRoot = *v.WorkRoot
	}
	if flagWasSet(fs, "incus-proxy-listen-port") {
		cfg.Incus.ProxyListenPort = *v.ProxyListenPort
		cfg.SSHPort = blank(*v.ProxyListenPort, cfg.SSHPort)
	}
	return nil
}
func (p testIncusProvider) Configure(cfg Config, rt Runtime) (Backend, error) {
	return testSSHBackend{spec: p.Spec()}, nil
}

type testIncusFlagValues struct {
	InstanceType    *string
	Image           *string
	User            *string
	WorkRoot        *string
	ProxyListenPort *string
}

func (testProxmoxProvider) Name() string      { return "proxmox" }
func (testProxmoxProvider) Aliases() []string { return nil }
func (testProxmoxProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        "proxmox",
		Kind:        ProviderKindSSHLease,
		Targets:     []TargetSpec{{OS: targetLinux}},
		Features:    FeatureSet{FeatureSSH, FeatureCrabboxSync, FeatureCleanup},
		Coordinator: CoordinatorNever,
	}
}
func (testProxmoxProvider) RegisterFlags(fs *flag.FlagSet, defaults Config) any {
	return testProxmoxFlagValues{
		APIURL:      fs.String("proxmox-api-url", defaults.Proxmox.APIURL, "Proxmox VE API URL"),
		Node:        fs.String("proxmox-node", defaults.Proxmox.Node, "Proxmox VE node name"),
		TemplateID:  fs.Int("proxmox-template-id", defaults.Proxmox.TemplateID, "Proxmox QEMU template VMID"),
		User:        fs.String("proxmox-user", defaults.Proxmox.User, "Proxmox VM user"),
		WorkRoot:    fs.String("proxmox-work-root", defaults.Proxmox.WorkRoot, "Proxmox VM work root"),
		InsecureTLS: fs.Bool("proxmox-insecure-tls", defaults.Proxmox.InsecureTLS, "allow self-signed Proxmox TLS certificates"),
	}
}
func (testProxmoxProvider) ApplyFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(testProxmoxFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "proxmox-api-url") {
		cfg.Proxmox.APIURL = *v.APIURL
	}
	if flagWasSet(fs, "proxmox-node") {
		cfg.Proxmox.Node = *v.Node
	}
	if flagWasSet(fs, "proxmox-template-id") {
		cfg.Proxmox.TemplateID = *v.TemplateID
		cfg.ServerType = proxmoxServerTypeForConfig(*cfg)
	}
	if flagWasSet(fs, "proxmox-user") {
		cfg.Proxmox.User = *v.User
		cfg.SSHUser = *v.User
	}
	if flagWasSet(fs, "proxmox-work-root") {
		cfg.Proxmox.WorkRoot = *v.WorkRoot
		cfg.WorkRoot = *v.WorkRoot
	}
	if flagWasSet(fs, "proxmox-insecure-tls") {
		cfg.Proxmox.InsecureTLS = *v.InsecureTLS
	}
	return nil
}
func (p testProxmoxProvider) Configure(cfg Config, rt Runtime) (Backend, error) {
	return testSSHBackend{spec: p.Spec()}, nil
}

type testProxmoxFlagValues struct {
	APIURL      *string
	Node        *string
	TemplateID  *int
	User        *string
	WorkRoot    *string
	InsecureTLS *bool
}

type testXCPNgProvider struct{}

func (testXCPNgProvider) Name() string      { return "xcp-ng" }
func (testXCPNgProvider) Aliases() []string { return nil }
func (testXCPNgProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        "xcp-ng",
		Family:      "xcp-ng",
		Kind:        ProviderKindSSHLease,
		Targets:     []TargetSpec{{OS: targetLinux}},
		Features:    FeatureSet{FeatureSSH, FeatureCrabboxSync, FeatureCleanup},
		Coordinator: CoordinatorNever,
	}
}
func (testXCPNgProvider) RegisterFlags(fs *flag.FlagSet, defaults Config) any {
	return testXCPNgFlagValues{
		APIURL:       fs.String("xcp-ng-api-url", defaults.XCPNg.APIURL, "XCP-ng API URL"),
		Username:     fs.String("xcp-ng-username", defaults.XCPNg.Username, "XCP-ng API username"),
		Template:     fs.String("xcp-ng-template", defaults.XCPNg.Template, "XCP-ng template name"),
		TemplateUUID: fs.String("xcp-ng-template-uuid", defaults.XCPNg.TemplateUUID, "XCP-ng template UUID"),
		SR:           fs.String("xcp-ng-sr", defaults.XCPNg.SR, "XCP-ng storage repository name"),
		SRUUID:       fs.String("xcp-ng-sr-uuid", defaults.XCPNg.SRUUID, "XCP-ng storage repository UUID"),
		Network:      fs.String("xcp-ng-network", defaults.XCPNg.Network, "XCP-ng network name"),
		NetworkUUID:  fs.String("xcp-ng-network-uuid", defaults.XCPNg.NetworkUUID, "XCP-ng network UUID"),
		Host:         fs.String("xcp-ng-host", defaults.XCPNg.Host, "XCP-ng host"),
		User:         fs.String("xcp-ng-user", defaults.XCPNg.User, "XCP-ng VM user"),
		WorkRoot:     fs.String("xcp-ng-work-root", defaults.XCPNg.WorkRoot, "XCP-ng VM work root"),
		InsecureTLS:  fs.Bool("xcp-ng-insecure-tls", defaults.XCPNg.InsecureTLS, "allow self-signed XCP-ng TLS certificates"),
	}
}
func (testXCPNgProvider) ApplyFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(testXCPNgFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "xcp-ng-api-url") {
		cfg.XCPNg.APIURL = *v.APIURL
	}
	if flagWasSet(fs, "xcp-ng-username") {
		cfg.XCPNg.Username = *v.Username
	}
	if flagWasSet(fs, "xcp-ng-template") {
		cfg.XCPNg.Template = *v.Template
		cfg.ServerType = xcpNgTestServerTypeForConfig(*cfg)
	}
	if flagWasSet(fs, "xcp-ng-template-uuid") {
		cfg.XCPNg.TemplateUUID = *v.TemplateUUID
		cfg.ServerType = xcpNgTestServerTypeForConfig(*cfg)
	}
	if flagWasSet(fs, "xcp-ng-sr") {
		cfg.XCPNg.SR = *v.SR
	}
	if flagWasSet(fs, "xcp-ng-sr-uuid") {
		cfg.XCPNg.SRUUID = *v.SRUUID
	}
	if flagWasSet(fs, "xcp-ng-network") {
		cfg.XCPNg.Network = *v.Network
	}
	if flagWasSet(fs, "xcp-ng-network-uuid") {
		cfg.XCPNg.NetworkUUID = *v.NetworkUUID
	}
	if flagWasSet(fs, "xcp-ng-host") {
		cfg.XCPNg.Host = *v.Host
	}
	if flagWasSet(fs, "xcp-ng-user") {
		cfg.XCPNg.User = *v.User
		cfg.SSHUser = *v.User
	}
	if flagWasSet(fs, "xcp-ng-work-root") {
		cfg.XCPNg.WorkRoot = *v.WorkRoot
		cfg.WorkRoot = *v.WorkRoot
	}
	if flagWasSet(fs, "xcp-ng-insecure-tls") {
		cfg.XCPNg.InsecureTLS = *v.InsecureTLS
	}
	return nil
}
func (testXCPNgProvider) ServerTypeForConfig(cfg Config) string {
	return xcpNgTestServerTypeForConfig(cfg)
}
func (testXCPNgProvider) ServerTypeForClass(string) string { return "template" }
func (p testXCPNgProvider) Configure(cfg Config, rt Runtime) (Backend, error) {
	return testSSHBackend{spec: p.Spec()}, nil
}

type testXCPNgFlagValues struct {
	APIURL       *string
	Username     *string
	Template     *string
	TemplateUUID *string
	SR           *string
	SRUUID       *string
	Network      *string
	NetworkUUID  *string
	Host         *string
	User         *string
	WorkRoot     *string
	InsecureTLS  *bool
}

func xcpNgTestServerTypeForConfig(cfg Config) string {
	if cfg.XCPNg.TemplateUUID != "" {
		return "template-" + cfg.XCPNg.TemplateUUID
	}
	if cfg.XCPNg.Template != "" {
		return "template-" + normalizeLeaseSlug(cfg.XCPNg.Template)
	}
	return "template"
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

type testExeDevProvider struct{}

func (testExeDevProvider) Name() string { return "exe-dev" }
func (testExeDevProvider) Aliases() []string {
	return []string{"exe", "exedev"}
}
func (testExeDevProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        "exe-dev",
		Kind:        ProviderKindSSHLease,
		Targets:     []TargetSpec{{OS: targetLinux}},
		Features:    FeatureSet{FeatureSSH, FeatureCrabboxSync},
		Coordinator: CoordinatorNever,
	}
}
func (testExeDevProvider) RegisterFlags(fs *flag.FlagSet, defaults Config) any {
	return testExeDevFlagValues{
		ControlHost: fs.String("exe-dev-control-host", defaults.ExeDev.ControlHost, "exe.dev SSH API host"),
		Image:       fs.String("exe-dev-image", defaults.ExeDev.Image, "exe.dev VM image"),
		User:        fs.String("exe-dev-user", defaults.ExeDev.User, "exe.dev VM SSH user"),
		WorkRoot:    fs.String("exe-dev-work-root", defaults.ExeDev.WorkRoot, "exe.dev VM work root"),
	}
}
func (testExeDevProvider) ApplyFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(testExeDevFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "exe-dev-control-host") {
		cfg.ExeDev.ControlHost = *v.ControlHost
	}
	if flagWasSet(fs, "exe-dev-image") {
		cfg.ExeDev.Image = *v.Image
	}
	if flagWasSet(fs, "exe-dev-user") {
		cfg.ExeDev.User = *v.User
		cfg.SSHUser = *v.User
	}
	if flagWasSet(fs, "exe-dev-work-root") {
		cfg.ExeDev.WorkRoot = *v.WorkRoot
		cfg.WorkRoot = *v.WorkRoot
	}
	return nil
}
func (p testExeDevProvider) Configure(cfg Config, rt Runtime) (Backend, error) {
	return testSSHBackend{spec: p.Spec()}, nil
}

type testExeDevFlagValues struct {
	ControlHost *string
	Image       *string
	User        *string
	WorkRoot    *string
}

type testRunPodProvider struct{}

func (testRunPodProvider) Name() string      { return "runpod" }
func (testRunPodProvider) Aliases() []string { return nil }
func (testRunPodProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        "runpod",
		Kind:        ProviderKindSSHLease,
		Targets:     []TargetSpec{{OS: targetLinux}},
		Features:    FeatureSet{FeatureSSH, FeatureCrabboxSync},
		Coordinator: CoordinatorNever,
	}
}
func (testRunPodProvider) RegisterFlags(*flag.FlagSet, Config) any { return noProviderFlags{} }
func (testRunPodProvider) ApplyFlags(*Config, *flag.FlagSet, any) error {
	return nil
}
func (p testRunPodProvider) Configure(cfg Config, rt Runtime) (Backend, error) {
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
		Features:    FeatureSet{FeatureCacheVolume, FeatureRunProof, FeatureRunSession},
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

type testNamespaceProvider struct{}

func (testNamespaceProvider) Name() string { return "namespace-devbox" }
func (testNamespaceProvider) Aliases() []string {
	return []string{"namespace", "namespace-devboxes"}
}
func (testNamespaceProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        "namespace-devbox",
		Kind:        ProviderKindSSHLease,
		Targets:     []TargetSpec{{OS: targetLinux}},
		Features:    FeatureSet{FeatureSSH, FeatureCrabboxSync},
		Coordinator: CoordinatorNever,
	}
}
func (testNamespaceProvider) RegisterFlags(fs *flag.FlagSet, defaults Config) any {
	return testNamespaceFlagValues{
		Image:    fs.String("namespace-image", defaults.Namespace.Image, "Namespace Devbox image"),
		Size:     fs.String("namespace-size", defaults.Namespace.Size, "Namespace Devbox size"),
		WorkRoot: fs.String("namespace-work-root", defaults.Namespace.WorkRoot, "Namespace Devbox work root"),
	}
}
func (testNamespaceProvider) ApplyFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(testNamespaceFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "namespace-image") {
		cfg.Namespace.Image = *v.Image
	}
	if flagWasSet(fs, "namespace-size") {
		cfg.Namespace.Size = *v.Size
	}
	if flagWasSet(fs, "namespace-work-root") {
		cfg.Namespace.WorkRoot = *v.WorkRoot
	}
	return nil
}
func (p testNamespaceProvider) Configure(cfg Config, rt Runtime) (Backend, error) {
	return testSSHBackend{spec: p.Spec()}, nil
}

type testNamespaceFlagValues struct {
	Image    *string
	Size     *string
	WorkRoot *string
}

type testMorphProvider struct{}

func (testMorphProvider) Name() string      { return "morph" }
func (testMorphProvider) Aliases() []string { return nil }
func (testMorphProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        "morph",
		Kind:        ProviderKindSSHLease,
		Targets:     []TargetSpec{{OS: targetLinux}},
		Features:    FeatureSet{FeatureSSH, FeatureCrabboxSync},
		Coordinator: CoordinatorNever,
	}
}

type testMorphFlagValues struct {
	APIURL          *string
	Snapshot        *string
	WorkRoot        *string
	DeleteOnRelease *bool
	WakeOnSSH       *bool
}

func (testMorphProvider) RegisterFlags(fs *flag.FlagSet, defaults Config) any {
	return testMorphFlagValues{
		APIURL:          fs.String("morph-api-url", defaults.Morph.APIURL, "Morph API URL"),
		Snapshot:        fs.String("morph-snapshot", defaults.Morph.Snapshot, "Morph snapshot"),
		WorkRoot:        fs.String("morph-work-root", defaults.Morph.WorkRoot, "Morph work root"),
		DeleteOnRelease: fs.Bool("morph-delete-on-release", defaults.Morph.DeleteOnRelease, "Morph delete on release"),
		WakeOnSSH:       fs.Bool("morph-wake-on-ssh", defaults.Morph.WakeOnSSH, "Morph wake on ssh"),
	}
}

func (testMorphProvider) ApplyFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if cfg.Provider == "morph" {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=morph")
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=morph; use --morph-snapshot")
		}
		if cfg.TargetOS != "" && cfg.TargetOS != targetLinux {
			return exit(2, "provider=morph supports target=linux only")
		}
	}
	v, ok := values.(testMorphFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "morph-api-url") {
		cfg.Morph.APIURL = *v.APIURL
	}
	if flagWasSet(fs, "morph-snapshot") {
		cfg.Morph.Snapshot = *v.Snapshot
	}
	if flagWasSet(fs, "morph-work-root") {
		cfg.Morph.WorkRoot = *v.WorkRoot
		cfg.WorkRoot = *v.WorkRoot
	}
	if flagWasSet(fs, "morph-delete-on-release") {
		cfg.Morph.DeleteOnRelease = *v.DeleteOnRelease
	}
	if flagWasSet(fs, "morph-wake-on-ssh") {
		cfg.Morph.WakeOnSSH = *v.WakeOnSSH
	}
	return nil
}

func (testMorphProvider) ServerTypeForConfig(cfg Config) string {
	return firstNonBlank(cfg.Morph.Snapshot, "snapshot")
}

func (testMorphProvider) ServerTypeForClass(string) string { return "snapshot" }

func (p testMorphProvider) Configure(cfg Config, rt Runtime) (Backend, error) {
	return testSSHBackend{spec: p.Spec()}, nil
}

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
	return testDaytonaBackend{testSSHBackend: testSSHBackend{spec: p.Spec()}}, nil
}

type testIsloProvider struct{}

func (testIsloProvider) Name() string      { return "islo" }
func (testIsloProvider) Aliases() []string { return nil }
func (testIsloProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:                "islo",
		Kind:                ProviderKindDelegatedRun,
		Targets:             []TargetSpec{{OS: targetLinux}},
		Features:            FeatureSet{FeatureSSH, FeatureURLBridge, FeatureRunSession, FeatureTailscale, FeaturePauseResume},
		Coordinator:         CoordinatorNever,
		TailscaleEgressOnly: true,
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
		cfg.isloImageExplicit = true
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
	return testIsloBackend{
		testDelegatedBackend: testDelegatedBackend{spec: p.Spec()},
		stderr:               rt.Stderr,
	}, nil
}

type testFreestyleProvider struct{}

func (testFreestyleProvider) Name() string      { return "freestyle" }
func (testFreestyleProvider) Aliases() []string { return nil }
func (testFreestyleProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        "freestyle",
		Kind:        ProviderKindDelegatedRun,
		Targets:     []TargetSpec{{OS: targetLinux}},
		Features:    FeatureSet{FeatureArchiveSync},
		Coordinator: CoordinatorNever,
	}
}

type testFreestyleFlagValues struct {
	APIURL   *string
	Workdir  *string
	VCPUs    *int
	MemoryGB *int
}

func (testFreestyleProvider) RegisterFlags(fs *flag.FlagSet, defaults Config) any {
	return testFreestyleFlagValues{
		APIURL:   fs.String("freestyle-api-url", defaults.Freestyle.APIURL, "Freestyle API URL"),
		Workdir:  fs.String("freestyle-workdir", defaults.Freestyle.Workdir, "Freestyle sandbox workdir"),
		VCPUs:    fs.Int("freestyle-vcpus", defaults.Freestyle.VCPUs, "Freestyle sandbox vCPUs"),
		MemoryGB: fs.Int("freestyle-memory-gb", defaults.Freestyle.MemoryGB, "Freestyle sandbox memory in GiB"),
	}
}
func (testFreestyleProvider) ApplyFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(testFreestyleFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "freestyle-api-url") {
		cfg.Freestyle.APIURL = *v.APIURL
	}
	if flagWasSet(fs, "freestyle-workdir") {
		cfg.Freestyle.Workdir = *v.Workdir
	}
	if flagWasSet(fs, "freestyle-vcpus") {
		cfg.Freestyle.VCPUs = *v.VCPUs
	}
	if flagWasSet(fs, "freestyle-memory-gb") {
		cfg.Freestyle.MemoryGB = *v.MemoryGB
	}
	return nil
}
func (p testFreestyleProvider) Configure(cfg Config, rt Runtime) (Backend, error) {
	return testDelegatedBackend{spec: p.Spec()}, nil
}

type testE2BProvider struct{}

func (testE2BProvider) Name() string      { return "e2b" }
func (testE2BProvider) Aliases() []string { return nil }
func (testE2BProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        "e2b",
		Kind:        ProviderKindDelegatedRun,
		Targets:     []TargetSpec{{OS: targetLinux}},
		Features:    FeatureSet{FeatureURLBridge},
		Coordinator: CoordinatorNever,
	}
}

type testE2BFlagValues struct {
	Template *string
	Workdir  *string
}

func (testE2BProvider) RegisterFlags(fs *flag.FlagSet, defaults Config) any {
	return testE2BFlagValues{
		Template: fs.String("e2b-template", defaults.E2B.Template, "E2B sandbox template ID"),
		Workdir:  fs.String("e2b-workdir", defaults.E2B.Workdir, "E2B sandbox workdir"),
	}
}
func (testE2BProvider) ApplyFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if cfg.Provider == "e2b" {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=e2b")
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=e2b")
		}
	}
	v, ok := values.(testE2BFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "e2b-template") {
		cfg.E2B.Template = *v.Template
	}
	if flagWasSet(fs, "e2b-workdir") {
		cfg.E2B.Workdir = *v.Workdir
	}
	return nil
}
func (p testE2BProvider) Configure(cfg Config, rt Runtime) (Backend, error) {
	return testDelegatedBackend{spec: p.Spec()}, nil
}

type testModalProvider struct{}

func (testModalProvider) Name() string      { return "modal" }
func (testModalProvider) Aliases() []string { return nil }
func (testModalProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        "modal",
		Kind:        ProviderKindDelegatedRun,
		Targets:     []TargetSpec{{OS: targetLinux}},
		Features:    FeatureSet{FeatureArchiveSync},
		Coordinator: CoordinatorNever,
	}
}

type testModalFlagValues struct {
	App     *string
	Image   *string
	Workdir *string
}

func (testModalProvider) RegisterFlags(fs *flag.FlagSet, defaults Config) any {
	return testModalFlagValues{
		App:     fs.String("modal-app", defaults.Modal.App, "Modal app name"),
		Image:   fs.String("modal-image", defaults.Modal.Image, "Modal sandbox image"),
		Workdir: fs.String("modal-workdir", defaults.Modal.Workdir, "Modal sandbox workdir"),
	}
}
func (testModalProvider) ApplyFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if cfg.Provider == "modal" {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=modal")
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=modal")
		}
	}
	v, ok := values.(testModalFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "modal-app") {
		cfg.Modal.App = *v.App
	}
	if flagWasSet(fs, "modal-image") {
		cfg.Modal.Image = *v.Image
	}
	if flagWasSet(fs, "modal-workdir") {
		cfg.Modal.Workdir = *v.Workdir
	}
	return nil
}
func (p testModalProvider) Configure(cfg Config, rt Runtime) (Backend, error) {
	return testDelegatedBackend{spec: p.Spec()}, nil
}

type testCloudflareProvider struct{}

var testCloudflareDoctorResult *DoctorResult

func (testCloudflareProvider) Name() string { return "cloudflare" }
func (testCloudflareProvider) Aliases() []string {
	return []string{"cf"}
}
func (testCloudflareProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        "cloudflare",
		Kind:        ProviderKindDelegatedRun,
		Targets:     []TargetSpec{{OS: targetLinux}},
		Features:    FeatureSet{FeatureArchiveSync, FeatureCleanup},
		Coordinator: CoordinatorNever,
	}
}
func (testCloudflareProvider) RegisterFlags(*flag.FlagSet, Config) any {
	return noProviderFlags{}
}
func (testCloudflareProvider) ApplyFlags(*Config, *flag.FlagSet, any) error {
	return nil
}
func (p testCloudflareProvider) Configure(cfg Config, rt Runtime) (Backend, error) {
	return testDoctorDelegatedBackend{testDelegatedBackend{spec: p.Spec()}}, nil
}

func (p testCloudflareProvider) ConfigureDoctor(cfg Config, rt Runtime) (DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	return backend.(DoctorBackend), nil
}

type testSpritesProvider struct{}

func (testSpritesProvider) Name() string      { return "sprites" }
func (testSpritesProvider) Aliases() []string { return nil }
func (testSpritesProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        "sprites",
		Kind:        ProviderKindSSHLease,
		Targets:     []TargetSpec{{OS: targetLinux}},
		Features:    FeatureSet{FeatureSSH, FeatureCrabboxSync},
		Coordinator: CoordinatorNever,
	}
}

type testSpritesFlagValues struct {
	APIURL   *string
	WorkRoot *string
}

func (testSpritesProvider) RegisterFlags(fs *flag.FlagSet, defaults Config) any {
	return testSpritesFlagValues{
		APIURL:   fs.String("sprites-api-url", defaults.Sprites.APIURL, "Sprites API URL"),
		WorkRoot: fs.String("sprites-work-root", defaults.Sprites.WorkRoot, "Sprites work root"),
	}
}
func (testSpritesProvider) ApplyFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if cfg.Provider == "sprites" {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=sprites")
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=sprites")
		}
	}
	v, ok := values.(testSpritesFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "sprites-api-url") {
		cfg.Sprites.APIURL = *v.APIURL
	}
	if flagWasSet(fs, "sprites-work-root") {
		cfg.Sprites.WorkRoot = *v.WorkRoot
	}
	return nil
}
func (p testSpritesProvider) Configure(cfg Config, rt Runtime) (Backend, error) {
	return testSSHBackend{spec: p.Spec()}, nil
}

type testLocalContainerProvider struct{}

func (testLocalContainerProvider) Name() string { return "local-container" }
func (testLocalContainerProvider) Aliases() []string {
	return []string{"docker", "container", "local-docker"}
}
func (testLocalContainerProvider) CreationOnlyFlagNames() []string {
	return []string{"local-container-volume"}
}
func (testLocalContainerProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        "local-container",
		Kind:        ProviderKindSSHLease,
		Targets:     []TargetSpec{{OS: targetLinux}},
		Features:    FeatureSet{FeatureSSH, FeatureCrabboxSync, FeatureCleanup, FeatureDesktop, FeatureBrowser, FeatureCacheVolume, FeatureCheckpoint, FeatureFork},
		Coordinator: CoordinatorNever,
	}
}

type testLocalContainerFlagValues struct {
	Runtime      *string
	Image        *string
	User         *string
	WorkRoot     *string
	CPUs         *int
	Memory       *string
	Network      *string
	DockerSocket *bool
	Volumes      *[]string
}

func (testLocalContainerProvider) RegisterFlags(fs *flag.FlagSet, defaults Config) any {
	volumes := append([]string(nil), defaults.LocalContainer.Volumes...)
	fs.Func("local-container-volume", "container volume", func(value string) error {
		volumes = append(volumes, value)
		return nil
	})
	return testLocalContainerFlagValues{
		Runtime:      fs.String("local-container-runtime", defaults.LocalContainer.Runtime, "Docker-compatible CLI"),
		Image:        fs.String("local-container-image", defaults.LocalContainer.Image, "container image"),
		User:         fs.String("local-container-user", defaults.LocalContainer.User, "container SSH user"),
		WorkRoot:     fs.String("local-container-work-root", defaults.LocalContainer.WorkRoot, "container work root"),
		CPUs:         fs.Int("local-container-cpus", defaults.LocalContainer.CPUs, "container CPUs"),
		Memory:       fs.String("local-container-memory", defaults.LocalContainer.Memory, "container memory"),
		Network:      fs.String("local-container-network", defaults.LocalContainer.Network, "container network"),
		DockerSocket: fs.Bool("local-container-docker-socket", defaults.LocalContainer.DockerSocket, "container Docker socket"),
		Volumes:      &volumes,
	}
}
func (testLocalContainerProvider) ApplyFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(testLocalContainerFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "local-container-runtime") {
		cfg.LocalContainer.Runtime = *v.Runtime
	}
	if flagWasSet(fs, "local-container-image") {
		cfg.LocalContainer.Image = *v.Image
		cfg.localContainerImageExplicit = true
	}
	if flagWasSet(fs, "local-container-user") {
		cfg.LocalContainer.User = *v.User
		cfg.SSHUser = *v.User
	}
	if flagWasSet(fs, "local-container-work-root") {
		cfg.LocalContainer.WorkRoot = *v.WorkRoot
		cfg.WorkRoot = *v.WorkRoot
	}
	if flagWasSet(fs, "local-container-cpus") {
		cfg.LocalContainer.CPUs = *v.CPUs
	}
	if flagWasSet(fs, "local-container-memory") {
		cfg.LocalContainer.Memory = *v.Memory
	}
	if flagWasSet(fs, "local-container-network") {
		cfg.LocalContainer.Network = *v.Network
	}
	if flagWasSet(fs, "local-container-docker-socket") {
		cfg.LocalContainer.DockerSocket = *v.DockerSocket
	}
	if v.Volumes != nil && len(*v.Volumes) > 0 {
		cfg.LocalContainer.Volumes = append([]string(nil), (*v.Volumes)...)
	}
	if cfg.Provider == "docker" || cfg.Provider == "container" || cfg.Provider == "local-docker" {
		cfg.Provider = "local-container"
	}
	return nil
}
func (p testLocalContainerProvider) Configure(cfg Config, rt Runtime) (Backend, error) {
	return testSSHBackend{spec: p.Spec()}, nil
}
func (testLocalContainerProvider) NativeCheckpointCapability(req NativeCheckpointRequest) (NativeCheckpointCapability, bool) {
	if req.Server.CloudID == "" || req.StrategyExplicit {
		return NativeCheckpointCapability{}, false
	}
	return NativeCheckpointCapability{Kind: checkpointKindDockerCommit, Direct: true}, true
}
func (testLocalContainerProvider) ApplyNativeCheckpointForkConfig(req NativeCheckpointForkRequest) error {
	if req.Record.Kind != checkpointKindDockerCommit {
		return exit(2, "provider=local-container does not support checkpoint kind=%s", req.Record.Kind)
	}
	req.Config.LocalContainer.Image = req.Record.ImageID
	req.Config.LocalContainer.Runtime = req.Record.Metadata["runtime"]
	req.Config.LocalContainer.User = req.Record.Metadata["container_user"]
	req.Config.LocalContainer.WorkRoot = req.Record.Metadata["container_work_root"]
	req.Config.SSHUser = req.Config.LocalContainer.User
	req.Config.WorkRoot = req.Config.LocalContainer.WorkRoot
	return nil
}
func (testLocalContainerProvider) ApplyNativeCheckpointForkFlags(cfg *Config, _ *flag.FlagSet, values any) error {
	v, ok := values.(testLocalContainerFlagValues)
	if ok && v.Volumes != nil {
		cfg.LocalContainer.Volumes = append([]string(nil), (*v.Volumes)...)
	}
	return nil
}

type testAppleVZProvider struct{}

func (testAppleVZProvider) Name() string { return "apple-vz" }
func (testAppleVZProvider) Aliases() []string {
	return []string{"applevz"}
}
func (testAppleVZProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        "apple-vz",
		Family:      "local-vm",
		Kind:        ProviderKindSSHLease,
		Targets:     []TargetSpec{{OS: targetLinux}},
		Features:    FeatureSet{FeatureSSH, FeatureCrabboxSync, FeatureCleanup},
		Coordinator: CoordinatorNever,
	}
}

type testAppleVZFlagValues struct {
	HelperPath  *string
	Image       *string
	ImageSHA256 *string
	User        *string
	WorkRoot    *string
	CPUs        *int
	MemoryMiB   *int
	DiskGiB     *int
}

func (testAppleVZProvider) RegisterFlags(fs *flag.FlagSet, defaults Config) any {
	return testAppleVZFlagValues{
		HelperPath:  fs.String("apple-vz-helper", defaults.AppleVZ.HelperPath, "apple-vz helper"),
		Image:       fs.String("apple-vz-image", defaults.AppleVZ.Image, "apple-vz image"),
		ImageSHA256: fs.String("apple-vz-image-sha256", defaults.AppleVZ.ImageSHA256, "apple-vz image sha256"),
		User:        fs.String("apple-vz-user", defaults.AppleVZ.User, "apple-vz user"),
		WorkRoot:    fs.String("apple-vz-work-root", defaults.AppleVZ.WorkRoot, "apple-vz work root"),
		CPUs:        fs.Int("apple-vz-cpus", defaults.AppleVZ.CPUs, "apple-vz CPUs"),
		MemoryMiB:   fs.Int("apple-vz-memory", defaults.AppleVZ.MemoryMiB, "apple-vz memory MiB"),
		DiskGiB:     fs.Int("apple-vz-disk", defaults.AppleVZ.DiskGiB, "apple-vz disk GiB"),
	}
}
func (testAppleVZProvider) ApplyFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(testAppleVZFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "apple-vz-helper") {
		cfg.AppleVZ.HelperPath = *v.HelperPath
	}
	if flagWasSet(fs, "apple-vz-image") {
		cfg.AppleVZ.Image = *v.Image
		cfg.AppleVZ.ImageSHA256 = ""
		cfg.appleVZImageExplicit = true
	}
	if flagWasSet(fs, "apple-vz-image-sha256") {
		cfg.AppleVZ.ImageSHA256 = *v.ImageSHA256
	}
	if flagWasSet(fs, "apple-vz-user") {
		cfg.AppleVZ.User = *v.User
		cfg.SSHUser = *v.User
	}
	if flagWasSet(fs, "apple-vz-work-root") {
		cfg.AppleVZ.WorkRoot = *v.WorkRoot
		cfg.WorkRoot = *v.WorkRoot
	}
	if flagWasSet(fs, "apple-vz-cpus") {
		cfg.AppleVZ.CPUs = *v.CPUs
	}
	if flagWasSet(fs, "apple-vz-memory") {
		cfg.AppleVZ.MemoryMiB = *v.MemoryMiB
	}
	if flagWasSet(fs, "apple-vz-disk") {
		cfg.AppleVZ.DiskGiB = *v.DiskGiB
	}
	return nil
}
func (p testAppleVZProvider) Configure(cfg Config, rt Runtime) (Backend, error) {
	return testSSHBackend{spec: p.Spec()}, nil
}

type testMultipassProvider struct{}

func (testMultipassProvider) Name() string { return "multipass" }
func (testMultipassProvider) Aliases() []string {
	return []string{"mp", "canonical-multipass"}
}
func (testMultipassProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        "multipass",
		Family:      "local-vm",
		Kind:        ProviderKindSSHLease,
		Targets:     []TargetSpec{{OS: targetLinux}},
		Features:    FeatureSet{FeatureSSH, FeatureCrabboxSync, FeatureCleanup, FeatureCacheVolume},
		Coordinator: CoordinatorNever,
	}
}

type testMultipassFlagValues struct {
	CLIPath       *string
	Image         *string
	User          *string
	WorkRoot      *string
	CPUs          *int
	Memory        *string
	Disk          *string
	LaunchTimeout *string
}

func (testMultipassProvider) RegisterFlags(fs *flag.FlagSet, defaults Config) any {
	return testMultipassFlagValues{
		CLIPath:       fs.String("multipass-cli", defaults.Multipass.CLIPath, "Multipass CLI"),
		Image:         fs.String("multipass-image", defaults.Multipass.Image, "Multipass image"),
		User:          fs.String("multipass-user", defaults.Multipass.User, "Multipass SSH user"),
		WorkRoot:      fs.String("multipass-work-root", defaults.Multipass.WorkRoot, "Multipass work root"),
		CPUs:          fs.Int("multipass-cpus", defaults.Multipass.CPUs, "Multipass CPUs"),
		Memory:        fs.String("multipass-memory", defaults.Multipass.Memory, "Multipass memory"),
		Disk:          fs.String("multipass-disk", defaults.Multipass.Disk, "Multipass disk"),
		LaunchTimeout: fs.String("multipass-launch-timeout", defaults.Multipass.LaunchTimeout.String(), "Multipass launch timeout"),
	}
}
func (testMultipassProvider) ApplyFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(testMultipassFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "multipass-cli") {
		cfg.Multipass.CLIPath = *v.CLIPath
	}
	if flagWasSet(fs, "multipass-image") {
		cfg.Multipass.Image = *v.Image
		cfg.multipassImageExplicit = true
	}
	if flagWasSet(fs, "multipass-user") {
		cfg.Multipass.User = *v.User
		cfg.SSHUser = *v.User
	}
	if flagWasSet(fs, "multipass-work-root") {
		cfg.Multipass.WorkRoot = *v.WorkRoot
		cfg.WorkRoot = *v.WorkRoot
	}
	if flagWasSet(fs, "multipass-cpus") {
		cfg.Multipass.CPUs = *v.CPUs
	}
	if flagWasSet(fs, "multipass-memory") {
		cfg.Multipass.Memory = *v.Memory
	}
	if flagWasSet(fs, "multipass-disk") {
		cfg.Multipass.Disk = *v.Disk
	}
	if flagWasSet(fs, "multipass-launch-timeout") {
		if err := ApplyLeaseDuration(&cfg.Multipass.LaunchTimeout, *v.LaunchTimeout); err != nil {
			return err
		}
	}
	if cfg.Provider == "mp" || cfg.Provider == "canonical-multipass" {
		cfg.Provider = "multipass"
	}
	return nil
}
func (p testMultipassProvider) Configure(cfg Config, rt Runtime) (Backend, error) {
	return testSSHBackend{spec: p.Spec()}, nil
}

type testTartProvider struct{}

func (testTartProvider) Name() string { return "tart" }
func (testTartProvider) Aliases() []string {
	return []string{"local-tart", "macos-vm"}
}
func (testTartProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        "tart",
		Family:      "local-vm",
		Kind:        ProviderKindSSHLease,
		Targets:     []TargetSpec{{OS: targetMacOS}},
		Features:    FeatureSet{FeatureSSH, FeatureCrabboxSync, FeatureCleanup},
		Coordinator: CoordinatorNever,
	}
}

type testTartFlagValues struct {
	Image  *string
	CPUs   *int
	Memory *int
	Disk   *int
}

func (testTartProvider) RegisterFlags(fs *flag.FlagSet, defaults Config) any {
	return testTartFlagValues{
		Image:  fs.String("tart-image", defaults.Tart.Image, "tart base image"),
		CPUs:   fs.Int("tart-cpu", defaults.Tart.CPUs, "tart CPUs"),
		Memory: fs.Int("tart-memory", defaults.Tart.Memory, "tart memory MB"),
		Disk:   fs.Int("tart-disk", defaults.Tart.Disk, "tart disk GB"),
	}
}
func (testTartProvider) ApplyFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(testTartFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "tart-image") {
		cfg.Tart.Image = *v.Image
		cfg.tartImageExplicit = true
	}
	if flagWasSet(fs, "tart-cpu") {
		cfg.Tart.CPUs = *v.CPUs
	}
	if flagWasSet(fs, "tart-memory") {
		cfg.Tart.Memory = *v.Memory
	}
	if flagWasSet(fs, "tart-disk") {
		cfg.Tart.Disk = *v.Disk
	}
	if cfg.Provider == "local-tart" || cfg.Provider == "macos-vm" {
		cfg.Provider = "tart"
	}
	return nil
}
func (p testTartProvider) Configure(cfg Config, rt Runtime) (Backend, error) {
	return testSSHBackend{spec: p.Spec()}, nil
}

type testHyperVProvider struct{}

func (testHyperVProvider) Name() string      { return "hyperv" }
func (testHyperVProvider) Aliases() []string { return nil }
func (testHyperVProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        "hyperv",
		Family:      "local-vm",
		Kind:        ProviderKindSSHLease,
		Targets:     []TargetSpec{{OS: targetWindows, WindowsMode: windowsModeNormal}},
		Features:    FeatureSet{FeatureSSH, FeatureCrabboxSync, FeatureCleanup},
		Coordinator: CoordinatorNever,
	}
}

type testHyperVFlagValues struct {
	Image  *string
	CPUs   *int
	Memory *int
	Switch *string
}

func (testHyperVProvider) RegisterFlags(fs *flag.FlagSet, defaults Config) any {
	return testHyperVFlagValues{
		Image:  fs.String("hyperv-image", defaults.HyperV.Image, "Hyper-V image"),
		CPUs:   fs.Int("hyperv-cpu", defaults.HyperV.CPUs, "Hyper-V CPUs"),
		Memory: fs.Int("hyperv-memory", defaults.HyperV.Memory, "Hyper-V memory MB"),
		Switch: fs.String("hyperv-switch", defaults.HyperV.Switch, "Hyper-V switch"),
	}
}
func (testHyperVProvider) ApplyFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(testHyperVFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "hyperv-image") {
		cfg.HyperV.Image = *v.Image
	}
	if flagWasSet(fs, "hyperv-cpu") {
		cfg.HyperV.CPUs = *v.CPUs
	}
	if flagWasSet(fs, "hyperv-memory") {
		cfg.HyperV.Memory = *v.Memory
	}
	if flagWasSet(fs, "hyperv-switch") {
		cfg.HyperV.Switch = *v.Switch
	}
	return nil
}
func (p testHyperVProvider) Configure(cfg Config, rt Runtime) (Backend, error) {
	return testSSHBackend{spec: p.Spec()}, nil
}

type testDockerSandboxProvider struct{}

func (testDockerSandboxProvider) Name() string      { return "docker-sandbox" }
func (testDockerSandboxProvider) Aliases() []string { return nil }
func (testDockerSandboxProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        "docker-sandbox",
		Family:      "docker-sandbox",
		Kind:        ProviderKindDelegatedRun,
		Targets:     []TargetSpec{{OS: targetLinux}},
		Features:    FeatureSet{FeatureRunSession},
		Coordinator: CoordinatorNever,
	}
}
func (testDockerSandboxProvider) RegisterFlags(fs *flag.FlagSet, defaults Config) any {
	return struct{ CPUs *float64 }{CPUs: fs.Float64("docker-sandbox-cpus", defaults.DockerSandbox.CPUs, "")}
}
func (testDockerSandboxProvider) ApplyFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if v, ok := values.(struct{ CPUs *float64 }); ok && flagWasSet(fs, "docker-sandbox-cpus") && v.CPUs != nil {
		cfg.DockerSandbox.CPUs = *v.CPUs
	}
	return testDockerSandboxProvider{}.ValidateConfig(*cfg)
}
func (testDockerSandboxProvider) ValidateConfig(cfg Config) error {
	if cfg.DockerSandbox.CPUs != math.Trunc(cfg.DockerSandbox.CPUs) {
		return exit(2, "docker-sandbox cpus must be a whole number")
	}
	return nil
}
func (p testDockerSandboxProvider) Configure(cfg Config, rt Runtime) (Backend, error) {
	if err := p.ValidateConfig(cfg); err != nil {
		return nil, err
	}
	return testDelegatedBackend{spec: p.Spec(), portsOutput: "127.0.0.1:41000->3000/tcp\n", copyErr: nil}, nil
}

type testDelegatedBackend struct {
	spec        ProviderSpec
	portsOutput string
	copyErr     error
}

type testIsloBackend struct {
	testDelegatedBackend
	stderr io.Writer
}

func (b testIsloBackend) Pause(_ context.Context, req PauseRequest) error {
	fmt.Fprintf(b.stderr, "paused id=%s\n", req.ID)
	return nil
}

func (b testIsloBackend) Resume(_ context.Context, req ResumeRequest) error {
	fmt.Fprintf(b.stderr, "resumed id=%s\n", req.ID)
	return nil
}

type testServiceControlProvider struct{}

func (testServiceControlProvider) Name() string      { return "service-control-test" }
func (testServiceControlProvider) Aliases() []string { return nil }
func (testServiceControlProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        "service-control-test",
		Family:      "service-control-test",
		Kind:        ProviderKindServiceControl,
		Targets:     []TargetSpec{{OS: targetLinux}},
		Coordinator: CoordinatorNever,
	}
}
func (testServiceControlProvider) RegisterFlags(*flag.FlagSet, Config) any { return nil }
func (testServiceControlProvider) ApplyFlags(*Config, *flag.FlagSet, any) error {
	return nil
}
func (p testServiceControlProvider) Configure(Config, Runtime) (Backend, error) {
	return testServiceControlBackend{spec: p.Spec()}, nil
}

type testServiceControlBackend struct {
	spec ProviderSpec
}

func (b testServiceControlBackend) Spec() ProviderSpec { return b.spec }

func (b testDelegatedBackend) Spec() ProviderSpec { return b.spec }
func (b testDelegatedBackend) Warmup(context.Context, WarmupRequest) error {
	return nil
}
func (b testDelegatedBackend) Run(context.Context, RunRequest) (RunResult, error) {
	return RunResult{
		Provider:    b.spec.Name,
		LeaseID:     "tbx_test",
		Slug:        "testbox",
		CommandText: "pnpm test",
		LogExcerpt:  "delegated test output\nsuite pass",
	}, nil
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
func (b testDelegatedBackend) Ports(_ context.Context, req PortsRequest) (string, error) {
	if req.JSON {
		payload := []map[string]string{{"mapping": "127.0.0.1:41000->3000/tcp"}}
		data, err := json.Marshal(payload)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	return b.portsOutput, nil
}
func (b testDelegatedBackend) Copy(context.Context, CopyRequest) error {
	return b.copyErr
}

type testDoctorDelegatedBackend struct {
	testDelegatedBackend
}

func (b testDoctorDelegatedBackend) Doctor(context.Context, DoctorRequest) (DoctorResult, error) {
	if testCloudflareDoctorResult != nil {
		return *testCloudflareDoctorResult, nil
	}
	return DoctorResult{Provider: b.spec.Name, Message: "direct_check=ready"}, nil
}

type testDaytonaBackend struct {
	testSSHBackend
}

func (b testDaytonaBackend) Warmup(context.Context, WarmupRequest) error {
	return nil
}
func (b testDaytonaBackend) Run(context.Context, RunRequest) (RunResult, error) {
	return RunResult{}, nil
}
func (b testDaytonaBackend) Status(context.Context, StatusRequest) (StatusView, error) {
	return StatusView{}, nil
}
func (b testDaytonaBackend) Stop(context.Context, StopRequest) error {
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
