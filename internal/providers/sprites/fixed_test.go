package sprites

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const fixedTestLeaseID = "cbx_0123456789ab"

// fixedSpritesAPI models the Sprites API contract the fixed path relies on:
// unique names (409 on reuse), 404 for missing names, labels stored at create.
type fixedSpritesAPI struct {
	mu           sync.Mutex
	organization string
	sprites      map[string]spritesInfo
	creates      int
	deletes      []string
	nextID       int
	// createErr is returned by CreateSprite; createLands decides whether the
	// Sprite exists anyway (a lost reply) or the request never landed.
	createErr    error
	createLands  bool
	getErrs      int
	deleteErr    error
	keepOnDelete bool
}

func newFixedSpritesAPI() *fixedSpritesAPI {
	return &fixedSpritesAPI{organization: "acme", sprites: map[string]spritesInfo{}}
}

func spritesStatusError(code int, body string) error {
	return &spritesAPIError{StatusCode: code, Status: fmt.Sprintf("%d %s", code, http.StatusText(code)), Body: body}
}

func (f *fixedSpritesAPI) GetOrganization(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.organization, nil
}

func (f *fixedSpritesAPI) CreateSprite(_ context.Context, name string, labels []string) (spritesInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates++
	if _, ok := f.sprites[name]; ok {
		return spritesInfo{}, spritesStatusError(http.StatusConflict, `{"error":"A sprite named '`+name+`' already exists"}`)
	}
	if f.createErr != nil && !f.createLands {
		return spritesInfo{}, f.createErr
	}
	f.nextID++
	sprite := spritesInfo{ID: fmt.Sprintf("sprite-%04d", f.nextID), Name: name, Organization: f.organization, Status: "running", Labels: append([]string(nil), labels...)}
	f.sprites[name] = sprite
	if f.createErr != nil {
		return spritesInfo{}, f.createErr
	}
	return sprite, nil
}

func (f *fixedSpritesAPI) GetSprite(_ context.Context, name string) (spritesInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErrs > 0 {
		f.getErrs--
		return spritesInfo{}, errors.New("synthetic get timeout")
	}
	sprite, ok := f.sprites[name]
	if !ok {
		return spritesInfo{}, spritesStatusError(http.StatusNotFound, `{"error":"sprite not found"}`)
	}
	return sprite, nil
}

func (f *fixedSpritesAPI) ListSprites(_ context.Context, prefix string) ([]spritesInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []spritesInfo
	for name, sprite := range f.sprites {
		if strings.HasPrefix(name, prefix) {
			out = append(out, sprite)
		}
	}
	return out, nil
}

func (f *fixedSpritesAPI) DeleteSprite(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, name)
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if _, ok := f.sprites[name]; !ok {
		return spritesStatusError(http.StatusNotFound, `{"error":"sprite not found"}`)
	}
	if !f.keepOnDelete {
		delete(f.sprites, name)
	}
	return nil
}

func (f *fixedSpritesAPI) counts() (int, int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates, len(f.deletes), len(f.sprites)
}

type okRunner struct{}

func (okRunner) Run(context.Context, core.LocalCommandRequest) (core.LocalCommandResult, error) {
	return core.LocalCommandResult{}, nil
}

func fixedSpritesFixture(t *testing.T) (*fixedSpritesAPI, func() *spritesBackend, core.AcquireRequest) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	api := newFixedSpritesAPI()
	newBackend := func() *spritesBackend {
		return &spritesBackend{
			cfg:    core.Config{TTL: 4 * time.Hour, IdleTimeout: 30 * time.Minute, Sprites: core.SpritesConfig{WorkRoot: "/home/sprite/crabbox"}},
			rt:     core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: okRunner{}},
			client: api,
			waitSSH: func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
				return nil
			},
		}
	}
	req := core.AcquireRequest{RequestedLeaseID: fixedTestLeaseID, RequestedSlug: "fixed-sprite", Repo: core.Repo{Root: t.TempDir()}, Keep: true}
	return api, newBackend, req
}

func readFixedSpriteClaim(t *testing.T) core.LeaseClaim {
	t.Helper()
	claim, err := core.ReadLeaseClaim(fixedTestLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	return claim
}

func TestFixedSpriteFreshCreateAndReplay(t *testing.T) {
	api, newBackend, req := fixedSpritesFixture(t)
	lease, err := newBackend().Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	wantName := core.LeaseProviderName(fixedTestLeaseID, "fixed-sprite")
	if lease.LeaseID != fixedTestLeaseID || lease.Server.Name != wantName || lease.Server.ImmutableID != "sprite-0001" {
		t.Fatalf("lease=%+v, want deterministic name %s", lease.Server, wantName)
	}
	claim := readFixedSpriteClaim(t)
	intent := claim.FixedCreateIntent
	if intent == nil || intent.State != "acquired" || claim.CloudID != wantName || claim.CloudImmutableID != "sprite-0001" {
		t.Fatalf("claim=%+v", claim)
	}
	if claim.Labels[fixedSpriteOrganizationKV] != "acme" {
		t.Fatalf("claim organization=%q", claim.Labels[fixedSpriteOrganizationKV])
	}
	sprite := api.sprites[wantName]
	if !spriteHasLabels(sprite, []string{"lease-cbx-0123456789ab", "attempt-" + intent.Attempt[fixedSpriteNonceKey]}) {
		t.Fatalf("sprite labels=%v", sprite.Labels)
	}
	replayed, err := newBackend().Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if creates, _, live := api.counts(); creates != 1 || live != 1 || replayed.Server.ImmutableID != "sprite-0001" {
		t.Fatalf("replay creates=%d live=%d lease=%+v", creates, live, replayed.Server)
	}
}

func releaseFixedSprite(t *testing.T, b *spritesBackend) error {
	t.Helper()
	lease, err := b.Resolve(t.Context(), core.ResolveRequest{ID: fixedTestLeaseID, ReleaseOnly: true})
	if err != nil {
		return err
	}
	return b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease})
}

func TestFixedSpriteCreateRecovery(t *testing.T) {
	for _, tc := range []struct {
		name        string
		arrange     func(*fixedSpritesAPI)
		firstFails  bool
		wantCreates int
	}{
		{name: "lost reply adopts landed sprite", arrange: func(f *fixedSpritesAPI) { f.createErr, f.createLands = errors.New("reply lost"), true }, wantCreates: 1},
		{name: "unobserved create is adopted on replay", arrange: func(f *fixedSpritesAPI) { f.getErrs = 1 }, firstFails: true, wantCreates: 1},
		{name: "create that never landed is resubmitted", arrange: func(f *fixedSpritesAPI) { f.createErr = errors.New("connection reset") }, firstFails: true, wantCreates: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api, newBackend, req := fixedSpritesFixture(t)
			tc.arrange(api)
			_, err := newBackend().Acquire(t.Context(), req)
			if tc.firstFails != (err != nil) {
				t.Fatalf("first acquire err=%v, want failure=%v", err, tc.firstFails)
			}
			if err != nil {
				if !strings.Contains(err.Error(), "outcome unresolved") {
					t.Fatalf("first acquire err=%v", err)
				}
				if claim := readFixedSpriteClaim(t); claim.FixedCreateIntent == nil || claim.CloudImmutableID != "" {
					t.Fatalf("unresolved create lost its claim: %+v", claim)
				}
				api.mu.Lock()
				api.createErr = nil
				api.mu.Unlock()
				if _, err := newBackend().Acquire(t.Context(), req); err != nil {
					t.Fatalf("replay: %v", err)
				}
			}
			creates, deletes, live := api.counts()
			if creates != tc.wantCreates || deletes != 0 || live != 1 {
				t.Fatalf("creates=%d deletes=%d live=%d", creates, deletes, live)
			}
			if claim := readFixedSpriteClaim(t); claim.CloudImmutableID != "sprite-0001" || claim.FixedCreateIntent.State != "acquired" {
				t.Fatalf("claim=%+v", claim)
			}
		})
	}
}

func TestFixedSpriteRefusesForeignSameName(t *testing.T) {
	api, newBackend, req := fixedSpritesFixture(t)
	name := core.LeaseProviderName(fixedTestLeaseID, "fixed-sprite")
	// Not labeled as a Crabbox Sprite, so slug allocation cannot steer around it.
	api.sprites[name] = spritesInfo{ID: "sprite-foreign", Name: name, Organization: "acme", Labels: []string{"team-a"}}
	for attempt := range 2 {
		if _, err := newBackend().Acquire(t.Context(), req); err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
			t.Fatalf("attempt %d err=%v, want lease_id_conflict", attempt, err)
		}
	}
	if err := releaseFixedSprite(t, newBackend()); err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
		t.Fatalf("stop err=%v, want lease_id_conflict", err)
	}
	if _, deletes, live := api.counts(); deletes != 0 || live != 1 {
		t.Fatalf("foreign sprite touched: deletes=%d live=%d", deletes, live)
	}
}

func TestFixedSpriteRefusesOrganizationChange(t *testing.T) {
	api, newBackend, req := fixedSpritesFixture(t)
	if _, err := newBackend().Acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	api.organization = "other"
	api.mu.Unlock()
	if _, err := newBackend().Acquire(t.Context(), req); err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
		t.Fatalf("acquire err=%v, want lease_id_conflict", err)
	}
	if err := releaseFixedSprite(t, newBackend()); err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
		t.Fatalf("stop err=%v, want lease_id_conflict", err)
	}
	if creates, deletes, live := api.counts(); creates != 1 || deletes != 0 || live != 1 {
		t.Fatalf("creates=%d deletes=%d live=%d", creates, deletes, live)
	}
}

func TestFixedSpriteReleaseTombstone(t *testing.T) {
	api, newBackend, req := fixedSpritesFixture(t)
	lease, err := newBackend().Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if err := releaseFixedSprite(t, newBackend()); err != nil {
		t.Fatal(err)
	}
	claim := readFixedSpriteClaim(t)
	if claim.FixedCreateIntent.State != "released" {
		t.Fatalf("no tombstone: %+v", claim.FixedCreateIntent)
	}
	if keep, err := newBackend().RetainLeaseClaimAfterReleaseWithClaim(lease, claim); !keep || err != nil {
		t.Fatalf("tombstone discarded: keep=%v err=%v", keep, err)
	}
	if _, err := newBackend().Acquire(t.Context(), req); err == nil {
		t.Fatal("released lease ID was reacquired")
	}
	if view, err := newBackend().Status(t.Context(), core.StatusRequest{ID: fixedTestLeaseID}); err != nil || view.State != "released" {
		t.Fatalf("status=%+v err=%v", view, err)
	}
	if err := releaseFixedSprite(t, newBackend()); err != nil {
		t.Fatalf("repeat stop: %v", err)
	}
	if creates, deletes, live := api.counts(); creates != 1 || deletes != 1 || live != 0 {
		t.Fatalf("creates=%d deletes=%d live=%d", creates, deletes, live)
	}
}

func TestFixedSpriteDeleteFailureRetainsClaim(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(*fixedSpritesAPI)
	}{
		{name: "delete error", arrange: func(f *fixedSpritesAPI) { f.deleteErr = spritesStatusError(http.StatusInternalServerError, "boom") }},
		{name: "delete acknowledged but sprite remains", arrange: func(f *fixedSpritesAPI) { f.keepOnDelete = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api, newBackend, req := fixedSpritesFixture(t)
			if _, err := newBackend().Acquire(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			tc.arrange(api)
			if err := releaseFixedSprite(t, newBackend()); err == nil {
				t.Fatal("failed delete reported success")
			}
			if claim := readFixedSpriteClaim(t); claim.FixedCreateIntent.State == "released" {
				t.Fatal("failed delete published a tombstone")
			}
			api.mu.Lock()
			api.deleteErr, api.keepOnDelete = nil, false
			api.mu.Unlock()
			if err := releaseFixedSprite(t, newBackend()); err != nil {
				t.Fatalf("retry stop: %v", err)
			}
			if _, _, live := api.counts(); live != 0 || readFixedSpriteClaim(t).FixedCreateIntent.State != "released" {
				t.Fatalf("retry did not finish: live=%d", live)
			}
		})
	}
}

func TestFixedSpriteConcurrentAcquireCreatesOnce(t *testing.T) {
	api, newBackend, req := fixedSpritesFixture(t)
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Go(func() {
			_, errs[i] = newBackend().Acquire(context.Background(), req)
		})
	}
	wg.Wait()
	succeeded := 0
	for _, err := range errs {
		if err == nil {
			succeeded++
		}
	}
	if creates, _, live := api.counts(); succeeded == 0 || creates != 1 || live != 1 {
		t.Fatalf("succeeded=%d creates=%d live=%d errs=%v", succeeded, creates, live, errs)
	}
}

func TestFixedSpriteRejectsNonCanonicalLeaseID(t *testing.T) {
	api, newBackend, req := fixedSpritesFixture(t)
	for _, id := range []string{"cbx_0123456789a", "cbx_0123456789abc", "cbx_0123456789AB", "box_0123456789ab", "cbx-0123456789ab"} {
		req.RequestedLeaseID = id
		if _, err := newBackend().Acquire(t.Context(), req); err == nil {
			t.Fatalf("lease ID %q accepted", id)
		}
	}
	if creates, _, _ := api.counts(); creates != 0 {
		t.Fatalf("non-canonical lease IDs created %d sprites", creates)
	}
}

func TestFixedSpriteStatusProjectsClaimPolicy(t *testing.T) {
	api, newBackend, req := fixedSpritesFixture(t)
	if _, err := newBackend().Acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	view, err := newBackend().Status(t.Context(), core.StatusRequest{ID: fixedTestLeaseID})
	if err != nil {
		t.Fatal(err)
	}
	if view.IdleTimeout == "" || view.ExpiresAt == "" {
		t.Fatalf("status lost recorded policy: %v", view.Labels)
	}
	if view.ServerID != core.LeaseProviderName(fixedTestLeaseID, "fixed-sprite") || view.State != "running" {
		t.Fatalf("status=%+v", view)
	}
	if creates, _, _ := api.counts(); creates != 1 {
		t.Fatalf("status created sprites: %d", creates)
	}
}

// Sprites addresses Sprites only by name, so a Sprite deleted out of band before
// its immutable ID was bound cannot be told apart from a create that never
// landed. Replay recreates it; a lease still never has two live Sprites.
func TestFixedSpriteUnboundOutOfBandDeleteRecreatesOnce(t *testing.T) {
	api, newBackend, req := fixedSpritesFixture(t)
	api.getErrs = 1
	if _, err := newBackend().Acquire(t.Context(), req); err == nil {
		t.Fatal("unobserved create reported success")
	}
	api.mu.Lock()
	clear(api.sprites)
	api.mu.Unlock()
	if _, err := newBackend().Acquire(t.Context(), req); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if creates, _, live := api.counts(); creates != 2 || live != 1 {
		t.Fatalf("creates=%d live=%d", creates, live)
	}
	if claim := readFixedSpriteClaim(t); claim.CloudImmutableID != "sprite-0002" {
		t.Fatalf("claim bound %q, want the recreated Sprite", claim.CloudImmutableID)
	}
	// Once bound, the same disappearance is a conflict instead of a silent recreate.
	api.mu.Lock()
	clear(api.sprites)
	api.mu.Unlock()
	if _, err := newBackend().Acquire(t.Context(), req); err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
		t.Fatalf("bound replay err=%v, want lease_id_conflict", err)
	}
	if err := releaseFixedSprite(t, newBackend()); err != nil {
		t.Fatalf("stop of a vanished bound Sprite: %v", err)
	}
	if creates, _, _ := api.counts(); creates != 2 {
		t.Fatalf("creates=%d after bound conflict", creates)
	}
}
