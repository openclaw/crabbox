package machine0

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const fixedMachine0TestLeaseID = "cbx_abcdef123456"

func TestMachine0FixedAcquireReplayAdoptsExactMachine(t *testing.T) {
	for _, tc := range []struct {
		name             string
		fileName         string
		omitKey          bool
		emptyKey         bool
		inventoryKeyType string
		noSelectedKey    bool
		detailKey        *machineKey
		missingDetailKey bool
		mismatchedID     bool
		wantFile         string
		wantGetDetail    int
		wantError        string
	}{
		{name: "provider filename remains authoritative without key name", fileName: "selected-provider-key", wantFile: "selected-provider-key"},
		{name: "sparse inventory retains managed key ownership", wantFile: "machine0__ci-managed"},
		{name: "inventory omits managed key entirely", omitKey: true, wantFile: "machine0__ci-managed", wantGetDetail: 1},
		{name: "machine detail omits managed key filename", omitKey: true, detailKey: &machineKey{Name: "ci-managed", Type: "MANAGED"}, wantFile: "machine0__ci-managed", wantGetDetail: 1},
		{name: "inventory key has no usable identity", emptyKey: true, wantFile: "machine0__ci-managed", wantGetDetail: 1},
		{name: "detail key mismatches durable selection", omitKey: true, detailKey: &machineKey{Name: "another-key", Type: "MANAGED", FileName: "machine0__another-key"}, wantGetDetail: 1, wantError: "durable selected SSH key"},
		{name: "detail omits durable selected key", omitKey: true, missingDetailKey: true, wantGetDetail: 1, wantError: "durable selected SSH key"},
		{name: "detail machine mismatches inventory identity", omitKey: true, mismatchedID: true, wantGetDetail: 1, wantError: "inventory resource identity"},
		{name: "public detail retains generic key fallback", omitKey: true, detailKey: &machineKey{Name: "ci-managed", Type: "PUBLIC"}, wantGetDetail: 1},
		{name: "public inventory rejects mismatched detail type", emptyKey: true, inventoryKeyType: "PUBLIC", wantGetDetail: 1, wantError: "inventory SSH key type"},
		{name: "public inventory retains type omitted by detail", emptyKey: true, inventoryKeyType: "PUBLIC", detailKey: &machineKey{Name: "ci-managed"}, wantGetDetail: 1},
		{name: "no selected provider key skips detail read", omitKey: true, noSelectedKey: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, api, req := fixedMachine0TestFixture(t)
			keyRoot := t.TempDir()
			t.Setenv("SSH_KEY_PATH", keyRoot)
			for _, fileName := range []string{"machine0__ci-managed", "selected-provider-key"} {
				if err := os.WriteFile(filepath.Join(keyRoot, fileName), []byte("fixture private key"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			b.cfg.SSHKey = filepath.Join(keyRoot, "unrelated-global-key")
			if !tc.noSelectedKey {
				b.cfg.Machine0.Key = "ci-managed"
			}
			create := api.createFn
			api.createFn = func(ctx context.Context, createReq createMachineRequest) error {
				if err := create(ctx, createReq); err != nil {
					return err
				}
				api.machine.Key = &machineKey{Name: "ci-managed", Type: "MANAGED", FileName: "machine0__ci-managed"}
				api.machines = []machine{api.machine}
				return nil
			}
			first, err := b.Acquire(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			replayed := api.machine
			if tc.omitKey {
				replayed.Key = nil
			} else if tc.emptyKey {
				replayed.Key = &machineKey{Type: tc.inventoryKeyType}
			} else {
				replayed.Key = &machineKey{Name: "ci-managed", Type: "MANAGED", FileName: tc.fileName}
				if tc.fileName != "" {
					replayed.Key.Name = ""
				}
			}
			api.machines = []machine{replayed}
			if tc.detailKey != nil || tc.missingDetailKey {
				api.machine.Key = tc.detailKey
			}
			if tc.mismatchedID {
				api.machine.ID = "vm-impostor"
			}
			getDetail := 0
			api.getFn = func(_ context.Context, name string) (machine, error) {
				getDetail++
				if name != replayed.Name {
					t.Fatalf("machine detail lookup=%q want=%q", name, replayed.Name)
				}
				return api.machine, nil
			}
			var readinessKey string
			b.waitSSH = func(_ context.Context, target *SSHTarget, _ time.Duration) error {
				readinessKey = target.Key
				return nil
			}
			second, err := b.Acquire(context.Background(), req)
			if tc.wantError != "" {
				assertMachine0Exit(t, err, 4, tc.wantError)
				if getDetail != tc.wantGetDetail {
					t.Fatalf("fixed replay machine detail reads=%d want=%d", getDetail, tc.wantGetDetail)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := b.cfg.SSHKey
			if tc.wantFile != "" {
				want = filepath.Join(keyRoot, tc.wantFile)
			}
			if second.SSH.Key != want || readinessKey != want {
				t.Fatalf("fixed replay selected SSH key %q and readiness key %q, want provider-owned key %q", second.SSH.Key, readinessKey, want)
			}
			if getDetail != tc.wantGetDetail {
				t.Fatalf("fixed replay machine detail reads=%d want=%d", getDetail, tc.wantGetDetail)
			}
			if len(api.created) != 1 {
				t.Fatalf("Create calls=%d want=1", len(api.created))
			}
			if first.LeaseID != fixedMachine0TestLeaseID || second.LeaseID != first.LeaseID || second.Server.CloudID != first.Server.CloudID {
				t.Fatalf("first=%#v second=%#v", first, second)
			}
			claim := readFixedMachine0Claim(t, fixedMachine0TestLeaseID)
			if claim.Provider != core.FixedMachine0ClaimProvider || claim.CloudID != first.Server.CloudID || claim.CloudImmutableID != first.Server.CloudID || claim.ProviderScope != machineScope(first.Server.CloudID) || claim.FixedCreateIntent.State != fixedMachine0IntentAcquired {
				t.Fatalf("claim=%#v", claim)
			}
		})
	}
}

func TestMachine0FixedAcquireReusesInitialInventory(t *testing.T) {
	b, api, req := fixedMachine0TestFixture(t)
	if _, err := b.Acquire(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if api.listCalls != 1 {
		t.Fatalf("initial fixed acquisition listed Machine0 inventory %d times, want 1", api.listCalls)
	}
	api.machines[0].IP = "203.0.113.11"
	replayed, err := b.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if api.listCalls != 2 {
		t.Fatalf("fixed replay listed Machine0 inventory %d times in total, want 2", api.listCalls)
	}
	if replayed.SSH.Host != api.machines[0].IP {
		t.Fatalf("fixed replay reused stale inventory IP %q, want %q", replayed.SSH.Host, api.machines[0].IP)
	}
}

func TestMachine0FixedAcquireRejectsIntentDrift(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*backend, *AcquireRequest)
	}{
		{name: "size", mutate: func(b *backend, _ *AcquireRequest) { b.cfg.Machine0.Size = "xlarge" }},
		{name: "region", mutate: func(b *backend, _ *AcquireRequest) { b.cfg.Machine0.Region = "us-east" }},
		{name: "image", mutate: func(b *backend, _ *AcquireRequest) { b.cfg.Machine0.Image = "nixos-loaded" }},
		{name: "keep", mutate: func(_ *backend, req *AcquireRequest) { req.Keep = true }},
		{name: "requested slug", mutate: func(_ *backend, req *AcquireRequest) { req.RequestedSlug = "other" }},
		{name: "idle timeout", mutate: func(b *backend, _ *AcquireRequest) { b.cfg.IdleTimeout = 2 * time.Hour }},
		{name: "cache volume", mutate: func(b *backend, _ *AcquireRequest) {
			b.cfg.Cache.Volumes = []core.CacheVolumeConfig{{Name: "npm", Key: "npm-fixed", Path: "/var/cache/npm"}}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, api, req := fixedMachine0TestFixture(t)
			if _, err := b.Acquire(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			tc.mutate(b, &req)
			_, err := b.Acquire(context.Background(), req)
			assertMachine0Exit(t, err, 4, "lease_id_conflict")
			if len(api.created) != 1 {
				t.Fatalf("intent drift called Create again: %d", len(api.created))
			}
		})
	}
}

func TestMachine0FixedReleasedTombstoneRefusesReplay(t *testing.T) {
	b, api, req := fixedMachine0TestFixture(t)
	lease, err := b.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	previous := readFixedMachine0Claim(t, lease.LeaseID)
	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if len(api.removed) != 1 {
		t.Fatalf("removed=%v", api.removed)
	}
	claim := readFixedMachine0Claim(t, lease.LeaseID)
	assertFixedMachine0Tombstone(t, claim)
	retained, err := b.RetainLeaseClaimAfterReleaseWithClaim(lease, previous)
	if err != nil || !retained {
		t.Fatalf("retained=%v err=%v", retained, err)
	}
	_, err = b.Acquire(context.Background(), req)
	assertMachine0Exit(t, err, 4, "is terminal and cannot be replayed")
}

func TestMachine0FixedClaimBindingMismatchesRefuse(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*LeaseClaim)
		want   string
	}{
		{name: "claim provider", mutate: func(claim *LeaseClaim) { claim.Provider = providerName }, want: "bound to provider=machine0"},
		{name: "name scope", mutate: func(claim *LeaseClaim) {
			intent := *claim.FixedCreateIntent
			intent.ProviderScope = machine0NameScope("crabbox-impostor")
			claim.FixedCreateIntent = &intent
		}, want: "bound to another create intent"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, api, req := fixedMachine0TestFixture(t)
			if _, err := b.Acquire(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			claim := readFixedMachine0Claim(t, req.RequestedLeaseID)
			replacement := claim
			tc.mutate(&replacement)
			if err := core.ReplaceLeaseClaimIfUnchanged(claim.LeaseID, claim, replacement); err != nil {
				t.Fatal(err)
			}
			_, err := b.Acquire(context.Background(), req)
			assertMachine0Exit(t, err, 4, tc.want)
			if len(api.created) != 1 {
				t.Fatalf("binding mismatch called Create again: %d", len(api.created))
			}
		})
	}
}

func TestMachine0FixedAdoptionRequiresDurableAttempt(t *testing.T) {
	repo := setupState(t)
	api := &fakeAPI{sizes: []machineSize{testSize()}, machines: []machine{}}
	b := testBackendWithAPI(api)
	req := AcquireRequest{RequestedLeaseID: fixedMachine0TestLeaseID, RequestedSlug: "fixed", Repo: core.Repo{Root: repo}}
	seedFixedMachine0PreparedClaim(t, b, req, nil)
	name := machine0MachineName(req.RequestedLeaseID, req.RequestedSlug)
	item := fixedMachine0TestMachine(createMachineRequest{Name: name, Size: b.configForRun().Machine0.Size, Region: b.configForRun().Machine0.Region, Image: b.configForRun().Machine0.Image})
	api.machines = []machine{item}
	api.machine = item

	_, err := b.Acquire(context.Background(), req)
	assertMachine0Exit(t, err, 4, "has no durable create attempt")
	if len(api.created) != 0 {
		t.Fatalf("Create calls=%d", len(api.created))
	}
}

func TestMachine0FixedAcquiredCloudIDMustMatchLiveMachine(t *testing.T) {
	b, api, req := fixedMachine0TestFixture(t)
	if _, err := b.Acquire(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	impostor := api.machine
	impostor.ID = "vm-impostor"
	api.machine = impostor
	api.machines = []machine{impostor}

	_, err := b.Acquire(context.Background(), req)
	assertMachine0Exit(t, err, 4, "does not match acquired CloudID")
	if len(api.created) != 1 {
		t.Fatalf("Create calls=%d", len(api.created))
	}
}

func TestMachine0FixedAdoptionValidatesPinnedMachineShape(t *testing.T) {
	b, api, req := fixedMachine0TestFixture(t)
	if _, err := b.Acquire(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	drifted := api.machine
	drifted.Size = "unexpected-size"
	api.machine = drifted
	api.machines = []machine{drifted}

	_, err := b.Acquire(context.Background(), req)
	assertMachine0Exit(t, err, 4, "does not match its durable create attempt")
	if len(api.created) != 1 {
		t.Fatalf("Create calls=%d", len(api.created))
	}
}

func TestMachine0FixedInventoryRejectsDuplicateMachineIDs(t *testing.T) {
	b, api, req := fixedMachine0TestFixture(t)
	if _, err := b.Acquire(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	duplicate := api.machine
	duplicate.Name = "other-name"
	api.machines = []machine{api.machine, duplicate}

	_, err := b.Acquire(context.Background(), req)
	assertMachine0Exit(t, err, 4, "duplicate resource ID")
	if len(api.created) != 1 {
		t.Fatalf("Create calls=%d", len(api.created))
	}
}

func TestMachine0FixedAcquiredMissingMachineNeverCreatesReplacement(t *testing.T) {
	b, api, req := fixedMachine0TestFixture(t)
	if _, err := b.Acquire(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	api.machines = []machine{}
	api.machine = machine{}

	_, err := b.Acquire(context.Background(), req)
	assertMachine0Exit(t, err, 4, "is missing its bound Machine0 machine")
	if len(api.created) != 1 {
		t.Fatalf("Create calls=%d", len(api.created))
	}
}

func TestMachine0FixedPinnedInvisibleAttemptFailsClosed(t *testing.T) {
	b, api, req := fixedMachine0TestFixture(t)
	if _, err := b.Acquire(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	claim := readFixedMachine0Claim(t, req.RequestedLeaseID)
	replacement := claim
	intent := *claim.FixedCreateIntent
	intent.State = fixedMachine0IntentPrepared
	replacement.FixedCreateIntent = &intent
	replacement.CloudID = ""
	replacement.CloudImmutableID = ""
	replacement.ProviderScope = intent.ProviderScope
	replacement.Labels = nil
	replacement.SSHHost = ""
	replacement.SSHPort = 0
	if err := core.ReplaceLeaseClaimIfUnchanged(claim.LeaseID, claim, replacement); err != nil {
		t.Fatal(err)
	}
	api.machines = []machine{}
	api.machine = machine{}

	_, err := b.Acquire(context.Background(), req)
	assertMachine0Exit(t, err, 4, "unresolved create attempt")
	if len(api.created) != 1 {
		t.Fatalf("Create calls=%d", len(api.created))
	}
}

func TestMachine0FixedDefiniteCreateFailureClearsAttemptForRetry(t *testing.T) {
	repo := setupState(t)
	createErr := errors.New("machine0 capacity unavailable")
	api := &fakeAPI{sizes: []machineSize{testSize()}, machines: []machine{}, createErr: createErr}
	b := testBackendWithAPI(api)
	req := AcquireRequest{RequestedLeaseID: fixedMachine0TestLeaseID, RequestedSlug: "fixed", Repo: core.Repo{Root: repo}}

	_, err := b.Acquire(context.Background(), req)
	if err != createErr {
		t.Fatalf("create error=%v want exact %v", err, createErr)
	}
	claim := readFixedMachine0Claim(t, req.RequestedLeaseID)
	if len(claim.FixedCreateIntent.Attempt) != 0 || claim.FixedCreateIntent.State != fixedMachine0IntentPrepared {
		t.Fatalf("claim after definite failure=%#v", claim)
	}
	api.createErr = nil
	api.createFn = func(_ context.Context, createReq createMachineRequest) error {
		item := fixedMachine0TestMachine(createReq)
		api.machine = item
		api.machines = []machine{item}
		return nil
	}
	lease, err := b.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.CloudID != "vm-fixed" || len(api.created) != 2 {
		t.Fatalf("lease=%#v Create calls=%d", lease, len(api.created))
	}
}

func TestMachine0FixedCreateErrorReconcilesVisibleMachine(t *testing.T) {
	repo := setupState(t)
	responseErr := errors.New("machine0 create response lost")
	api := &fakeAPI{sizes: []machineSize{testSize()}, machines: []machine{}}
	b := testBackendWithAPI(api)
	api.createFn = func(_ context.Context, createReq createMachineRequest) error {
		item := fixedMachine0TestMachine(createReq)
		api.machine = item
		api.machines = []machine{item}
		return responseErr
	}
	req := AcquireRequest{RequestedLeaseID: fixedMachine0TestLeaseID, RequestedSlug: "fixed", Repo: core.Repo{Root: repo}}

	lease, err := b.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.CloudID != "vm-fixed" || len(api.created) != 1 {
		t.Fatalf("lease=%#v Create calls=%d", lease, len(api.created))
	}
	claim := readFixedMachine0Claim(t, req.RequestedLeaseID)
	if claim.FixedCreateIntent.State != fixedMachine0IntentAcquired || len(claim.FixedCreateIntent.Attempt) == 0 {
		t.Fatalf("claim=%#v", claim)
	}
}

func TestMachine0FixedRepositoryBindingRequiresReclaim(t *testing.T) {
	b, api, req := fixedMachine0TestFixture(t)
	if _, err := b.Acquire(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	req.Repo.Root = t.TempDir()
	_, err := b.Acquire(context.Background(), req)
	assertMachine0Exit(t, err, 4, "bound to another repository")
	req.Reclaim = true
	if _, err := b.Acquire(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(api.created) != 1 {
		t.Fatalf("Create calls=%d", len(api.created))
	}
}

func TestMachine0FixedSuspendReleaseKeepsLiveClaimAndReplayStartsMachine(t *testing.T) {
	b, api, req := fixedMachine0TestFixture(t)
	b.cfg.Machine0.ReleasePolicy = "suspend"
	lease, err := b.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	claim := readFixedMachine0Claim(t, lease.LeaseID)
	if claim.FixedCreateIntent.State != fixedMachine0IntentAcquired || claim.CloudID != lease.Server.CloudID || claim.ProviderScope != machineScope(lease.Server.CloudID) {
		t.Fatalf("suspended claim=%#v", claim)
	}
	if !b.RetainLeaseClaimAfterRelease(lease) || len(api.removed) != 0 || len(api.suspended) != 1 {
		t.Fatalf("removed=%v suspended=%v retained=%v", api.removed, api.suspended, b.RetainLeaseClaimAfterRelease(lease))
	}
	starting := api.machine
	starting.Status = "STARTING"
	starting.IP = ""
	ready := api.machine
	ready.Status = "RUNNING"
	ready.IP = "203.0.113.77"
	api.getSequence = []machine{starting, ready}
	replayed, err := b.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Server.CloudID != lease.Server.CloudID || replayed.SSH.Host != ready.IP || len(api.created) != 1 || len(api.started) != 1 {
		t.Fatalf("replayed=%#v created=%d started=%v", replayed, len(api.created), api.started)
	}
}

func TestMachine0FixedAcquireClaimDoesNotPersistSecrets(t *testing.T) {
	const secret = "machine0-claim-secret-sentinel"
	b, _, req := fixedMachine0TestFixture(t)
	b.cfg.CoordToken = secret
	b.cfg.CoordAdminToken = secret
	b.cfg.Access.ClientSecret = secret
	b.cfg.SSHKey = secret
	b.cfg.Tailscale.AuthKey = secret
	if _, err := b.Acquire(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	stateDir, err := core.CrabboxStateDir()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(stateDir, "claims", req.RequestedLeaseID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secret) {
		t.Fatalf("claim persisted secret material: %s", data)
	}
}

func TestMachine0FixedCleanupSkipsReleasedTombstone(t *testing.T) {
	b, _, req := fixedMachine0TestFixture(t)
	lease, err := b.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	b.api.(*fakeAPI).machines = []machine{}
	if err := b.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	assertFixedMachine0Tombstone(t, readFixedMachine0Claim(t, req.RequestedLeaseID))
}

func fixedMachine0TestFixture(t *testing.T) (*backend, *fakeAPI, AcquireRequest) {
	t.Helper()
	repo := setupState(t)
	xlarge := testSize()
	xlarge.Size = "xlarge"
	api := &fakeAPI{sizes: []machineSize{testSize(), xlarge}, machines: []machine{}}
	b := testBackendWithAPI(api)
	api.createFn = func(_ context.Context, req createMachineRequest) error {
		item := fixedMachine0TestMachine(req)
		api.machine = item
		api.machines = []machine{item}
		return nil
	}
	req := AcquireRequest{RequestedLeaseID: fixedMachine0TestLeaseID, RequestedSlug: "fixed", Repo: core.Repo{Root: repo}}
	return b, api, req
}

func fixedMachine0TestMachine(req createMachineRequest) machine {
	item := readyMachine("203.0.113.10")
	item.ID = "vm-fixed"
	item.Name = req.Name
	item.Size = req.Size
	item.Region = req.Region
	item.Image = req.Image
	item.ImageVersion = req.ImageVersion
	return item
}

func seedFixedMachine0PreparedClaim(t *testing.T, b *backend, req AcquireRequest, attempt *machine0CreateAttempt) {
	t.Helper()
	cfg := b.configForRun()
	fingerprint, err := core.FixedMachine0CreateIntentFingerprint(cfg, core.FixedMachine0CreateIntentRequest{RequestedSlug: core.NormalizeLeaseSlug(req.RequestedSlug), Keep: req.Keep})
	if err != nil {
		t.Fatal(err)
	}
	slug := core.NormalizeLeaseSlug(req.RequestedSlug)
	name := machine0MachineName(req.RequestedLeaseID, slug)
	now := time.Now().UTC()
	err = core.WithDurableLeaseClaimLock(req.RequestedLeaseID, func(claim *core.LeaseClaim, _ bool, persist func() error) error {
		*claim = core.LeaseClaim{
			LeaseID: req.RequestedLeaseID, Slug: slug, Provider: core.FixedMachine0ClaimProvider,
			ProviderScope: machine0NameScope(name), TargetOS: cfg.TargetOS, RepoRoot: req.Repo.Root,
			ClaimedAt: now.Format(time.RFC3339), LastUsedAt: now.Format(time.RFC3339), IdleTimeoutSeconds: int(cfg.IdleTimeout.Seconds()),
			FixedCreateIntent: &core.FixedCreateIntent{
				Version: fixedMachine0CreateIntentVersion, Fingerprint: fingerprint, ProviderScope: machine0NameScope(name),
				Slug: slug, CreatedAt: now.Format(time.RFC3339Nano), State: fixedMachine0IntentPrepared,
			},
		}
		if attempt != nil {
			data, marshalErr := json.Marshal(attempt)
			if marshalErr != nil {
				return marshalErr
			}
			claim.FixedCreateIntent.Attempt = map[string]string{"machine0": string(data)}
		}
		return persist()
	})
	if err != nil {
		t.Fatal(err)
	}
}

func readFixedMachine0Claim(t *testing.T, leaseID string) LeaseClaim {
	t.Helper()
	claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil || !exists {
		t.Fatalf("claim exists=%v err=%v", exists, err)
	}
	return claim
}

func assertMachine0Exit(t *testing.T, err error, code int, contains string) {
	t.Helper()
	var exitErr core.ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != code || !strings.Contains(err.Error(), contains) {
		t.Fatalf("err=%v code=%d want code=%d containing %q", err, exitErr.Code, code, contains)
	}
}

func assertFixedMachine0Tombstone(t *testing.T, claim LeaseClaim) {
	t.Helper()
	if claim.Provider != core.FixedMachine0ClaimProvider || claim.FixedCreateIntent == nil ||
		claim.FixedCreateIntent.State != fixedMachine0IntentReleased || claim.ProviderScope != claim.FixedCreateIntent.ProviderScope || claim.Slug != claim.FixedCreateIntent.Slug ||
		claim.CloudID != "" || claim.CloudNumericID != 0 || claim.CloudImmutableID != "" || len(claim.Labels) != 0 || claim.SSHHost != "" || claim.SSHPort != 0 ||
		len(claim.FixedCreateIntent.Attempt) != 0 || len(claim.FixedCreateIntent.FailedAttempts) != 0 {
		t.Fatalf("invalid terminal tombstone=%#v", claim)
	}
	for _, value := range []string{claim.StaticHost, claim.StaticUser, claim.StaticPort, claim.StaticWorkRoot, claim.TargetOS, claim.WindowsMode, claim.Pond, claim.RepoRoot, claim.TailscaleIPv4, claim.TailscaleFQDN, claim.TailscaleHostname, claim.BridgeURL, claim.CoordinatorRegistrationURL, claim.RuntimeAdapterRegistrationID, claim.RuntimeAdapterPendingRegistrationID} {
		if value != "" {
			t.Fatalf("terminal tombstone retained unrelated fields=%#v", claim)
		}
	}
	if len(claim.TailscaleTags) != 0 || len(claim.CacheVolumes) != 0 {
		t.Fatalf("terminal tombstone retained unrelated slices=%#v", claim)
	}
}
