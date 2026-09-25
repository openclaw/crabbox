package boxd

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func TestStopCLIReplayAfterVerifiedAbsence(t *testing.T) {
	testutil.IsolateUserDirs(t)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "config.yaml"))
	t.Chdir(t.TempDir())
	b, fake := fixtureBackend(t)
	lease := acquireFixture(t, b, false)
	var stderr bytes.Buffer
	app := core.App{Stdout: io.Discard, Stderr: &stderr}
	args := []string{"stop", "--provider", "boxd", "--boxd-api-url", b.cfg.Boxd.APIURL, "--boxd-grpc-url", b.cfg.Boxd.GRPCURL, "--id", lease.LeaseID}
	if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if got, want := b.ReleaseLeaseMessage(lease), "verified absent lease="+lease.LeaseID+"; removed ownership claim"; got != want {
		t.Fatalf("stop output=%q want=%q", got, want)
	}
	assertNoClaims(t)
	if fake.count("DestroyVm vm-1") != 1 || len(liveRows(fake)) != 0 {
		t.Fatal("first stop did not confirm deletion")
	}
	fake.mu.Lock()
	calls := len(fake.calls)
	fake.mu.Unlock()
	stderr.Reset()
	err := app.Run(t.Context(), args)
	var exit core.ExitError
	if !errors.As(err, &exit) || exit.Code != 1 {
		t.Fatalf("missing receipt must retain explicit exit 1: %v", err)
	}
	if !strings.Contains(err.Error(), "lease "+lease.LeaseID+" has no local claim") ||
		!strings.Contains(err.Error(), "if an earlier stop verified absence, nothing remains to do") ||
		!strings.Contains(err.Error(), "check the ID, provider, and provider inventory") ||
		strings.Contains(err.Error(), "strict claim identifier mismatch") {
		t.Fatalf("replay diagnostic=%q", err)
	}
	assertNoClaims(t)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.calls) != calls || stderr.Len() != 0 {
		t.Fatalf("replay performed remote operations or reported success: calls=%v stderr=%q", fake.calls[calls:], stderr.String())
	}
}
