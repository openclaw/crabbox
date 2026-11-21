package cli

import (
	"testing"
	"time"
)

func TestActionsWorkspaceBindingToleratesConcurrentHeartbeatPublication(t *testing.T) {
	isolateRunTestUserDirs(t, t.TempDir())
	target := actionsWorkspaceTestSSH(t, `exec /bin/sh -c "$remote"`)
	repo := Repo{Root: t.TempDir(), RemoteURL: "https://github.com/example-org/source.git"}
	workspace := t.TempDir()
	runGit(t, workspace, "init", "-q")
	runGit(t, workspace, "remote", "add", "origin", "git@github.com:example-org/workflow.git")
	state := actionsHydrationState{Workspace: workspace, RunID: "123", EnvFile: workspace + "/environment"}
	const leaseID = "cbx_binding_heartbeat"
	if err := ClaimLeaseForRepoProvider(leaseID, "", "aws", repo.Root, time.Minute, false); err != nil {
		t.Fatal(err)
	}
	claim, exists, err := prepareActionsWorkspaceClaim(t.Context(), leaseID, repo)
	if err != nil || !exists {
		t.Fatalf("prepare claim: exists=%t err=%v", exists, err)
	}
	if _, err := UpdateLeaseClaimLabelsAndLastUsedIfUnchanged(leaseID, claim, map[string]string{"state": "running"}, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := bindActionsWorkspaceClaim(t.Context(), target, repo, GitHubRepo{Owner: "example-org", Name: "workflow"}, state, claim, exists); err != nil {
		t.Fatalf("heartbeat publication broke workspace binding: %v", err)
	}
	saved, _, err := ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.ActionsWorkspace == nil || saved.Labels["state"] != "running" {
		t.Fatalf("binding lost data: workspace=%v labels=%v", saved.ActionsWorkspace, saved.Labels)
	}
}

func TestActionsWorkspaceClearToleratesHeartbeatPublicationAfterRead(t *testing.T) {
	isolateRunTestUserDirs(t, t.TempDir())
	repo := Repo{Root: t.TempDir()}
	const leaseID = "cbx_clear_heartbeat"
	if err := ClaimLeaseForRepoProvider(leaseID, "", "aws", repo.Root, time.Minute, false); err != nil {
		t.Fatal(err)
	}
	seeded, _, err := ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	bound := cloneLeaseClaim(seeded)
	bound.ActionsWorkspace = &ActionsWorkspaceBinding{Origin: "https://github.com/example-org/workflow", MarkerFingerprint: "fingerprint"}
	read, err := ReplaceLeaseClaimIfUnchangedDurableReturningContext(t.Context(), leaseID, seeded, bound)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateLeaseClaimLabelsAndLastUsedIfUnchanged(leaseID, read, map[string]string{"state": "running"}, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	cleared, err := clearActionsWorkspaceBinding(t.Context(), leaseID, read)
	if err != nil {
		t.Fatalf("heartbeat publication after the claim read aborted preparation: %v", err)
	}
	if cleared.ActionsWorkspace != nil || cleared.Labels["state"] != "running" {
		t.Fatalf("cleared claim = workspace %v labels %v", cleared.ActionsWorkspace, cleared.Labels)
	}
}

func TestActionsWorkspaceClearRejectsOwnershipChange(t *testing.T) {
	isolateRunTestUserDirs(t, t.TempDir())
	repo := Repo{Root: t.TempDir()}
	const leaseID = "cbx_clear_ownership"
	if err := ClaimLeaseForRepoProvider(leaseID, "", "aws", repo.Root, time.Minute, false); err != nil {
		t.Fatal(err)
	}
	seeded, _, err := ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	bound := cloneLeaseClaim(seeded)
	bound.ActionsWorkspace = &ActionsWorkspaceBinding{Origin: "https://github.com/example-org/workflow", MarkerFingerprint: "fingerprint"}
	read, err := ReplaceLeaseClaimIfUnchangedDurableReturningContext(t.Context(), leaseID, seeded, bound)
	if err != nil {
		t.Fatal(err)
	}
	moved := cloneLeaseClaim(read)
	moved.RepoRoot = t.TempDir()
	if _, err := ReplaceLeaseClaimIfUnchangedDurableReturningContext(t.Context(), leaseID, read, moved); err != nil {
		t.Fatal(err)
	}
	if _, err := clearActionsWorkspaceBinding(t.Context(), leaseID, read); err == nil {
		t.Fatal("ownership change was retried over")
	}
}

func TestActionsClaimLifecycleOnlyChangeAcceptsSameSecondHeartbeat(t *testing.T) {
	captured := leaseClaim{LeaseID: "cbx_same_second", Provider: "hyperv", LastUsedAt: "2026-01-02T03:04:05Z", Revision: "r1", Labels: map[string]string{"state": "ready"}}
	heartbeat := cloneLeaseClaim(captured)
	heartbeat.Revision = "r2"
	heartbeat.Labels = map[string]string{"state": "ready", "last_touched_at": "x"}
	if !actionsClaimLifecycleOnlyChange(captured, heartbeat) {
		t.Fatal("same-second heartbeat-only change was rejected")
	}
	moved := cloneLeaseClaim(heartbeat)
	moved.RepoRoot = "/other"
	if actionsClaimLifecycleOnlyChange(captured, moved) {
		t.Fatal("ownership change was accepted as lifecycle-only")
	}
	older := cloneLeaseClaim(heartbeat)
	older.LastUsedAt = "2026-01-02T03:04:04Z"
	if actionsClaimLifecycleOnlyChange(captured, older) {
		t.Fatal("older last-use time was accepted")
	}
}

func TestActionsClaimLifecycleOnlyChangeRejectsForeignLabel(t *testing.T) {
	captured := leaseClaim{LeaseID: "cbx_x", LastUsedAt: "2026-01-01T00:00:00Z", Labels: map[string]string{"state": "leased"}}
	heartbeat := cloneLeaseClaim(captured)
	heartbeat.Labels = map[string]string{"state": "ready", "last_touched_at": "x"}
	if !actionsClaimLifecycleOnlyChange(captured, heartbeat) {
		t.Fatal("heartbeat labels rejected")
	}
	foreign := cloneLeaseClaim(captured)
	foreign.Labels = map[string]string{"state": "leased", "owner_note": "other-writer"}
	if actionsClaimLifecycleOnlyChange(captured, foreign) {
		t.Fatal("foreign label change treated as heartbeat")
	}
}
