package orgo

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"strings"
	"testing"
	"time"
)

type fakeOrgoAPI struct {
	workspaces         []orgoWorkspace
	computers          map[string]orgoComputer
	createdWorkspace   string
	deletedComputers   []string
	deletedWorkspaces  []string
	deleteComputerErr  error
	deleteWorkspaceErr error
	bashCommands       []string
	bashExitCode       int
	bashStdout         string
	bashStderr         string
	omitWorkspaceID    bool
}

func newFakeOrgoAPI() *fakeOrgoAPI {
	return &fakeOrgoAPI{
		computers:  map[string]orgoComputer{},
		bashStdout: "crabbox-orgo-ok\n",
	}
}

func (f *fakeOrgoAPI) CreateWorkspace(_ context.Context, name string) (orgoWorkspace, error) {
	f.createdWorkspace = name
	return orgoWorkspace{ID: "ws_created", Name: name, Status: "active"}, nil
}

func (f *fakeOrgoAPI) DeleteWorkspace(_ context.Context, id string) error {
	f.deletedWorkspaces = append(f.deletedWorkspaces, id)
	return f.deleteWorkspaceErr
}

func (f *fakeOrgoAPI) ListWorkspaces(context.Context) ([]orgoWorkspace, error) {
	return f.workspaces, nil
}

func (f *fakeOrgoAPI) GetWorkspace(_ context.Context, id string) (orgoWorkspace, error) {
	for _, workspace := range f.workspaces {
		if workspace.ID == id {
			return workspace, nil
		}
	}
	var computers []orgoComputer
	for _, computer := range f.computers {
		if computer.WorkspaceID == id {
			computers = append(computers, computer)
		}
	}
	return orgoWorkspace{ID: id, Status: "active", Computers: computers}, nil
}

func (f *fakeOrgoAPI) CreateComputer(_ context.Context, req orgoCreateComputerRequest) (orgoComputer, error) {
	computer := orgoComputer{
		ID:            "computer_test",
		InstanceID:    "instance_test",
		Name:          req.Name,
		Status:        "running",
		OS:            req.OS,
		RAMGB:         req.RAMGB,
		CPUs:          req.CPUs,
		DiskGB:        req.DiskGB,
		Resolution:    req.Resolution,
		ConnectionURL: "https://www.orgo.ai/desktops/instance_test",
	}
	if !f.omitWorkspaceID {
		computer.WorkspaceID = req.WorkspaceID
	}
	f.computers[computer.ID] = computer
	return computer, nil
}

func (f *fakeOrgoAPI) GetComputer(_ context.Context, id string) (orgoComputer, error) {
	computer, ok := f.computers[id]
	if !ok {
		return orgoComputer{}, exit(4, "missing computer %s", id)
	}
	return computer, nil
}

func (f *fakeOrgoAPI) DeleteComputer(_ context.Context, id string) error {
	f.deletedComputers = append(f.deletedComputers, id)
	delete(f.computers, id)
	return f.deleteComputerErr
}

func (f *fakeOrgoAPI) RunBash(_ context.Context, _ string, command string, stdout, stderr io.Writer) (int, error) {
	f.bashCommands = append(f.bashCommands, command)
	if f.bashStdout != "" {
		_, _ = io.WriteString(stdout, f.bashStdout)
	}
	if f.bashStderr != "" {
		_, _ = io.WriteString(stderr, f.bashStderr)
	}
	return f.bashExitCode, nil
}

func TestProviderRegistersSecretSafeFlags(t *testing.T) {
	fs := flag.NewFlagSet("orgo", flag.ContinueOnError)
	cfg := Config{}
	values := RegisterOrgoProviderFlags(fs, cfg)
	if fs.Lookup("orgo-api-key") != nil {
		t.Fatalf("Orgo API key must not be registered as a CLI flag")
	}
	if err := fs.Parse([]string{
		"--orgo-api-base", "https://orgo.test/api",
		"--orgo-workspace-id", "ws_test",
		"--orgo-ram", "8",
		"--orgo-cpu", "2",
		"--orgo-disk", "32",
		"--orgo-resolution", "1440x900x24",
	}); err != nil {
		t.Fatal(err)
	}
	cfg.Provider = providerName
	if err := ApplyOrgoProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Orgo.APIBase != "https://orgo.test/api" || cfg.Orgo.WorkspaceID != "ws_test" || cfg.Orgo.RAMGB != 8 || cfg.Orgo.CPUs != 2 || cfg.Orgo.DiskGB != 32 || cfg.Orgo.Resolution != "1440x900x24" {
		t.Fatalf("applied cfg=%#v", cfg.Orgo)
	}
}

func TestRunCreatesExecutesAndDeletesTemporaryWorkspace(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeOrgoAPI()
	var stdout, stderr bytes.Buffer
	backend := NewOrgoBackend(Provider{}.Spec(), Config{Orgo: OrgoConfig{APIKey: "test-key"}}, Runtime{Stdout: &stdout, Stderr: &stderr}).(*orgoBackend)
	backend.client = fake

	result, err := backend.Run(context.Background(), RunRequest{
		Repo:       Repo{Root: t.TempDir()},
		NoSync:     true,
		Command:    []string{"printf", "crabbox-orgo-ok"},
		Env:        map[string]string{"EXAMPLE_TOKEN": "secret value"},
		EnvSummary: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || !result.SyncDelegated {
		t.Fatalf("result=%#v", result)
	}
	if strings.Contains(result.CommandText, "EXAMPLE_TOKEN") || strings.Contains(result.CommandText, "secret value") {
		t.Fatalf("proof command leaked forwarded env: %q", result.CommandText)
	}
	if got := stdout.String(); !strings.Contains(got, "crabbox-orgo-ok") {
		t.Fatalf("stdout=%q", got)
	}
	if !strings.Contains(stderr.String(), "env forwarding provider=orgo behavior=forwarded") {
		t.Fatalf("stderr missing env summary: %q", stderr.String())
	}
	if fake.createdWorkspace == "" || !strings.HasPrefix(fake.createdWorkspace, "crabbox-cbx_") {
		t.Fatalf("workspace not created with lease name: %q", fake.createdWorkspace)
	}
	if len(fake.bashCommands) != 1 || !strings.Contains(fake.bashCommands[0], "export EXAMPLE_TOKEN=") || !strings.Contains(fake.bashCommands[0], "crabbox-orgo-ok") {
		t.Fatalf("bash commands=%#v", fake.bashCommands)
	}
	if got := strings.Join(fake.deletedComputers, ","); got != "computer_test" {
		t.Fatalf("deleted computers=%q", got)
	}
	if got := strings.Join(fake.deletedWorkspaces, ","); got != "ws_created" {
		t.Fatalf("deleted workspaces=%q", got)
	}
}

func TestCreateComputerPreservesRequestedWorkspaceWhenResponseOmitsIt(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeOrgoAPI()
	fake.omitWorkspaceID = true
	backend := NewOrgoBackend(Provider{}.Spec(), Config{Orgo: OrgoConfig{APIKey: "test-key"}}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*orgoBackend)
	lease, err := backend.createComputer(context.Background(), fake, Repo{Root: t.TempDir()}, "workspace-proof", false)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Computer.WorkspaceID != "ws_created" {
		t.Fatalf("workspace id=%q", lease.Computer.WorkspaceID)
	}
	if err := backend.deleteLease(context.Background(), fake, lease); err != nil {
		t.Fatal(err)
	}
}

func TestStopByComputerIDDeletesTemporaryWorkspaceAndClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeOrgoAPI()
	backend := NewOrgoBackend(Provider{}.Spec(), Config{Orgo: OrgoConfig{APIKey: "test-key"}}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*orgoBackend)
	backend.client = fake
	lease, err := backend.createComputer(context.Background(), fake, Repo{Root: t.TempDir()}, "cloud-id-stop", false)
	if err != nil {
		t.Fatal(err)
	}
	otherLeaseID := "cbx_slug_collision_1234567890"
	if err := claimLeaseForRepoProviderEndpoint(otherLeaseID, lease.Computer.ID, t.TempDir(), time.Minute, false, Server{
		CloudID:  "other-computer",
		Provider: providerName,
		Name:     "other-computer",
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeLeaseClaim(otherLeaseID) })
	if err := backend.Stop(context.Background(), StopRequest{ID: lease.Computer.ID}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fake.deletedComputers, ","); got != lease.Computer.ID {
		t.Fatalf("deleted computers=%q", got)
	}
	if got := strings.Join(fake.deletedWorkspaces, ","); got != "ws_created" {
		t.Fatalf("deleted workspaces=%q", got)
	}
	if _, ok, err := resolveLeaseClaimForProvider(lease.LeaseID); err != nil || ok {
		t.Fatalf("claim retained ok=%t err=%v", ok, err)
	}
}

func TestWarmupClaimsSlugForStatusAndStop(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeOrgoAPI()
	var stdout, stderr bytes.Buffer
	cfg := Config{Orgo: OrgoConfig{APIKey: "test-key", WorkspaceID: "ws_existing"}}
	backend := NewOrgoBackend(Provider{}.Spec(), cfg, Runtime{Stdout: &stdout, Stderr: &stderr}).(*orgoBackend)
	backend.client = fake

	if err := backend.Warmup(context.Background(), WarmupRequest{
		Repo:          Repo{Root: t.TempDir()},
		RequestedSlug: "orgo-smoke",
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "slug=orgo-smoke") {
		t.Fatalf("stdout=%q", stdout.String())
	}
	view, err := backend.Status(context.Background(), StatusRequest{ID: "orgo-smoke", Wait: true})
	if err != nil {
		t.Fatal(err)
	}
	if view.ServerID != "computer_test" || view.Slug != "orgo-smoke" || !view.Ready {
		t.Fatalf("status=%#v", view)
	}
	if err := backend.Stop(context.Background(), StopRequest{ID: "orgo-smoke"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fake.deletedComputers, ","); got != "computer_test" {
		t.Fatalf("deleted computers=%q", got)
	}
	if len(fake.deletedWorkspaces) != 0 {
		t.Fatalf("explicit workspace should not be deleted: %#v", fake.deletedWorkspaces)
	}
}

func TestListMergesLocalClaimLabels(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeOrgoAPI()
	var stdout, stderr bytes.Buffer
	cfg := Config{Orgo: OrgoConfig{APIKey: "test-key", WorkspaceID: "ws_existing"}}
	backend := NewOrgoBackend(Provider{}.Spec(), cfg, Runtime{Stdout: &stdout, Stderr: &stderr}).(*orgoBackend)
	backend.client = fake

	if err := backend.Warmup(context.Background(), WarmupRequest{
		Repo:          Repo{Root: t.TempDir()},
		RequestedSlug: "orgo-list",
	}); err != nil {
		t.Fatal(err)
	}

	claims, err := listLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 {
		t.Fatalf("claims=%#v", claims)
	}
	if got := claims[0].Labels["lease"]; got != claims[0].LeaseID {
		t.Fatalf("claim lease label=%q, want %q", got, claims[0].LeaseID)
	}
	if got := claims[0].Labels["slug"]; got != "orgo-list" {
		t.Fatalf("claim slug label=%q", got)
	}

	views, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 {
		t.Fatalf("views=%#v", views)
	}
	view := views[0]
	if view.CloudID != "computer_test" {
		t.Fatalf("cloud id=%q", view.CloudID)
	}
	if got := view.Labels["lease"]; got != claims[0].LeaseID {
		t.Fatalf("view lease label=%q, want %q", got, claims[0].LeaseID)
	}
	if got := view.Labels["slug"]; got != "orgo-list" {
		t.Fatalf("view slug label=%q", got)
	}
	if got := view.Labels[orgoWorkspaceLabel]; got != "ws_existing" {
		t.Fatalf("workspace label=%q", got)
	}
}

func TestDoctorCountsInventoryComputers(t *testing.T) {
	fake := newFakeOrgoAPI()
	fake.computers["computer_one"] = orgoComputer{ID: "computer_one", WorkspaceID: "ws_existing", Status: "running"}
	fake.computers["computer_two"] = orgoComputer{ID: "computer_two", WorkspaceID: "ws_existing", Status: "stopped"}
	backend := NewOrgoBackend(Provider{}.Spec(), Config{Orgo: OrgoConfig{APIKey: "test-key", WorkspaceID: "ws_existing"}}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*orgoBackend)
	backend.client = fake

	result, err := backend.Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Provider != providerName {
		t.Fatalf("provider=%q", result.Provider)
	}
	if !strings.Contains(result.Message, "inventory=ready") || !strings.Contains(result.Message, "leases=2") {
		t.Fatalf("doctor message=%q", result.Message)
	}
}

func TestBuildCommandQuotesForwardedEnvValues(t *testing.T) {
	backend := NewOrgoBackend(Provider{}.Spec(), Config{Orgo: OrgoConfig{APIKey: "test-key"}}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*orgoBackend)

	command, err := backend.buildCommand(RunRequest{
		Command: []string{"printf", "ok"},
		Env: map[string]string{
			"PIPE": "|",
			"SEMI": ";",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(command, "export PIPE='|'\n") {
		t.Fatalf("PIPE export was not quoted: %q", command)
	}
	if !strings.Contains(command, "export SEMI=';'\n") {
		t.Fatalf("SEMI export was not quoted: %q", command)
	}
	if strings.Contains(command, "export PIPE=|\n") || strings.Contains(command, "export SEMI=;\n") {
		t.Fatalf("control operator leaked unquoted: %q", command)
	}
}

func TestRunKeepOnFailurePreservesComputer(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeOrgoAPI()
	fake.bashExitCode = 7
	backend := NewOrgoBackend(Provider{}.Spec(), Config{Orgo: OrgoConfig{APIKey: "test-key", WorkspaceID: "ws_existing"}}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*orgoBackend)
	backend.client = fake

	result, err := backend.Run(context.Background(), RunRequest{
		Repo:          Repo{Root: t.TempDir()},
		NoSync:        true,
		KeepOnFailure: true,
		Command:       []string{"false"},
	})
	if err == nil {
		t.Fatalf("expected failing command")
	}
	if result.ExitCode != 7 {
		t.Fatalf("exit=%d", result.ExitCode)
	}
	if len(fake.deletedComputers) != 0 {
		t.Fatalf("keep-on-failure deleted computer: %#v", fake.deletedComputers)
	}
}

func TestDeleteLeaseAttemptsWorkspaceCleanupAfterComputerDeleteFailure(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	computerErr := errors.New("computer delete failed")
	fake := newFakeOrgoAPI()
	fake.deleteComputerErr = computerErr
	backend := NewOrgoBackend(Provider{}.Spec(), Config{Orgo: OrgoConfig{APIKey: "test-key"}}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*orgoBackend)

	err := backend.deleteLease(context.Background(), fake, orgoLease{
		LeaseID:          "lease_test",
		Computer:         orgoComputer{ID: "computer_test"},
		CreatedWorkspace: "ws_created",
	})
	if !errors.Is(err, computerErr) {
		t.Fatalf("err=%v, want computer delete error", err)
	}
	if got := strings.Join(fake.deletedComputers, ","); got != "computer_test" {
		t.Fatalf("deleted computers=%q", got)
	}
	if got := strings.Join(fake.deletedWorkspaces, ","); got != "ws_created" {
		t.Fatalf("deleted workspaces=%q", got)
	}
}
