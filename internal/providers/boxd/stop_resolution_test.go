package boxd

import (
	"errors"
	"io"
	"path/filepath"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
	"github.com/openclaw/crabbox/internal/testutil"
)

func TestReleaseCanonicalIdentifierDoesNotFallBackToClaimSlug(t *testing.T) {
	b, fake := fixtureBackend(t)
	const missingID = "cbx_aaaaaaaaaaaa"
	lease, err := b.Acquire(t.Context(), core.AcquireRequest{RequestedSlug: missingID})
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.Resolve(t.Context(), core.ResolveRequest{ID: missingID, ReleaseOnly: true})
	if !errors.Is(err, shared.ErrStrictClaimMismatch) || err.Error() != shared.ErrStrictClaimMismatch.Error() {
		t.Fatalf("Resolve release-only err=%v", err)
	}
	if onlyClaim(t).LeaseID != lease.LeaseID || len(liveRows(fake)) != 1 || fake.count("DestroyVm vm-1") != 0 {
		t.Fatal("missing canonical ID affected the lookalike lease")
	}
}

func TestStopCLIClaimMismatchKeepsStrictDiagnostic(t *testing.T) {
	for _, scenario := range []string{"provider", "scope"} {
		t.Run(scenario, func(t *testing.T) {
			testutil.IsolateUserDirs(t)
			t.Setenv("CRABBOX_PROVIDER", "boxd")
			t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "config.yaml"))
			t.Chdir(t.TempDir())
			b, fake := fixtureBackend(t)
			lease := acquireFixture(t, b, false)
			args := []string{"stop", "--provider", "boxd", "--boxd-api-url", b.cfg.Boxd.APIURL, "--boxd-grpc-url", b.cfg.Boxd.GRPCURL, "--id", lease.LeaseID}
			if scenario == "scope" {
				args = append(args, "--boxd-org", "other-org")
			} else if err := core.WithDurableLeaseClaimLock(lease.LeaseID, func(claim *core.LeaseClaim, _ bool, persist func() error) error {
				claim.Provider = "other"
				return persist()
			}); err != nil {
				t.Fatal(err)
			}
			app := core.App{Stdout: io.Discard, Stderr: io.Discard}
			err := app.Run(t.Context(), args)
			if !errors.Is(err, shared.ErrStrictClaimMismatch) || err.Error() != shared.ErrStrictClaimMismatch.Error() {
				t.Fatalf("claim mismatch diagnostic changed: %v", err)
			}
			if onlyClaim(t).LeaseID != lease.LeaseID || len(liveRows(fake)) != 1 || fake.count("DestroyVm vm-1") != 0 {
				t.Fatal("mismatched stop affected the lease")
			}
		})
	}
}
