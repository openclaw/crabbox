package nomad

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	nomadapi "github.com/hashicorp/nomad/api"
	core "github.com/openclaw/crabbox/internal/cli"
)

type lifecycleFakeClient struct {
	jobs         map[string]*nomadapi.Job
	evals        map[string]*nomadapi.Evaluation
	allocs       map[string][]*nomadapi.AllocationListStub
	registers    int
	deregisters  []string
	deregistered map[string]bool
	jobInfoErr   map[string]error
}

func newLifecycleFakeClient() *lifecycleFakeClient {
	return &lifecycleFakeClient{
		jobs:         map[string]*nomadapi.Job{},
		evals:        map[string]*nomadapi.Evaluation{},
		allocs:       map[string][]*nomadapi.AllocationListStub{},
		deregistered: map[string]bool{},
		jobInfoErr:   map[string]error{},
	}
}

func (f *lifecycleFakeClient) AgentSelf(context.Context) (*nomadapi.AgentSelf, error) {
	return &nomadapi.AgentSelf{}, nil
}

func (f *lifecycleFakeClient) Regions(context.Context) ([]string, error) {
	return []string{"global"}, nil
}

func (f *lifecycleFakeClient) NamespaceInfo(context.Context, string) (*nomadapi.Namespace, error) {
	return &nomadapi.Namespace{Name: "team-a"}, nil
}

func (f *lifecycleFakeClient) RegisterJob(_ context.Context, job *nomadapi.Job) (string, error) {
	f.registers++
	jobID := stringValue(job.ID)
	f.jobs[jobID] = cloneJob(job)
	evalID := "eval-" + jobID
	f.evals[evalID] = &nomadapi.Evaluation{ID: evalID, JobID: jobID, Status: nomadapi.EvalStatusComplete}
	f.allocs[jobID] = []*nomadapi.AllocationListStub{runningAlloc(jobID, "alloc-"+jobID, "node-1", "worker-1", "crabbox")}
	return evalID, nil
}

func (f *lifecycleFakeClient) JobInfo(_ context.Context, jobID string) (*nomadapi.Job, error) {
	if err := f.jobInfoErr[jobID]; err != nil {
		return nil, err
	}
	job := f.jobs[jobID]
	if job == nil || f.deregistered[jobID] {
		return nil, errors.New("Unexpected response code: 404")
	}
	return cloneJob(job), nil
}

func (f *lifecycleFakeClient) JobAllocations(_ context.Context, jobID string, _ bool) ([]*nomadapi.AllocationListStub, error) {
	return f.allocs[jobID], nil
}

func (f *lifecycleFakeClient) EvaluationInfo(_ context.Context, evalID string) (*nomadapi.Evaluation, error) {
	if eval := f.evals[evalID]; eval != nil {
		return eval, nil
	}
	return &nomadapi.Evaluation{ID: evalID, Status: nomadapi.EvalStatusComplete}, nil
}

func (f *lifecycleFakeClient) DeregisterJob(_ context.Context, jobID string, purge bool) (string, error) {
	if !purge {
		return "", errors.New("expected purge deregister")
	}
	f.deregisters = append(f.deregisters, jobID)
	f.deregistered[jobID] = true
	return "eval-deregister-" + jobID, nil
}

type fakeClock struct{ now time.Time }

func (c fakeClock) Now() time.Time { return c.now }

func testNomadConfig() core.Config {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.Pond = "pool-a"
	cfg.TTL = time.Hour
	cfg.IdleTimeout = 30 * time.Minute
	cfg.Nomad.Address = "https://nomad.example.test:4646"
	cfg.Nomad.Region = "global"
	cfg.Nomad.Namespace = "team-a"
	cfg.Nomad.Task = "crabbox"
	cfg.Nomad.Workdir = "/workspace/crabbox"
	cfg.Nomad.AllocReadyTimeout = time.Second
	cfg.Nomad.EvalTimeout = time.Second
	return cfg
}

func testBackend(t *testing.T, fake *lifecycleFakeClient) (*backend, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	cfg := testNomadConfig()
	return &backend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		rt: Runtime{
			Stdout: stdout,
			Stderr: stderr,
			Clock:  fakeClock{now: time.Date(2026, 6, 24, 20, 0, 0, 0, time.UTC)},
		},
		clientFactory: func(Config, Runtime) (Client, error) { return fake, nil },
	}, stdout, stderr
}

func TestJobspecDefaultContainsOwnershipMetadataWithoutSecretsOrLocalPaths(t *testing.T) {
	cfg := testNomadConfig()
	cfg.Nomad.TokenEnv = "SECRET_NOMAD_TOKEN"
	in := jobSpecInput{LeaseID: "cbx_123456789abc", Slug: "blue-lobster", JobID: "crabbox-123456789abc", ExpiresAt: time.Date(2026, 6, 24, 21, 0, 0, 0, time.UTC)}
	job, err := buildJobSpec(cfg, in)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range ownershipMetadata(cfg, in) {
		if job.Meta[key] != value {
			t.Fatalf("metadata[%s]=%q want %q", key, job.Meta[key], value)
		}
	}
	rendered := strings.Join(mapValues(job.Meta), "\n")
	for _, forbidden := range []string{"SECRET_NOMAD_TOKEN", "nomad.example.test:4646", t.TempDir()} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("metadata leaked %q in %#v", forbidden, job.Meta)
		}
	}
}

func TestJobspecTemplateRequiresOwnershipMetadata(t *testing.T) {
	cfg := testNomadConfig()
	template := filepath.Join(t.TempDir(), "job.json")
	tpl := `{"ID":"{{.JobID}}","Name":"{{.JobID}}","Type":"service","TaskGroups":[{"Name":"crabbox","Tasks":[{"Name":"crabbox","Driver":"docker"}]}]}`
	if err := os.WriteFile(template, []byte(tpl), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Nomad.JobSpecTemplate = template
	_, err := buildJobSpec(cfg, jobSpecInput{LeaseID: "cbx_123456789abc", Slug: "blue-lobster", JobID: "crabbox-123456789abc"})
	if err == nil || !strings.Contains(err.Error(), "ownership metadata") {
		t.Fatalf("err=%v, want ownership metadata error", err)
	}
}

func TestWarmupRegistersJobAndWritesNomadClaim(t *testing.T) {
	fake := newLifecycleFakeClient()
	b, stdout, _ := testBackend(t, fake)
	repo := Repo{Root: filepath.Join(t.TempDir(), "repo"), Name: "my-app"}
	if err := b.Warmup(context.Background(), WarmupRequest{Repo: repo, Keep: true, RequestedSlug: "blue-lobster"}); err != nil {
		t.Fatal(err)
	}
	if fake.registers != 1 {
		t.Fatalf("registers=%d", fake.registers)
	}
	claims, err := listNomadLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 {
		t.Fatalf("claims=%#v", claims)
	}
	claim := claims[0]
	if claim.Provider != providerName || claim.ProviderScope != claimScope(b.cfg) || claim.Slug != "blue-lobster" {
		t.Fatalf("claim=%#v", claim)
	}
	if claim.Labels[claimLabelJobID] == "" || claim.Labels[claimLabelAllocationID] == "" ||
		claim.Labels[claimLabelNamespace] != "team-a" || claim.Labels[claimLabelRegion] != "global" ||
		claim.Labels[claimLabelTask] != "crabbox" || claim.Labels[claimLabelWorkdir] != "/workspace/crabbox" {
		t.Fatalf("labels=%#v", claim.Labels)
	}
	if !strings.Contains(stdout.String(), "allocation=") {
		t.Fatalf("stdout=%s", stdout.String())
	}
}

func TestListAndStatusKeepMissingJobsVisible(t *testing.T) {
	fake := newLifecycleFakeClient()
	b, _, _ := testBackend(t, fake)
	claim := createClaim(t, b, "cbx_111111111111", "quiet-crab", "crabbox-111111111111", "alloc-1")
	delete(fake.jobs, claim.Labels[claimLabelJobID])
	views, err := b.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Status != "missing-or-inaccessible" {
		t.Fatalf("views=%#v", views)
	}
	status, err := b.Status(context.Background(), StatusRequest{ID: "quiet-crab"})
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "missing-or-inaccessible" || status.Ready {
		t.Fatalf("status=%#v", status)
	}
}

func TestStatusRejectsWrongNamespaceScope(t *testing.T) {
	fake := newLifecycleFakeClient()
	b, _, _ := testBackend(t, fake)
	createClaim(t, b, "cbx_222222222222", "scope-crab", "crabbox-222222222222", "alloc-2")
	b.cfg.Nomad.Namespace = "other"
	_, err := b.Status(context.Background(), StatusRequest{ID: "scope-crab"})
	if err == nil || !strings.Contains(err.Error(), "different nomad scope") {
		t.Fatalf("err=%v, want scope error", err)
	}
}

func TestStopValidatesRemoteOwnershipBeforeDeregister(t *testing.T) {
	fake := newLifecycleFakeClient()
	b, _, _ := testBackend(t, fake)
	claim := createClaim(t, b, "cbx_333333333333", "owned-crab", "crabbox-333333333333", "alloc-3")
	fake.jobs[claim.Labels[claimLabelJobID]].Meta[metadataLeaseID] = "cbx_other"
	err := b.Stop(context.Background(), StopRequest{ID: "owned-crab"})
	if err == nil || !strings.Contains(err.Error(), "ownership metadata") {
		t.Fatalf("err=%v, want ownership error", err)
	}
	if len(fake.deregisters) != 0 {
		t.Fatalf("deregisters=%v", fake.deregisters)
	}
}

func TestStopRemovesClaimWhenOwnedJobAlreadyGone(t *testing.T) {
	fake := newLifecycleFakeClient()
	b, _, _ := testBackend(t, fake)
	claim := createClaim(t, b, "cbx_444444444444", "gone-crab", "crabbox-444444444444", "alloc-4")
	delete(fake.jobs, claim.Labels[claimLabelJobID])
	if err := b.Stop(context.Background(), StopRequest{ID: "gone-crab"}); err != nil {
		t.Fatal(err)
	}
	if retained, err := readLeaseClaim(claim.LeaseID); err != nil || retained.LeaseID != "" {
		t.Fatalf("retained=%#v err=%v", retained, err)
	}
}

func TestCleanupDryRunAndLiveOwnedExpiredClaims(t *testing.T) {
	fake := newLifecycleFakeClient()
	b, stdout, _ := testBackend(t, fake)
	claim := createClaim(t, b, "cbx_555555555555", "old-crab", "crabbox-555555555555", "alloc-5")
	expireClaim(t, claim, time.Date(2026, 6, 24, 19, 0, 0, 0, time.UTC))
	if err := b.Cleanup(context.Background(), CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deregisters) != 0 || !strings.Contains(stdout.String(), "would deregister") {
		t.Fatalf("dry-run stdout=%s deregisters=%v", stdout.String(), fake.deregisters)
	}
	stdout.Reset()
	if err := b.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deregisters) != 1 || fake.deregisters[0] != claim.Labels[claimLabelJobID] {
		t.Fatalf("deregisters=%v", fake.deregisters)
	}
	if retained, err := readLeaseClaim(claim.LeaseID); err != nil || retained.LeaseID != "" {
		t.Fatalf("retained=%#v err=%v", retained, err)
	}
}

func TestCleanupRefusesUnownedExpiredRemoteJob(t *testing.T) {
	fake := newLifecycleFakeClient()
	b, _, _ := testBackend(t, fake)
	claim := createClaim(t, b, "cbx_666666666666", "bad-crab", "crabbox-666666666666", "alloc-6")
	expireClaim(t, claim, time.Date(2026, 6, 24, 19, 0, 0, 0, time.UTC))
	fake.jobs[claim.Labels[claimLabelJobID]].Meta[metadataManaged] = "false"
	err := b.Cleanup(context.Background(), CleanupRequest{})
	if err == nil || !strings.Contains(err.Error(), "ownership metadata") {
		t.Fatalf("err=%v, want ownership error", err)
	}
	if len(fake.deregisters) != 0 {
		t.Fatalf("deregisters=%v", fake.deregisters)
	}
}

func TestSelectAllocationPrefersRunningTask(t *testing.T) {
	allocs := []*nomadapi.AllocationListStub{
		runningAlloc("job", "alloc-pending", "node-0", "pending", "other"),
		runningAlloc("job", "alloc-running", "node-1", "worker-1", "crabbox"),
	}
	allocs[0].ClientStatus = nomadapi.AllocClientStatusPending
	ready, err := selectAllocation(allocs, "job", "crabbox")
	if err != nil {
		t.Fatal(err)
	}
	if ready.AllocationID != "alloc-running" || ready.State() != "running" {
		t.Fatalf("ready=%#v", ready)
	}
}

func createClaim(t *testing.T, b *backend, leaseID, slug, jobID, allocID string) LeaseClaim {
	t.Helper()
	ready := allocationReadiness{
		JobID:         jobID,
		AllocationID:  allocID,
		Task:          b.cfg.Nomad.Task,
		NodeID:        "node-1",
		NodeName:      "worker-1",
		ClientStatus:  nomadapi.AllocClientStatusRunning,
		DesiredStatus: nomadapi.AllocDesiredStatusRun,
		TaskState:     "running",
	}
	expiresAt := b.now().Add(time.Hour)
	claim, err := writeNomadClaim(b.cfg, leaseID, slug, Repo{Root: filepath.Join(t.TempDir(), "repo"), Name: "repo"}, false, ready, expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	job, err := buildJobSpec(b.cfg, jobSpecInput{LeaseID: leaseID, Slug: slug, JobID: jobID, ExpiresAt: expiresAt})
	if err != nil {
		t.Fatal(err)
	}
	if fake, ok := mustClient(t, b).(*lifecycleFakeClient); ok {
		fake.jobs[jobID] = job
		fake.allocs[jobID] = []*nomadapi.AllocationListStub{runningAlloc(jobID, allocID, "node-1", "worker-1", b.cfg.Nomad.Task)}
	}
	return claim
}

func mustClient(t *testing.T, b *backend) Client {
	t.Helper()
	client, err := b.client()
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func expireClaim(t *testing.T, claim LeaseClaim, expiresAt time.Time) {
	t.Helper()
	labels := map[string]string{}
	for key, value := range claim.Labels {
		labels[key] = value
	}
	labels[claimLabelExpiresAt] = expiresAt.UTC().Format(time.RFC3339)
	if _, err := updateLeaseClaimLabelsIfUnchanged(claim.LeaseID, claim, labels); err != nil {
		t.Fatal(err)
	}
}

func runningAlloc(jobID, allocID, nodeID, nodeName, task string) *nomadapi.AllocationListStub {
	return &nomadapi.AllocationListStub{
		ID:            allocID,
		JobID:         jobID,
		NodeID:        nodeID,
		NodeName:      nodeName,
		DesiredStatus: nomadapi.AllocDesiredStatusRun,
		ClientStatus:  nomadapi.AllocClientStatusRunning,
		TaskStates: map[string]*nomadapi.TaskState{
			task: {State: "running"},
		},
	}
}

func cloneJob(job *nomadapi.Job) *nomadapi.Job {
	if job == nil {
		return nil
	}
	cloned := *job
	cloned.Meta = map[string]string{}
	for k, v := range job.Meta {
		cloned.Meta[k] = v
	}
	return &cloned
}

func mapValues(values map[string]string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}
