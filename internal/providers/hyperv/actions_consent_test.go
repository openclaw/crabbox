package hyperv

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

// After `actions hydrate --github-runner --repo` records workspace consent on a
// Hyper-V claim, the next command resolves the lease with repository context and
// publishes it. That publication must keep the consent for the same VM. A VM
// recreated under the same name (for example from the hydrated guest's disk), a
// legacy claim that never recorded the VM Id, and --reclaim must all revoke it,
// and a VM whose Id cannot be read must not be published at all.
func TestResolvedHyperVLeaseKeepsActionsConsentOnlyForTheSameVM(t *testing.T) {
	testutil.IsolateUserDirs(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })

	const vmID = "6f1f2a64-31c7-4f3e-9a0d-2b8f5c1d7e90"
	const replacementID = "0c4e9b7d-58a2-4f61-8d3e-7a1b2c3d4e5f"
	liveID := vmID
	newVM := regexp.MustCompile(`New-VM -Name '([^']+)'`)
	var vmName string
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.onRun = func(req core.LocalCommandRequest) {
		if len(req.Args) == 0 {
			return
		}
		if m := newVM.FindStringSubmatch(req.Args[len(req.Args)-1]); m != nil {
			vmName = m[1]
		}
	}
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if len(req.Args) == 0 {
			return core.LocalCommandResult{}, nil, false
		}
		script := req.Args[len(req.Args)-1]
		switch {
		case strings.Contains(script, "Get-VMNetworkAdapter") && strings.Contains(script, "IPAddresses"):
			return core.LocalCommandResult{Stdout: `["192.0.2.10"]`}, nil, true
		case strings.Contains(script, "Get-VM") && strings.Contains(script, "ConvertTo-Json") && vmName != "":
			// Like Get-VM, report the Id only when the query selects it.
			vm := fmt.Sprintf(`{"Name":%q,"State":2}`, vmName)
			if strings.Contains(script, "Name='Id'") && liveID != "" {
				vm = fmt.Sprintf(`{"Name":%q,"State":2,"Id":%q}`, vmName, liveID)
			}
			if strings.Contains(script, "-like 'crabbox-*'") {
				return core.LocalCommandResult{Stdout: "[" + vm + "]"}, nil, true
			}
			return core.LocalCommandResult{Stdout: vm}, nil, true
		}
		return core.LocalCommandResult{}, nil, false
	}
	b := testBackend(runner)
	b.sshReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }
	repo := core.Repo{Root: t.TempDir()}

	lease, err := b.Acquire(t.Context(), core.AcquireRequest{Repo: repo, RequestedSlug: "consent", Keep: true})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	acquired, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if acquired.CloudImmutableID != vmID {
		t.Errorf("acquired claim CloudImmutableID=%q, want the VM Id %q that List reports", acquired.CloudImmutableID, vmID)
	}

	// What `actions hydrate --github-runner --repo` records after verifying the workspace.
	bind := func(edit func(*core.LeaseClaim)) {
		t.Helper()
		current, err := core.ReadLeaseClaim(lease.LeaseID)
		if err != nil {
			t.Fatal(err)
		}
		bound := current
		bound.ActionsWorkspace = &core.ActionsWorkspaceBinding{Origin: "https://github.com/example-org/workflow", MarkerFingerprint: "fingerprint"}
		if edit != nil {
			edit(&bound)
		}
		if err := core.ReplaceLeaseClaimIfUnchanged(lease.LeaseID, current, bound); err != nil {
			t.Fatal(err)
		}
	}
	consent := func(step string) *core.ActionsWorkspaceBinding {
		t.Helper()
		claim, err := core.ReadLeaseClaim(lease.LeaseID)
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		return claim.ActionsWorkspace
	}
	resolve := func(step string, reclaim bool) {
		t.Helper()
		if _, err := b.Resolve(t.Context(), core.ResolveRequest{ID: lease.LeaseID, Repo: repo, Reclaim: reclaim}); err != nil {
			t.Fatalf("Resolve (%s): %v", step, err)
		}
	}
	bind(nil)

	// What `crabbox run` from the same checkout does first.
	resolve("same VM", false)
	if consent("resolve") == nil {
		t.Fatal("resolving the lease for the repository dropped the Actions workspace consent")
	}

	// A list view of the same VM carries its Id; publishing it must not look like a new resource.
	views, err := b.List(t.Context(), core.ListRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	current, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	cfg := b.configForRun()
	target := core.SSHTargetFromConfig(cfg, "192.0.2.10")
	if _, err := core.ClaimLeaseTargetForRepoConfigScopeIfUnchanged(lease.LeaseID, current.Slug, cfg, current.ProviderScope, views[0], target, repo.Root, cfg.IdleTimeout, false, current, true); err != nil {
		t.Fatalf("publish listed lease: %v", err)
	}
	if consent("list") == nil {
		t.Fatalf("publishing the listed lease dropped the Actions workspace consent (list Id %q)", views[0].ImmutableID)
	}

	// The same name now belongs to a different VM, such as one recreated from the
	// hydrated guest's disk with the marker, origin, and SSH key intact.
	liveID = replacementID
	resolve("replacement VM", false)
	if consent("replacement VM") != nil {
		t.Fatal("a VM recreated under the same name kept the previous VM's Actions workspace consent")
	}
	if claim, err := core.ReadLeaseClaim(lease.LeaseID); err != nil || claim.CloudImmutableID != replacementID {
		t.Fatalf("claim identity after replacement = %q, %v; want the live Id %q", claim.CloudImmutableID, err, replacementID)
	}

	// A claim written before Acquire recorded the VM Id cannot prove it names this VM.
	bind(func(claim *core.LeaseClaim) { claim.CloudImmutableID = "" })
	resolve("legacy claim", false)
	if consent("legacy claim") != nil {
		t.Fatal("a claim without a recorded VM Id kept its Actions workspace consent")
	}

	// Without a live Id, Resolve cannot tell whether the VM was replaced.
	bind(nil)
	liveID = ""
	_, err = b.Resolve(t.Context(), core.ResolveRequest{ID: lease.LeaseID, Repo: repo})
	if code := core.ExitCodeForError(err, 0); code != 4 || !strings.Contains(err.Error(), "did not report its Id") {
		t.Fatalf("Resolve without a live VM Id: exit %d, %v; want exit 4 refusing to publish", code, err)
	}
	if consent("no live Id") == nil {
		t.Fatal("a refused resolve changed the claim")
	}
	liveID = replacementID

	// An explicit reclaim still revokes the consent.
	resolve("reclaim", true)
	if consent("reclaim") != nil {
		t.Fatal("--reclaim kept the Actions workspace consent")
	}
}
