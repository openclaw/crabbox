package cli

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/openclaw/crabbox/internal/testutil"
)

func TestFixedLeaseTerminalIdentityRetention(t *testing.T) {
	for _, retain := range []bool{false, true} {
		name := "compact_default"
		if retain {
			name = "retained_identity"
		}
		t.Run(name, func(t *testing.T) {
			dirs := testutil.IsolateUserDirs(t)
			kind := FixedLeaseKind{ClaimProvider: "test-fixed-v1", IntentVersion: 1, Label: "test"}
			if retain {
				kind.TerminalIdentityLabels = []string{"owner", "fingerprint"}
			}
			const id = "cbx_abcdef123492"
			err := WithDurableLeaseClaimLock(id, func(c *LeaseClaim, _ bool, persist func() error) error {
				*c = LeaseClaim{
					LeaseID: id, Provider: kind.ClaimProvider, ProviderScope: "scope", Slug: "test-slug",
					CloudID: "resource", CloudNumericID: 42, CloudImmutableID: "immutable", RepoRoot: "/repository",
					SSHHost: "127.0.0.1", SSHPort: 22, TargetOS: "linux", IdleTimeoutSeconds: 60,
					Labels:            map[string]string{"owner": "owner", "fingerprint": "fingerprint", "state": "active"},
					FixedCreateIntent: &FixedCreateIntent{Version: 1, ProviderScope: "scope", Slug: "test-slug", Fingerprint: "fingerprint", State: "acquired", Attempt: map[string]string{"attempt": "active"}, FailedAttempts: []string{"failure"}},
				}
				return persist()
			})
			if err != nil {
				t.Fatal(err)
			}
			claim, err := ReadLeaseClaim(id)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dirs.StateHome, "crabbox", "claims", id+".json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			failure := errors.New("cleanup failed")
			if err := kind.FinalizeAfterCleanup(claim, func() error { return failure }); !errors.Is(err, failure) {
				t.Fatalf("cleanup error=%v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(before) != string(after) {
				t.Fatal("failed cleanup changed claim")
			}
			if err := kind.FinalizeAfterCleanup(claim, func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			terminal, err := ReadLeaseClaim(id)
			if err != nil {
				t.Fatal(err)
			}
			if err := kind.ValidateTerminalClaim(terminal, claim, id, nil); err != nil {
				t.Fatal(err)
			}
			if terminal.SSHHost != "" || terminal.SSHPort != 0 || terminal.TargetOS != "" || terminal.IdleTimeoutSeconds != 0 || len(terminal.FixedCreateIntent.Attempt)+len(terminal.FixedCreateIntent.FailedAttempts) != 0 {
				t.Fatal("terminal claim retained active state")
			}
			if retain {
				if terminal.CloudID != "resource" || terminal.CloudNumericID != 42 || terminal.CloudImmutableID != "immutable" || terminal.RepoRoot != "/repository" || !reflect.DeepEqual(terminal.Labels, map[string]string{"owner": "owner", "fingerprint": "fingerprint"}) {
					t.Fatal("terminal claim lost selected identity")
				}
				terminal.CloudID = "replacement"
				if err := kind.ValidateTerminalClaim(terminal, claim, id, nil); err == nil {
					t.Fatal("changed resource identity accepted")
				}
			} else if terminal.CloudID != "" || terminal.CloudNumericID != 0 || terminal.CloudImmutableID != "" || terminal.RepoRoot != "" || len(terminal.Labels) != 0 {
				t.Fatal("default compact behavior changed")
			}
		})
	}
}
