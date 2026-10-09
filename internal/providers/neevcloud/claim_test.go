package neevcloud

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

// fakeAPI is an in-memory sandbox API that records every effect on a sandbox.
type fakeAPI struct {
	mu        sync.Mutex
	sandboxes map[string]shared.EnvdSandbox
	effects   []string
	onConnect func()
}

func newFakeAPI() *fakeAPI { return &fakeAPI{sandboxes: map[string]shared.EnvdSandbox{}} }

func (f *fakeAPI) record(effect string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.effects = append(f.effects, effect)
}

func (f *fakeAPI) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.effects...)
}

func (f *fakeAPI) CreateSandbox(_ context.Context, req shared.EnvdSandboxCreateRequest) (shared.EnvdSandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := "sb-" + strings.TrimPrefix(req.Metadata["lease"], "cbx_")
	sb := shared.EnvdSandbox{SandboxID: id, Alias: sandboxName(req.Metadata["slug"], req.Metadata["lease"]), State: "running", Metadata: req.Metadata, Domain: "https://sandbox.example.test"}
	f.sandboxes[id] = sb
	return sb, nil
}

func (f *fakeAPI) ConnectSandbox(_ context.Context, id string, _ int) (shared.EnvdSandboxSession, error) {
	if f.onConnect != nil {
		f.onConnect()
	}
	return shared.EnvdSandboxSession{SandboxID: id, Domain: "https://sandbox.example.test"}, nil
}

func (f *fakeAPI) GetSandbox(_ context.Context, id string) (shared.EnvdSandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sb, ok := f.sandboxes[id]
	if !ok {
		return shared.EnvdSandbox{}, &apiError{StatusCode: 404, Status: "404 Not Found"}
	}
	return sb, nil
}

func (f *fakeAPI) ListSandboxes(_ context.Context, labels map[string]string) ([]shared.EnvdSandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []shared.EnvdSandbox
	for _, sb := range f.sandboxes {
		if labelsMatch(sb.Metadata, labels) {
			out = append(out, sb)
		}
	}
	return out, nil
}

func (f *fakeAPI) DeleteSandbox(_ context.Context, id string) error {
	f.record("delete " + id)
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sandboxes, id)
	return nil
}

func (f *fakeAPI) UploadFile(_ context.Context, s shared.EnvdSandboxSession, target string, _ io.Reader) error {
	f.record("upload " + s.SandboxID + " " + target)
	return nil
}

func (f *fakeAPI) StartProcess(_ context.Context, s shared.EnvdSandboxSession, req shared.EnvdSandboxProcessRequest) (int, error) {
	f.record("exec " + s.SandboxID + " " + req.Command)
	return 0, nil
}

// claimedBackend returns a backend with an isolated claim store and one sandbox kept and claimed by repoA.
func claimedBackend(t *testing.T) (*backend, *fakeAPI, string, core.LeaseClaim) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := newFakeAPI()
	old := newClient
	newClient = func(core.Config, core.Runtime) (shared.EnvdSandboxAPI, error) { return api, nil }
	t.Cleanup(func() { newClient = old })
	b := &backend{spec: Provider{}.Spec(), cfg: testConfig("https://api-a.example.test"), rt: core.Runtime{Stdout: io.Discard, Stderr: io.Discard}}
	b.cfg.Provider = providerName
	repoA := t.TempDir()
	claim, _, err := b.createSandbox(context.Background(), api, core.Repo{Root: repoA}, true, false, "")
	if err != nil {
		t.Fatal(err)
	}
	return b, api, repoA, claim
}

func TestAdmitRunClaimEnforcesRepositoryOwnership(t *testing.T) {
	b, api, repoA, claim := claimedBackend(t)
	repoB := t.TempDir()
	ids := map[string]string{
		"lease":        claim.LeaseID,
		"slug":         claim.Slug,
		"synthetic id": leasePrefix + claim.CloudID,
		"sandbox id":   claim.CloudID,
	}
	for name, id := range ids {
		t.Run("failure: other repository by "+name, func(t *testing.T) {
			if _, err := b.admitRunClaim(context.Background(), api, id, repoB, false); err == nil {
				t.Fatalf("repoB admitted to repoA's sandbox via %s %q", name, id)
			}
		})
		t.Run("success: owning repository by "+name, func(t *testing.T) {
			got, err := b.admitRunClaim(context.Background(), api, id, repoA, false)
			if err != nil || got.CloudID != claim.CloudID {
				t.Fatalf("claim=%+v err=%v", got, err)
			}
		})
	}
	t.Run("success: reclaim moves ownership", func(t *testing.T) {
		got, err := b.admitRunClaim(context.Background(), api, leasePrefix+claim.CloudID, repoB, true)
		if err != nil || got.CloudID != claim.CloudID {
			t.Fatalf("claim=%+v err=%v", got, err)
		}
		if _, err := b.admitRunClaim(context.Background(), api, claim.LeaseID, repoA, false); err == nil {
			t.Fatal("repoA still admitted after repoB reclaimed")
		}
	})
}

func TestAdmitRunClaimRefusesUnclaimedSandbox(t *testing.T) {
	b, api, repoA, _ := claimedBackend(t)
	// A sandbox carrying Crabbox labels but no local claim, such as one created elsewhere.
	api.sandboxes["sb-foreign"] = shared.EnvdSandbox{SandboxID: "sb-foreign", Metadata: map[string]string{
		"crabbox": "true", "provider": providerName, "lease": "cbx_aaaaaaaaaaaa", "slug": "foreign",
	}}
	for _, id := range []string{"sb-foreign", leasePrefix + "sb-foreign", "cbx_aaaaaaaaaaaa"} {
		if _, err := b.admitRunClaim(context.Background(), api, id, repoA, false); err == nil {
			t.Fatalf("unclaimed sandbox admitted via %q", id)
		}
	}
}

func TestAdmitRunClaimRefusesOtherScope(t *testing.T) {
	b, api, repoA, claim := claimedBackend(t)
	b.cfg.Neevcloud.ProjectID = "prj-2"
	if _, err := b.admitRunClaim(context.Background(), api, claim.LeaseID, repoA, false); err == nil {
		t.Fatal("claim from another project admitted")
	}
}

func TestRunStopsWhenClaimIsReassignedBeforeEffects(t *testing.T) {
	b, api, repoA, claim := claimedBackend(t)
	repoB := t.TempDir()
	reassigned := 0
	// Another process reclaims the lease for repoB while this run waits for readiness.
	api.onConnect = func() {
		sandbox, err := api.GetSandbox(context.Background(), claim.CloudID)
		if err != nil {
			t.Error(err)
			return
		}
		current, ok, err := core.ReadLeaseClaimWithPresence(claim.LeaseID)
		if err != nil || !ok {
			t.Errorf("read claim ok=%v err=%v", ok, err)
			return
		}
		if _, err := core.ClaimLeaseTargetForRepoConfigIfUnchanged(claim.LeaseID, claim.Slug, b.claimConfig(), sandboxViews.Server(sandbox),
			core.SSHTarget{}, repoB, time.Duration(claim.IdleTimeoutSeconds)*time.Second, true, current, true); err != nil {
			t.Error(err)
		}
		reassigned++
	}
	_, err := b.Run(context.Background(), core.RunRequest{
		ID: claim.LeaseID, Repo: core.Repo{Root: repoA}, Command: []string{"echo", "hi"}, NoSync: true,
	})
	if reassigned != 1 {
		t.Fatalf("reassigned %d times, want 1", reassigned)
	}
	if err == nil {
		t.Fatal("run succeeded after its claim was reassigned")
	}
	if effects := api.recorded(); len(effects) != 0 {
		t.Fatalf("stale run reached the sandbox: %v", effects)
	}
	if _, ok := api.sandboxes[claim.CloudID]; !ok {
		t.Fatal("stale run deleted the reassigned sandbox")
	}
}

// A fresh run fences on the claim it committed, so a reclaim after that commit stops its upload,
// exec and cleanup delete: the sandbox stays with the new owner.
func TestFreshRunStopsWhenReclaimedBeforeEffects(t *testing.T) {
	b, api, _, _ := claimedBackend(t)
	repoA, repoB := t.TempDir(), t.TempDir()
	reclaimed := ""
	api.onConnect = func() {
		if reclaimed != "" {
			return
		}
		api.mu.Lock()
		all := make([]shared.EnvdSandbox, 0, len(api.sandboxes))
		for _, sb := range api.sandboxes {
			all = append(all, sb)
		}
		api.mu.Unlock()
		// The fresh run's sandbox is the one whose claim names repoA.
		for _, sb := range all {
			leaseID := sb.Metadata["lease"]
			current, ok, err := core.ReadLeaseClaimWithPresence(leaseID)
			if err != nil || !ok || current.RepoRoot != repoA {
				continue
			}
			if _, err := core.ClaimLeaseTargetForRepoConfigIfUnchanged(leaseID, current.Slug, b.claimConfig(), sandboxViews.Server(sb),
				core.SSHTarget{}, repoB, time.Duration(current.IdleTimeoutSeconds)*time.Second, true, current, true); err != nil {
				t.Error(err)
				return
			}
			reclaimed = sb.SandboxID
		}
	}
	before := len(api.recorded())
	_, err := b.Run(context.Background(), core.RunRequest{Repo: core.Repo{Root: repoA}, Command: []string{"echo", "hi"}, NoSync: true})
	if reclaimed == "" {
		t.Fatal("the fresh lease was never reclaimed")
	}
	if err == nil {
		t.Fatal("run succeeded after its fresh lease was reclaimed")
	}
	if effects := api.recorded()[before:]; len(effects) != 0 {
		t.Fatalf("displaced run reached the sandbox: %v", effects)
	}
	if _, ok := api.sandboxes[reclaimed]; !ok {
		t.Fatal("displaced run deleted the reclaimed sandbox")
	}
}

func TestRunOnOwnedLeaseExecutesAndKeepsSandbox(t *testing.T) {
	b, api, repoA, claim := claimedBackend(t)
	result, err := b.Run(context.Background(), core.RunRequest{
		ID: claim.LeaseID, Repo: core.Repo{Root: repoA}, Command: []string{"echo", "hi"}, NoSync: true,
	})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	effects := strings.Join(api.recorded(), "\n")
	if !strings.Contains(effects, "exec "+claim.CloudID) || strings.Contains(effects, "delete") {
		t.Fatalf("effects=%s", effects)
	}
}

func TestStopDeletesOnlyClaimedSandbox(t *testing.T) {
	b, api, _, claim := claimedBackend(t)
	if err := b.Stop(context.Background(), core.StopRequest{ID: "cbx_ffffffffffff"}); err == nil {
		t.Fatal("stop without a claim succeeded")
	}
	if err := b.Stop(context.Background(), core.StopRequest{ID: claim.Slug}); err != nil {
		t.Fatal(err)
	}
	if effects := api.recorded(); len(effects) != 1 || effects[0] != "delete "+claim.CloudID {
		t.Fatalf("effects=%v", effects)
	}
	if _, ok, err := core.ReadLeaseClaimWithPresence(claim.LeaseID); err != nil || ok {
		t.Fatalf("claim still present ok=%v err=%v", ok, err)
	}
}
