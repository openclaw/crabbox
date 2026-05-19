package runpod

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRunpodProviderSpec(t *testing.T) {
	spec := Provider{}.Spec()
	if spec.Name != providerName {
		t.Fatalf("spec.Name = %q, want %q", spec.Name, providerName)
	}
	if spec.Kind != "ssh-lease" {
		t.Fatalf("spec.Kind = %q, want ssh-lease", spec.Kind)
	}
	aliases := Provider{}.Aliases()
	if len(aliases) != 2 || aliases[0] != "run-pod" || aliases[1] != "runpodio" {
		t.Fatalf("aliases = %#v, want [run-pod runpodio]", aliases)
	}
}

func TestRunpodIsRunpodProviderNameAcceptsAliases(t *testing.T) {
	for _, name := range []string{"runpod", "Run-Pod", "  runpodio  ", "RUNPOD"} {
		if !isRunpodProviderName(name) {
			t.Fatalf("isRunpodProviderName(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", "exe-dev", "railway", "runpods"} {
		if isRunpodProviderName(name) {
			t.Fatalf("isRunpodProviderName(%q) = true, want false", name)
		}
	}
}

func TestRunpodClientRequiresAPIKey(t *testing.T) {
	cfg := Config{}
	cfg.Runpod.APIURL = "https://api.runpod.io/graphql"
	if _, err := newRunpodClient(cfg, Runtime{}); err == nil {
		t.Fatal("newRunpodClient accepted empty API key")
	}
}

func TestRunpodClientRejectsBareHTTPURL(t *testing.T) {
	cfg := Config{}
	cfg.Runpod.APIKey = "test-key"
	cfg.Runpod.APIURL = "http://api.runpod.io/graphql"
	if _, err := newRunpodClient(cfg, Runtime{}); err == nil {
		t.Fatal("newRunpodClient accepted plaintext http URL")
	}
}

func TestRunpodClientAllowsLoopbackHTTPURL(t *testing.T) {
	cfg := Config{}
	cfg.Runpod.APIKey = "test-key"
	cfg.Runpod.APIURL = "http://127.0.0.1:8080/graphql"
	if _, err := newRunpodClient(cfg, Runtime{}); err != nil {
		t.Fatalf("loopback http rejected: %v", err)
	}
}

func TestRunpodTokenFlagIsNotRegistered(t *testing.T) {
	cfg := Config{}
	cfg.Runpod.APIKey = "secret-key"
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	RegisterRunpodProviderFlags(fs, cfg)
	for _, name := range []string{"runpod-token", "runpod-api-token", "runpod-key", "runpod-api-key"} {
		if fs.Lookup(name) != nil {
			t.Fatalf("runpod API key surfaced as a flag --%s", name)
		}
	}
	for _, name := range []string{"runpod-url", "runpod-cloud-type", "runpod-instance-id", "runpod-image", "runpod-template-id", "runpod-disk-gb", "runpod-user", "runpod-work-root"} {
		if fs.Lookup(name) == nil {
			t.Fatalf("%s flag missing", name)
		}
	}
}

func TestRunpodFlagsRejectGenericClassAndType(t *testing.T) {
	for _, args := range [][]string{
		{"--class", "beast"},
		{"--type", "ubuntu:24.04"},
	} {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fs.String("class", "", "")
		fs.String("type", "", "")
		values := RegisterRunpodProviderFlags(fs, Config{})
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		cfg := Config{Provider: providerName}
		err := ApplyRunpodProviderFlags(&cfg, fs, values)
		if err == nil || !strings.Contains(err.Error(), "not supported for provider=runpod") {
			t.Fatalf("args=%v err=%v", args, err)
		}
	}
}

func TestRunpodConfigureRejectsUnsupportedTargetAndTailscale(t *testing.T) {
	for name, cfg := range map[string]Config{
		"macos target": {TargetOS: "macos"},
		"tailscale":    {TargetOS: targetLinux, Tailscale: TailscaleConfig{Enabled: true}},
		"network":      {TargetOS: targetLinux, Network: "tailscale"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Provider{}.Configure(cfg, Runtime{Stdout: io.Discard, Stderr: io.Discard})
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestRunpodDefaultsPickCheapCPUFlavorAndPreserveCustomWorkRoot(t *testing.T) {
	cfg := Config{}
	applyRunpodDefaults(&cfg)
	if cfg.Runpod.InstanceID != "cpu3c-2-4" {
		t.Fatalf("default instanceId = %q, want cpu3c-2-4 (cheapest documented CPU flavor)", cfg.Runpod.InstanceID)
	}
	if cfg.Runpod.CloudType != "ALL" {
		t.Fatalf("default cloudType = %q, want ALL", cfg.Runpod.CloudType)
	}
	if cfg.Runpod.DiskGB != 10 {
		t.Fatalf("default diskGB = %d, want 10", cfg.Runpod.DiskGB)
	}

	cfg = Config{WorkRoot: "/custom/crabbox"}
	applyRunpodDefaults(&cfg)
	if cfg.WorkRoot != "/custom/crabbox" || cfg.Runpod.WorkRoot != "/custom/crabbox" {
		t.Fatalf("workRoot=%q runpod.workRoot=%q", cfg.WorkRoot, cfg.Runpod.WorkRoot)
	}

	cfg = Config{WorkRoot: "/custom/crabbox", Runpod: RunpodConfig{WorkRoot: "/runpod/crabbox"}}
	applyRunpodDefaults(&cfg)
	if cfg.WorkRoot != "/runpod/crabbox" || cfg.Runpod.WorkRoot != "/runpod/crabbox" {
		t.Fatalf("workRoot=%q runpod.workRoot=%q", cfg.WorkRoot, cfg.Runpod.WorkRoot)
	}
}

func TestRunpodSSHEndpointPicksPublicPort22(t *testing.T) {
	pod := runpodPod{Runtime: &runpodRuntime{Ports: []runpodRuntimePort{
		{IP: "10.0.0.1", PrivatePort: 8888, PublicPort: 41234, IsIPPublic: true, Type: "tcp"},
		{IP: "203.0.113.5", PrivatePort: 22, PublicPort: 32100, IsIPPublic: true, Type: "tcp"},
		{IP: "203.0.113.5", PrivatePort: 22, PublicPort: 32101, IsIPPublic: true, Type: "tcp"},
	}}}
	host, port := pod.SSHEndpoint()
	if host != "203.0.113.5" || port != 32100 {
		t.Fatalf("host=%q port=%d, want 203.0.113.5:32100", host, port)
	}

	pod = runpodPod{Runtime: &runpodRuntime{Ports: []runpodRuntimePort{
		{IP: "10.0.0.1", PrivatePort: 22, PublicPort: 32100, IsIPPublic: false, Type: "tcp"},
	}}}
	host, port = pod.SSHEndpoint()
	if host != "" || port != 0 {
		t.Fatalf("expected no public ssh endpoint, got %q:%d", host, port)
	}

	pod = runpodPod{Runtime: nil}
	host, port = pod.SSHEndpoint()
	if host != "" || port != 0 {
		t.Fatalf("nil runtime should yield empty endpoint, got %q:%d", host, port)
	}
}

func TestRunpodSSHTargetUsesPublicPortAndUser(t *testing.T) {
	cfg := Config{Runpod: RunpodConfig{User: "root", WorkRoot: "/tmp/crabbox"}}
	applyRunpodDefaults(&cfg)
	pod := runpodPod{Name: "crabbox-blue-12345678", ID: "pod_abc", Runtime: &runpodRuntime{Ports: []runpodRuntimePort{
		{IP: "203.0.113.7", PrivatePort: 22, PublicPort: 41010, IsIPPublic: true, Type: "tcp"},
	}}}
	target := runpodSSHTarget(cfg, pod)
	if target.Host != "203.0.113.7" || target.Port != "41010" {
		t.Fatalf("target=%#v", target)
	}
	if target.User != "root" {
		t.Fatalf("target user=%q, want root", target.User)
	}
	if target.TargetOS != targetLinux || target.NetworkKind != networkPublic {
		t.Fatalf("target=%#v", target)
	}
}

func TestRunpodLeaseIdentityHandlesNamedAndManualPods(t *testing.T) {
	leaseID, slug := runpodLeaseIdentity("crabbox-blue-12345678")
	if leaseID != "rpod_12345678" || slug != "blue" {
		t.Fatalf("leaseID=%q slug=%q", leaseID, slug)
	}
	leaseID, slug = runpodLeaseIdentity("manual-pod")
	if leaseID != "rpod_manual-pod" || slug != "manual-pod" {
		t.Fatalf("leaseID=%q slug=%q", leaseID, slug)
	}
	leaseID, slug = runpodLeaseIdentity("")
	if leaseID != "rpod_manual" || slug != "manual" {
		t.Fatalf("leaseID=%q slug=%q", leaseID, slug)
	}
}

func TestRunpodDoctorChecksAuthAndListPods(t *testing.T) {
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("auth = %q, want Bearer test-key", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content-type = %q", r.Header.Get("Content-Type"))
		}
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(body, &payload)
		queries = append(queries, payload.Query)
		if strings.Contains(payload.Query, "myself") && !strings.Contains(payload.Query, "pods") {
			_, _ = io.WriteString(w, `{"data":{"myself":{"id":"user_x","email":"a@b","clientBalance":0,"signedTermsOfService":true}}}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"myself":{"pods":[]}}}`)
	}))
	defer server.Close()

	cfg := Config{}
	cfg.Runpod.APIKey = "test-key"
	cfg.Runpod.APIURL = server.URL
	doctor, err := Provider{}.ConfigureDoctor(cfg, Runtime{Stdout: io.Discard, Stderr: io.Discard, HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	result, err := doctor.Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatalf("doctor returned err: %v", err)
	}
	if result.Provider != providerName {
		t.Fatalf("provider=%q", result.Provider)
	}
	if len(queries) != 2 {
		t.Fatalf("queries=%v, want 2 (whoami + list)", queries)
	}
	if !strings.Contains(queries[0], "myself") {
		t.Fatalf("first query = %q, want whoami", queries[0])
	}
}

func TestRunpodDoctorReportsMissingAPIKey(t *testing.T) {
	doctor, err := Provider{}.ConfigureDoctor(Config{}, Runtime{Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	_, err = doctor.Doctor(context.Background(), DoctorRequest{})
	if err == nil || !strings.Contains(err.Error(), "RUNPOD_API_KEY") {
		t.Fatalf("err=%v, want clear missing-key message", err)
	}
}

type fakeRunpodAPI struct {
	mu          sync.Mutex
	whoamiCalls int
	listCalls   int
	deployCalls []runpodDeployInput
	terminated  []string
	listPods    []runpodPod
	getPod      func(string) (runpodPod, error)
	deployPod   runpodPod
}

func (f *fakeRunpodAPI) Whoami(_ context.Context) (runpodMyself, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.whoamiCalls++
	return runpodMyself{ID: "user_x", Email: "a@b"}, nil
}
func (f *fakeRunpodAPI) DeployCpuPod(_ context.Context, input runpodDeployInput) (runpodPod, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deployCalls = append(f.deployCalls, input)
	return f.deployPod, nil
}
func (f *fakeRunpodAPI) GetPod(_ context.Context, podID string) (runpodPod, error) {
	if f.getPod != nil {
		return f.getPod(podID)
	}
	return f.deployPod, nil
}
func (f *fakeRunpodAPI) ListPods(_ context.Context) ([]runpodPod, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	return f.listPods, nil
}
func (f *fakeRunpodAPI) TerminatePod(_ context.Context, podID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminated = append(f.terminated, podID)
	return nil
}

func TestRunpodListFiltersCrabboxPodsByDefault(t *testing.T) {
	fake := &fakeRunpodAPI{listPods: []runpodPod{
		{ID: "pod_a", Name: "crabbox-blue-12345678", DesiredStatus: "RUNNING"},
		{ID: "pod_b", Name: "manual-pod", DesiredStatus: "RUNNING"},
	}}
	backend := &runpodLeaseBackend{cfg: Config{Runpod: RunpodConfig{APIKey: "k"}}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}, client: fake}
	views, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Name != "crabbox-blue-12345678" || views[0].Provider != providerName {
		t.Fatalf("views=%#v", views)
	}
	views, err = backend.List(context.Background(), ListRequest{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 2 {
		t.Fatalf("views=%#v", views)
	}
}

func TestRunpodWaitForPodSSHReturnsWhenPublicPortReady(t *testing.T) {
	calls := 0
	fake := &fakeRunpodAPI{getPod: func(id string) (runpodPod, error) {
		calls++
		if calls < 2 {
			return runpodPod{ID: id, Name: "crabbox-blue-12345678", DesiredStatus: "PROVISIONING"}, nil
		}
		return runpodPod{ID: id, Name: "crabbox-blue-12345678", DesiredStatus: "RUNNING", Runtime: &runpodRuntime{Ports: []runpodRuntimePort{
			{IP: "203.0.113.9", PrivatePort: 22, PublicPort: 41200, IsIPPublic: true, Type: "tcp"},
		}}}, nil
	}}
	backend := &runpodLeaseBackend{
		cfg:                 Config{Runpod: RunpodConfig{APIKey: "k"}},
		rt:                  Runtime{Stdout: io.Discard, Stderr: io.Discard},
		client:              fake,
		pollInitialOverride: 10 * time.Millisecond,
		pollTimeoutOverride: 2 * time.Second,
	}
	pod, err := backend.waitForPodSSH(context.Background(), fake, "pod_a")
	if err != nil {
		t.Fatal(err)
	}
	host, port := pod.SSHEndpoint()
	if host != "203.0.113.9" || port != 41200 {
		t.Fatalf("host=%q port=%d", host, port)
	}
}

func TestRunpodReleaseLeaseTerminatesPod(t *testing.T) {
	fake := &fakeRunpodAPI{}
	backend := &runpodLeaseBackend{cfg: Config{Runpod: RunpodConfig{APIKey: "k"}}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}, client: fake}
	req := ReleaseLeaseRequest{Lease: LeaseTarget{Server: Server{CloudID: "pod_z"}, LeaseID: "rpod_abcdef12"}}
	if err := backend.ReleaseLease(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(fake.terminated) != 1 || fake.terminated[0] != "pod_z" {
		t.Fatalf("terminated=%v", fake.terminated)
	}
}

func TestRunpodClientSendsBearerAndGraphQLBody(t *testing.T) {
	var (
		gotAuth        string
		gotContentType string
		gotMethod      string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		gotMethod = r.Method
		_, _ = io.WriteString(w, `{"data":{"myself":{"id":"user_x","email":"a@b","clientBalance":0,"signedTermsOfService":true}}}`)
	}))
	defer server.Close()
	cfg := Config{}
	cfg.Runpod.APIKey = "test-key"
	cfg.Runpod.APIURL = server.URL
	client, err := newRunpodClient(cfg, Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Whoami(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method=%q", gotMethod)
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("auth=%q", gotAuth)
	}
	if gotContentType != "application/json" {
		t.Fatalf("content-type=%q", gotContentType)
	}
}
