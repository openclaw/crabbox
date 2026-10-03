package linode

import (
	"context"
	"errors"
	"maps"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type lostReplyLinodeAPI struct{ *fakeLinodeAPI }

func (f lostReplyLinodeAPI) CreateLinode(ctx context.Context, req createLinodeRequest) (linodeInstance, error) {
	_, err := f.fakeLinodeAPI.CreateLinode(ctx, req)
	if err != nil {
		return linodeInstance{}, err
	}
	return linodeInstance{}, errors.New("create response lost")
}

func fixedRequest(t *testing.T) core.AcquireRequest {
	t.Helper()
	return core.AcquireRequest{RequestedLeaseID: "cbx_abcdef123456", RequestedSlug: "fixed", Repo: core.Repo{Root: t.TempDir()}, Keep: true}
}

func requireFixedConflict(t *testing.T, err error) {
	t.Helper()
	if err == nil || core.ExitCodeForError(err, 1) != 4 || !strings.Contains(err.Error(), "lease_id_conflict") {
		t.Fatalf("want lease_id_conflict exit 4, got %v", err)
	}
}

func TestFixedLinodeAcquire(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		lostReply, readinessFailure bool
	}{
		{name: "fresh and replay"}, {name: "lost create response", lostReply: true}, {name: "interrupted readiness", readinessFailure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeLinodeAPI{}
			b := newTestBackend(t, api)
			if !b.SupportsRequestedLeaseID() {
				t.Fatal("fixed IDs unsupported")
			}
			req := fixedRequest(t)
			if tc.lostReply {
				b.clientFactory = func(core.Runtime) (linodeAPI, error) { return lostReplyLinodeAPI{api}, nil }
			}
			if tc.readinessFailure {
				b.waitSSH = func(context.Context, *core.SSHTarget, string, time.Duration) error {
					return errors.New("SSH interrupted")
				}
			}
			calls := 0
			req.OnAcquired = func(core.LeaseTarget) error { calls++; return nil }
			first, err := b.Acquire(t.Context(), req)
			if tc.lostReply || tc.readinessFailure {
				if err == nil {
					t.Fatal("expected interrupted acquire")
				}
				if calls != 0 || len(api.deleted) != 0 {
					t.Fatal("failed acquisition acknowledged or rolled back")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			b.waitSSH = func(context.Context, *core.SSHTarget, string, time.Duration) error { return nil }
			lease, err := b.Acquire(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			if lease.LeaseID != req.RequestedLeaseID || lease.Server.ID != 100 || len(api.createRequests) != 1 || calls == 0 {
				t.Fatalf("replay identity=%s/%d creates=%d callbacks=%d", lease.LeaseID, lease.Server.ID, len(api.createRequests), calls)
			}
			if first.LeaseID != "" && first.SSH.Key != lease.SSH.Key {
				t.Fatal("replay replaced SSH key")
			}
			claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
			account, _ := api.AccountID(t.Context())
			if err != nil || claim.ProviderScope != account || claim.CloudID != "100" || claim.FixedCreateIntent.State != "acquired" {
				t.Fatalf("claim=%+v err=%v", claim, err)
			}
			if err := validateFixedLinode(claim, api.created[0]); err != nil {
				t.Fatal(err)
			}
			for _, tag := range api.created[0].Tags {
				if len(tag) > maxLinodeTagLength {
					t.Fatalf("tag too long: %s", tag)
				}
			}
			create := api.createRequests[0]
			if len(create.AuthorizedKeys) != 1 || create.RootPass == "" || create.Metadata == nil || create.Metadata.UserData == "" {
				t.Fatal("incomplete create inputs")
			}
		})
	}
}

func TestFixedLinodeChangedIntent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*linodeLeaseBackend, *fakeLinodeAPI, *core.AcquireRequest)
	}{
		{"account", func(_ *linodeLeaseBackend, a *fakeLinodeAPI, _ *core.AcquireRequest) { a.accountID = "euuid:other" }},
		{"repository", func(_ *linodeLeaseBackend, _ *fakeLinodeAPI, r *core.AcquireRequest) { r.Repo.Root += "/other" }},
		{"keep", func(_ *linodeLeaseBackend, _ *fakeLinodeAPI, r *core.AcquireRequest) { r.Keep = false }},
		{"slug", func(_ *linodeLeaseBackend, _ *fakeLinodeAPI, r *core.AcquireRequest) { r.RequestedSlug = "other" }},
		{"region", func(b *linodeLeaseBackend, _ *fakeLinodeAPI, _ *core.AcquireRequest) { b.Cfg.Linode.Region = "us-sea" }},
		{"image", func(b *linodeLeaseBackend, _ *fakeLinodeAPI, _ *core.AcquireRequest) {
			b.Cfg.Linode.Image = "linode/debian12"
		}},
		{"type", func(b *linodeLeaseBackend, _ *fakeLinodeAPI, _ *core.AcquireRequest) {
			b.Cfg.Linode.Type = "g6-standard-2"
		}},
		{"firewall", func(b *linodeLeaseBackend, _ *fakeLinodeAPI, _ *core.AcquireRequest) { b.Cfg.Linode.FirewallID = "52" }},
		{"interfaces", func(_ *linodeLeaseBackend, a *fakeLinodeAPI, _ *core.AcquireRequest) {
			a.accountSettings.InterfacesForNewLinodes = "linode_only"
		}},
		{"TTL", func(b *linodeLeaseBackend, _ *fakeLinodeAPI, _ *core.AcquireRequest) { b.Cfg.TTL += time.Minute }},
		{"idle", func(b *linodeLeaseBackend, _ *fakeLinodeAPI, _ *core.AcquireRequest) {
			b.Cfg.IdleTimeout += time.Minute
		}},
		{"SSH user", func(b *linodeLeaseBackend, _ *fakeLinodeAPI, _ *core.AcquireRequest) { b.Cfg.SSHUser = "alice" }},
		{"SSH port", func(b *linodeLeaseBackend, _ *fakeLinodeAPI, _ *core.AcquireRequest) { b.Cfg.SSHPort = "2222" }},
		{"work root", func(b *linodeLeaseBackend, _ *fakeLinodeAPI, _ *core.AcquireRequest) { b.Cfg.WorkRoot = "/other" }},
		{"pond", func(b *linodeLeaseBackend, _ *fakeLinodeAPI, _ *core.AcquireRequest) { b.Cfg.Pond = "other" }},
		{"bootstrap", func(b *linodeLeaseBackend, _ *fakeLinodeAPI, _ *core.AcquireRequest) {
			b.Cfg.Tailscale.Hostname = "other"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeLinodeAPI{}
			b := newTestBackend(t, api)
			b.Cfg.Linode.FirewallID = "51"
			b.Cfg.Tailscale.Enabled = true
			b.Cfg.Tailscale.AuthKey = "fixture-auth-key"
			req := fixedRequest(t)
			if _, err := b.Acquire(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			before, _ := core.ReadLeaseClaim(req.RequestedLeaseID)
			tc.mutate(b, api, &req)
			_, err := b.Acquire(t.Context(), req)
			requireFixedConflict(t, err)
			after, _ := core.ReadLeaseClaim(req.RequestedLeaseID)
			if !reflect.DeepEqual(before, after) || len(api.createRequests) != 1 {
				t.Fatal("conflict mutated claim or allocated")
			}
		})
	}
}

func TestFixedLinodeKeyPolicy(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "stored key wins", true: "missing stored key"}[missing], func(t *testing.T) {
			api := &fakeLinodeAPI{}
			b := newTestBackend(t, api)
			req := fixedRequest(t)
			first, err := b.Acquire(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			key, err := os.ReadFile(first.SSH.Key)
			if err != nil {
				t.Fatal(err)
			}
			b.Cfg.SSHKey = "/does-not-exist/other-key"
			if missing {
				core.RemoveStoredTestboxKey(req.RequestedLeaseID)
			}
			replay, err := b.Acquire(t.Context(), req)
			if missing {
				if err == nil {
					t.Fatal("missing key was regenerated")
				}
				if _, err := os.Stat(first.SSH.Key); !os.IsNotExist(err) {
					t.Fatal("replay wrote a replacement key")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				after, _ := os.ReadFile(replay.SSH.Key)
				if string(key) != string(after) {
					t.Fatal("stored key replaced")
				}
			}
			if len(api.createRequests) != 1 {
				t.Fatal("duplicate create")
			}
		})
	}
}

func TestFixedLinodeUncertainInventory(t *testing.T) {
	for _, tc := range []string{"empty", "duplicate", "wrong nonce", "wrong fingerprint", "wrong name"} {
		t.Run(tc, func(t *testing.T) {
			api := &fakeLinodeAPI{}
			b := newTestBackend(t, api)
			b.clientFactory = func(core.Runtime) (linodeAPI, error) { return lostReplyLinodeAPI{api}, nil }
			req := fixedRequest(t)
			if _, err := b.Acquire(t.Context(), req); err == nil {
				t.Fatal("expected lost response")
			}
			switch tc {
			case "empty":
				api.created = nil
			case "duplicate":
				item := api.created[0]
				item.ID++
				api.linodes = append(api.linodes, item)
			case "wrong name":
				api.created[0].Label = "other"
			default:
				labels := labelsFromTags(api.created[0].Tags)
				key := "fixed_attempt"
				if tc == "wrong fingerprint" {
					key = "fixed_intent_sha256"
				}
				labels[key] = "other"
				api.created[0].Tags = tagsFromLabels(labels)
			}
			if _, err := b.Acquire(t.Context(), req); err == nil {
				t.Fatal("uncertain inventory adopted")
			}
			if _, err := b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true}); err == nil {
				t.Fatal("uncertain inventory released")
			}
			if len(api.createRequests) != 1 || len(api.deleted) != 0 {
				t.Fatal("uncertain inventory mutated")
			}
		})
	}
}

func TestFixedLinodeLifecycle(t *testing.T) {
	for _, tc := range []string{"ready", "lost response", "missing instance", "delete retry", "missing key"} {
		t.Run(tc, func(t *testing.T) {
			api := &fakeLinodeAPI{}
			b := newTestBackend(t, api)
			req := fixedRequest(t)
			if tc == "lost response" {
				b.clientFactory = func(core.Runtime) (linodeAPI, error) { return lostReplyLinodeAPI{api}, nil }
			}
			_, err := b.Acquire(t.Context(), req)
			if (err != nil) != (tc == "lost response") {
				t.Fatal(err)
			}
			before, _ := core.ReadLeaseClaim(req.RequestedLeaseID)
			for _, id := range []string{req.RequestedLeaseID, req.RequestedSlug, "100"} {
				lease, err := b.Resolve(t.Context(), core.ResolveRequest{ID: id, StatusOnly: true, NoLocalStateMutations: true})
				if err != nil || lease.Server.ID != 100 {
					t.Fatalf("inspect %s: %v", id, err)
				}
			}
			after, _ := core.ReadLeaseClaim(req.RequestedLeaseID)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("inspection changed claim")
			}
			if tc == "ready" {
				lease, err := b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, Repo: req.Repo})
				if err != nil {
					t.Fatal(err)
				}
				api.created[0].Tags = append(api.created[0].Tags, "operator-tag")
				touched, err := b.Touch(t.Context(), core.TouchRequest{Lease: lease, State: "ready"})
				if err != nil {
					t.Fatal(err)
				}
				if touched.Labels["fixed_attempt"] != before.Labels["fixed_attempt"] {
					t.Fatal("heartbeat lost fixed identity")
				}
				if _, err := b.Acquire(t.Context(), req); err != nil {
					t.Fatalf("replay after heartbeat: %v", err)
				}
			}
			if tc == "missing instance" {
				api.created = nil
			}
			if tc == "missing key" {
				if err := core.RemoveStoredTestboxConnectionArtifacts(req.RequestedLeaseID); err != nil {
					t.Fatal(err)
				}
			}
			lease, err := b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			if tc == "delete retry" {
				api.deleteErr = errors.New("temporary delete failure")
				if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err == nil {
					t.Fatal("expected delete failure")
				}
				api.deleteErr = nil
				lease, err = b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true})
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
				t.Fatal(err)
			}
			claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
			if err != nil || claim.FixedCreateIntent.State != "released" {
				t.Fatalf("terminal claim: %+v %v", claim, err)
			}
			retained, err := b.RetainLeaseClaimAfterReleaseWithClaim(lease, before)
			if err != nil || !retained {
				t.Fatalf("terminal retention: %v %v", retained, err)
			}
			_, err = b.Acquire(t.Context(), req)
			requireFixedConflict(t, err)
			deletes := len(api.deleted)
			lease, err = b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
				t.Fatal(err)
			}
			if len(api.deleted) != deletes || len(api.createRequests) != 1 {
				t.Fatal("terminal replay mutated provider")
			}
		})
	}
}

func TestFixedLinodeRejectsUnclaimedInventory(t *testing.T) {
	api := &fakeLinodeAPI{}
	b := newTestBackend(t, api)
	req := fixedRequest(t)
	labels := core.DirectLeaseLabels(b.Cfg, req.RequestedLeaseID, req.RequestedSlug, providerName, "", true, time.Now())
	api.linodes = []linodeInstance{{ID: 101, Label: core.LeaseProviderName(req.RequestedLeaseID, req.RequestedSlug), Tags: tagsFromLabels(labels)}}
	_, err := b.Acquire(t.Context(), req)
	requireFixedConflict(t, err)
	if len(api.createRequests) != 0 {
		t.Fatal("unclaimed lease duplicated")
	}
}

func TestFixedLinodeTagIdentity(t *testing.T) {
	api := &fakeLinodeAPI{}
	b := newTestBackend(t, api)
	req := fixedRequest(t)
	if _, err := b.Acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	before := maps.Clone(labelsFromTags(api.created[0].Tags))
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"fixed_attempt", "fixed_intent_sha256"} {
		if before[key] == "" {
			t.Fatalf("missing %s", key)
		}
		tags := append(append([]string(nil), api.created[0].Tags...), encodeTagKV(key, "conflict")...)
		item := api.created[0]
		item.Tags = tags
		if validateFixedLinode(claim, item) == nil {
			t.Fatalf("conflicting %s accepted", key)
		}
	}
}

func TestFixedLinodeLifecycleRejectsAccountChange(t *testing.T) {
	api := &fakeLinodeAPI{}
	b := newTestBackend(t, api)
	req := fixedRequest(t)
	lease, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := core.ReadLeaseClaim(req.RequestedLeaseID)
	writes := api.updateCalls
	api.accountID = "euuid:other"
	for _, id := range []string{req.RequestedLeaseID, req.RequestedSlug, "100"} {
		for _, release := range []bool{false, true} {
			_, err := b.Resolve(t.Context(), core.ResolveRequest{ID: id, ReleaseOnly: release, StatusOnly: !release, NoLocalStateMutations: !release})
			requireFixedConflict(t, err)
		}
	}
	if _, err := b.Touch(t.Context(), core.TouchRequest{Lease: lease, State: "ready"}); err == nil {
		t.Fatal("heartbeat accepted another account")
	}
	if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err == nil {
		t.Fatal("release accepted another account")
	}
	after, _ := core.ReadLeaseClaim(req.RequestedLeaseID)
	if !reflect.DeepEqual(before, after) || len(api.deleted) != 0 || api.updateCalls != writes {
		t.Fatal("account conflict mutated lease")
	}
}

func TestLinodeSlugResolutionPrefersLiveInventory(t *testing.T) {
	api := &fakeLinodeAPI{}
	b := newTestBackend(t, api)
	req := fixedRequest(t)
	req.RequestedLeaseID = ""
	lease, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	staleID := "cbx_abcdef654321"
	if err := core.WithDurableLeaseClaimLockContext(t.Context(), staleID, func(stale *core.LeaseClaim, _ bool, persist func() error) error {
		*stale = core.CloneLeaseClaim(claim)
		stale.LeaseID, stale.CloudID, stale.CloudNumericID = staleID, "99", 99
		stale.Labels["lease"] = staleID
		return persist()
	}); err != nil {
		t.Fatal(err)
	}
	resolved, err := b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedSlug, StatusOnly: true, NoLocalStateMutations: true})
	if err != nil || resolved.LeaseID != lease.LeaseID {
		t.Fatalf("live slug lookup: %s %v", resolved.LeaseID, err)
	}
}
