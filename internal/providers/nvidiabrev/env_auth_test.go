package nvidiabrev

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestNvidiaBrevEnvironmentKeyOverridesSavedOrganization(t *testing.T) {
	for _, saved := range []bool{false, true} {
		t.Run(map[bool]string{false: "headless", true: "stale saved credentials"}[saved], func(t *testing.T) {
			_, home := isolateNvidiaBrevState(t)
			if !saved {
				if err := os.Remove(filepath.Join(home, ".brev", "credentials.json")); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("BREV_API_KEY", "bak-test-env-key")
			runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{{args: "org ls", stdout: "Your organizations:\n NAME       ID\n * Test Team org-env\n"}}}
			client, err := newBrevClient(core.Config{}, core.Runtime{Exec: runner})
			if err != nil {
				t.Fatal(err)
			}
			org, err := client.activeOrg(context.Background())
			if err != nil || org.ID != "org-env" || org.Name != "Test Team" {
				t.Fatalf("effective organization=%+v err=%v", org, err)
			}
			if !slices.Contains(runner.calls[0].Env, "NO_COLOR=1") || strings.Contains(runner.joinedCalls(), "bak-test-env-key") {
				t.Fatal("organization lookup must suppress color and keep credentials off argv")
			}
		})
	}
}

func TestNvidiaBrevEnvironmentKeyRetainsLegacyMisboundClaim(t *testing.T) {
	state, _ := isolateNvidiaBrevState(t)
	t.Setenv("BREV_API_KEY", "bak-test-env-key")
	// Older releases selected saved credentials even while this environment key
	// made Brev operate in another organization. Preserve that upgrade state.
	saved, ok, err := readLocalEffectiveBrevOrg()
	if err != nil || !ok || saved.ID != "org-test" {
		t.Fatalf("legacy saved organization=%+v exists=%v err=%v", saved, ok, err)
	}
	leaseID := "cbx_111122223333"
	server := workspaceToServer(core.Config{}, brevWorkspace{ID: "ws-saved", Name: "crabbox-saved-111122223333", Status: "RUNNING"}, leaseID, "saved", true)
	if err := claimTestNvidiaBrevLeaseTargetForRepoConfig(leaseID, "saved", core.Config{Provider: providerName}, server, core.SSHTarget{}, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	claimPath := filepath.Join(state, "crabbox", "claims", leaseID+".json")
	before, err := os.ReadFile(claimPath)
	if err != nil {
		t.Fatal(err)
	}
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{{args: "org ls", stdout: "NAME ID\n* Environment org-env\n"}}}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), core.Config{}, core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	err = backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: leaseID, Server: server}})
	if err == nil || !strings.Contains(err.Error(), "active Brev organization changed") {
		t.Fatalf("cross-organization release error=%v", err)
	}
	if runner.joinedCalls() != "org ls" {
		t.Fatalf("cross-organization request reached provider lifecycle: %s", runner.joinedCalls())
	}
	after, readErr := os.ReadFile(claimPath)
	if readErr != nil || !bytes.Equal(before, after) {
		t.Fatalf("legacy claim was changed or removed: %v", readErr)
	}
	if !strings.Contains(err.Error(), "BREV_API_KEY overrides") {
		t.Fatalf("recovery hint ignores effective credentials: %v", err)
	}
}

func TestNvidiaBrevEnvironmentKeyLookupDoesNotFallBack(t *testing.T) {
	for _, response := range []scriptedBrevResponse{
		{args: "org ls", err: errors.New("invalid API key")},
		{args: "org ls", stdout: "NAME ID\nSaved org-saved\n"},
		{args: "org ls", stdout: "NAME ID\n* MissingID\n"},
		{args: "org ls", stdout: "NAME ID\n* One org-one\n* Two org-two\n"},
	} {
		isolateNvidiaBrevState(t)
		t.Setenv("BREV_API_KEY", "bak-test-env-key")
		runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{response}}
		client, err := newBrevClient(core.Config{}, core.Runtime{Exec: runner})
		if err != nil {
			t.Fatal(err)
		}
		if org, err := client.activeOrg(context.Background()); err == nil || org.ID != "" {
			t.Fatalf("invalid environment identity fell back: org=%+v err=%v", org, err)
		}
		if runner.joinedCalls() != "org ls" {
			t.Fatalf("unexpected fallback: %s", runner.joinedCalls())
		}
	}
}
