package incus

import (
	"context"
	"flag"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/lxc/incus/v7/shared/cliconfig"
	core "github.com/openclaw/crabbox/internal/cli"
)

type fakeClient struct {
	instances    map[string]*api.Instance
	states       map[string]*api.InstanceState
	deleted      []string
	created      []api.InstancesPost
	updated      []string
	stateUpdates []string
	getCalls     map[string]int
	getErr       error
	listErr      error
	createErr    error
	updateErr    error
	stateErr     error
	deleteErr    error
}

func (f *fakeClient) ListInstances() ([]api.Instance, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]api.Instance, 0, len(f.instances))
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
	if state, ok := f.states[name]; ok {
		if host := bestAddress(*inst, state); host != "" {
			inst.Config[labelKey("host")] = host
		}
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
		if len(state.Network) == 0 {
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
	delete(f.instances, name)
	delete(f.states, name)
	f.deleted = append(f.deleted, name)
	return nil
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
		"--incus-proxy-listen-port", "2201",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := defaults
	if err := applyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Incus.InstanceType != "virtual-machine" || cfg.Incus.User != "ubuntu" || cfg.WorkRoot != "/workspace/incus" || cfg.SSHPort != "2201" {
		t.Fatalf("flags not applied: %#v", cfg.Incus)
	}
	if cfg.ServerType != "virtual-machine:images:ubuntu/24.04/cloud" {
		t.Fatalf("serverType=%q", cfg.ServerType)
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

func TestSSHTargetHostUsesIncusHostWhenProxyPortConfigured(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Incus.Address = "https://incus-host.example.test:8443"
	cfg.Incus.ProxyListenPort = "2222"

	server := core.Server{}
	server.PublicNet.IPv4.IP = "198.51.100.24"
	if got := sshTargetHost(server, cfg); got != "incus-host.example.test" {
		t.Fatalf("sshTargetHost=%q want incus-host.example.test", got)
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

	_, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "cleanup-check"})
	if err == nil {
		t.Fatal("Acquire unexpectedly succeeded")
	}
	if len(fake.deleted) != 1 {
		t.Fatalf("deleted=%v want partial instance cleanup", fake.deleted)
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
