package ssh

import (
	"context"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestStaticClaimCollisionAddsSharingHint(t *testing.T) {
	for _, tc := range []struct {
		name   string
		refuse func(*staticLeaseBackend, string, string) error
	}{
		{name: "acquire", refuse: func(b *staticLeaseBackend, _ string, repo string) error {
			_, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: repo}})
			return err
		}},
		{name: "prepared-resolve", refuse: func(b *staticLeaseBackend, leaseID string, repo string) error {
			_, err := b.Resolve(context.Background(), core.ResolveRequest{ID: leaseID, Repo: core.Repo{Root: repo}, Prepare: true})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _, repoA := staticArchitectureFixture(t, "linux", "normal", "")
			lease, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: repoA}})
			if err != nil {
				t.Fatal(err)
			}
			b = NewStaticSSHLeaseBackend(Provider{}.Spec(), b.Cfg, b.RT).(*staticLeaseBackend)
			repoB := t.TempDir()
			err = tc.refuse(b, lease.LeaseID, repoB)
			var exitErr core.ExitError
			if !core.AsExitError(err, &exitErr) || exitErr.Code != 2 {
				t.Fatalf("collision must keep exit code 2: %v", err)
			}
			msg := exitErr.Message
			for _, want := range []string{
				"lease " + lease.LeaseID + " is claimed by repo " + repoA,
				"use --reclaim to claim it for " + repoB,
				"static.id (CRABBOX_STATIC_ID)",
				"<workRoot>/<id>/<repo>",
				"--reclaim takes over the other repository's lease and workdir",
			} {
				if !strings.Contains(msg, want) {
					t.Errorf("collision message missing %q:\n%s", want, msg)
				}
			}
		})
	}
}

func TestCheckStaticLeaseClaimOwnerPassesThrough(t *testing.T) {
	repo := t.TempDir()
	claim := core.LeaseClaim{LeaseID: "static_build", RepoRoot: repo}
	for _, tc := range []struct {
		name    string
		repo    string
		reclaim bool
	}{
		{name: "same-repo", repo: repo},
		{name: "reclaim", repo: t.TempDir(), reclaim: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkStaticLeaseClaimOwner(claim.LeaseID, claim, tc.repo, tc.reclaim); err != nil {
				t.Fatalf("authorized owner check failed: %v", err)
			}
		})
	}
}
