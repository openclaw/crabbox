package azure

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type finalizeAzureClient struct {
	*fakeAzureClient
	verify func(context.Context, string, string) (core.LeaseRecoveryHold, error)
	calls  int
}

func (c *finalizeAzureClient) VerifyFailedLeaseHoldAbsent(ctx context.Context, id, slug string) (core.LeaseRecoveryHold, error) {
	c.calls++
	if c.verify != nil {
		return c.verify(ctx, id, slug)
	}
	return finalizeTestReceipt(id, slug, "finalized"), nil
}

func (c *finalizeAzureClient) InspectFailedLeaseHold(_ context.Context, expected core.Server) (core.LeaseRecoveryHold, error) {
	return finalizeTestReceipt(expected.Labels["lease"], expected.Labels["slug"], "held"), nil
}

func (c *finalizeAzureClient) InspectUnboundFailedLeaseHold(ctx context.Context, expected core.Server, _ core.AzureFixedCompanions) (core.LeaseRecoveryHold, error) {
	return c.InspectFailedLeaseHold(ctx, expected)
}

func (c *finalizeAzureClient) InspectClaimlessFailedLeaseHold(_ context.Context, id, slug string) (core.LeaseRecoveryHold, error) {
	return finalizeTestReceipt(id, slug, "held"), nil
}

func finalizeTestReceipt(id, slug, status string) core.LeaseRecoveryHold {
	name := core.LeaseProviderName(id, slug)
	prefix := "/subscriptions/test-sub/resourceGroups/rg/providers/"
	receipt := core.LeaseRecoveryHold{Schema: "crabbox.lease-hold.v1", Provider: "azure", LeaseID: id, Status: status, UnacceptedChanges: "unknown",
		Resources: []core.LeaseHeldResource{
			{Kind: "vm", ID: prefix + "Microsoft.Compute/virtualMachines/" + name, State: "absent"},
			{Kind: "nic", ID: prefix + "Microsoft.Network/networkInterfaces/" + name + "-nic", State: "absent"},
			{Kind: "public-ip", ID: prefix + "Microsoft.Network/publicIPAddresses/" + name + "-pip", State: "absent"},
			{Kind: "disk", ID: prefix + "Microsoft.Compute/disks/" + name + "-osdisk", State: "absent"},
			{Kind: "nsg", ID: prefix + "Microsoft.Network/networkSecurityGroups/" + name + "-q-nsg", State: "absent"},
		}}
	if status == "held" {
		receipt.Resources[3].State = "retained"
		receipt.Resources[3].ImmutableID = "held-disk"
	}
	return receipt
}

const finalizeTestID = "cbx_abcdef123491"
const finalizeTestSlug = "held-worker"

func finalizeTestBackend(t *testing.T, kind string) (*azureLeaseBackend, *finalizeAzureClient) {
	t.Helper()
	client := &finalizeAzureClient{fakeAzureClient: &fakeAzureClient{}}
	backend := fixedAzureTestBackend(t, client.fakeAzureClient)
	newAzureClient = func(context.Context, core.Config) (azureClient, error) { return client, nil }
	t.Setenv("CRABBOX_PROVIDER", "azure")
	t.Setenv("CRABBOX_COORDINATOR", "")
	if kind == "claimless" {
		if _, err := backend.HoldFailedLeaseWithSlug(t.Context(), finalizeTestID, finalizeTestSlug); err != nil {
			t.Fatal(err)
		}
		return backend, client
	}
	if kind == "unbound" {
		client.fixedCapacityErr = &azureFixedVMShortage{Code: "AllocationFailed", Err: errors.New("capacity unavailable")}
		client.fixedSettleErr = errors.New("cleanup uncertain")
	}
	_, err := backend.Acquire(t.Context(), core.AcquireRequest{RequestedLeaseID: finalizeTestID, RequestedSlug: finalizeTestSlug, Repo: core.Repo{Root: t.TempDir()}})
	if (err != nil) != (kind == "unbound") {
		t.Fatalf("acquire fixture: %v", err)
	}
	if _, err := backend.HoldFailedLease(t.Context(), finalizeTestID); err != nil {
		t.Fatal(err)
	}
	// Acquisition is fixture setup, not part of the read-only operation.
	client.deleted, client.plainDeletes, client.tagged = nil, nil, nil
	client.ownedExpected, client.cleanupExpected, client.createLeaseIDs = nil, nil, nil
	return backend, client
}

func readFinalizeTestClaim(t *testing.T) core.LeaseClaim {
	t.Helper()
	claim, err := core.ReadLeaseClaim(finalizeTestID)
	if err != nil {
		t.Fatal(err)
	}
	return claim
}

func assertFinalizeNoMutations(t *testing.T, client *finalizeAzureClient) {
	t.Helper()
	if len(client.deleted) != 0 || len(client.plainDeletes) != 0 || len(client.tagged) != 0 || len(client.ownedExpected) != 0 || len(client.cleanupExpected) != 0 || len(client.createLeaseIDs) != 0 {
		t.Fatal("finalization mutated cloud resources")
	}
}

func TestAzureHoldFinalizeCommandAndReplay(t *testing.T) {
	for _, kind := range []string{"bound", "unbound", "claimless"} {
		t.Run(kind, func(t *testing.T) {
			backend, client := finalizeTestBackend(t, kind)
			before := readFinalizeTestClaim(t)
			var output bytes.Buffer
			app := core.App{Stdout: &output, Stderr: io.Discard}
			args := []string{"hold", "--provider", "azure", "--id", finalizeTestID, "--finalize", "--json"}
			if err := app.Run(t.Context(), args); err != nil {
				t.Fatal(err)
			}
			want, _ := json.Marshal(finalizeTestReceipt(finalizeTestID, finalizeTestSlug, "finalized"))
			if !bytes.Equal(bytes.TrimSpace(output.Bytes()), want) {
				t.Fatalf("CLI receipt=%s want=%s", output.Bytes(), want)
			}
			stored := readFinalizeTestClaim(t)
			if stored.Provider != "azure-recovery-held-v1" || stored.RecoveryHold.Status != "finalized" ||
				!reflect.DeepEqual(stored.FixedCreateIntent, before.FixedCreateIntent) || stored.CloudID != before.CloudID || stored.CloudImmutableID != before.CloudImmutableID {
				t.Fatalf("terminal claim lost custody: %+v", stored)
			}
			client.verify = func(context.Context, string, string) (core.LeaseRecoveryHold, error) {
				t.Fatal("terminal retry must not re-observe or recreate the hold")
				return core.LeaseRecoveryHold{}, nil
			}
			restarted := NewAzureLeaseBackend(Provider{}.Spec(), backend.Cfg, backend.RT).(*azureLeaseBackend)
			if got, err := restarted.FinalizeFailedLeaseHold(t.Context(), finalizeTestID); err != nil || !reflect.DeepEqual(got, *stored.RecoveryHold) {
				t.Fatalf("restart replay: %+v %v", got, err)
			}
			output.Reset()
			if err := app.Run(t.Context(), args[:len(args)-1]); err != nil || !strings.HasPrefix(output.String(), "finalized lease=") {
				t.Fatalf("text receipt=%q err=%v", output.String(), err)
			}
			output.Reset()
			if err := app.Run(t.Context(), []string{"hold", "--provider", "azure", "--id", finalizeTestID, "--json"}); err != nil || !bytes.Equal(bytes.TrimSpace(output.Bytes()), want) {
				t.Fatalf("ordinary hold recreated terminal hold: %s %v", output.Bytes(), err)
			}
			if _, err := restarted.Acquire(t.Context(), core.AcquireRequest{RequestedLeaseID: finalizeTestID, RequestedSlug: finalizeTestSlug, Repo: core.Repo{Root: t.TempDir()}}); err == nil {
				t.Fatal("terminal ID was reused")
			}
			for _, force := range []bool{false, true} {
				stop := []string{"stop", "--provider", "azure", "--id", finalizeTestID}
				if force {
					stop = append(stop, "--force")
				}
				if err := app.Run(t.Context(), stop); err == nil {
					t.Fatal("terminal held claim allowed stop")
				}
			}
			if err := restarted.Cleanup(t.Context(), core.CleanupRequest{}); err != nil {
				t.Fatal(err)
			}
			legacy := stored
			legacy.RecoveryHold = nil
			if err := validateExactAzureClaim(legacy, azureServerFromClaim(legacy), finalizeTestID, azureTestClaimScope); err == nil {
				t.Fatal("older writer accepted held-provider marker")
			}
			if got := readFinalizeTestClaim(t); !reflect.DeepEqual(got, stored) || client.calls != 1 {
				t.Fatalf("replay changed durable receipt: calls=%d", client.calls)
			}
			assertFinalizeNoMutations(t, client)
		})
	}
}

func TestAzureHoldFinalizeFreshProcess(t *testing.T) {
	if os.Getenv("CRABBOX_TEST_HOLD_FINALIZE_CHILD") == "1" {
		// No verifier capability: replay must rely on the durable terminal record.
		newAzureClient = func(context.Context, core.Config) (azureClient, error) { return &fakeAzureClient{}, nil }
		app := core.App{Stdout: os.Stdout, Stderr: os.Stderr}
		if err := app.Run(t.Context(), []string{"hold", "--provider", "azure", "--id", finalizeTestID, "--finalize", "--json"}); err != nil {
			t.Fatal(err)
		}
		return
	}
	backend, _ := finalizeTestBackend(t, "claimless")
	got, err := backend.FinalizeFailedLeaseHold(t.Context(), finalizeTestID)
	if err != nil {
		t.Fatal(err)
	}
	before := readFinalizeTestClaim(t)
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestAzureHoldFinalizeFreshProcess$")
	cmd.Env = append(os.Environ(), "CRABBOX_TEST_HOLD_FINALIZE_CHILD=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fresh process: %s %v", output, err)
	}
	want, _ := json.Marshal(got)
	if !bytes.Equal(bytes.SplitN(output, []byte("\n"), 2)[0], want) {
		t.Fatalf("fresh receipt differs: %s", output)
	}
	if !reflect.DeepEqual(readFinalizeTestClaim(t), before) {
		t.Fatal("fresh process rewrote the tombstone")
	}
}

func TestAzureHoldFinalizeRejectsInvalidClaim(t *testing.T) {
	cases := map[string]func(*core.LeaseClaim){
		"provider":                   func(c *core.LeaseClaim) { c.Provider = "azure" },
		"scope":                      func(c *core.LeaseClaim) { c.ProviderScope = "subscription:other|resource-group:rg" },
		"empty scope":                func(c *core.LeaseClaim) { c.ProviderScope = "" },
		"missing hold":               func(c *core.LeaseClaim) { c.RecoveryHold = nil },
		"slug":                       func(c *core.LeaseClaim) { c.Slug = "other" },
		"missing slug":               func(c *core.LeaseClaim) { c.Slug = "" },
		"cloud ID":                   func(c *core.LeaseClaim) { c.CloudID = "other" },
		"schema":                     func(c *core.LeaseClaim) { c.RecoveryHold.Schema = "unknown" },
		"receipt lease":              func(c *core.LeaseClaim) { c.RecoveryHold.LeaseID = "cbx_abcdef123499" },
		"receipt provider":           func(c *core.LeaseClaim) { c.RecoveryHold.Provider = "other" },
		"receipt status":             func(c *core.LeaseClaim) { c.RecoveryHold.Status = "released" },
		"receipt changes":            func(c *core.LeaseClaim) { c.RecoveryHold.UnacceptedChanges = "none" },
		"missing resource":           func(c *core.LeaseClaim) { c.RecoveryHold.Resources = c.RecoveryHold.Resources[:4] },
		"wrong resource":             func(c *core.LeaseClaim) { c.RecoveryHold.Resources[3].ID += "-other" },
		"retained VM":                func(c *core.LeaseClaim) { c.RecoveryHold.Resources[0].State = "retained" },
		"unidentified retained disk": func(c *core.LeaseClaim) { c.RecoveryHold.Resources[3].ImmutableID = "" },
		"invalid terminal":           func(c *core.LeaseClaim) { c.RecoveryHold.Status = "finalized" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			backend, client := finalizeTestBackend(t, "claimless")
			if err := core.WithDurableLeaseClaimLock(finalizeTestID, func(c *core.LeaseClaim, _ bool, persist func() error) error { mutate(c); return persist() }); err != nil {
				t.Fatal(err)
			}
			before := readFinalizeTestClaim(t)
			if _, err := backend.FinalizeFailedLeaseHold(t.Context(), finalizeTestID); err == nil {
				t.Fatal("invalid claim finalized")
			}
			if client.calls != 0 || !reflect.DeepEqual(readFinalizeTestClaim(t), before) {
				t.Fatal("invalid claim changed or invoked verifier")
			}
			assertFinalizeNoMutations(t, client)
		})
	}
	for _, field := range []string{"state", "scope", "slug", "nonce", "name", "fingerprint", "version"} {
		t.Run("fixed intent "+field, func(t *testing.T) {
			backend, client := finalizeTestBackend(t, "bound")
			if err := core.WithDurableLeaseClaimLock(finalizeTestID, func(c *core.LeaseClaim, _ bool, persist func() error) error {
				i := c.FixedCreateIntent
				switch field {
				case "state":
					i.State = "prepared"
				case "scope":
					i.ProviderScope = "other"
				case "slug":
					i.Slug = "other"
				case "nonce":
					i.Attempt["nonce"] = "other"
				case "name":
					i.Attempt["name"] = "other"
				case "fingerprint":
					i.Fingerprint = "bad"
				case "version":
					i.Version++
				}
				return persist()
			}); err != nil {
				t.Fatal(err)
			}
			before := readFinalizeTestClaim(t)
			if _, err := backend.FinalizeFailedLeaseHold(t.Context(), finalizeTestID); err == nil {
				t.Fatal("invalid fixed intent finalized")
			}
			if client.calls != 0 || !reflect.DeepEqual(readFinalizeTestClaim(t), before) {
				t.Fatal("invalid fixed claim changed")
			}
		})
	}
}

func TestAzureHoldFinalizeRetainsHoldOnFailedVerification(t *testing.T) {
	mutations := map[string]func(*core.LeaseRecoveryHold){
		"schema":            func(r *core.LeaseRecoveryHold) { r.Schema = "unknown" },
		"lease":             func(r *core.LeaseRecoveryHold) { r.LeaseID = "cbx_abcdef123499" },
		"provider":          func(r *core.LeaseRecoveryHold) { r.Provider = "other" },
		"status":            func(r *core.LeaseRecoveryHold) { r.Status = "held" },
		"salvage assertion": func(r *core.LeaseRecoveryHold) { r.UnacceptedChanges = "none" },
		"missing resource":  func(r *core.LeaseRecoveryHold) { r.Resources = r.Resources[:4] },
		"extra resource":    func(r *core.LeaseRecoveryHold) { r.Resources = append(r.Resources, r.Resources[0]) },
		"wrong order":       func(r *core.LeaseRecoveryHold) { r.Resources[0], r.Resources[1] = r.Resources[1], r.Resources[0] },
		"wrong resource":    func(r *core.LeaseRecoveryHold) { r.Resources[3].ID += "-other" },
		"unknown state":     func(r *core.LeaseRecoveryHold) { r.Resources[3].State = "unknown" },
	}
	for index, kind := range []string{"vm", "nic", "public-ip", "disk", "nsg"} {
		mutations[kind+" survives"] = func(r *core.LeaseRecoveryHold) {
			r.Resources[index].State = "retained"
			r.Resources[index].ImmutableID = "surviving"
		}
	}
	for _, name := range []string{"read failure", "ambiguous absence", "cancelled", "unsupported client"} {
		mutations[name] = nil
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			backend, client := finalizeTestBackend(t, "claimless")
			before := readFinalizeTestClaim(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			client.verify = func(context.Context, string, string) (core.LeaseRecoveryHold, error) {
				r := finalizeTestReceipt(finalizeTestID, finalizeTestSlug, "finalized")
				if mutate != nil {
					mutate(&r)
				}
				switch name {
				case "read failure":
					return r, errors.New("read failed")
				case "ambiguous absence":
					return r, errors.New("inventory incomplete")
				case "cancelled":
					cancel()
				}
				return r, nil
			}
			if name == "unsupported client" {
				newAzureClient = func(context.Context, core.Config) (azureClient, error) { return client.fakeAzureClient, nil }
			}
			if got, err := backend.FinalizeFailedLeaseHold(ctx, finalizeTestID); err == nil || got.Status == "finalized" {
				t.Fatalf("unproven hold finalized: %+v %v", got, err)
			}
			if !reflect.DeepEqual(readFinalizeTestClaim(t), before) {
				t.Fatal("failed verification changed hold")
			}
			assertFinalizeNoMutations(t, client)
		})
	}
}

func TestAzureHoldFinalizeRequiresClaimAndScopeOnReplay(t *testing.T) {
	backend, client := finalizeTestBackend(t, "claimless")
	if _, err := backend.FinalizeFailedLeaseHold(t.Context(), "cbx_abcdef123499"); err == nil {
		t.Fatal("missing hold finalized")
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence("cbx_abcdef123499"); err != nil || exists {
		t.Fatalf("missing claim created: %v", err)
	}
	if _, err := backend.FinalizeFailedLeaseHold(t.Context(), finalizeTestID); err != nil {
		t.Fatal(err)
	}
	before := readFinalizeTestClaim(t)
	client.claimScope = "subscription:other|resource-group:rg"
	if _, err := backend.FinalizeFailedLeaseHold(t.Context(), finalizeTestID); err == nil {
		t.Fatal("terminal replay accepted wrong scope")
	}
	if !reflect.DeepEqual(before, readFinalizeTestClaim(t)) || client.calls != 1 {
		t.Fatal("wrong scope altered terminal claim")
	}
}

func TestAzureHoldFinalizeHoldsClaimLockAndHonorsCancellation(t *testing.T) {
	backend, client := finalizeTestBackend(t, "claimless")
	before := readFinalizeTestClaim(t)
	ctx, cancel := context.WithCancel(t.Context())
	client.verify = func(context.Context, string, string) (core.LeaseRecoveryHold, error) {
		lockCtx, lockCancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
		defer lockCancel()
		err := core.WithDurableLeaseClaimLockContext(lockCtx, finalizeTestID, func(*core.LeaseClaim, bool, func() error) error {
			t.Error("verifier ran outside claim lock")
			return nil
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("lock wait: %v", err)
		}
		cancel()
		return finalizeTestReceipt(finalizeTestID, finalizeTestSlug, "finalized"), nil
	}
	defer cancel()
	if _, err := backend.FinalizeFailedLeaseHold(ctx, finalizeTestID); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	if !reflect.DeepEqual(before, readFinalizeTestClaim(t)) {
		t.Fatal("cancelled finalization changed claim")
	}
}

func TestAzureHoldFinalizePersistenceFailure(t *testing.T) {
	backend, client := finalizeTestBackend(t, "claimless")
	before := readFinalizeTestClaim(t)
	state, err := core.CrabboxStateDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(state, "claims")
	client.verify = func(context.Context, string, string) (core.LeaseRecoveryHold, error) {
		if err := os.Chmod(dir, 0500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
		// Some platforms or privileged users cannot enforce a read-only directory.
		if f, err := os.CreateTemp(dir, "permission-check-"); err == nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
			t.Skip("filesystem does not enforce directory write permissions")
		}
		return finalizeTestReceipt(finalizeTestID, finalizeTestSlug, "finalized"), nil
	}
	if got, err := backend.FinalizeFailedLeaseHold(t.Context(), finalizeTestID); err == nil || got.Status != "" {
		t.Fatalf("failed persistence returned success: %+v %v", got, err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, readFinalizeTestClaim(t)) {
		t.Fatal("persistence failure lost original hold")
	}
	client.verify = nil
	if _, err := backend.FinalizeFailedLeaseHold(t.Context(), finalizeTestID); err != nil {
		t.Fatal(err)
	}
	assertFinalizeNoMutations(t, client)
}

type noHoldFinalizeProvider struct{}

func (noHoldFinalizeProvider) RegisterFlags(*flag.FlagSet, core.Config) any { return nil }
func (noHoldFinalizeProvider) Spec() core.ProviderSpec {
	return core.ProviderSpec{Name: "test-hold-no-finalize", Kind: core.ProviderKindSSHLease, Coordinator: core.CoordinatorNever}
}
func (p noHoldFinalizeProvider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return &shared.DirectSSHBackend{SpecValue: p.Spec(), Cfg: cfg, RT: rt}, nil
}
func (noHoldFinalizeProvider) ApplyFlags(*core.Config, *flag.FlagSet, any) error { return nil }

func TestAzureHoldFinalizeUnsupportedFlagShapes(t *testing.T) {
	_, client := finalizeTestBackend(t, "claimless")
	before := readFinalizeTestClaim(t)
	core.RegisterProvider(noHoldFinalizeProvider{})
	app := core.App{Stdout: io.Discard, Stderr: io.Discard}
	if err := app.Run(t.Context(), []string{"hold", "--provider", "test-hold-no-finalize", "--id", finalizeTestID, "--finalize"}); err == nil || !strings.Contains(err.Error(), "does not support failed-lease hold finalization") {
		t.Fatalf("unsupported provider: %v", err)
	}
	base := []string{"hold", "--provider", "azure", "--id", finalizeTestID, "--finalize"}
	for _, args := range [][]string{
		{"hold", "--id", finalizeTestID, "--finalize"},
		{"hold", "--provider", "azure", "--finalize"},
		{"hold", "--provider", "azure", "--id", "slug", "--finalize"},
		append(append([]string{}, base...), "--slug", finalizeTestSlug),
		append(append([]string{}, base...), "--slug="),
		append(append([]string{}, base...), "extra"),
		append(append([]string{}, base...), "--force"),
		append(append([]string{}, base...), "--finalize=invalid"),
	} {
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			if err := app.Run(t.Context(), args); err == nil {
				t.Fatal("invalid flags accepted")
			}
		})
	}
	if client.calls != 0 || !reflect.DeepEqual(before, readFinalizeTestClaim(t)) {
		t.Fatal("invalid flags invoked finalization")
	}
}
