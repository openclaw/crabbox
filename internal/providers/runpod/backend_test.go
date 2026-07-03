package runpod

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
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

func TestRunpodClientRedactsReflectedCredential(t *testing.T) {
	const secret = "runpod-secret-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"Bearer `+secret+` quota exceeded"}`)
	}))
	defer server.Close()

	client, err := newRunpodClient(Config{Runpod: RunpodConfig{APIKey: secret, APIURL: server.URL}}, Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Whoami(context.Background())
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[redacted]") || !strings.Contains(err.Error(), "quota exceeded") {
		t.Fatalf("Whoami error=%v, want redacted useful provider error", err)
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
	cfg.Runpod.APIURL = "https://rest.runpod.io/v1"
	if _, err := newRunpodClient(cfg, Runtime{}); err == nil {
		t.Fatal("newRunpodClient accepted empty API key")
	}
}

func TestRunpodClientRejectsBareHTTPURL(t *testing.T) {
	cfg := Config{}
	cfg.Runpod.APIKey = "test-key"
	cfg.Runpod.APIURL = "http://rest.runpod.io/v1"
	if _, err := newRunpodClient(cfg, Runtime{}); err == nil {
		t.Fatal("newRunpodClient accepted plaintext http URL")
	}
}

func TestRunpodClientAllowsLoopbackHTTPURL(t *testing.T) {
	cfg := Config{}
	cfg.Runpod.APIKey = "test-key"
	cfg.Runpod.APIURL = "http://127.0.0.1:8080/v1"
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

func TestRunpodDefaultsPickPublicSSHGPUAndPreserveCustomWorkRoot(t *testing.T) {
	cfg := Config{}
	applyRunpodDefaults(&cfg)
	if cfg.Runpod.APIURL != "https://rest.runpod.io/v1" {
		t.Fatalf("default apiUrl = %q, want REST API", cfg.Runpod.APIURL)
	}
	if cfg.Runpod.InstanceID != "NVIDIA L4,NVIDIA RTX 4000 Ada Generation,NVIDIA RTX A4000,NVIDIA GeForce RTX 3090,NVIDIA GeForce RTX 4090,NVIDIA RTX A5000,NVIDIA RTX A4500" {
		t.Fatalf("default instanceId = %q, want GPU priority list", cfg.Runpod.InstanceID)
	}
	if cfg.Runpod.CloudType != "SECURE" {
		t.Fatalf("default cloudType = %q, want SECURE", cfg.Runpod.CloudType)
	}
	if cfg.Runpod.DiskGB != 20 {
		t.Fatalf("default diskGB = %d, want 20", cfg.Runpod.DiskGB)
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
	endpoint := pod.SSHEndpoint()
	if endpoint.Host != "203.0.113.5" || endpoint.Port != 32100 || endpoint.Kind != "public-tcp" || !endpoint.Public {
		t.Fatalf("endpoint=%#v, want public 203.0.113.5:32100", endpoint)
	}

	pod = runpodPod{Runtime: &runpodRuntime{Ports: []runpodRuntimePort{
		{IP: "10.0.0.1", PrivatePort: 22, PublicPort: 32100, IsIPPublic: false, Type: "tcp"},
	}}, Machine: runpodMachine{PodHostID: "pod-host-id"}}
	endpoint = pod.SSHEndpoint()
	if endpoint.Host != "" || endpoint.Port != 0 {
		t.Fatalf("private mapping should not yield SSH endpoint, got %#v", endpoint)
	}

	pod = runpodPod{Runtime: nil}
	endpoint = pod.SSHEndpoint()
	if endpoint.Host != "" || endpoint.Port != 0 {
		t.Fatalf("nil runtime without host id should yield empty endpoint, got %#v", endpoint)
	}
}

func TestRunpodDeployPayloadUsesGPUAvailabilityPriority(t *testing.T) {
	payload := runpodDeployPayload(runpodDeployInput{
		Name:              "crabbox-blue-12345678",
		ImageName:         "runpod/pytorch:custom",
		InstanceID:        "NVIDIA L4, NVIDIA RTX A4000",
		CloudType:         "SECURE",
		ContainerDiskInGb: 20,
		Ports:             "22/tcp",
	})
	if payload["computeType"] != "GPU" || payload["gpuTypePriority"] != "availability" {
		t.Fatalf("payload=%#v", payload)
	}
	gpuIDs, ok := payload["gpuTypeIds"].([]string)
	if !ok || len(gpuIDs) != 2 || gpuIDs[0] != "NVIDIA L4" || gpuIDs[1] != "NVIDIA RTX A4000" {
		t.Fatalf("gpuTypeIds=%#v", payload["gpuTypeIds"])
	}
	ports, ok := payload["ports"].([]string)
	if !ok || len(ports) != 1 || ports[0] != "22/tcp" {
		t.Fatalf("ports=%#v", payload["ports"])
	}
	if payload["supportPublicIp"] != true {
		t.Fatalf("supportPublicIp=%#v", payload["supportPublicIp"])
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

func TestRunpodDefaultsUseRootInsteadOfLocalUser(t *testing.T) {
	t.Setenv("USER", "alice")
	cfg := Config{}
	applyRunpodDefaults(&cfg)
	if cfg.SSHUser != "root" {
		t.Fatalf("ssh user=%q, want root", cfg.SSHUser)
	}

	cfg = Config{Runpod: RunpodConfig{User: "ubuntu"}}
	applyRunpodDefaults(&cfg)
	if cfg.SSHUser != "ubuntu" {
		t.Fatalf("explicit runpod user=%q, want ubuntu", cfg.SSHUser)
	}

	cfg = Config{SSHUser: "custom"}
	applyRunpodDefaults(&cfg)
	if cfg.SSHUser != "custom" {
		t.Fatalf("explicit generic user=%q, want custom", cfg.SSHUser)
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
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("auth = %q, want Bearer test-key", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Content-Type") != "" {
			t.Errorf("content-type = %q", r.Header.Get("Content-Type"))
		}
		paths = append(paths, r.URL.Path)
		_, _ = io.WriteString(w, `[]`)
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
	if len(paths) != 2 || paths[0] != "/pods" || paths[1] != "/pods" {
		t.Fatalf("paths=%v, want two /pods reads", paths)
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
	mu           sync.Mutex
	whoamiCalls  int
	listCalls    int
	deployCalls  []runpodDeployInput
	terminated   []string
	listPods     []runpodPod
	getPod       func(string) (runpodPod, error)
	terminatePod func(context.Context, string) error
	deployPod    runpodPod
}

func (f *fakeRunpodAPI) Whoami(_ context.Context) (runpodMyself, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.whoamiCalls++
	return runpodMyself{ID: "user_x", Email: "a@b"}, nil
}
func (f *fakeRunpodAPI) DeployPod(_ context.Context, input runpodDeployInput) (runpodPod, error) {
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
func (f *fakeRunpodAPI) TerminatePod(ctx context.Context, podID string) error {
	f.mu.Lock()
	f.terminated = append(f.terminated, podID)
	terminatePod := f.terminatePod
	f.mu.Unlock()
	if terminatePod != nil {
		return terminatePod(ctx, podID)
	}
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
	endpoint := pod.SSHEndpoint()
	if endpoint.Host != "203.0.113.9" || endpoint.Port != 41200 {
		t.Fatalf("endpoint=%#v", endpoint)
	}
}

func TestRunpodWaitForSSHRejectsProxyOnlyPods(t *testing.T) {
	fake := &fakeRunpodAPI{getPod: func(id string) (runpodPod, error) {
		return runpodPod{ID: id, Name: "crabbox-blue-12345678", DesiredStatus: "RUNNING", Machine: runpodMachine{PodHostID: "pod_abc-1234"}}, nil
	}}
	backend := &runpodLeaseBackend{
		cfg:                 Config{Runpod: RunpodConfig{APIKey: "k"}},
		rt:                  Runtime{Stdout: io.Discard, Stderr: io.Discard},
		client:              fake,
		pollInitialOverride: 10 * time.Millisecond,
		pollTimeoutOverride: 2 * time.Second,
	}
	_, err := backend.waitForPodSSH(context.Background(), fake, "pod_a")
	if err == nil || !strings.Contains(err.Error(), "ssh endpoint not exposed") {
		t.Fatalf("err=%v, want timeout without public TCP endpoint", err)
	}
}

func TestRunpodAcquireRollbackUsesBoundedCleanup(t *testing.T) {
	terminateErr := errors.New("terminate failed")
	fake := &fakeRunpodAPI{
		deployPod: runpodPod{ID: "pod_failed", Name: "crabbox-blue-12345678", DesiredStatus: "PROVISIONING"},
		getPod: func(id string) (runpodPod, error) {
			return runpodPod{ID: id, Name: "crabbox-blue-12345678", DesiredStatus: "PROVISIONING"}, nil
		},
		terminatePod: func(ctx context.Context, podID string) error {
			if podID != "pod_failed" {
				t.Fatalf("podID=%q, want pod_failed", podID)
			}
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("cleanup context should have a deadline")
			}
			return terminateErr
		},
	}
	backend := &runpodLeaseBackend{
		cfg:                    Config{Runpod: RunpodConfig{APIKey: "k"}},
		rt:                     Runtime{Stdout: io.Discard, Stderr: io.Discard},
		client:                 fake,
		pollInitialOverride:    time.Millisecond,
		pollTimeoutOverride:    5 * time.Millisecond,
		cleanupTimeoutOverride: 20 * time.Millisecond,
	}

	_, err := backend.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: t.TempDir()}})
	if err == nil || !strings.Contains(err.Error(), "ssh endpoint not exposed") || !strings.Contains(err.Error(), "cleanup failed") || !errors.Is(err, terminateErr) {
		t.Fatalf("err=%v, want original wait failure plus cleanup failure", err)
	}
	if len(fake.terminated) != 1 || fake.terminated[0] != "pod_failed" {
		t.Fatalf("terminated=%v", fake.terminated)
	}
}

func TestRunpodAcquireRollbackCannotBlockForever(t *testing.T) {
	fake := &fakeRunpodAPI{
		deployPod: runpodPod{ID: "pod_blocked", Name: "crabbox-blue-12345678", DesiredStatus: "PROVISIONING"},
		getPod: func(id string) (runpodPod, error) {
			return runpodPod{ID: id, Name: "crabbox-blue-12345678", DesiredStatus: "PROVISIONING"}, nil
		},
		terminatePod: func(ctx context.Context, _ string) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}
	backend := &runpodLeaseBackend{
		cfg:                    Config{Runpod: RunpodConfig{APIKey: "k"}},
		rt:                     Runtime{Stdout: io.Discard, Stderr: io.Discard},
		client:                 fake,
		pollInitialOverride:    time.Millisecond,
		pollTimeoutOverride:    5 * time.Millisecond,
		cleanupTimeoutOverride: 20 * time.Millisecond,
	}
	start := time.Now()

	_, err := backend.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: t.TempDir()}})
	if err == nil || !strings.Contains(err.Error(), "cleanup failed") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want bounded cleanup deadline error", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Acquire took %s, cleanup should be bounded", elapsed)
	}
	if len(fake.terminated) != 1 || fake.terminated[0] != "pod_blocked" {
		t.Fatalf("terminated=%v", fake.terminated)
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

func TestRunpodClientSendsBearerAndRESTRequest(t *testing.T) {
	var (
		gotAuth        string
		gotContentType string
		gotMethod      string
		gotPath        string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		gotMethod = r.Method
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, `[]`)
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
	if gotMethod != http.MethodGet {
		t.Fatalf("method=%q", gotMethod)
	}
	if gotPath != "/pods" {
		t.Fatalf("path=%q", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("auth=%q", gotAuth)
	}
	if gotContentType != "" {
		t.Fatalf("content-type=%q", gotContentType)
	}
}

func TestRunpodClientRefusesCrossOriginRedirectBeforeReplay(t *testing.T) {
	var targetRequests int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetRequests++
		t.Errorf("redirect target received %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer target.Close()

	trusted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/stolen", http.StatusTemporaryRedirect)
	}))
	defer trusted.Close()
	cfg := Config{Runpod: RunpodConfig{APIKey: "test-key", APIURL: trusted.URL}}
	client, err := newRunpodClient(cfg, Runtime{HTTP: trusted.Client()})
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.DeployPod(context.Background(), runpodDeployInput{
		Name: "test-pod", ImageName: "example/image", InstanceID: "NVIDIA L4", Ports: "22/tcp",
	})
	if err == nil || !strings.Contains(err.Error(), "refused cross-origin redirect") {
		t.Fatalf("DeployPod error = %v, want cross-origin refusal", err)
	}
	if targetRequests != 0 {
		t.Fatalf("redirect target received %d requests, want 0", targetRequests)
	}
}

func TestRunpodClientFollowsSameOriginRedirect(t *testing.T) {
	var redirectedAuth string
	var redirectedBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/pods":
			http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
		case "/redirected":
			redirectedAuth = r.Header.Get("Authorization")
			if err := json.NewDecoder(r.Body).Decode(&redirectedBody); err != nil {
				t.Errorf("decode redirected body: %v", err)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"pod_ok","status":"RUNNING"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	cfg := Config{Runpod: RunpodConfig{APIKey: "test-key", APIURL: server.URL}}
	client, err := newRunpodClient(cfg, Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}

	pod, err := client.DeployPod(context.Background(), runpodDeployInput{
		Name: "test-pod", ImageName: "example/image", InstanceID: "NVIDIA L4", Ports: "22/tcp",
	})
	if err != nil {
		t.Fatal(err)
	}
	if pod.ID != "pod_ok" || redirectedAuth != "Bearer test-key" || redirectedBody["name"] != "test-pod" {
		t.Fatalf("pod=%#v auth=%q body=%#v", pod, redirectedAuth, redirectedBody)
	}
}

func TestRunpodClientPreservesCallerRedirectPolicy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/redirected", http.StatusFound)
	}))
	defer server.Close()
	callerErr := errors.New("caller refused redirect")
	callerChecks := 0
	httpClient := server.Client()
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		callerChecks++
		return callerErr
	}
	client, err := newRunpodClient(
		Config{Runpod: RunpodConfig{APIKey: "test-key", APIURL: server.URL}},
		Runtime{HTTP: httpClient},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.Whoami(context.Background())
	if !errors.Is(err, callerErr) || callerChecks != 1 {
		t.Fatalf("Whoami error = %v, caller checks = %d", err, callerChecks)
	}
}

func TestRunpodClientRetriesGPUCapacityFallbacks(t *testing.T) {
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/pods" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var payload struct {
			GPUTypeIDs []string `json:"gpuTypeIds"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.GPUTypeIDs) != 1 {
			t.Fatalf("gpuTypeIds=%#v, want one id per fallback attempt", payload.GPUTypeIDs)
		}
		seen = append(seen, payload.GPUTypeIDs[0])
		if payload.GPUTypeIDs[0] == "NVIDIA L4" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"create pod: There are no instances currently available","status":500}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"pod_ok","name":"crabbox-blue-12345678","status":"RUNNING"}`)
	}))
	defer server.Close()

	cfg := Config{}
	cfg.Runpod.APIKey = "test-key"
	cfg.Runpod.APIURL = server.URL
	client, err := newRunpodClient(cfg, Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	pod, err := client.DeployPod(context.Background(), runpodDeployInput{
		Name:              "crabbox-blue-12345678",
		ImageName:         "runpod/pytorch:custom",
		InstanceID:        "NVIDIA L4,NVIDIA RTX 4000 Ada Generation",
		CloudType:         "SECURE",
		ContainerDiskInGb: 20,
		Ports:             "22/tcp",
	})
	if err != nil {
		t.Fatal(err)
	}
	if pod.ID != "pod_ok" {
		t.Fatalf("pod=%#v", pod)
	}
	if len(seen) != 2 || seen[0] != "NVIDIA L4" || seen[1] != "NVIDIA RTX 4000 Ada Generation" {
		t.Fatalf("seen=%v", seen)
	}
}
