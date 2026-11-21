package islo

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"

	gosdk "github.com/islo-labs/go-sdk"
	core "github.com/openclaw/crabbox/internal/cli"
)

func setupIsloSSHClaimTest(t *testing.T, client *fakeIsloSyncClient) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	isolateIsloTestHome(t)
	t.Setenv("CRABBOX_ISLO_API_KEY", "test")
	root := t.TempDir()
	if out, err := exec.Command("git", "init", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	t.Chdir(root)
	t.Cleanup(swapNewIsloClient(client))
}

func runIsloClaimCommand(t *testing.T, args ...string) string {
	t.Helper()
	var stderr bytes.Buffer
	app := core.App{Stdout: &bytes.Buffer{}, Stderr: &stderr, Stdin: strings.NewReader("")}
	if err := app.Run(context.Background(), args); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, stderr.String())
	}
	return stderr.String()
}

func TestSSHDuringRunKeepsRunCleanup(t *testing.T) {
	client := &fakeIsloSyncClient{createName: isloTeardownName, createID: isloTestResourceID}
	setupIsloSSHClaimTest(t, client)
	sshCalls := 0
	client.execHook = func(req *gosdk.ExecRequest) {
		if strings.Contains(strings.Join(req.GetCommand(), " "), "workload-marker") {
			sshCalls++
			runIsloClaimCommand(t, "ssh", "--provider", "islo", "--id", isloTeardownLeaseID)
		}
	}
	runIsloClaimCommand(t, "run", "--provider", "islo", "--no-sync", "--", "printf", "workload-marker")
	if sshCalls != 1 || client.deleteCalls != 1 {
		t.Fatalf("ssh calls=%d deletes=%d, want 1 each", sshCalls, client.deleteCalls)
	}
	requireIsloClaimDropped(t, isloTeardownLeaseID)
}

func TestStopCreatorAdvisorySurvivesSSH(t *testing.T) {
	client := &fakeIsloSyncClient{createName: isloTeardownName, createID: isloTestResourceID, createdBy: "key-a", createdByEntity: "api_key"}
	setupIsloSSHClaimTest(t, client)
	runIsloClaimCommand(t, "run", "--provider", "islo", "--keep", "--no-sync", "--", "true")
	client.createdBy, client.createdByEntity = "key-b", "user"
	runIsloClaimCommand(t, "ssh", "--provider", "islo", "--id", isloTeardownLeaseID)
	claim, ok, err := resolveExactIsloLeaseClaim(isloTeardownLeaseID)
	if err != nil || !ok || claim.Labels[isloCreatedByLabel] != "key-a" || claim.Labels[isloCreatedByEntityLabel] != "api_key" {
		t.Fatalf("original attribution lost: claim=%+v ok=%t err=%v", claim, ok, err)
	}
	stderr := runIsloClaimCommand(t, "stop", "--provider", "islo", isloTeardownLeaseID)
	for _, want := range []string{"different creator attribution", "created_by is now", "created_by_entity is now"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stop stderr=%q, want %q", stderr, want)
		}
	}
	if client.deleteCalls != 1 {
		t.Fatalf("deletes=%d, want 1", client.deleteCalls)
	}
}

func TestIsloEndpointRefreshPreservesOnlyBoundResourceMetadata(t *testing.T) {
	claim := core.LeaseClaim{LeaseID: isloTeardownLeaseID, Provider: isloProvider, CloudID: isloTestResourceID, CloudImmutableID: isloTestResourceID,
		Labels: isloIdentity{ID: isloTestResourceID, Name: isloTeardownName, CreatedBy: "original", CreatedByEntity: "api_key"}.labels()}
	for _, change := range []string{"same", "resource", "immutable", "missing resource", "missing immutable", "provider", "legacy"} {
		t.Run(change, func(t *testing.T) {
			existing := claim
			provider := isloProvider
			server := core.Server{CloudID: claim.CloudID, ImmutableID: claim.CloudImmutableID, Labels: map[string]string{"state": "running", isloCreatedByLabel: "current"}}
			switch change {
			case "resource":
				server.CloudID = "replacement"
			case "immutable":
				server.ImmutableID = "replacement"
			case "missing resource":
				server.CloudID = ""
			case "missing immutable":
				server.ImmutableID = ""
			case "provider":
				provider = "other"
			case "legacy":
				existing.Labels = nil
			}
			got, err := (Provider{}).PrepareLeaseClaimEndpoint(existing, provider, "", server, false)
			if change != "same" && change != "legacy" {
				if err == nil {
					t.Fatal("refresh accepted a different resource")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Labels["state"] != "running" || server.Labels[isloCreatedByLabel] != "current" {
				t.Fatal("runtime labels lost or input mutated")
			}
			for _, key := range []string{isloSandboxNameLabel, isloCreatedByLabel, isloCreatedByEntityLabel} {
				if got.Labels[key] != existing.Labels[key] {
					t.Fatalf("changed acquisition label %s", key)
				}
			}
		})
	}
}
