package cli

import (
	"regexp"
	"strings"
	"testing"
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
	got := allocateDirectLeaseSlug(leaseID, []Server{
		{Labels: map[string]string{"lease": "cbx_000000000000", "slug": base}},
	})
	if got == base {
		t.Fatalf("slug collision was not repaired: %q", got)
	}
	if !strings.HasPrefix(got, base+"-") {
		t.Fatalf("collision slug=%q want %q suffix", got, base)
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
