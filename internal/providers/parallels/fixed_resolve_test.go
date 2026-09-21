package parallels

import (
	"context"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestParallelsFixedResolveAttestsBeforeGuestIO(t *testing.T) {
	for _, drift := range []string{"host", "name", "directory"} {
		for _, statusOnly := range []bool{false, true} {
			t.Run(drift+map[bool]string{false: "/reuse", true: "/status"}[statusOnly], func(t *testing.T) {
				backend, runner, req := fixedParallelsFixture(t)
				lease, err := backend.Acquire(context.Background(), req)
				if err != nil {
					t.Fatal(err)
				}
				runner.mu.Lock()
				runner.execCalls = nil
				if drift == "host" {
					runner.serverID = "replacement-server"
				}
				for i := range runner.vms {
					if runner.vms[i].ID != lease.Server.CloudID {
						continue
					}
					if drift == "name" {
						runner.vms[i].Name += "-renamed"
					}
					if drift == "directory" {
						runner.vms[i].Home = "/foreign/clone.pvm/"
					}
				}
				runner.mu.Unlock()
				_, err = backend.Resolve(context.Background(), core.ResolveRequest{ID: lease.LeaseID, Repo: req.Repo, StatusOnly: statusOnly})
				if err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
					t.Errorf("resolve err=%v, want identity conflict", err)
				}
				runner.mu.Lock()
				defer runner.mu.Unlock()
				if len(runner.execCalls) != 0 {
					t.Errorf("unattested resolve performed guest I/O: %v", runner.execCalls)
				}
			})
		}
	}
}

func TestParallelsFixedResolveFollowsRenamedFleetEntry(t *testing.T) {
	backend, _, req := fixedParallelsFixture(t)
	backend.Cfg.Parallels.SelectedHost = "old-entry"
	lease, err := backend.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	backend.Cfg.Parallels.SelectedHost = "new-entry"
	resolved, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: lease.LeaseID, Repo: req.Repo})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Server.CloudID != lease.Server.CloudID {
		t.Fatalf("resolved wrong VM: %+v", resolved)
	}
}

func TestParallelsFixedReleaseReportsTerminalOutcome(t *testing.T) {
	backend, _, req := fixedParallelsFixture(t)
	lease, err := backend.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		outcome, err := backend.ReleaseLeaseWithOutcome(context.Background(), core.ReleaseLeaseRequest{Lease: lease})
		if err != nil {
			t.Fatal(err)
		}
		if !outcome.Terminal {
			t.Fatal("successful fixed release did not report terminal outcome")
		}
	}
}
