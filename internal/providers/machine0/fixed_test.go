package machine0

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const fixedMachine0TestLeaseID = "cbx_abcdef123456"

func TestMachine0FixedReplayAfterCapacityDisappears(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("catalog-row-missing=%t", missing), func(t *testing.T) {
			b, api, req := fixedMachine0TestFixture(t)
			first, err := b.Acquire(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			api.sizes = []machineSize{testSize()}
			api.sizes[0].Regions = nil
			if missing {
				api.sizes = nil
			}
			replayed, err := b.Acquire(context.Background(), req)
			if err != nil || replayed.Server.CloudID != first.Server.CloudID {
				t.Fatalf("exact replay after capacity disappeared: id=%s err=%v", replayed.Server.CloudID, err)
			}
			b.cfg.ServerType, b.cfg.ServerTypeExplicit = "unknown-native-size", true
			_, err = b.Acquire(context.Background(), req)
			assertMachine0Exit(t, err, 4, "lease_id_conflict")
			if len(api.created) != 1 || len(api.removed)+len(api.started)+len(api.suspended) != 0 {
				t.Fatalf("replay mutated machines: creates=%d removed=%v started=%v suspended=%v", len(api.created), api.removed, api.started, api.suspended)
			}
		})
	}
}

func TestMachine0FixedCreateRejectsSubstitutedSizeAndPinsReplay(t *testing.T) {
	b, api, req := fixedMachine0TestFixture(t)
	create := api.createFn
	api.createFn = func(ctx context.Context, request createMachineRequest) error {
		if err := create(ctx, request); err != nil {
			return err
		}
		api.machine.Size = "medium"
		api.machines = []machine{api.machine}
		return nil
	}
	_, err := b.Acquire(context.Background(), req)
	assertMachine0Exit(t, err, 4, "size=")
	claim := readFixedMachine0Claim(t, req.RequestedLeaseID)
	if claim.FixedCreateIntent.State != fixedMachine0IntentPrepared || len(claim.FixedCreateIntent.Attempt) == 0 {
		t.Fatalf("failed create lost pinned intent: %#v", claim.FixedCreateIntent)
	}
	_, err = b.Acquire(context.Background(), req)
	assertMachine0Exit(t, err, 4, "durable create attempt")
	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: req.RequestedLeaseID}}); err == nil {
		t.Fatal("release authorized a mismatched VM without an attested resource ID")
	}
	if len(api.created) != 1 || len(api.removed) != 0 {
		t.Fatalf("fixed failure must retain attempt without duplicate create: created=%d removed=%v", len(api.created), api.removed)
	}
}

func TestMachine0FixedNewCreateStillRequiresCapacity(t *testing.T) {
	b, api, req := fixedMachine0TestFixture(t)
	api.sizes[0].Regions = nil
	_, err := b.Acquire(context.Background(), req)
	assertMachine0Exit(t, err, 2, "not currently available")
	if len(api.created) != 0 {
		t.Fatal("created without regional capacity")
	}
	api.sizes[0].Regions = []string{"eu"}
	// A persisted empty record cannot be distinguished from legacy erased intent.
	_, err = b.Acquire(context.Background(), req)
	assertMachine0Exit(t, err, 4, "retain its claim")
	if len(api.created) != 0 {
		t.Fatal("capacity recovery inferred that a persisted claim never submitted")
	}
}

func TestMachine0FixedAcquireBindsCheckpointIdentity(t *testing.T) {
	b, api, req := fixedMachine0TestFixture(t)
	req.RequestedCheckpointID = "chk_fixed_machine0"
	creates := 0
	create := api.createFn
	api.createFn = func(ctx context.Context, request createMachineRequest) error {
		creates++
		return create(ctx, request)
	}
	first, err := b.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	claim := readFixedMachine0Claim(t, req.RequestedLeaseID)
	if claim.FixedCreateIntent == nil || claim.FixedCreateIntent.CheckpointID != req.RequestedCheckpointID {
		t.Fatalf("fixed Machine0 checkpoint intent=%#v", claim.FixedCreateIntent)
	}
	replayed, err := b.Acquire(context.Background(), req)
	if err != nil || replayed.Server.CloudID != first.Server.CloudID || creates != 1 {
		t.Fatalf("replayed=%#v err=%v creates=%d", replayed, err, creates)
	}
	drifted := req
	drifted.RequestedCheckpointID = "chk_other_machine0"
	_, err = b.Acquire(context.Background(), drifted)
	if err == nil || !strings.Contains(err.Error(), req.RequestedCheckpointID) || !strings.Contains(err.Error(), drifted.RequestedCheckpointID) {
		t.Fatalf("checkpoint mismatch err=%v", err)
	}
	if creates != 1 {
		t.Fatalf("creates=%d after checkpoint mismatch, want 1", creates)
	}
}

func TestMachine0FixedAcquireReplayAdoptsExactMachine(t *testing.T) {
	managed := &machineKey{Name: "ci-managed", Type: "MANAGED", FileName: "machine0__ci-managed"}
	for _, tc := range []struct {
		name          string
		inventoryKey  *machineKey
		detailKey     *machineKey
		noSelectedKey bool
		resume        bool
		wantFile      string
		wantError     string
	}{
		{name: "detail filename overrides inventory", inventoryKey: &machineKey{FileName: "selected-provider-key"}, detailKey: managed, wantFile: "machine0__ci-managed"},
		{name: "detail filename remains authoritative without selected key name", noSelectedKey: true, detailKey: &machineKey{FileName: "selected-provider-key"}, wantFile: "selected-provider-key"},
		{name: "sparse inventory retains managed key ownership", inventoryKey: &machineKey{Name: "ci-managed", Type: "MANAGED"}, detailKey: managed, wantFile: "machine0__ci-managed"},
		{name: "inventory omits managed key entirely", detailKey: managed, wantFile: "machine0__ci-managed"},
		{name: "machine detail omits managed key filename", detailKey: &machineKey{Name: "ci-managed", Type: "MANAGED"}, wantFile: "machine0__ci-managed"},
		{name: "inventory key has no usable identity", inventoryKey: &machineKey{}, detailKey: managed, wantFile: "machine0__ci-managed"},
		{name: "detail key mismatches durable selection", inventoryKey: managed, detailKey: &machineKey{Name: "another-key", Type: "MANAGED"}, wantError: "durable selected SSH key"},
		{name: "detail omits durable selected key", inventoryKey: managed, wantError: "durable selected SSH key"},
		{name: "inventory key mismatches detail name", inventoryKey: &machineKey{Name: "another-key"}, detailKey: managed, wantError: "inventory SSH key name"},
		{name: "public detail retains generic key fallback", detailKey: &machineKey{Name: "ci-managed", Type: "PUBLIC"}},
		{name: "public inventory rejects mismatched detail type", inventoryKey: &machineKey{Type: "PUBLIC"}, detailKey: managed, wantError: "inventory SSH key type"},
		{name: "public inventory retains type omitted by detail", inventoryKey: &machineKey{Name: "ci-managed", Type: "PUBLIC"}, detailKey: &machineKey{Name: "ci-managed"}},
		{name: "public type survives readiness detail refresh", inventoryKey: &machineKey{Name: "ci-managed", Type: "PUBLIC"}, detailKey: &machineKey{Name: "ci-managed"}, resume: true},
		{name: "managed inventory retains type omitted by detail", inventoryKey: managed, detailKey: &machineKey{Name: "ci-managed"}, wantFile: "machine0__ci-managed"},
		{name: "no selected provider key retains generic fallback", noSelectedKey: true},
		{name: "unselected key rejects name conflict", noSelectedKey: true, inventoryKey: &machineKey{Name: "another-key"}, detailKey: managed, wantError: "inventory SSH key name"},
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
			b.cfg.Machine0.ImageVersion = 1
			if !tc.noSelectedKey {
				b.cfg.Machine0.Key = "ci-managed"
			}
			create := api.createFn
			api.createFn = func(ctx context.Context, request createMachineRequest) error {
				if err := create(ctx, request); err != nil {
					return err
				}
				api.machine.Key = managed
				return nil
			}
			first, err := b.Acquire(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			summary := api.machine
			summary.ImageVersion, summary.Key = 0, tc.inventoryKey
			api.machines = []machine{summary}
			api.machine.Key = tc.detailKey
			if tc.wantError != "" {
				api.machine.Status = "STOPPED"
			}
			before := readFixedMachine0Claim(t, req.RequestedLeaseID)
			getDetail := 0
			api.getFn = func(_ context.Context, name string) (machine, error) {
				getDetail++
				if name != summary.Name {
					t.Fatalf("machine detail lookup=%q want=%q", name, summary.Name)
				}
				detail := api.machine
				if detail.Key != nil {
					key := *detail.Key
					detail.Key = &key
				}
				if tc.resume && getDetail == 1 {
					detail.Status = "STOPPED"
				}
				return detail, nil
			}
			var readinessKey string
			b.waitSSH = func(_ context.Context, target *SSHTarget, _ time.Duration) error {
				readinessKey = target.Key
				return nil
			}
			second, err := b.Acquire(context.Background(), req)
			wantGet, wantStart := 1, 0
			if tc.resume {
				wantGet, wantStart = 2, 1
			}
			if getDetail != wantGet || len(api.created) != 1 || len(api.started) != wantStart || len(api.removed)+len(api.primed) != 0 {
				t.Fatalf("unexpected replay calls: detail=%d creates=%d starts=%v removes=%v primes=%v", getDetail, len(api.created), api.started, api.removed, api.primed)
			}
			if tc.wantError != "" {
				assertMachine0Exit(t, err, 4, tc.wantError)
				if readinessKey != "" || !reflect.DeepEqual(before, readFixedMachine0Claim(t, req.RequestedLeaseID)) {
					t.Fatal("rejected detail reached SSH readiness or changed the claim")
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
				t.Fatalf("fixed replay SSH key=%q readiness key=%q want=%q", second.SSH.Key, readinessKey, want)
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

func TestMachine0FixedAcquireUsesFreshInventoryAtOwnershipBoundary(t *testing.T) {
	b, api, req := fixedMachine0TestFixture(t)
	if _, err := b.Acquire(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if api.listCalls != 2 {
		t.Fatalf("initial fixed acquisition listed Machine0 inventory %d times, want 2 authoritative reads", api.listCalls)
	}
	api.machine.IP = "203.0.113.11"
	replayed, err := b.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if api.listCalls != 3 {
		t.Fatalf("fixed replay listed Machine0 inventory %d times in total, want 3", api.listCalls)
	}
	if replayed.SSH.Host != api.machine.IP {
		t.Fatalf("fixed replay reused stale inventory IP %q, want detail IP %q", replayed.SSH.Host, api.machine.IP)
	}
}

func TestMachine0FixedAcquireRejectsMachineAppearingAfterSlugBinding(t *testing.T) {
	b, api, req := fixedMachine0TestFixture(t)
	cfg := b.configForRun()
	unowned := fixedMachine0TestMachine(createMachineRequest{
		Name: machine0MachineName(req.RequestedLeaseID, req.RequestedSlug),
		Size: cfg.Machine0.Size, Region: cfg.Machine0.Region, Image: cfg.Machine0.Image,
	})
	unowned.ID = "vm-unowned"
	api.listFn = func(_ context.Context, call int) ([]machine, error) {
		if call == 1 {
			return nil, nil
		}
		return []machine{unowned}, nil
	}
	api.createFn = func(context.Context, createMachineRequest) error {
		api.machine = unowned
		return errors.New("machine name already exists")
	}

	_, err := b.Acquire(context.Background(), req)
	assertMachine0Exit(t, err, 4, "has no durable create attempt")
	if len(api.created) != 0 {
		t.Fatalf("attempted to create or adopt an unowned machine: create calls=%d", len(api.created))
	}
	claim := readFixedMachine0Claim(t, req.RequestedLeaseID)
	if claim.CloudID != "" || len(claim.FixedCreateIntent.Attempt) != 0 {
		t.Fatalf("bound an unowned machine or create attempt: claim=%#v", claim)
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
	for _, tc := range []struct {
		name   string
		mutate func(*machine)
		want   string
	}{
		{"size", func(m *machine) { m.Size = "unexpected-size" }, "size="},
		{"region", func(m *machine) { m.Region = "us-east" }, "region="},
		{"image", func(m *machine) { m.Image = "another-image" }, "image="},
		{"missing version", func(m *machine) { m.ImageVersion = 0 }, "imageVersion=0, want 1"},
		{"wrong version", func(m *machine) { m.ImageVersion = 2 }, "imageVersion=2, want 1"},
		{"substituted ID", func(m *machine) { m.ID = "vm-impostor" }, "inventory resource identity"},
		{"missing ID", func(m *machine) { m.ID = "" }, "inventory resource identity"},
		{"substituted name", func(m *machine) { m.Name = "another-machine" }, "inventory resource identity"},
		{"read failure", nil, "detail read failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, api, req := fixedMachine0TestFixture(t)
			b.cfg.Machine0.ImageVersion = 1
			if _, err := b.Acquire(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			before := readFixedMachine0Claim(t, req.RequestedLeaseID)
			api.machine.Status = "STOPPED"
			readErr := errors.New("detail read failed")
			if tc.mutate != nil {
				tc.mutate(&api.machine)
			} else {
				api.getFn = func(context.Context, string) (machine, error) { return machine{}, readErr }
			}
			b.waitSSH = func(context.Context, *SSHTarget, time.Duration) error {
				t.Fatal("unattested detail reached SSH readiness")
				return nil
			}
			_, err := b.Acquire(context.Background(), req)
			if tc.mutate == nil {
				if !errors.Is(err, readErr) {
					t.Fatalf("detail error was masked: %v", err)
				}
			} else {
				assertMachine0Exit(t, err, 4, tc.want)
			}
			if len(api.created) != 1 || len(api.started)+len(api.removed)+len(api.primed) != 0 || !reflect.DeepEqual(before, readFixedMachine0Claim(t, req.RequestedLeaseID)) {
				t.Fatal("rejected detail mutated provider or durable claim")
			}
		})
	}
}

func TestMachine0FixedInventoryRejectsAmbiguousIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows func(machine) []machine
		want string
	}{
		{"duplicate ID", func(m machine) []machine { other := m; other.Name = "another-name"; return []machine{m, other} }, "duplicate resource ID"},
		{"multiple names", func(m machine) []machine { other := m; other.ID = "vm-other"; return []machine{m, other} }, "multiple Machine0 machines"},
		{"missing ID", func(m machine) []machine { m.ID = ""; return []machine{m} }, "has no resource ID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, api, req := fixedMachine0TestFixture(t)
			if _, err := b.Acquire(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			before := readFixedMachine0Claim(t, req.RequestedLeaseID)
			api.machines = tc.rows(api.machine)
			api.getFn = func(context.Context, string) (machine, error) {
				t.Fatal("ambiguous inventory reached detail lookup")
				return machine{}, nil
			}
			_, err := b.Acquire(context.Background(), req)
			assertMachine0Exit(t, err, 4, tc.want)
			if len(api.created) != 1 || len(api.started)+len(api.removed) != 0 || !reflect.DeepEqual(before, readFixedMachine0Claim(t, req.RequestedLeaseID)) {
				t.Fatal("ambiguous inventory mutated provider or durable claim")
			}
		})
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

func TestMachine0FixedAmbiguousCreateFailureRetainsAttempt(t *testing.T) {
	b, api, req := fixedMachine0TestFixture(t)
	createErr := errors.New("machine0 create response lost")
	api.createFn = func(context.Context, createMachineRequest) error { return createErr }
	_, err := b.Acquire(context.Background(), req)
	if !errors.Is(err, createErr) {
		t.Fatalf("create error=%v want %v", err, createErr)
	}
	claim := readFixedMachine0Claim(t, req.RequestedLeaseID)
	if len(claim.FixedCreateIntent.Attempt) == 0 || claim.FixedCreateIntent.State != fixedMachine0IntentPrepared {
		t.Fatalf("ambiguous failure lost its attempt: %#v", claim)
	}
	_, err = b.Acquire(context.Background(), req)
	assertMachine0Exit(t, err, 4, "unresolved create attempt")
	if len(api.created) != 1 {
		t.Fatalf("ambiguous failure allowed %d create calls", len(api.created))
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
	api.getSequence = []machine{api.machine, starting, ready}
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

func TestMachine0FixedUnboundClaimsFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*LeaseClaim)
	}{
		{"durable attempt", func(c *LeaseClaim) { c.FixedCreateIntent.Attempt = map[string]string{"machine0": "unresolved"} }},
		{"failed attempts", func(c *LeaseClaim) { c.FixedCreateIntent.FailedAttempts = []string{"unresolved"} }},
		{"missing intent", func(c *LeaseClaim) { c.FixedCreateIntent = nil }},
		{"prepared invalid timestamp", func(c *LeaseClaim) { c.FixedCreateIntent.CreatedAt = "invalid" }},
		{"prepared wrong scope", func(c *LeaseClaim) { c.ProviderScope = "machine0:name:other" }},
		{"released wrong version", func(c *LeaseClaim) { c.FixedCreateIntent.Version++ }},
		{"released wrong scope", func(c *LeaseClaim) {
			c.FixedCreateIntent.ProviderScope = "machine0:name:other"
			c.ProviderScope = c.FixedCreateIntent.ProviderScope
		}},
		{"released wrong provider", func(c *LeaseClaim) { c.Provider = providerName; c.CloudID = "vm-other" }},
		{"released resource", func(c *LeaseClaim) { c.CloudID = "vm-other" }},
		{"released immutable identity", func(c *LeaseClaim) { c.CloudImmutableID = "vm-other" }},
		{"released attempt", func(c *LeaseClaim) { c.FixedCreateIntent.Attempt = map[string]string{"machine0": "unresolved"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, api, req := fixedMachine0TestFixture(t)
			seedFixedMachine0PreparedClaim(t, b, req, nil)
			err := core.WithDurableLeaseClaimLock(req.RequestedLeaseID, func(claim *core.LeaseClaim, _ bool, persist func() error) error {
				if strings.HasPrefix(tc.name, "released") {
					*claim = fixedMachine0LeaseKind.TerminalClaim(*claim, time.Now())
				}
				tc.mutate(claim)
				return persist()
			})
			if err != nil {
				t.Fatal(err)
			}
			configureMachine0CommandFixture(t, machine{}, "", "15m")
			for _, command := range []string{"inspect", "status", "stop"} {
				_, err := runSelectionCLI(command, "--provider", providerName, "--id", req.RequestedLeaseID)
				assertMachine0Exit(t, err, 4, "lease_id_conflict")
			}
			before := readFixedMachine0Claim(t, req.RequestedLeaseID)
			api.getFn = func(context.Context, string) (machine, error) {
				t.Fatal("unbound claim reached native lookup")
				return machine{}, nil
			}
			for _, request := range []ResolveRequest{{StatusOnly: true}, {ReleaseOnly: true}, {StatusOnly: true, ReadyProbe: true}, {Reclaim: true}} {
				request.ID = req.RequestedLeaseID
				_, err := b.Resolve(context.Background(), request)
				if err == nil || strings.Contains(err.Error(), "not found") {
					t.Fatalf("invalid or unresolved claim became absence/success: %v", err)
				}
			}
			err = b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: req.RequestedLeaseID}})
			if err == nil || strings.Contains(err.Error(), "not found") {
				t.Fatalf("invalid or unresolved release became absence/success: %v", err)
			}
			if strings.HasPrefix(tc.name, "released") {
				for _, policy := range []string{"destroy", "suspend"} {
					b.cfg.Machine0.ReleasePolicy = policy
					if _, err := b.RetainLeaseClaimAfterReleaseWithClaim(LeaseTarget{LeaseID: req.RequestedLeaseID}, before); err == nil {
						t.Fatal("invalid terminal passed shared retention")
					}
				}
			}
			if after := readFixedMachine0Claim(t, req.RequestedLeaseID); !reflect.DeepEqual(after, before) {
				t.Fatal("refusal changed durable claim")
			}
			if api.listCalls+len(api.created)+len(api.removed)+len(api.suspended) != 0 {
				t.Fatal("unbound claim reached native inventory/mutation")
			}
		})
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
