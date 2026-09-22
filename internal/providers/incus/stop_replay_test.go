package incus

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func incusStopFixture(t *testing.T) (*fakeClient, core.LeaseTarget, core.App) {
	t.Helper()
	b, fake, req := lifecycleFixture(t)
	testutil.IsolateUserDirs(t)
	t.Setenv("CRABBOX_PROVIDER", "incus")
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "config.yaml"))
	t.Chdir(req.Repo.Root)
	lease, err := b.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return fake, lease, core.App{Stdout: io.Discard, Stderr: io.Discard}
}

func TestIncusStopCLIDoesNotReplayTerminalSlugOverLiveLease(t *testing.T) {
	fake, lease, app := incusStopFixture(t)
	if err := app.Run(context.Background(), []string{"stop", "--provider", "incus", lease.LeaseID}); err != nil {
		t.Fatal(err)
	}
	terminal, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	// Model a retained receipt restored after another lease acquired the same slug.
	if err := core.RemoveLeaseClaimIfUnchanged(lease.LeaseID, terminal); err != nil {
		t.Fatal(err)
	}
	b := newBackend(Provider{}.Spec(), core.BaseConfig(), core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	next, err := b.Acquire(context.Background(), core.AcquireRequest{
		RequestedLeaseID: "cbx_ffffffffffff", RequestedSlug: terminal.Slug,
		Repo: core.Repo{Root: t.TempDir()}, Keep: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := core.WithDurableLeaseClaimLock(lease.LeaseID, func(claim *core.LeaseClaim, _ bool, persist func() error) error {
		*claim = terminal
		return persist()
	}); err != nil {
		t.Fatal(err)
	}
	terminal, err = core.ReadLeaseClaim(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Run(context.Background(), []string{"stop", "--provider", "incus", lease.LeaseID}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) != 1 || fake.instances[next.Server.Name] == nil {
		t.Fatal("exact old lease ID touched the new live lease")
	}
	if err := app.Run(context.Background(), []string{"stop", "--provider", "incus", terminal.Slug}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) != 2 || fake.deleted[1] != next.Server.Name {
		t.Fatalf("slug did not stop the current live lease: deleted=%v", fake.deleted)
	}
	after, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil || !reflect.DeepEqual(terminal, after) {
		t.Fatal("slug stop changed the old terminal receipt")
	}
}

func TestIncusStopCLIRejectsUnprovenRelease(t *testing.T) {
	for _, scenario := range []string{"scope-endpoint", "scope-project", "scope-certificate", "malformed-terminal", "missing-claim", "lookup-failure", "unmanaged-instance"} {
		t.Run(scenario, func(t *testing.T) {
			fake, lease, app := incusStopFixture(t)
			args := []string{"stop", "--provider", "incus", lease.LeaseID}
			if strings.HasPrefix(scenario, "scope-") || scenario == "malformed-terminal" {
				if err := app.Run(context.Background(), args); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "scope-endpoint", "scope-project", "scope-certificate":
				fake.identity, _ = fake.Identity()
				switch scenario {
				case "scope-endpoint":
					fake.identity.Endpoint = "unix:/other/incus.socket"
				case "scope-project":
					fake.identity.Project = "other"
				case "scope-certificate":
					fake.identity.Certificate = "other-daemon"
				}
			case "malformed-terminal":
				if err := core.WithDurableLeaseClaimLock(lease.LeaseID, func(claim *core.LeaseClaim, _ bool, persist func() error) error {
					claim.FixedCreateIntent.Version++
					return persist()
				}); err != nil {
					t.Fatal(err)
				}
			case "missing-claim":
				claim, err := core.ReadLeaseClaim(lease.LeaseID)
				if err != nil {
					t.Fatal(err)
				}
				if err := core.RemoveLeaseClaimIfUnchanged(lease.LeaseID, claim); err != nil {
					t.Fatal(err)
				}
			case "lookup-failure":
				fake.getErr = errors.New("provider lookup unavailable")
			case "unmanaged-instance":
				delete(fake.instances[lease.Server.Name].Config, labelKey("crabbox"))
			}
			before, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			deletes := len(fake.deleted)
			if err := app.Run(context.Background(), args); err == nil {
				t.Fatal("unproven release reported success")
			}
			after, stillExists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
			if err != nil || exists != stillExists || !reflect.DeepEqual(before, after) || len(fake.deleted) != deletes {
				t.Fatalf("failed stop changed custody: claim=%+v deletes=%v err=%v", after, fake.deleted, err)
			}
		})
	}
}

func TestIncusStopCLIReconcilesKnownAbsentInstance(t *testing.T) {
	for _, deleting := range []bool{false, true} {
		t.Run(map[bool]string{false: "acquired", true: "deleting"}[deleting], func(t *testing.T) {
			fake, lease, app := incusStopFixture(t)
			args := []string{"stop", "--provider", "incus", lease.LeaseID}
			if deleting {
				fake.lostDeleteReply = errors.New("delete reply lost")
				if err := app.Run(context.Background(), args); err == nil {
					t.Fatal("lost delete reply unexpectedly succeeded")
				}
				fake.lostDeleteReply = nil
			} else if err := fake.DeleteInstance(lease.Server.Name); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := app.Run(context.Background(), args); err != nil {
					t.Fatalf("reconcile/replay: %v", err)
				}
			}
			claim, err := core.ReadLeaseClaim(lease.LeaseID)
			if err != nil || claim.FixedCreateIntent.State != "released" || len(fake.deleted) != 1 {
				t.Fatalf("claim=%+v deletes=%v err=%v", claim, fake.deleted, err)
			}
		})
	}
}

func TestIncusStopCLIReattestsTerminalClaimAtRelease(t *testing.T) {
	for _, scenario := range []string{"malformed-terminal", "changed-scope"} {
		t.Run(scenario, func(t *testing.T) {
			fake, lease, app := incusStopFixture(t)
			args := []string{"stop", "--provider", "incus", lease.LeaseID}
			if err := app.Run(context.Background(), args); err != nil {
				t.Fatal(err)
			}
			calls := 0
			newClient = func(core.Config) (instanceClient, error) {
				calls++
				if calls == 2 {
					// Change custody after Resolve, before the release owner re-reads it.
					if scenario == "changed-scope" {
						fake.identity, _ = fake.Identity()
						fake.identity.Project = "other"
					} else if err := core.WithDurableLeaseClaimLock(lease.LeaseID, func(claim *core.LeaseClaim, _ bool, persist func() error) error {
						claim.FixedCreateIntent.Fingerprint = ""
						return persist()
					}); err != nil {
						t.Fatal(err)
					}
				}
				return fake, nil
			}
			if err := app.Run(context.Background(), args); err == nil || calls != 2 || len(fake.deleted) != 1 {
				t.Fatalf("release did not re-attest: calls=%d deletes=%v err=%v", calls, fake.deleted, err)
			}
		})
	}
}

func TestIncusStopCLIReplaysReleasedClaim(t *testing.T) {
	fake, lease, app := incusStopFixture(t)
	if err := app.Run(context.Background(), []string{"stop", "--provider", "incus", lease.LeaseID}); err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil || claim.FixedCreateIntent == nil || claim.FixedCreateIntent.State != "released" {
		t.Fatalf("terminal claim=%+v err=%v", claim, err)
	}
	fake.listErr = errors.New("terminal replay must not need instance inventory")
	fake.getErr = errors.New("terminal replay must not look up an instance")
	var stderr bytes.Buffer
	app.Stderr = &stderr
	for _, args := range [][]string{
		{"stop", "--provider", "incus", lease.LeaseID},
		{"stop", "--provider", "incus", "--incus-delete-on-release=false", lease.LeaseID},
	} {
		stderr.Reset()
		if err := app.Run(context.Background(), args); err != nil {
			t.Fatalf("replay %v: %v", args, err)
		}
		if got, want := stderr.String(), "deleted lease="+lease.LeaseID+" instance=-\n"; got != want {
			t.Fatalf("replay %v output=%q want=%q", args, got, want)
		}
	}
	if len(fake.deleted) != 1 || len(fake.instances) != 0 {
		t.Fatalf("replay changed remote resources: deleted=%v instances=%v", fake.deleted, fake.instances)
	}
	after, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil || !reflect.DeepEqual(claim, after) {
		t.Fatalf("replay changed terminal receipt: claim=%+v err=%v", after, err)
	}
}

func TestIncusStopCLIReportsRetainedInstance(t *testing.T) {
	fake, lease, app := incusStopFixture(t)
	var stderr bytes.Buffer
	app.Stderr = &stderr
	for range 2 {
		stderr.Reset()
		if err := app.Run(context.Background(), []string{"stop", "--provider", "incus", "--incus-delete-on-release=false", lease.LeaseID}); err != nil {
			t.Fatal(err)
		}
		if got, want := stderr.String(), "stopped lease="+lease.LeaseID+" instance="+lease.Server.Name+" retained=true\n"; got != want {
			t.Fatalf("retained stop output=%q want=%q", got, want)
		}
	}
	claim, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil || claim.FixedCreateIntent == nil || claim.FixedCreateIntent.State != "acquired" || len(fake.deleted) != 0 || fake.instances[lease.Server.Name] == nil {
		t.Fatalf("retained stop changed custody: claim=%+v deletes=%v err=%v", claim, fake.deleted, err)
	}
}
