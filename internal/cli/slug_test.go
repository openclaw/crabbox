package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestNewLeaseSlugDeterministic(t *testing.T) {
	first := newLeaseSlug("cbx_000000000001")
	second := newLeaseSlug("cbx_000000000001")
	if first != second {
		t.Fatalf("newLeaseSlug not deterministic: %q != %q", first, second)
	}
	if first == newLeaseSlug("cbx_000000000002") {
		t.Fatalf("different IDs should usually spread across slugs: %q", first)
	}
}

func TestLeaseSlugGoldenFixtures(t *testing.T) {
	tests := map[string]string{
		"cbx_000000000001": "tidal-lobster",
		"cbx_abcdef123456": "blue-prawn",
		"cbx_deadbeefcafe": "silver-crab",
	}
	for leaseID, want := range tests {
		if got := newLeaseSlug(leaseID); got != want {
			t.Fatalf("newLeaseSlug(%q)=%q want %q", leaseID, got, want)
		}
	}
}

func TestLeaseSlugFormat(t *testing.T) {
	slug := newLeaseSlug("cbx_abcdef123456")
	if !regexp.MustCompile(`^[a-z0-9]+-[a-z0-9]+$`).MatchString(slug) {
		t.Fatalf("slug %q is not DNS-ish two-word form", slug)
	}
	if len(leaseProviderName("cbx_abcdef123456", slug)) > 63 {
		t.Fatalf("provider name too long for slug %q", slug)
	}
}

func TestSlugWithCollisionSuffix(t *testing.T) {
	got := slugWithCollisionSuffix("Blue Lobster", "cbx_abcdef123456")
	if !strings.HasPrefix(got, "blue-lobster-") || len(got) != len("blue-lobster-0000") {
		t.Fatalf("unexpected collision slug %q", got)
	}
	got = slugWithCollisionSuffix("", "cbx_abcdef123456")
	if !strings.HasPrefix(got, newLeaseSlug("cbx_abcdef123456")+"-") {
		t.Fatalf("empty collision base did not fall back to generated slug: %q", got)
	}
}

func TestAllocateDirectLeaseSlugAddsSuffixOnCollision(t *testing.T) {
	leaseID := "cbx_000000000001"
	base := newLeaseSlug(leaseID)
	got, err := allocateDirectLeaseSlug(leaseID, "", []Server{
		{Labels: map[string]string{"lease": "cbx_000000000000", "slug": base}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got == base {
		t.Fatalf("slug collision was not repaired: %q", got)
	}
	if !strings.HasPrefix(got, base+"-") {
		t.Fatalf("collision slug=%q want %q suffix", got, base)
	}
}

func TestRequestedLeaseSlugNormalizesAndValidates(t *testing.T) {
	got, err := requestedLeaseSlug(" Update Flow Smoke ")
	if err != nil {
		t.Fatal(err)
	}
	if got != "update-flow-smoke" {
		t.Fatalf("slug=%q", got)
	}
	if _, err := requestedLeaseSlug("!!!"); err == nil {
		t.Fatal("expected empty normalized slug error")
	}
	if _, err := requestedLeaseSlug("proxmox-live-smoke-34567890-0123456789ab"); err != nil {
		t.Fatalf("live smoke slug rejected by CLI parser: %v", err)
	}
}

func TestAllocateDirectLeaseSlugUsesRequestedSlug(t *testing.T) {
	got, err := allocateDirectLeaseSlug("cbx_000000000001", "Update Flow Smoke", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "update-flow-smoke" {
		t.Fatalf("slug=%q", got)
	}
	got, err = allocateDirectLeaseSlug("cbx_000000000001", "Update Flow Smoke", []Server{
		{Labels: map[string]string{"lease": "cbx_000000000000", "slug": "update-flow-smoke"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "update-flow-smoke-") {
		t.Fatalf("collision slug=%q", got)
	}
}

func TestAllocateDirectLeaseSlugAvoidsLocalClaimCollisionForRequestedSlug(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := claimLeaseForRepoProvider("cbx_000000000000", "update-flow-smoke", "cloudflare", "/repo-a", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	got, err := allocateDirectLeaseSlug("cbx_000000000001", "Update Flow Smoke", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got == "update-flow-smoke" {
		t.Fatalf("claim collision was not repaired: %q", got)
	}
	if !strings.HasPrefix(got, "update-flow-smoke-") {
		t.Fatalf("collision slug=%q want update-flow-smoke suffix", got)
	}
}

func TestAllocateDirectLeaseSlugAvoidsLocalClaimCollisionForGeneratedSlug(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_000000000001"
	base := newLeaseSlug(leaseID)
	if err := claimLeaseForRepoProvider("cbx_000000000000", base, "digitalocean", "/repo-a", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	got, err := allocateDirectLeaseSlug(leaseID, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got == base {
		t.Fatalf("claim collision was not repaired: %q", got)
	}
	if !strings.HasPrefix(got, base+"-") || len(got) != len(base+"-0000") {
		t.Fatalf("collision slug=%q want four-hex suffix", got)
	}
}

func TestAllocateDirectLeaseSlugGeneratedIgnoresCorruptUnrelatedClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	path, err := leaseClaimPath("cbx_badbadbadbad")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	leaseID := "cbx_000000000001"
	got, err := allocateDirectLeaseSlug(leaseID, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != newLeaseSlug(leaseID) {
		t.Fatalf("slug=%q", got)
	}
}

func TestAllocateClaimLeaseSlugAvoidsLocalClaimCollision(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := claimLeaseForRepoProvider("cbx_000000000000", "update-flow-smoke", "cloudflare", "/repo-a", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	got, err := allocateClaimLeaseSlug("cbx_000000000001", "Update Flow Smoke")
	if err != nil {
		t.Fatal(err)
	}
	if got == "update-flow-smoke" {
		t.Fatalf("claim collision was not repaired: %q", got)
	}
	if !strings.HasPrefix(got, "update-flow-smoke-") {
		t.Fatalf("collision slug=%q want update-flow-smoke suffix", got)
	}
}

func TestAllocateClaimLeaseSlugGeneratedIgnoresCorruptUnrelatedClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	path, err := leaseClaimPath("cbx_badbadbadbad")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := allocateClaimLeaseSlug("cbx_000000000001", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != newLeaseSlug("cbx_000000000001") {
		t.Fatalf("slug=%q", got)
	}
}

func TestAllocateClaimLeaseSlugAvoidsGeneratedCollision(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	firstID := ""
	secondID := ""
	seen := map[string]string{}
	for i := 0; i < 1000; i++ {
		leaseID := fmt.Sprintf("cbx_%012x", i)
		slug := newLeaseSlug(leaseID)
		if prior := seen[slug]; prior != "" {
			firstID = prior
			secondID = leaseID
			break
		}
		seen[slug] = leaseID
	}
	if firstID == "" {
		t.Fatal("could not find deterministic generated slug collision")
	}
	base := newLeaseSlug(firstID)
	if err := claimLeaseForRepoProvider(firstID, base, "cloudflare", "/repo-a", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	got, err := allocateClaimLeaseSlug(secondID, "")
	if err != nil {
		t.Fatal(err)
	}
	if got == base || !strings.HasPrefix(got, base+"-") {
		t.Fatalf("generated collision slug=%q base=%q", got, base)
	}
}

func TestAllocateClaimLeaseSlugRejectsOccupiedFallback(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_ffffffffffff"
	base := "exhausted"
	candidates := []string{base}
	for attempt := 0; attempt < 19; attempt++ {
		candidates = append(candidates, slugWithCollisionSuffix(base, fmt.Sprintf("%s-%d", leaseID, attempt)))
	}
	candidates = append(candidates, slugWithCollisionSuffix(base, leaseID))
	for i, slug := range candidates {
		owner := fmt.Sprintf("cbx_%012x", i+1)
		if err := claimLeaseForRepoProvider(owner, slug, "cloudflare", fmt.Sprintf("/repo-%d", i), time.Minute, false); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := allocateClaimLeaseSlug(leaseID, base); err == nil {
		t.Fatalf("slug=%q want exhaustion error", got)
	}
}

func TestAllocateDirectLeaseSlugRejectsOccupiedFallback(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_eeeeeeeeeeee"
	base := "exhausted-direct"
	candidates := []string{base}
	for attempt := 0; attempt < 19; attempt++ {
		candidates = append(candidates, slugWithCollisionSuffix(base, fmt.Sprintf("%s-%d", leaseID, attempt)))
	}
	candidates = append(candidates, slugWithCollisionSuffix(base, leaseID))
	servers := make([]Server, 0, len(candidates))
	for i, slug := range candidates {
		servers = append(servers, Server{Labels: map[string]string{
			"lease": fmt.Sprintf("cbx_%012x", i+1),
			"slug":  slug,
		}})
	}
	if got, err := allocateDirectLeaseSlug(leaseID, base, servers); err == nil {
		t.Fatalf("slug=%q want exhaustion error", got)
	}
}

func TestLeaseProviderNameUsesSlug(t *testing.T) {
	if got := leaseProviderName("cbx_abcdef123456", "blue-lobster"); got != "crabbox-blue-lobster-c80c2195" {
		t.Fatalf("provider name=%q", got)
	}
	if got := leaseProviderName("cbx_abcdef123456", ""); got != "crabbox-cbx-abcdef123456" {
		t.Fatalf("fallback provider name=%q", got)
	}
}

func TestFindServerByAliasDoesNotLetMalformedLeaseShadowSlug(t *testing.T) {
	servers := []Server{
		{Name: "crabbox-blue-lobster", Labels: map[string]string{"lease": "cbx_111111111111", "slug": "blue-lobster"}},
		{Name: "crabbox-cbx-222222222222", Labels: map[string]string{"lease": "blue-lobster", "slug": "amber-krill"}},
	}
	_, leaseID, err := findServerByAlias(servers, "blue-lobster")
	if err != nil {
		t.Fatal(err)
	}
	if leaseID != "cbx_111111111111" {
		t.Fatalf("leaseID=%q want slug match with canonical lease", leaseID)
	}
}

func TestFindServerByAliasPrefersCanonicalID(t *testing.T) {
	servers := []Server{
		{Name: "crabbox-blue-lobster", Labels: map[string]string{"lease": "cbx_111111111111", "slug": "blue-lobster"}},
		{Name: "crabbox-amber-krill", Labels: map[string]string{"lease": "cbx_222222222222", "slug": "amber-krill"}},
	}
	_, leaseID, err := findServerByAlias(servers, "cbx_222222222222")
	if err != nil {
		t.Fatal(err)
	}
	if leaseID != "cbx_222222222222" {
		t.Fatalf("leaseID=%q want canonical ID exact match", leaseID)
	}
}

func TestFindServerByAliasDoesNotRetargetMissingCanonicalID(t *testing.T) {
	id := "cbx_222222222222"
	servers := []Server{{
		Name:   id,
		Labels: map[string]string{"lease": "cbx_111111111111", "slug": id},
	}}
	server, leaseID, err := findServerByAlias(servers, id)
	if err != nil {
		t.Fatal(err)
	}
	if leaseID != "" || server.DisplayID() != "0" {
		t.Fatalf("matched server=%s lease=%q, want no alias fallback", server.DisplayID(), leaseID)
	}
}

func TestFindServerByAliasMatchesCloudInstanceName(t *testing.T) {
	servers := []Server{
		{Name: "crabbox-fallback-zone", CloudID: "crabbox-fallback-zone", Labels: map[string]string{"lease": "cbx_333333333333", "slug": "fallback-zone"}},
	}
	server, leaseID, err := findServerByAlias(servers, "crabbox-fallback-zone")
	if err != nil {
		t.Fatal(err)
	}
	if leaseID != "cbx_333333333333" || server.CloudID != "crabbox-fallback-zone" {
		t.Fatalf("matched lease=%q cloud=%q", leaseID, server.CloudID)
	}
}

func TestFindServerByAliasAmbiguousSlugFails(t *testing.T) {
	servers := []Server{
		{Labels: map[string]string{"lease": "cbx_111111111111", "slug": "blue-lobster"}},
		{Labels: map[string]string{"lease": "cbx_222222222222", "slug": "blue-lobster"}},
	}
	if _, _, err := findServerByAlias(servers, "blue-lobster"); err == nil {
		t.Fatal("expected ambiguous slug error")
	}
}

func TestServerSlugHandlesMissingLabels(t *testing.T) {
	if got := serverSlug(Server{}); got != "" {
		t.Fatalf("serverSlug without labels=%q", got)
	}
}
