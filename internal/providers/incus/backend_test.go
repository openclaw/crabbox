package incus

import (
	"context"
	"flag"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/lxc/incus/v7/shared/cliconfig"
	core "github.com/openclaw/crabbox/internal/cli"
)

type fakeClient struct {
	instances            map[string]*api.Instance
	states               map[string]*api.InstanceState
	listOrder            []string
	deleted              []string
	created              []api.InstancesPost
	updated              []string
	stateUpdates         []string
	preserveEmptyNetwork bool
	deleteRequiresStop   bool
	getCalls             map[string]int
	getErr               error
	listErr              error
	createErr            error
	updateErr            error
	stateErr             error
	deleteErr            error
}

func (f *fakeClient) ListInstances() ([]api.Instance, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]api.Instance, 0, len(f.instances))
	if len(f.listOrder) > 0 {
		seen := make(map[string]struct{}, len(f.listOrder))
		for _, name := range f.listOrder {
			inst, ok := f.instances[name]
			if !ok {
				continue
			}
			out = append(out, *inst)
			seen[name] = struct{}{}
		}
		for name, inst := range f.instances {
			if _, ok := seen[name]; ok {
				continue
			}
			out = append(out, *inst)
		}
		return out, nil
	}
	for _, inst := range f.instances {
		out = append(out, *inst)
	}
	return out, nil
}

func (f *fakeClient) GetInstance(name string) (*api.Instance, string, error) {
	if f.getErr != nil {
		return nil, "", f.getErr
	}
	if f.getCalls == nil {
		f.getCalls = map[string]int{}
	}
	f.getCalls[name]++
	inst, ok := f.instances[name]
	if !ok {
		return nil, "", core.Exit(4, "missing instance %s", name)
	}
	copy := *inst
	copy.Config = cloneMap(inst.Config)
	copy.Devices = cloneMapMap(inst.Devices)
	copy.ExpandedDevices = cloneMapMap(inst.ExpandedDevices)
	return &copy, "etag", nil
}

func (f *fakeClient) CreateInstance(req api.InstancesPost) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.created = append(f.created, req)
	config := cloneMap(req.Config)
	if config == nil {
		config = map[string]string{}
	}
	devices := cloneMapMap(req.Devices)
	profiles := append([]string(nil), req.Profiles...)
	f.instances[req.Name] = &api.Instance{
		Name: req.Name,
		Type: string(req.Type),
		InstancePut: api.InstancePut{
			Config:   config,
			Devices:  devices,
			Profiles: profiles,
		},
		ExpandedDevices: cloneMapMap(devices),
		Status:          "Stopped",
		StatusCode:      api.Stopped,
	}
	if _, ok := f.states[req.Name]; !ok {
		f.states[req.Name] = &api.InstanceState{Status: "Stopped", StatusCode: api.Stopped}
	}
	return nil
}

func (f *fakeClient) UpdateInstance(name string, put api.InstancePut, etag string) error {
	_ = etag
	if f.updateErr != nil {
		return f.updateErr
	}
	inst := f.instances[name]
	inst.Config = cloneMap(put.Config)
	inst.Devices = cloneMapMap(put.Devices)
	inst.ExpandedDevices = cloneMapMap(put.Devices)
	inst.Profiles = append([]string(nil), put.Profiles...)
	f.updated = append(f.updated, name)
	return nil
}

func (f *fakeClient) SetInstanceState(name string, put api.InstanceStatePut, etag string) error {
	_ = etag
	if f.stateErr != nil {
		return f.stateErr
	}
	inst := f.instances[name]
	state := f.states[name]
	if state == nil {
		state = &api.InstanceState{}
		f.states[name] = state
	}
	switch put.Action {
	case "start":
		inst.Status = "Running"
		inst.StatusCode = api.Running
		state.Status = "Running"
		state.StatusCode = api.Running
		if len(state.Network) == 0 && !f.preserveEmptyNetwork {
			state.Network = map[string]api.InstanceStateNetwork{
				"eth0": {Addresses: []api.InstanceStateNetworkAddress{{Family: "inet", Scope: "global", Address: "198.51.100.24"}}},
			}
		}
	case "stop":
		inst.Status = "Stopped"
		inst.StatusCode = api.Stopped
		state.Status = "Stopped"
		state.StatusCode = api.Stopped
	}
	f.stateUpdates = append(f.stateUpdates, name+":"+put.Action)
	return nil
}

func (f *fakeClient) GetInstanceState(name string) (*api.InstanceState, string, error) {
	state := f.states[name]
	if state == nil {
		state = &api.InstanceState{}
	}
	copy := *state
	if copy.Network == nil {
		copy.Network = map[string]api.InstanceStateNetwork{}
	}
	return &copy, "etag", nil
}

func (f *fakeClient) DeleteInstance(name string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if f.deleteRequiresStop {
		if inst, ok := f.instances[name]; ok && strings.EqualFold(inst.Status, "running") {
			return core.Exit(5, "instance is running")
		}
	}
	delete(f.instances, name)
	delete(f.states, name)
	f.deleted = append(f.deleted, name)
	return nil
}

type stateCountingClient struct {
	*fakeClient
	getStateCalls int
	afterGetState func(int)
}

func (c *stateCountingClient) GetInstanceState(name string) (*api.InstanceState, string, error) {
	c.getStateCalls++
	state, etag, err := c.fakeClient.GetInstanceState(name)
	if c.afterGetState != nil {
		c.afterGetState(c.getStateCalls)
	}
	return state, etag, err
}

func TestProviderSpecAndFlags(t *testing.T) {
	p := Provider{}
	if p.Name() != providerName {
		t.Fatalf("Name=%q want %s", p.Name(), providerName)
	}
	got, err := core.ProviderFor("incus")
	if err != nil {
		t.Fatalf("ProviderFor(incus): %v", err)
	}
	if got.Name() != providerName {
		t.Fatalf("ProviderFor(incus).Name=%q", got.Name())
	}
	spec := p.Spec()
	if spec.Kind != core.ProviderKindSSHLease || spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("unexpected spec: %#v", spec)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("targets=%v want linux only", spec.Targets)
	}
	for _, feature := range []core.Feature{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup} {
		if !spec.Features.Has(feature) {
			t.Fatalf("features=%v missing %s", spec.Features, feature)
		}
	}

	defaults := core.BaseConfig()
	defaults.Provider = providerName
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	values := registerFlags(fs, defaults)
	if err := fs.Parse([]string{
		"--incus-instance-type", "vm",
		"--incus-image", "images:ubuntu/24.04/cloud",
		"--incus-user", "ubuntu",
		"--incus-work-root", "/workspace/incus",
		"--incus-launch-port", "2201",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := defaults
	if err := applyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	applyDefaults(&cfg)
	if cfg.Incus.InstanceType != "virtual-machine" || cfg.Incus.User != "ubuntu" || cfg.WorkRoot != "/workspace/incus" || cfg.SSHPort != "2201" {
		t.Fatalf("flags not applied: %#v", cfg.Incus)
	}
	if cfg.ServerType != "virtual-machine:images:ubuntu/24.04/cloud" {
		t.Fatalf("serverType=%q", cfg.ServerType)
	}
}

func TestApplyFlagsRejectsInvalidInstanceType(t *testing.T) {
	defaults := core.BaseConfig()
	defaults.Provider = providerName
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	values := registerFlags(fs, defaults)
	if err := fs.Parse([]string{"--incus-instance-type", "vmm"}); err != nil {
		t.Fatal(err)
	}
	cfg := defaults
	err := applyFlags(&cfg, fs, values)
	if err == nil {
		t.Fatal("expected error for invalid instance-type")
	}
	if !strings.Contains(err.Error(), "unsupported") || !strings.Contains(err.Error(), "vmm") {
		t.Fatalf("error=%q want unsupported instance-type message", err)
	}
}

func TestApplyDefaultsPreservesExplicitSSHUserAndWorkRoot(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.SSHUser = "alice"
	cfg.WorkRoot = "/tmp/custom"

	applyDefaults(&cfg)

	if cfg.SSHUser != "alice" {
		t.Fatalf("SSHUser=%q want alice", cfg.SSHUser)
	}
	if cfg.WorkRoot != "/tmp/custom" {
		t.Fatalf("WorkRoot=%q want /tmp/custom", cfg.WorkRoot)
	}
}

func TestApplyDefaultsAllowsIncusSpecificUserAndWorkRootOverride(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.SSHUser = "alice"
	cfg.WorkRoot = "/tmp/custom"
	cfg.Incus.User = "ubuntu"
	cfg.Incus.WorkRoot = "/workspace/incus"

	applyDefaults(&cfg)

	if cfg.SSHUser != "ubuntu" {
		t.Fatalf("SSHUser=%q want ubuntu", cfg.SSHUser)
	}
	if cfg.WorkRoot != "/workspace/incus" {
		t.Fatalf("WorkRoot=%q want /workspace/incus", cfg.WorkRoot)
	}
}

func TestApplyDefaultsPreservesExplicitSSHPort(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.SSHPort = "2205"
	cfg.Incus.ProxyListenPort = "2222"
	cfg.Incus.LaunchPort = "2201"

	applyDefaults(&cfg)

	if cfg.SSHPort != "2205" {
		t.Fatalf("SSHPort=%q want 2205", cfg.SSHPort)
	}
}

func TestApplyDefaultsUsesIncusLaunchPortWhenTopLevelSSHPortIsDefault(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.SSHPort = "22"
	cfg.Incus.ProxyListenPort = ""
	cfg.Incus.LaunchPort = "2205"

	applyDefaults(&cfg)

	if cfg.SSHPort != "2205" {
		t.Fatalf("SSHPort=%q want 2205", cfg.SSHPort)
	}
}

func TestValidateConfigRejectsProxyForVMInstances(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Incus.InstanceType = "virtual-machine"
	cfg.Incus.ProxyListenPort = "2222"

	err := validateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "static NIC") {
		t.Fatalf("validateConfig err=%v want VM proxy rejection", err)
	}
}

func TestDevicesForCreateUsesNonNATForContainerInstances(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Incus.InstanceType = "container"
	cfg.Incus.ProxyListenPort = "2222"

	devices := devicesForCreate(cfg)
	device, ok := devices["crabbox-ssh"]
	if !ok {
		t.Fatal("expected crabbox-ssh proxy device")
	}
	if _, hasNat := device["nat"]; hasNat {
		t.Fatalf("container proxy device should not have nat key, got nat=%q", device["nat"])
	}
}

func TestApplyDefaultsDoesNotClobberExplicitValuesWithDefaultIncusValues(t *testing.T) {
	base := core.BaseConfig()
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.SSHUser = "alice"
	cfg.WorkRoot = "/tmp/custom"
	cfg.Incus.User = base.Incus.User
	cfg.Incus.WorkRoot = base.Incus.WorkRoot

	applyDefaults(&cfg)

	if cfg.SSHUser != "alice" {
		t.Fatalf("SSHUser=%q want alice", cfg.SSHUser)
	}
	if cfg.WorkRoot != "/tmp/custom" {
		t.Fatalf("WorkRoot=%q want /tmp/custom", cfg.WorkRoot)
	}
}

func TestImageSourceForConfigResolvesDefaultImagesRemote(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), ".config"))

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Incus.Image = "images:ubuntu/24.04/cloud"

	source := imageSourceForConfig(cfg)
	if source.Server != cliconfig.ImagesRemote.Addrs[0] {
		t.Fatalf("source.Server=%q want %q", source.Server, cliconfig.ImagesRemote.Addrs[0])
	}
	if source.Protocol != "simplestreams" {
		t.Fatalf("source.Protocol=%q want simplestreams", source.Protocol)
	}
	if source.Alias != "ubuntu/24.04/cloud" {
		t.Fatalf("source.Alias=%q want ubuntu/24.04/cloud", source.Alias)
	}
}

func TestImageSourceForConfigLeavesUnqualifiedAliasesLocalByDefault(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Incus.Image = "local-alias"

	source := imageSourceForConfig(cfg)
	if source.Server != "" {
		t.Fatalf("source.Server=%q want empty", source.Server)
	}
	if source.Protocol != "" {
		t.Fatalf("source.Protocol=%q want empty", source.Protocol)
	}
	if source.Alias != "local-alias" {
		t.Fatalf("source.Alias=%q want local-alias", source.Alias)
	}
}

func TestImageSourceForConfigUsesExplicitRemoteImageServerForUnqualifiedAliases(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Incus.Image = "ubuntu/24.04/cloud"
	cfg.Incus.RemoteImageServer = "https://images.example.test"

	source := imageSourceForConfig(cfg)
	if source.Server != "https://images.example.test" {
		t.Fatalf("source.Server=%q want https://images.example.test", source.Server)
	}
	if source.Protocol != "simplestreams" {
		t.Fatalf("source.Protocol=%q want simplestreams", source.Protocol)
	}
	if source.Alias != "ubuntu/24.04/cloud" {
		t.Fatalf("source.Alias=%q want ubuntu/24.04/cloud", source.Alias)
	}
}

func TestImageSourceForConfigStripsLocalDaemonRemotePrefix(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Incus.Image = "local:my-custom-image"

	source := imageSourceForConfig(cfg)
	if source.Server != "" {
		t.Fatalf("source.Server=%q want empty for local daemon remote", source.Server)
	}
	if source.Protocol != "" {
		t.Fatalf("source.Protocol=%q want empty for local daemon remote", source.Protocol)
	}
	if source.Alias != "my-custom-image" {
		t.Fatalf("source.Alias=%q want my-custom-image", source.Alias)
	}
}

func TestSSHTargetHostUsesIncusHostWhenProxyPortConfigured(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Incus.Address = "https://incus-host.example.test:8443"
	cfg.Incus.ProxyListenHost = "0.0.0.0"
	cfg.Incus.ProxyListenPort = "2222"

	server := core.Server{}
	server.PublicNet.IPv4.IP = "198.51.100.24"
	if got := sshTargetHost(server, cfg); got != "incus-host.example.test" {
		t.Fatalf("sshTargetHost=%q want incus-host.example.test", got)
	}
}

func TestConnectionArgsForAddressLoadsTLSCertContents(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	configDir := writeIncusConfig(t, home, "default-remote: trusted\nremotes:\n  trusted:\n    addr: https://incus-host.example.test:8443\n    protocol: incus\n")
	writeClientCertificateFiles(t, configDir)

	certPath := filepath.Join(t.TempDir(), "server.crt")
	if err := os.WriteFile(certPath, []byte("PEM-CONTENT"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Incus.Address = "https://incus-host.example.test:8443"
	cfg.Incus.TLSServerCert = certPath

	args, err := connectionArgsForAddress(cfg)
	if err != nil {
		t.Fatalf("connectionArgsForAddress: %v", err)
	}
	if args.TLSServerCert != "PEM-CONTENT" {
		t.Fatalf("TLSServerCert=%q want PEM contents", args.TLSServerCert)
	}
}

func TestInstanceConfigForCreateUsesGuestLaunchPortBehindProxy(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.SSHPort = "2222"
	cfg.Incus.LaunchPort = "2201"
	cfg.Incus.ProxyListenPort = "2222"

	userData := instanceConfigForCreate(cfg, nil, "ssh-ed25519 test-key")["cloud-init.user-data"]
	if !strings.Contains(userData, "      Port 2201\n") {
		t.Fatalf("cloud-init missing guest launch port:\n%s", userData)
	}
	if strings.Contains(userData, "      Port 2222\n") {
		t.Fatalf("cloud-init incorrectly uses host proxy port:\n%s", userData)
	}
}

func TestDoctorReportsSocketModeWithoutMutation(t *testing.T) {
	oldNewClient := newClient
	fake := &fakeClient{
		instances: map[string]*api.Instance{
			"crabbox-doctor": {
				Name:       "crabbox-doctor",
				Status:     "Running",
				StatusCode: api.Running,
				InstancePut: api.InstancePut{
					Config: map[string]string{
						labelKey("crabbox"): "true",
						labelKey("lease"):   "cbx_doctor1234",
						labelKey("slug"):    "doctor-slug",
					},
				},
			},
		},
		states: map[string]*api.InstanceState{},
	}
	newClient = func(cfg Config) (instanceClient, error) {
		_ = cfg
		return fake, nil
	}
	t.Cleanup(func() { newClient = oldNewClient })

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Incus.Socket = "/var/lib/incus/unix.socket"
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)

	result, err := b.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	for _, want := range []string{
		"control_plane=local",
		"leases=1",
		"runtime=go_client",
		"mode=socket",
		"protocol=unix",
		"endpoint=/var/lib/incus/unix.socket",
		"project=default",
		"auth=unix_socket",
	} {
		if !strings.Contains(result.Message, want) {
			t.Fatalf("doctor message missing %q: %s", want, result.Message)
		}
	}
	if len(fake.created) != 0 || len(fake.updated) != 0 || len(fake.stateUpdates) != 0 || len(fake.deleted) != 0 {
		t.Fatalf("doctor mutated instance state: created=%d updated=%d stateUpdates=%d deleted=%d", len(fake.created), len(fake.updated), len(fake.stateUpdates), len(fake.deleted))
	}
}

func TestConfigureRejectsUnsupportedTargetAndTailscale(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetMacOS
	if _, err := (Provider{}).Configure(cfg, core.Runtime{}); err == nil {
		t.Fatal("Configure accepted macos target")
	}

	cfg = core.BaseConfig()
	cfg.Provider = providerName
	cfg.Tailscale.Enabled = true
	if _, err := (Provider{}).Configure(cfg, core.Runtime{}); err == nil {
		t.Fatal("Configure accepted tailscale")
	}
}

func TestDoctorReportsSocketModeForDefaultLocalRemote(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("default local remote uses unix socket, only valid on Linux")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	oldNewClient := newClient
	fake := &fakeClient{
		instances: map[string]*api.Instance{},
		states:    map[string]*api.InstanceState{},
	}
	newClient = func(cfg Config) (instanceClient, error) {
		_ = cfg
		return fake, nil
	}
	t.Cleanup(func() { newClient = oldNewClient })

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)

	result, err := b.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if !strings.Contains(result.Message, "mode=socket") {
		t.Fatalf("doctor message should report mode=socket for default local unix remote: %s", result.Message)
	}
	if strings.Contains(result.Message, "control_plane=remote") {
		t.Fatalf("doctor message should not report control_plane=remote for default local unix remote: %s", result.Message)
	}
}

func TestAcquireResolveListTouchReleaseAndCleanup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))

	oldNewClient := newClient
	oldWait := waitForSSHReady
	fake := &fakeClient{
		instances: map[string]*api.Instance{
			"foreign": {
				Name:       "foreign",
				Status:     "Running",
				StatusCode: api.Running,
				InstancePut: api.InstancePut{
					Config: map[string]string{},
				},
			},
		},
		states: map[string]*api.InstanceState{
			"foreign": {
				Status:     "Running",
				StatusCode: api.Running,
				Network: map[string]api.InstanceStateNetwork{
					"eth0": {Addresses: []api.InstanceStateNetworkAddress{{Family: "inet", Scope: "global", Address: "192.0.2.9"}}},
				},
			},
		},
	}
	newClient = func(cfg Config) (instanceClient, error) {
		_ = cfg
		return fake, nil
	}
	waitForSSHReady = func(ctx context.Context, target *SSHTarget, stderr io.Writer, phase string, timeout time.Duration) error {
		_ = ctx
		_ = stderr
		_ = phase
		_ = timeout
		if target.Host == "" {
			return core.Exit(5, "missing host")
		}
		return nil
	}
	t.Cleanup(func() {
		newClient = oldNewClient
		waitForSSHReady = oldWait
	})

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Incus.Profile = "crabbox"
	cfg.Incus.ProxyListenPort = "2222"
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)

	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "blue-lobster"})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID == "" || lease.Server.Name == "" || lease.SSH.Host == "" {
		t.Fatalf("acquire returned incomplete lease: %#v", lease)
	}
	if len(fake.created) != 1 {
		t.Fatalf("created=%d want 1", len(fake.created))
	}
	created := fake.created[0]
	if created.Name == "" || created.Config["cloud-init.user-data"] == "" {
		t.Fatalf("create request missing cloud-init or name: %#v", created)
	}
	if created.Config[labelKey("crabbox")] != "true" || created.Config[labelKey("slug")] != "blue-lobster" {
		t.Fatalf("create request missing labels: %#v", created.Config)
	}
	if got := created.Devices["crabbox-ssh"]["listen"]; !strings.Contains(got, ":2222") {
		t.Fatalf("proxy device listen=%q", got)
	}

	views, err := b.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Name != lease.Server.Name {
		t.Fatalf("views=%#v", views)
	}

	resolved, err := b.Resolve(context.Background(), ResolveRequest{ID: lease.LeaseID})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Server.Name != lease.Server.Name {
		t.Fatalf("resolved=%#v", resolved)
	}

	touched, err := b.Touch(context.Background(), core.TouchRequest{Lease: lease, State: "running"})
	if err != nil {
		t.Fatal(err)
	}
	if touched.Labels["state"] != "running" {
		t.Fatalf("touch labels=%v", touched.Labels)
	}
	if len(fake.updated) == 0 {
		t.Fatal("touch did not update instance metadata")
	}

	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease, Force: true}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) == 0 {
		t.Fatal("release did not delete instance")
	}

	stale := &api.Instance{
		Name:       "crabbox-stale",
		Status:     "Stopped",
		StatusCode: api.Stopped,
		InstancePut: api.InstancePut{
			Config: map[string]string{
				labelKey("crabbox"):           "true",
				labelKey("lease"):             "cbx_stale",
				labelKey("provider"):          providerName,
				labelKey("slug"):              "stale-slug",
				labelKey("state"):             "ready",
				labelKey("created_at"):        core.LeaseLabelTime(time.Now().UTC().Add(-2 * time.Hour)),
				labelKey("last_touched_at"):   core.LeaseLabelTime(time.Now().UTC().Add(-2 * time.Hour)),
				labelKey("idle_timeout"):      "60",
				labelKey("idle_timeout_secs"): "60",
				labelKey("ttl_secs"):          "120",
				labelKey("expires_at"):        core.LeaseLabelTime(time.Now().UTC().Add(-time.Hour)),
			},
		},
	}
	fake.instances[stale.Name] = stale
	fake.states[stale.Name] = &api.InstanceState{Status: "Stopped", StatusCode: api.Stopped}
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) < 2 {
		t.Fatalf("cleanup deleted=%v want stale instance removed too", fake.deleted)
	}
}

func TestAcquireCleansUpPartialInstanceEvenWhenDeleteOnReleaseFalse(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))

	oldNewClient := newClient
	oldWait := waitForSSHReady
	fake := &fakeClient{
		deleteRequiresStop: true,
		instances:          map[string]*api.Instance{},
		states:             map[string]*api.InstanceState{},
	}
	newClient = func(cfg Config) (instanceClient, error) {
		_ = cfg
		return fake, nil
	}
	waitForSSHReady = func(ctx context.Context, target *SSHTarget, stderr io.Writer, phase string, timeout time.Duration) error {
		_ = ctx
		_ = target
		_ = stderr
		_ = phase
		_ = timeout
		return core.Exit(5, "simulated ssh failure")
	}
	t.Cleanup(func() {
		newClient = oldNewClient
		waitForSSHReady = oldWait
	})

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Incus.DeleteOnRelease = false
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)

	_, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "cleanup-check"})
	if err == nil {
		t.Fatal("Acquire unexpectedly succeeded")
	}
	if len(fake.deleted) != 1 {
		t.Fatalf("deleted=%v want partial instance cleanup", fake.deleted)
	}
	if len(fake.stateUpdates) != 2 || fake.stateUpdates[0] != fake.created[0].Name+":start" || fake.stateUpdates[1] != fake.created[0].Name+":stop" {
		t.Fatalf("stateUpdates=%v want start then stop before delete", fake.stateUpdates)
	}
}

func TestAcquireKeepFailurePreservesStoredKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))

	oldNewClient := newClient
	oldWait := waitForSSHReady
	fake := &fakeClient{
		instances: map[string]*api.Instance{},
		states:    map[string]*api.InstanceState{},
	}
	newClient = func(cfg Config) (instanceClient, error) {
		_ = cfg
		return fake, nil
	}
	waitForSSHReady = func(ctx context.Context, target *SSHTarget, stderr io.Writer, phase string, timeout time.Duration) error {
		_ = ctx
		_ = target
		_ = stderr
		_ = phase
		_ = timeout
		return core.Exit(5, "simulated ssh failure")
	}
	t.Cleanup(func() {
		newClient = oldNewClient
		waitForSSHReady = oldWait
	})

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Incus.DeleteOnRelease = false
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)

	_, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "keep-on-fail", Keep: true})
	if err == nil {
		t.Fatal("Acquire unexpectedly succeeded")
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("deleted=%v want retained instance on Keep=true failure", fake.deleted)
	}
	if len(fake.created) != 1 {
		t.Fatalf("created=%d want 1", len(fake.created))
	}

	var leaseID string
	for _, inst := range fake.instances {
		leaseID = inst.Config[labelKey("lease")]
		if leaseID != "" {
			break
		}
	}
	if leaseID == "" {
		t.Fatal("expected created instance to include a lease label")
	}
	keyPath, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		t.Fatalf("TestboxKeyPath: %v", err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("expected retained acquire failure to preserve key %q: %v", keyPath, err)
	}
}

func TestAcquireKeepBootstrapRetryCleansRetainedAttempt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))

	oldNewClient := newClient
	oldWait := waitForSSHReady
	fake := &fakeClient{
		instances: map[string]*api.Instance{},
		states:    map[string]*api.InstanceState{},
	}
	waitCalls := 0
	newClient = func(cfg Config) (instanceClient, error) {
		_ = cfg
		return fake, nil
	}
	waitForSSHReady = func(ctx context.Context, target *SSHTarget, stderr io.Writer, phase string, timeout time.Duration) error {
		_ = ctx
		_ = target
		_ = stderr
		_ = phase
		_ = timeout
		waitCalls++
		if waitCalls == 1 {
			return core.Exit(5, "timed out waiting for SSH on 203.0.113.10 during bootstrap")
		}
		return nil
	}
	t.Cleanup(func() {
		newClient = oldNewClient
		waitForSSHReady = oldWait
	})

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Incus.DeleteOnRelease = false
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)

	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "keep-retry", Keep: true})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if waitCalls != 2 {
		t.Fatalf("waitCalls=%d want 2", waitCalls)
	}
	if len(fake.created) != 2 {
		t.Fatalf("created=%d want 2", len(fake.created))
	}
	if len(fake.deleted) != 1 {
		t.Fatalf("deleted=%v want one cleaned retry", fake.deleted)
	}
	if fake.deleted[0] != fake.created[0].Name {
		t.Fatalf("deleted instance=%q want %q", fake.deleted[0], fake.created[0].Name)
	}
	if len(fake.instances) != 1 {
		t.Fatalf("instances=%d want 1", len(fake.instances))
	}

	firstLeaseID := fake.created[0].Config[labelKey("lease")]
	firstKeyPath, err := core.TestboxKeyPath(firstLeaseID)
	if err != nil {
		t.Fatalf("TestboxKeyPath(first): %v", err)
	}
	if _, err := os.Stat(firstKeyPath); !os.IsNotExist(err) {
		t.Fatalf("expected first retry key to be removed, stat err=%v", err)
	}

	if lease.LeaseID != fake.created[1].Config[labelKey("lease")] {
		t.Fatalf("lease.LeaseID=%q want %q", lease.LeaseID, fake.created[1].Config[labelKey("lease")])
	}
	secondKeyPath, err := core.TestboxKeyPath(lease.LeaseID)
	if err != nil {
		t.Fatalf("TestboxKeyPath(second): %v", err)
	}
	if _, err := os.Stat(secondKeyPath); err != nil {
		t.Fatalf("expected final keep lease key %q: %v", secondKeyPath, err)
	}
}

func TestAcquireUsesProxyHostWhenGuestAddressUnavailable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))

	oldNewClient := newClient
	oldWait := waitForSSHReady
	fake := &fakeClient{
		instances:            map[string]*api.Instance{},
		states:               map[string]*api.InstanceState{},
		preserveEmptyNetwork: true,
	}
	newClient = func(cfg Config) (instanceClient, error) {
		_ = cfg
		return fake, nil
	}
	waitForSSHReady = func(ctx context.Context, target *SSHTarget, stderr io.Writer, phase string, timeout time.Duration) error {
		_ = ctx
		_ = stderr
		_ = phase
		_ = timeout
		if target.Host != "incus-host.example.test" || target.Port != "2222" {
			t.Fatalf("target=%#v want incus proxy endpoint", target)
		}
		return nil
	}
	t.Cleanup(func() {
		newClient = oldNewClient
		waitForSSHReady = oldWait
	})

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Incus.Address = "https://incus-host.example.test:8443"
	cfg.Incus.ProxyListenHost = "0.0.0.0"
	cfg.Incus.ProxyListenPort = "2222"
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)

	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "proxy-only"})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if lease.SSH.Host != "incus-host.example.test" {
		t.Fatalf("lease.SSH.Host=%q want incus-host.example.test", lease.SSH.Host)
	}
	if lease.Server.PublicNet.IPv4.IP != "incus-host.example.test" {
		t.Fatalf("lease.Server.PublicNet.IPv4.IP=%q want incus-host.example.test", lease.Server.PublicNet.IPv4.IP)
	}
	created := fake.instances[lease.Server.CloudID]
	if got := created.Config[labelKey("proxy_host")]; got != "incus-host.example.test" {
		t.Fatalf("stored proxy_host=%q want incus-host.example.test", got)
	}
	if got := created.Config[labelKey("proxy_port")]; got != "2222" {
		t.Fatalf("stored proxy_port=%q want 2222", got)
	}
}

func TestResolveUsesPersistedProxyEndpointWhenFlagsAreOmitted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))

	oldNewClient := newClient
	oldWait := waitForSSHReady
	fake := &fakeClient{
		instances: map[string]*api.Instance{
			"crabbox-retained": {
				Name:       "crabbox-retained",
				Status:     "Stopped",
				StatusCode: api.Stopped,
				InstancePut: api.InstancePut{Config: map[string]string{
					labelKey("crabbox"):    "true",
					labelKey("lease"):      "cbx_abcd12345678",
					labelKey("slug"):       "proxy-retained",
					labelKey("state"):      "stopped",
					labelKey("ssh_port"):   "2222",
					labelKey("proxy_host"): "incus-host.example.test",
					labelKey("proxy_port"): "2222",
				}},
			},
		},
		states: map[string]*api.InstanceState{
			"crabbox-retained": {Status: "Stopped", StatusCode: api.Stopped},
		},
	}
	newClient = func(cfg Config) (instanceClient, error) {
		_ = cfg
		return fake, nil
	}
	waitForSSHReady = func(ctx context.Context, target *SSHTarget, stderr io.Writer, phase string, timeout time.Duration) error {
		_ = ctx
		_ = stderr
		_ = phase
		_ = timeout
		if target.Host != "incus-host.example.test" || target.Port != "2222" {
			t.Fatalf("target=%#v want persisted proxy endpoint", target)
		}
		return nil
	}
	t.Cleanup(func() {
		newClient = oldNewClient
		waitForSSHReady = oldWait
	})

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	if _, _, err := core.EnsureTestboxKeyForConfig(cfg, "cbx_abcd12345678"); err != nil {
		t.Fatalf("EnsureTestboxKeyForConfig: %v", err)
	}
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)

	lease, err := b.Resolve(context.Background(), ResolveRequest{ID: "cbx_abcd12345678"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if lease.SSH.Host != "incus-host.example.test" || lease.SSH.Port != "2222" {
		t.Fatalf("lease.SSH=%#v want persisted proxy endpoint", lease.SSH)
	}
	if lease.Server.PublicNet.IPv4.IP != "incus-host.example.test" {
		t.Fatalf("lease.Server.PublicNet.IPv4.IP=%q want persisted proxy host", lease.Server.PublicNet.IPv4.IP)
	}
}

func TestResolveStartsStoppedInstanceAndPersistsReadyLabels(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))

	oldNewClient := newClient
	oldWait := waitForSSHReady
	fake := &fakeClient{
		instances: map[string]*api.Instance{
			"crabbox-retained": {
				Name:       "crabbox-retained",
				Status:     "Stopped",
				StatusCode: api.Stopped,
				InstancePut: api.InstancePut{Config: map[string]string{
					labelKey("crabbox"): "true",
					labelKey("lease"):   "cbx_deadbeefcafe",
					labelKey("slug"):    "retained-slug",
					labelKey("state"):   "stopped",
					labelKey("host"):    "192.0.2.10",
				}},
			},
		},
		states: map[string]*api.InstanceState{
			"crabbox-retained": {Status: "Stopped", StatusCode: api.Stopped},
		},
	}
	newClient = func(cfg Config) (instanceClient, error) {
		_ = cfg
		return fake, nil
	}
	waitForSSHReady = func(ctx context.Context, target *SSHTarget, stderr io.Writer, phase string, timeout time.Duration) error {
		_ = ctx
		_ = stderr
		_ = phase
		_ = timeout
		if target.Host != "198.51.100.24" {
			t.Fatalf("target.Host=%q want refreshed address", target.Host)
		}
		return nil
	}
	t.Cleanup(func() {
		newClient = oldNewClient
		waitForSSHReady = oldWait
	})

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	if _, _, err := core.EnsureTestboxKeyForConfig(cfg, "cbx_deadbeefcafe"); err != nil {
		t.Fatalf("EnsureTestboxKeyForConfig: %v", err)
	}
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)

	lease, err := b.Resolve(context.Background(), ResolveRequest{ID: "cbx_deadbeefcafe"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(fake.stateUpdates) == 0 || fake.stateUpdates[0] != "crabbox-retained:start" {
		t.Fatalf("stateUpdates=%v want start", fake.stateUpdates)
	}
	if lease.SSH.Host != "198.51.100.24" {
		t.Fatalf("lease.SSH.Host=%q want refreshed address", lease.SSH.Host)
	}
	if got := fake.instances["crabbox-retained"].Config[labelKey("host")]; got != "198.51.100.24" {
		t.Fatalf("stored host=%q want refreshed address", got)
	}
	if got := fake.instances["crabbox-retained"].Config[labelKey("state")]; got != "ready" {
		t.Fatalf("stored state=%q want ready", got)
	}
}

func TestResolveChecksRepoClaimBeforeStartingInstance(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))

	oldNewClient := newClient
	fake := &fakeClient{
		instances: map[string]*api.Instance{
			"crabbox-retained": {
				Name:       "crabbox-retained",
				Status:     "Stopped",
				StatusCode: api.Stopped,
				InstancePut: api.InstancePut{Config: map[string]string{
					labelKey("crabbox"): "true",
					labelKey("lease"):   "cbx_claimed",
					labelKey("slug"):    "claimed",
				}},
			},
		},
		states: map[string]*api.InstanceState{
			"crabbox-retained": {Status: "Stopped", StatusCode: api.Stopped},
		},
	}
	newClient = func(Config) (instanceClient, error) { return fake, nil }
	t.Cleanup(func() { newClient = oldNewClient })

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	if err := core.ClaimLeaseForRepoProviderScopePond("cbx_claimed", "claimed", providerName, instanceScope("crabbox-retained"), cfg.Pond, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)

	_, err := b.Resolve(context.Background(), ResolveRequest{
		ID:   "crabbox-retained",
		Repo: core.Repo{Root: t.TempDir()},
	})
	if err == nil || !strings.Contains(err.Error(), "is claimed by repo") {
		t.Fatalf("Resolve error=%v", err)
	}
	if len(fake.stateUpdates) != 0 {
		t.Fatalf("claim conflict mutated instance state: %v", fake.stateUpdates)
	}
}

func TestResolveRestoresRepoClaimWhenStartFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))

	oldNewClient := newClient
	fake := &fakeClient{
		instances: map[string]*api.Instance{
			"crabbox-failing": {
				Name:       "crabbox-failing",
				Status:     "Stopped",
				StatusCode: api.Stopped,
				InstancePut: api.InstancePut{Config: map[string]string{
					labelKey("crabbox"): "true",
					labelKey("lease"):   "cbx_failing",
					labelKey("slug"):    "failing",
				}},
			},
		},
		states: map[string]*api.InstanceState{
			"crabbox-failing": {Status: "Stopped", StatusCode: api.Stopped},
		},
		stateErr: io.ErrUnexpectedEOF,
	}
	newClient = func(Config) (instanceClient, error) { return fake, nil }
	t.Cleanup(func() { newClient = oldNewClient })

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	_, err := b.Resolve(context.Background(), ResolveRequest{
		ID:   "crabbox-failing",
		Repo: core.Repo{Root: t.TempDir()},
	})
	if err == nil {
		t.Fatal("Resolve succeeded")
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence("cbx_failing"); err != nil || exists {
		t.Fatalf("failed resolve retained claim exists=%v err=%v", exists, err)
	}
}

func TestResolveFallsBackToConfiguredKeyWhenStoredKeyIsMissing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))

	oldNewClient := newClient
	oldWait := waitForSSHReady
	fake := &fakeClient{
		instances: map[string]*api.Instance{
			"crabbox-nokey": {
				Name:       "crabbox-nokey",
				Status:     "Running",
				StatusCode: api.Running,
				InstancePut: api.InstancePut{Config: map[string]string{
					labelKey("crabbox"): "true",
					labelKey("lease"):   "cbx_a1b2c3d4e5f6",
					labelKey("slug"):    "nokey-slug",
					labelKey("state"):   "ready",
					labelKey("host"):    "198.51.100.24",
				}},
			},
		},
		states: map[string]*api.InstanceState{
			"crabbox-nokey": {Status: "Running", StatusCode: api.Running, Network: map[string]api.InstanceStateNetwork{
				"eth0": {Addresses: []api.InstanceStateNetworkAddress{{Family: "inet", Scope: "global", Address: "198.51.100.24"}}},
			}},
		},
	}
	newClient = func(cfg Config) (instanceClient, error) {
		_ = cfg
		return fake, nil
	}
	waitForSSHReady = func(ctx context.Context, target *SSHTarget, stderr io.Writer, phase string, timeout time.Duration) error {
		_ = ctx
		_ = stderr
		_ = phase
		_ = timeout
		return nil
	}
	t.Cleanup(func() {
		newClient = oldNewClient
		waitForSSHReady = oldWait
	})

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.SSHKey = filepath.Join(t.TempDir(), "configured-key")
	if err := os.WriteFile(cfg.SSHKey, []byte("fake"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)

	lease, err := b.Resolve(context.Background(), ResolveRequest{ID: "cbx_a1b2c3d4e5f6"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if lease.SSH.Key != cfg.SSHKey {
		t.Fatalf("SSH.Key=%q want configured key %q when stored key is missing", lease.SSH.Key, cfg.SSHKey)
	}
}

func TestResolveUsesLeaseLabelsForSSHUserAndPort(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))

	oldNewClient := newClient
	oldWait := waitForSSHReady
	fake := &fakeClient{
		instances: map[string]*api.Instance{
			"crabbox-label": {
				Name:       "crabbox-label",
				Status:     "Running",
				StatusCode: api.Running,
				InstancePut: api.InstancePut{Config: map[string]string{
					labelKey("crabbox"):  "true",
					labelKey("lease"):    "cbx_1abe1e55f00d",
					labelKey("slug"):     "label-slug",
					labelKey("state"):    "ready",
					labelKey("host"):     "198.51.100.24",
					labelKey("ssh_user"): "ubuntu",
					labelKey("ssh_port"): "2201",
				}},
			},
		},
		states: map[string]*api.InstanceState{
			"crabbox-label": {Status: "Running", StatusCode: api.Running, Network: map[string]api.InstanceStateNetwork{
				"eth0": {Addresses: []api.InstanceStateNetworkAddress{{Family: "inet", Scope: "global", Address: "198.51.100.24"}}},
			}},
		},
	}
	newClient = func(cfg Config) (instanceClient, error) {
		_ = cfg
		return fake, nil
	}
	waitForSSHReady = func(ctx context.Context, target *SSHTarget, stderr io.Writer, phase string, timeout time.Duration) error {
		_ = ctx
		_ = stderr
		_ = phase
		_ = timeout
		return nil
	}
	t.Cleanup(func() {
		newClient = oldNewClient
		waitForSSHReady = oldWait
	})

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)

	lease, err := b.Resolve(context.Background(), ResolveRequest{ID: "cbx_1abe1e55f00d"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if lease.SSH.User != "ubuntu" {
		t.Fatalf("SSH.User=%q want ubuntu from lease label", lease.SSH.User)
	}
	if lease.SSH.Port != "2201" {
		t.Fatalf("SSH.Port=%q want 2201 from lease label", lease.SSH.Port)
	}
}

func TestResolveIgnoresStaleHostLabelWhileWaitingForLiveAddress(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))

	oldNewClient := newClient
	oldWait := waitForSSHReady
	fake := &fakeClient{
		instances: map[string]*api.Instance{
			"crabbox-stale-host": {
				Name:       "crabbox-stale-host",
				Status:     "Stopped",
				StatusCode: api.Stopped,
				InstancePut: api.InstancePut{Config: map[string]string{
					labelKey("crabbox"): "true",
					labelKey("lease"):   "cbx_deadbeefcafe",
					labelKey("slug"):    "stale-slug",
					labelKey("state"):   "stopped",
					labelKey("host"):    "192.0.2.10",
				}},
			},
		},
		states: map[string]*api.InstanceState{
			"crabbox-stale-host": {Status: "Stopped", StatusCode: api.Stopped},
		},
		preserveEmptyNetwork: true,
	}
	counting := &stateCountingClient{fakeClient: fake}
	counting.afterGetState = func(calls int) {
		if calls == 1 {
			fake.states["crabbox-stale-host"] = &api.InstanceState{
				Status:     "Running",
				StatusCode: api.Running,
				Network: map[string]api.InstanceStateNetwork{
					"eth0": {Addresses: []api.InstanceStateNetworkAddress{{Family: "inet", Scope: "global", Address: "198.51.100.24"}}},
				},
			}
		}
	}
	newClient = func(cfg Config) (instanceClient, error) {
		_ = cfg
		return counting, nil
	}
	waitForSSHReady = func(ctx context.Context, target *SSHTarget, stderr io.Writer, phase string, timeout time.Duration) error {
		_ = ctx
		_ = stderr
		_ = phase
		_ = timeout
		if target.Host != "198.51.100.24" {
			t.Fatalf("target.Host=%q want live address", target.Host)
		}
		return nil
	}
	t.Cleanup(func() {
		newClient = oldNewClient
		waitForSSHReady = oldWait
	})

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Incus.StartTimeout = 5 * time.Second
	if _, _, err := core.EnsureTestboxKeyForConfig(cfg, "cbx_deadbeefcafe"); err != nil {
		t.Fatalf("EnsureTestboxKeyForConfig: %v", err)
	}
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)

	lease, err := b.Resolve(context.Background(), ResolveRequest{ID: "cbx_deadbeefcafe"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if counting.getStateCalls < 2 {
		t.Fatalf("Resolve should keep polling for a live address, got %d GetInstanceState calls", counting.getStateCalls)
	}
	if lease.SSH.Host != "198.51.100.24" {
		t.Fatalf("lease.SSH.Host=%q want live address", lease.SSH.Host)
	}
}

func TestResolvePrefersLiveAddressOverStoredHost(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))

	oldNewClient := newClient
	oldWait := waitForSSHReady
	fake := &fakeClient{
		instances: map[string]*api.Instance{
			"crabbox-running": {
				Name:       "crabbox-running",
				Status:     "Running",
				StatusCode: api.Running,
				InstancePut: api.InstancePut{Config: map[string]string{
					labelKey("crabbox"): "true",
					labelKey("lease"):   "cbx_feedfaceb00c",
					labelKey("slug"):    "running-slug",
					labelKey("state"):   "ready",
					labelKey("host"):    "192.0.2.10",
				}},
			},
		},
		states: map[string]*api.InstanceState{
			"crabbox-running": {
				Status:     "Running",
				StatusCode: api.Running,
				Network: map[string]api.InstanceStateNetwork{
					"eth1": {Addresses: []api.InstanceStateNetworkAddress{{Family: "inet", Scope: "global", Address: "198.51.100.99"}}},
					"eth0": {Addresses: []api.InstanceStateNetworkAddress{{Family: "inet", Scope: "global", Address: "198.51.100.24"}}},
				},
			},
		},
	}
	newClient = func(cfg Config) (instanceClient, error) {
		_ = cfg
		return fake, nil
	}
	waitForSSHReady = func(ctx context.Context, target *SSHTarget, stderr io.Writer, phase string, timeout time.Duration) error {
		_ = ctx
		_ = stderr
		_ = phase
		_ = timeout
		if target.Host != "198.51.100.24" {
			t.Fatalf("target.Host=%q want deterministic live address", target.Host)
		}
		return nil
	}
	t.Cleanup(func() {
		newClient = oldNewClient
		waitForSSHReady = oldWait
	})

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	if _, _, err := core.EnsureTestboxKeyForConfig(cfg, "cbx_feedfaceb00c"); err != nil {
		t.Fatalf("EnsureTestboxKeyForConfig: %v", err)
	}
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)

	lease, err := b.Resolve(context.Background(), ResolveRequest{ID: "cbx_feedfaceb00c"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(fake.stateUpdates) != 0 {
		t.Fatalf("stateUpdates=%v want no restart for running instance", fake.stateUpdates)
	}
	if lease.SSH.Host != "198.51.100.24" {
		t.Fatalf("lease.SSH.Host=%q want deterministic live address", lease.SSH.Host)
	}
	if got := fake.instances["crabbox-running"].Config[labelKey("host")]; got != "198.51.100.24" {
		t.Fatalf("stored host=%q want deterministic live address", got)
	}
}

func TestResolveStatusOnlyUsesLiveStoppedState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))

	oldNewClient := newClient
	fake := &fakeClient{
		instances: map[string]*api.Instance{
			"crabbox-status": {
				Name:       "crabbox-status",
				Status:     "Running",
				StatusCode: api.Running,
				InstancePut: api.InstancePut{Config: map[string]string{
					labelKey("crabbox"): "true",
					labelKey("lease"):   "cbx_0badf00dbeef",
					labelKey("slug"):    "status-slug",
					labelKey("state"):   "ready",
					labelKey("host"):    "192.0.2.10",
				}},
			},
		},
		states: map[string]*api.InstanceState{
			"crabbox-status": {Status: "Stopped", StatusCode: api.Stopped},
		},
	}
	newClient = func(cfg Config) (instanceClient, error) {
		_ = cfg
		return fake, nil
	}
	t.Cleanup(func() { newClient = oldNewClient })

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)

	lease, err := b.Resolve(context.Background(), ResolveRequest{ID: "cbx_0badf00dbeef", StatusOnly: true})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if lease.Server.Status != "stopped" {
		t.Fatalf("server status=%q want stopped", lease.Server.Status)
	}
	if lease.Server.Labels["state"] != "stopped" {
		t.Fatalf("label state=%q want stopped", lease.Server.Labels["state"])
	}
}

func TestResolveStatusOnlyPromotesRunningStateWhenStoredLabelIsStale(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))

	oldNewClient := newClient
	fake := &fakeClient{
		instances: map[string]*api.Instance{
			"crabbox-status": {
				Name:       "crabbox-status",
				Status:     "Running",
				StatusCode: api.Running,
				InstancePut: api.InstancePut{Config: map[string]string{
					labelKey("crabbox"): "true",
					labelKey("lease"):   "cbx_feedfacebead",
					labelKey("slug"):    "status-slug",
					labelKey("state"):   "stopped",
					labelKey("host"):    "192.0.2.10",
				}},
			},
		},
		states: map[string]*api.InstanceState{
			"crabbox-status": {Status: "Running", StatusCode: api.Running},
		},
	}
	newClient = func(cfg Config) (instanceClient, error) {
		_ = cfg
		return fake, nil
	}
	t.Cleanup(func() { newClient = oldNewClient })

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)

	lease, err := b.Resolve(context.Background(), ResolveRequest{ID: "cbx_feedfacebead", StatusOnly: true})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if lease.Server.Status != "running" {
		t.Fatalf("server status=%q want running", lease.Server.Status)
	}
	if lease.Server.Labels["state"] != "running" {
		t.Fatalf("label state=%q want running", lease.Server.Labels["state"])
	}
}

func TestReleaseLeaseRetainsStoppedInstanceWhenDeleteOnReleaseFalse(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))

	oldNewClient := newClient
	fake := &fakeClient{
		instances: map[string]*api.Instance{
			"crabbox-retained": {
				Name:       "crabbox-retained",
				Status:     "Running",
				StatusCode: api.Running,
				InstancePut: api.InstancePut{Config: map[string]string{
					labelKey("crabbox"): "true",
					labelKey("lease"):   "cbx_retain123456",
					labelKey("slug"):    "retained-slug",
					labelKey("release"): "delete",
				}},
			},
		},
		states: map[string]*api.InstanceState{
			"crabbox-retained": {Status: "Running", StatusCode: api.Running},
		},
	}
	newClient = func(cfg Config) (instanceClient, error) {
		_ = cfg
		return fake, nil
	}
	t.Cleanup(func() { newClient = oldNewClient })

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Incus.DeleteOnRelease = false
	core.MarkDeleteOnReleaseExplicit(&cfg, providerName)
	if _, _, err := core.EnsureTestboxKeyForConfig(cfg, "cbx_retain123456"); err != nil {
		t.Fatalf("EnsureTestboxKeyForConfig: %v", err)
	}
	if err := core.ClaimLeaseForRepoProviderScopePond("cbx_retain123456", "retained-slug", providerName, "instance:crabbox-retained", "", t.TempDir(), cfg.IdleTimeout, false); err != nil {
		t.Fatalf("ClaimLeaseForRepoProviderScopePond: %v", err)
	}
	if err := core.UpdateLeaseClaimEndpoint("cbx_retain123456", core.Server{Labels: map[string]string{"state": "ready"}}, core.SSHTarget{Host: "198.51.100.24", Port: "22"}); err != nil {
		t.Fatalf("UpdateLeaseClaimEndpoint: %v", err)
	}
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)

	lease := core.LeaseTarget{LeaseID: "cbx_retain123456", Server: core.Server{Name: "crabbox-retained", CloudID: "crabbox-retained", Labels: map[string]string{"lease": "cbx_retain123456", "slug": "retained-slug", "release": "delete"}}}
	if !b.RetainLeaseClaimAfterRelease(lease) {
		t.Fatal("explicit retain policy did not override stored delete policy")
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease, Force: true}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("deleted=%v want retained instance to be stopped, not deleted", fake.deleted)
	}
	if len(fake.stateUpdates) == 0 || fake.stateUpdates[0] != "crabbox-retained:stop" {
		t.Fatalf("stateUpdates=%v want stop", fake.stateUpdates)
	}
	if got := fake.instances["crabbox-retained"].Config[labelKey("state")]; got != "stopped" {
		t.Fatalf("stored state=%q want stopped", got)
	}
	if got := fake.instances["crabbox-retained"].Config[labelKey("release")]; got != "stop" {
		t.Fatalf("stored release=%q want stop", got)
	}
	if got := fake.instances["crabbox-retained"].Config[labelKey("host")]; got != "" {
		t.Fatalf("stored host=%q want cleared", got)
	}
	claim, err := core.ReadLeaseClaim("cbx_retain123456")
	if err != nil {
		t.Fatalf("ReadLeaseClaim: %v", err)
	}
	if claim.Labels["state"] != "stopped" {
		t.Fatalf("claim state=%q want stopped", claim.Labels["state"])
	}
	if claim.Labels["release"] != "stop" {
		t.Fatalf("claim release=%q want stop", claim.Labels["release"])
	}
	if claim.SSHHost != "" || claim.SSHPort != 0 {
		t.Fatalf("claim endpoint=%s:%d want cleared", claim.SSHHost, claim.SSHPort)
	}
	if _, err := core.TestboxKeyPath("cbx_retain123456"); err != nil {
		t.Fatalf("expected retained lease key to remain available: %v", err)
	}
}

func TestReleaseLeaseRetainIsIdempotentWhenInstanceAlreadyStopped(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))

	oldNewClient := newClient
	fake := &fakeClient{
		instances: map[string]*api.Instance{
			"crabbox-retained": {
				Name:       "crabbox-retained",
				Status:     "Stopped",
				StatusCode: api.Stopped,
				InstancePut: api.InstancePut{Config: map[string]string{
					labelKey("crabbox"): "true",
					labelKey("lease"):   "cbx_abcdef123456",
					labelKey("slug"):    "stopped-retained",
					labelKey("state"):   "stopped",
				}},
			},
		},
		states: map[string]*api.InstanceState{
			"crabbox-retained": {Status: "Stopped", StatusCode: api.Stopped},
		},
	}
	newClient = func(cfg Config) (instanceClient, error) {
		_ = cfg
		return fake, nil
	}
	t.Cleanup(func() { newClient = oldNewClient })

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Incus.DeleteOnRelease = false
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)

	lease := core.LeaseTarget{LeaseID: "cbx_abcdef123456", Server: core.Server{Name: "crabbox-retained", CloudID: "crabbox-retained"}}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease, Force: true}); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}
	if len(fake.stateUpdates) != 0 {
		t.Fatalf("stateUpdates=%v want no stop for already stopped instance", fake.stateUpdates)
	}
	if got := fake.instances["crabbox-retained"].Config[labelKey("state")]; got != "stopped" {
		t.Fatalf("stored state=%q want stopped", got)
	}
}

func TestReleaseLeaseDeleteOnReleaseRemovesClaimAndKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))

	oldNewClient := newClient
	fake := &fakeClient{
		deleteRequiresStop: true,
		instances: map[string]*api.Instance{
			"crabbox-delete": {
				Name:       "crabbox-delete",
				Status:     "Running",
				StatusCode: api.Running,
				InstancePut: api.InstancePut{Config: map[string]string{
					labelKey("crabbox"): "true",
					labelKey("lease"):   "cbx_delete12345",
					labelKey("slug"):    "delete-slug",
				}},
			},
		},
		states: map[string]*api.InstanceState{
			"crabbox-delete": {Status: "Running", StatusCode: api.Running},
		},
	}
	newClient = func(cfg Config) (instanceClient, error) {
		_ = cfg
		return fake, nil
	}
	t.Cleanup(func() { newClient = oldNewClient })

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Incus.DeleteOnRelease = true
	if _, _, err := core.EnsureTestboxKeyForConfig(cfg, "cbx_delete12345"); err != nil {
		t.Fatalf("EnsureTestboxKeyForConfig: %v", err)
	}
	if err := core.ClaimLeaseForRepoProviderScopePond("cbx_delete12345", "delete-slug", providerName, "instance:crabbox-delete", "", t.TempDir(), cfg.IdleTimeout, false); err != nil {
		t.Fatalf("ClaimLeaseForRepoProviderScopePond: %v", err)
	}
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)

	lease := core.LeaseTarget{LeaseID: "cbx_delete12345", Server: core.Server{Name: "crabbox-delete", CloudID: "crabbox-delete", Labels: map[string]string{"lease": "cbx_delete12345", "slug": "delete-slug"}}}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease, Force: true}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != "crabbox-delete" {
		t.Fatalf("deleted=%v want crabbox-delete removed", fake.deleted)
	}
	if len(fake.stateUpdates) != 1 || fake.stateUpdates[0] != "crabbox-delete:stop" {
		t.Fatalf("stateUpdates=%v want stop before delete", fake.stateUpdates)
	}
	claim, err := core.ReadLeaseClaim("cbx_delete12345")
	if err != nil {
		t.Fatalf("ReadLeaseClaim: %v", err)
	}
	if claim.LeaseID != "" {
		t.Fatalf("expected delete-on-release to remove lease claim, got %#v", claim)
	}
	keyPath, err := core.TestboxKeyPath("cbx_delete12345")
	if err != nil {
		t.Fatalf("TestboxKeyPath: %v", err)
	}
	if _, err := os.Stat(keyPath); err == nil {
		t.Fatal("expected delete-on-release to remove stored key")
	}
}

func TestListSkipsForeignInstancesBeforeManagedOnes(t *testing.T) {
	oldNewClient := newClient
	fake := &fakeClient{
		instances: map[string]*api.Instance{
			"foreign": {
				Name:        "foreign",
				Status:      "Running",
				StatusCode:  api.Running,
				InstancePut: api.InstancePut{Config: map[string]string{}},
			},
			"crabbox-managed": {
				Name:       "crabbox-managed",
				Status:     "Running",
				StatusCode: api.Running,
				InstancePut: api.InstancePut{Config: map[string]string{
					labelKey("crabbox"): "true",
					labelKey("lease"):   "cbx_abcd1234ef56",
					labelKey("slug"):    "managed",
				}},
			},
		},
		listOrder: []string{"foreign", "crabbox-managed"},
	}
	newClient = func(cfg Config) (instanceClient, error) {
		_ = cfg
		return fake, nil
	}
	t.Cleanup(func() { newClient = oldNewClient })

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)

	views, err := b.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(views) != 1 || views[0].Name != "crabbox-managed" {
		t.Fatalf("views=%#v want only managed instance", views)
	}
}

func TestResolveSkipsForeignInstancesBeforeManagedOnes(t *testing.T) {
	oldNewClient := newClient
	fake := &fakeClient{
		instances: map[string]*api.Instance{
			"foreign": {
				Name:        "foreign",
				Status:      "Running",
				StatusCode:  api.Running,
				InstancePut: api.InstancePut{Config: map[string]string{}},
			},
			"crabbox-managed": {
				Name:       "crabbox-managed",
				Status:     "Running",
				StatusCode: api.Running,
				InstancePut: api.InstancePut{Config: map[string]string{
					labelKey("crabbox"): "true",
					labelKey("lease"):   "cbx_abcd1234ef56",
					labelKey("slug"):    "managed",
					labelKey("state"):   "ready",
				}},
			},
		},
		states: map[string]*api.InstanceState{
			"foreign":         {Status: "Running", StatusCode: api.Running},
			"crabbox-managed": {Status: "Running", StatusCode: api.Running},
		},
		listOrder: []string{"foreign", "crabbox-managed"},
	}
	newClient = func(cfg Config) (instanceClient, error) {
		_ = cfg
		return fake, nil
	}
	t.Cleanup(func() { newClient = oldNewClient })

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)

	lease, err := b.Resolve(context.Background(), ResolveRequest{ID: "cbx_abcd1234ef56", StatusOnly: true})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if lease.Server.Name != "crabbox-managed" {
		t.Fatalf("resolved=%#v want managed instance", lease)
	}
}

func TestCleanupContinuesPastForeignAndFreshInstances(t *testing.T) {
	oldNewClient := newClient
	now := time.Now().UTC()
	fake := &fakeClient{
		instances: map[string]*api.Instance{
			"foreign": {
				Name:        "foreign",
				Status:      "Running",
				StatusCode:  api.Running,
				InstancePut: api.InstancePut{Config: map[string]string{}},
			},
			"crabbox-fresh": {
				Name:       "crabbox-fresh",
				Status:     "Running",
				StatusCode: api.Running,
				InstancePut: api.InstancePut{Config: map[string]string{
					labelKey("crabbox"):           "true",
					labelKey("lease"):             "cbx_fresh123456",
					labelKey("slug"):              "fresh",
					labelKey("state"):             "ready",
					labelKey("created_at"):        core.LeaseLabelTime(now.Add(-5 * time.Minute)),
					labelKey("last_touched_at"):   core.LeaseLabelTime(now.Add(-5 * time.Minute)),
					labelKey("idle_timeout"):      "3600",
					labelKey("idle_timeout_secs"): "3600",
					labelKey("ttl_secs"):          "7200",
					labelKey("expires_at"):        core.LeaseLabelTime(now.Add(time.Hour)),
				}},
			},
			"crabbox-stale": {
				Name:       "crabbox-stale",
				Status:     "Stopped",
				StatusCode: api.Stopped,
				InstancePut: api.InstancePut{Config: map[string]string{
					labelKey("crabbox"):           "true",
					labelKey("lease"):             "cbx_stale123456",
					labelKey("slug"):              "stale",
					labelKey("state"):             "ready",
					labelKey("created_at"):        core.LeaseLabelTime(now.Add(-2 * time.Hour)),
					labelKey("last_touched_at"):   core.LeaseLabelTime(now.Add(-2 * time.Hour)),
					labelKey("idle_timeout"):      "60",
					labelKey("idle_timeout_secs"): "60",
					labelKey("ttl_secs"):          "120",
					labelKey("expires_at"):        core.LeaseLabelTime(now.Add(-time.Hour)),
				}},
			},
		},
		states: map[string]*api.InstanceState{
			"foreign":       {Status: "Running", StatusCode: api.Running},
			"crabbox-fresh": {Status: "Running", StatusCode: api.Running},
			"crabbox-stale": {Status: "Stopped", StatusCode: api.Stopped},
		},
		listOrder: []string{"foreign", "crabbox-fresh", "crabbox-stale"},
	}
	newClient = func(cfg Config) (instanceClient, error) {
		_ = cfg
		return fake, nil
	}
	t.Cleanup(func() { newClient = oldNewClient })

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)

	if err := b.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != "crabbox-stale" {
		t.Fatalf("deleted=%v want only stale instance removed", fake.deleted)
	}
}

func TestCleanupStopsRunningStaleInstancesBeforeDelete(t *testing.T) {
	oldNewClient := newClient
	now := time.Now().UTC()
	fake := &fakeClient{
		deleteRequiresStop: true,
		instances: map[string]*api.Instance{
			"crabbox-stale": {
				Name:       "crabbox-stale",
				Status:     "Running",
				StatusCode: api.Running,
				InstancePut: api.InstancePut{Config: map[string]string{
					labelKey("crabbox"):           "true",
					labelKey("lease"):             "cbx_stale654321",
					labelKey("slug"):              "stale-running",
					labelKey("state"):             "ready",
					labelKey("created_at"):        core.LeaseLabelTime(now.Add(-2 * time.Hour)),
					labelKey("last_touched_at"):   core.LeaseLabelTime(now.Add(-2 * time.Hour)),
					labelKey("idle_timeout"):      "60",
					labelKey("idle_timeout_secs"): "60",
					labelKey("ttl_secs"):          "120",
					labelKey("expires_at"):        core.LeaseLabelTime(now.Add(-time.Hour)),
				}},
			},
		},
		states: map[string]*api.InstanceState{
			"crabbox-stale": {Status: "Running", StatusCode: api.Running},
		},
	}
	newClient = func(cfg Config) (instanceClient, error) {
		_ = cfg
		return fake, nil
	}
	t.Cleanup(func() { newClient = oldNewClient })

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)

	if err := b.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != "crabbox-stale" {
		t.Fatalf("deleted=%v want stale instance removed", fake.deleted)
	}
	if len(fake.stateUpdates) != 1 || fake.stateUpdates[0] != "crabbox-stale:stop" {
		t.Fatalf("stateUpdates=%v want stop before delete", fake.stateUpdates)
	}
}

func TestReleaseLeaseMessageReflectsDeleteAndRetainPaths(t *testing.T) {
	lease := core.LeaseTarget{
		LeaseID: "cbx_123456789abc",
		Server:  core.Server{Name: "crabbox-retained", CloudID: "crabbox-retained", Labels: map[string]string{"release": "delete"}},
	}

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Incus.DeleteOnRelease = true
	deleteBackend := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	if got := deleteBackend.ReleaseLeaseMessage(lease); got != "deleted lease=cbx_123456789abc instance=crabbox-retained" {
		t.Fatalf("delete message=%q", got)
	}
	if deleteBackend.RetainLeaseClaimAfterRelease(lease) {
		t.Fatal("delete-on-release backend retained claim")
	}

	lease.Server.Labels["release"] = "stop"
	retainBackend := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	if got := retainBackend.ReleaseLeaseMessage(lease); got != "stopped lease=cbx_123456789abc instance=crabbox-retained retained=true" {
		t.Fatalf("retain message=%q", got)
	}
	if !retainBackend.RetainLeaseClaimAfterRelease(lease) {
		t.Fatal("stop-on-release backend removed claim")
	}

	core.MarkDeleteOnReleaseExplicit(&cfg, providerName)
	explicitBackend := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	if got := explicitBackend.ReleaseLeaseMessage(lease); got != "deleted lease=cbx_123456789abc instance=crabbox-retained" {
		t.Fatalf("explicit delete message=%q", got)
	}
}

func cloneMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneMapMap(in map[string]map[string]string) map[string]map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]map[string]string, len(in))
	for k, v := range in {
		out[k] = cloneMap(v)
	}
	return out
}
