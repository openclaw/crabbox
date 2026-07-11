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
	workspaces              []orgoWorkspace
	computers               map[string]orgoComputer
	createdWorkspace        string
	deletedComputers        []string
	deletedWorkspaces       []string
	deleteComputerDeadline  bool
	deleteWorkspaceDeadline bool
	startedComputers        []string
	deleteComputerErr       error
	deleteWorkspaceErr      error
	getWorkspaceErr         error
	createComputerErr       error
	missingDeleteNotFound   bool
	bashCommands            []string
	bashExitCode            int
	bashStdout              string
	bashStderr              string
	omitWorkspaceID         bool
	computerStatuses        []string
	getComputerCalls        int
	bashStatuses            []string
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

func (f *fakeOrgoAPI) DeleteWorkspace(ctx context.Context, id string) error {
	_, f.deleteWorkspaceDeadline = ctx.Deadline()
	f.deletedWorkspaces = append(f.deletedWorkspaces, id)
	return f.deleteWorkspaceErr
}

func (f *fakeOrgoAPI) ListWorkspaces(context.Context) ([]orgoWorkspace, error) {
	return f.workspaces, nil
}

func (f *fakeOrgoAPI) GetWorkspace(_ context.Context, id string) (orgoWorkspace, error) {
	if f.getWorkspaceErr != nil {
		return orgoWorkspace{}, f.getWorkspaceErr
	}
	for _, workspace := range f.workspaces {
		if workspace.ID == id {
			return workspace, nil
		}
	}
	computers := []orgoComputer{}
	for _, computer := range f.computers {
		if computer.WorkspaceID == id {
			computers = append(computers, computer)
		}
	}
	return orgoWorkspace{ID: id, Status: "active", Computers: computers}, nil
}

func (f *fakeOrgoAPI) CreateComputer(_ context.Context, req orgoCreateComputerRequest) (orgoComputer, error) {
	if f.createComputerErr != nil {
		return orgoComputer{}, f.createComputerErr
	}
	status := "running"
	if len(f.computerStatuses) > 0 {
		status = f.computerStatuses[0]
		f.computerStatuses = f.computerStatuses[1:]
	}
	computer := orgoComputer{
		ID:            "computer_test",
		InstanceID:    "instance_test",
		Name:          req.Name,
		Status:        status,
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
	f.getComputerCalls++
	if len(f.computerStatuses) > 0 {
		computer.Status = f.computerStatuses[0]
		f.computerStatuses = f.computerStatuses[1:]
		f.computers[id] = computer
	}
	return computer, nil
}

func (f *fakeOrgoAPI) StartComputer(_ context.Context, id string) error {
	f.startedComputers = append(f.startedComputers, id)
	computer := f.computers[id]
	computer.Status = "running"
	f.computers[id] = computer
	return nil
}

func (f *fakeOrgoAPI) DeleteComputer(ctx context.Context, id string) error {
	_, f.deleteComputerDeadline = ctx.Deadline()
	f.deletedComputers = append(f.deletedComputers, id)
	if _, ok := f.computers[id]; !ok && f.missingDeleteNotFound {
		return exit(4, "missing computer %s", id)
	}
	delete(f.computers, id)
	return f.deleteComputerErr
}

func (f *fakeOrgoAPI) RunBash(_ context.Context, id string, command string, stdout, stderr io.Writer) (int, error) {
	f.bashStatuses = append(f.bashStatuses, f.computers[id].Status)
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

func TestProviderAliasesRejectUnsupportedMachineFlags(t *testing.T) {
	for _, provider := range []string{providerName, "orgo-ai", " ORGO-AI "} {
		t.Run(provider, func(t *testing.T) {
			fs := flag.NewFlagSet(provider, flag.ContinueOnError)
			cfg := Config{Provider: provider}
			values := RegisterOrgoProviderFlags(fs, cfg)
			fs.String("class", "", "")
			if err := fs.Parse([]string{"--class", "large"}); err != nil {
				t.Fatal(err)
			}
			if err := ApplyOrgoProviderFlags(&cfg, fs, values); err == nil || !strings.Contains(err.Error(), "--class is not supported") {
				t.Fatalf("err=%v", err)
			}
		})
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
		Env:        map[string]string{"EXAMPLE_TOKEN": "test value"},
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
	if !fake.deleteComputerDeadline || !fake.deleteWorkspaceDeadline {
		t.Fatalf("cleanup context missing deadline: computer=%t workspace=%t", fake.deleteComputerDeadline, fake.deleteWorkspaceDeadline)
	}
}

func TestRunWaitsForNewComputerBeforeBash(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeOrgoAPI()
	fake.computerStatuses = []string{"creating", "running"}
	backend := NewOrgoBackend(Provider{}.Spec(), Config{Orgo: OrgoConfig{APIKey: "test-key"}}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*orgoBackend)
	backend.client = fake

	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Root: t.TempDir()},
		NoSync:  true,
		Command: []string{"true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit=%d", result.ExitCode)
	}
	if fake.getComputerCalls == 0 {
		t.Fatal("new computer readiness was not polled")
	}
	if got := strings.Join(fake.bashStatuses, ","); got != "running" {
		t.Fatalf("bash states=%q, want running", got)
	}
}

func TestRunStartsReusedComputerBeforeBash(t *testing.T) {
	for _, tt := range []struct {
		name     string
		statuses []string
	}{
		{name: "suspended", statuses: []string{"suspended", "running"}},
		{name: "finishes stopping", statuses: []string{"stopping", "stopped", "running"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			fake := newFakeOrgoAPI()
			fake.computers["computer_test"] = orgoComputer{
				ID: "computer_test", Name: "orgo-reused", WorkspaceID: "ws_existing", Status: tt.statuses[0],
			}
			fake.computerStatuses = append([]string(nil), tt.statuses...)
			backend := NewOrgoBackend(Provider{}.Spec(), Config{Orgo: OrgoConfig{APIKey: "test-key"}}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*orgoBackend)
			backend.client = fake

			result, err := backend.Run(context.Background(), RunRequest{ID: "computer_test", NoSync: true, Command: []string{"true"}})
			if err != nil {
				t.Fatal(err)
			}
			if result.ExitCode != 0 {
				t.Fatalf("exit=%d", result.ExitCode)
			}
			if got := strings.Join(fake.startedComputers, ","); got != "computer_test" {
				t.Fatalf("started computers=%q", got)
			}
			if got := strings.Join(fake.bashStatuses, ","); got != "running" {
				t.Fatalf("bash states=%q, want running", got)
			}
		})
	}
}

func TestCreateComputerCleansUpTerminalStartupFailure(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeOrgoAPI()
	fake.computerStatuses = []string{"creating", "error"}
	backend := NewOrgoBackend(Provider{}.Spec(), Config{Orgo: OrgoConfig{APIKey: "test-key"}}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*orgoBackend)
	backend.client = fake

	_, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Root: t.TempDir()},
		NoSync:  true,
		Command: []string{"true"},
	})
	if err == nil || !strings.Contains(err.Error(), "entered error state while starting") {
		t.Fatalf("err=%v", err)
	}
	if len(fake.bashCommands) != 0 {
		t.Fatalf("bash ran before readiness: %#v", fake.bashCommands)
	}
	if got := strings.Join(fake.deletedComputers, ","); got != "computer_test" {
		t.Fatalf("deleted computers=%q", got)
	}
	if got := strings.Join(fake.deletedWorkspaces, ","); got != "ws_created" {
		t.Fatalf("deleted workspaces=%q", got)
	}
}

func TestCreateComputerReportsRollbackFailureWithResourceIdentity(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeOrgoAPI()
	fake.computerStatuses = []string{"creating", "failed"}
	fake.deleteComputerErr = errors.New("computer cleanup failed")
	fake.deleteWorkspaceErr = errors.New("workspace cleanup failed")
	backend := NewOrgoBackend(Provider{}.Spec(), Config{Orgo: OrgoConfig{APIKey: "test-key"}}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*orgoBackend)

	_, err := backend.createComputer(context.Background(), fake, Repo{Root: t.TempDir()}, "rollback-proof", false)
	if err == nil {
		t.Fatal("create unexpectedly succeeded")
	}
	for _, want := range []string{"entered failed state", "computer_test", "ws_created", "computer cleanup failed", "workspace cleanup failed"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err=%q missing %q", err, want)
		}
	}
}

func TestCreateComputerReportsWorkspaceRollbackFailureAfterCreateError(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeOrgoAPI()
	fake.createComputerErr = &orgoHTTPError{StatusCode: 400, Body: "computer create failed"}
	fake.deleteWorkspaceErr = errors.New("workspace cleanup failed")
	backend := NewOrgoBackend(Provider{}.Spec(), Config{Orgo: OrgoConfig{APIKey: "test-key"}}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*orgoBackend)

	_, err := backend.createComputer(context.Background(), fake, Repo{Root: t.TempDir()}, "rollback-proof", false)
	if err == nil {
		t.Fatal("create unexpectedly succeeded")
	}
	for _, want := range []string{"computer create failed", "ws_created", "workspace cleanup failed"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err=%q missing %q", err, want)
		}
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
	if lease.Computer.Name != "crabbox-"+lease.LeaseID {
		t.Fatalf("computer name=%q, want lease-unique identity", lease.Computer.Name)
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

func TestStopRefusesUnclaimedComputerID(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeOrgoAPI()
	fake.computers["computer_unclaimed"] = orgoComputer{ID: "computer_unclaimed", Status: "running"}
	backend := NewOrgoBackend(Provider{}.Spec(), Config{Orgo: OrgoConfig{APIKey: "test-key"}}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*orgoBackend)
	backend.client = fake

	err := backend.Stop(context.Background(), StopRequest{ID: "computer_unclaimed"})
	if err == nil || !strings.Contains(err.Error(), "refuses to stop unclaimed") {
		t.Fatalf("err=%v", err)
	}
	if len(fake.deletedComputers) != 0 {
		t.Fatalf("deleted unclaimed computer: %#v", fake.deletedComputers)
	}
}

func TestStopRetriesWorkspaceCleanupAfterComputerWasDeleted(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeOrgoAPI()
	fake.missingDeleteNotFound = true
	backend := NewOrgoBackend(Provider{}.Spec(), Config{Orgo: OrgoConfig{APIKey: "test-key"}}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*orgoBackend)
	backend.client = fake
	lease, err := backend.createComputer(context.Background(), fake, Repo{Root: t.TempDir()}, "partial-cleanup", false)
	if err != nil {
		t.Fatal(err)
	}
	fake.deleteWorkspaceErr = errors.New("transient workspace delete failure")
	if err := backend.Stop(context.Background(), StopRequest{ID: lease.LeaseID}); err == nil {
		t.Fatal("first stop unexpectedly succeeded")
	}
	fake.deleteWorkspaceErr = nil
	if err := backend.Stop(context.Background(), StopRequest{ID: lease.LeaseID}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fake.deletedComputers, ","); got != "computer_test" {
		t.Fatalf("deleted computers=%q", got)
	}
	if got := strings.Join(fake.deletedWorkspaces, ","); got != "ws_created,ws_created" {
		t.Fatalf("deleted workspaces=%q", got)
	}
	if _, ok, err := resolveLeaseClaimForProvider(lease.LeaseID); err != nil || ok {
		t.Fatalf("claim retained ok=%t err=%v", ok, err)
	}
}

func TestStopRetainsClaimWhenNotFoundCouldBeAuthorizationFailure(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeOrgoAPI()
	backend := NewOrgoBackend(Provider{}.Spec(), Config{Orgo: OrgoConfig{APIKey: "test-key"}}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*orgoBackend)
	backend.client = fake
	lease, err := backend.createComputer(context.Background(), fake, Repo{Root: t.TempDir()}, "ambiguous-not-found", false)
	if err != nil {
		t.Fatal(err)
	}
	delete(fake.computers, lease.Computer.ID)
	fake.getWorkspaceErr = exit(4, "workspace unavailable")
	if err := backend.Stop(context.Background(), StopRequest{ID: lease.LeaseID}); err == nil {
		t.Fatal("stop unexpectedly accepted ambiguous absence")
	}
	if len(fake.deletedComputers) != 0 || len(fake.deletedWorkspaces) != 0 {
		t.Fatalf("ambiguous absence triggered deletion: computers=%#v workspaces=%#v", fake.deletedComputers, fake.deletedWorkspaces)
	}
	if _, ok, err := resolveLeaseClaimForProvider(lease.LeaseID); err != nil || !ok {
		t.Fatalf("claim missing ok=%t err=%v", ok, err)
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

func TestStatusWaitStopsOnTerminalStates(t *testing.T) {
	for _, state := range []string{"error", "failed", "deleted"} {
		t.Run(state, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			fake := newFakeOrgoAPI()
			fake.computers["computer_test"] = orgoComputer{ID: "computer_test", Status: state}
			backend := NewOrgoBackend(Provider{}.Spec(), Config{Orgo: OrgoConfig{APIKey: "test-key"}}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*orgoBackend)
			backend.client = fake
			_, err := backend.Status(context.Background(), StatusRequest{ID: "computer_test", Wait: true})
			if err == nil || !strings.Contains(err.Error(), "entered "+state+" state") {
				t.Fatalf("err=%v", err)
			}
		})
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

func TestDeleteLeaseTreatsOwnedWorkspaceDeletionAsAuthoritative(t *testing.T) {
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
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got := strings.Join(fake.deletedComputers, ","); got != "computer_test" {
		t.Fatalf("deleted computers=%q", got)
	}
	if got := strings.Join(fake.deletedWorkspaces, ","); got != "ws_created" {
		t.Fatalf("deleted workspaces=%q", got)
	}
}
