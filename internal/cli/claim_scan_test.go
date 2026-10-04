package cli

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestClaimLookupReadCountsAndAmbiguity(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	claims := []leaseClaim{
		{LeaseID: "cbx_000000000001", Slug: "shared"},
		{LeaseID: "cbx_000000000002", Slug: "shared"},
		{LeaseID: "tbx_test", Slug: "other"},
		{LeaseID: "cbx_000000000003", Slug: "tbx-test"},
		{LeaseID: "cbx_000000000004", Slug: "cbx-000000000001"},
	}
	for _, c := range claims {
		writeClaimsListFixture(t, c.LeaseID+".json", c)
	}
	for _, tc := range []struct {
		id        string
		unique    bool
		count     int
		want      string
		ambiguous bool
	}{
		{"cbx_000000000001", false, 1, "cbx_000000000001", false},
		{"cbx_000000000001", true, 1, "cbx_000000000001", false},
		{"cbx_ffffffffffff", true, 1, "", false},
		{"shared", false, 1, "cbx_000000000001", false},
		{"shared", true, 2, "", true},
		{"absent", true, len(claims), "", false},
		{"tbx_test", true, len(claims), "", true},
	} {
		t.Run(tc.id+map[bool]string{true: "-unique", false: "-first"}[tc.unique], func(t *testing.T) {
			reads := 0
			read := func(id string) (leaseClaim, bool, error) { reads++; return ReadLeaseClaimWithPresence(id) }
			got, ok, err := findMatchingLeaseClaim(t.Context(), tc.id, func(leaseClaim) bool { return true }, tc.unique, read)
			if reads != tc.count || got.LeaseID != tc.want || ok != (tc.want != "") {
				t.Fatalf("reads=%d claim=%q ok=%v err=%v", reads, got.LeaseID, ok, err)
			}
			if tc.ambiguous {
				if err == nil || !strings.Contains(err.Error(), "multiple claims") {
					t.Fatalf("expected ambiguity: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
	got, ok, err := findUniqueLeaseClaim(t.Context(), claims[0].LeaseID, func(leaseClaim) bool { return false })
	if err != nil || ok || got.LeaseID != "" {
		t.Fatalf("provider filter ignored: %+v %v %v", got, ok, err)
	}
}

func TestClaimScanCancellationBetweenFiles(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for _, id := range []string{"cbx_000000000001", "cbx_000000000002"} {
		writeClaimsListFixture(t, id+".json", leaseClaim{LeaseID: id})
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reads := 0
	_, err := snapshotLeaseClaimsReadOnlyContext(ctx, func(path, id string, info os.FileInfo) (leaseClaim, bool, error) {
		reads++
		claim, exists, err := readLeaseClaimSnapshotWithPresence(path, id, info)
		cancel()
		return claim, exists, err
	})
	if !errors.Is(err, context.Canceled) || reads != 1 {
		t.Fatalf("reads=%d err=%v", reads, err)
	}
}

func TestResolveLeaseClaimRejectsAmbiguousSlugs(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	claims := []leaseClaim{
		{LeaseID: "cbx_000000000001", Provider: "aws", ProviderScope: "scope-a", Slug: "same-slug"},
		{LeaseID: "cbx_000000000002", Provider: "aws", ProviderScope: "scope-a", Slug: "same-slug"},
		{LeaseID: "cbx_000000000003", Provider: "gcp", ProviderScope: "scope-a", Slug: "same-slug"},
		{LeaseID: "cbx_000000000004", Provider: "aws", ProviderScope: "scope-b", Slug: "same-slug"},
	}
	for _, c := range claims {
		writeClaimsListFixture(t, c.LeaseID+".json", c)
	}
	for name, lookup := range map[string]func(string) (leaseClaim, bool, error){
		"unscoped": ResolveLeaseClaim,
		"provider": func(id string) (leaseClaim, bool, error) { return ResolveLeaseClaimForProvider(id, "aws") },
		"provider-exact": func(id string) (leaseClaim, bool, error) {
			c, ok, _, err := ResolveLeaseClaimForProviderWithExact(id, "aws")
			return c, ok, err
		},
		"scope-exact": func(id string) (leaseClaim, bool, error) {
			c, ok, _, err := ResolveLeaseClaimForProviderScopeWithExact(id, "aws", "scope-a")
			return c, ok, err
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, ok, err := lookup("SAME SLUG")
			if err == nil || !strings.Contains(err.Error(), "multiple") || ok || c.LeaseID != "" {
				t.Fatalf("ambiguous slug selected claim: %+v %v %v", c, ok, err)
			}
			c, ok, err = lookup(claims[0].LeaseID)
			if err != nil || !ok || c.LeaseID != claims[0].LeaseID {
				t.Fatalf("exact ID lost: %+v %v %v", c, ok, err)
			}
		})
	}
	if c, ok, err := ResolveLeaseClaimForProvider("same-slug", "gcp"); err != nil || !ok || c.LeaseID != claims[2].LeaseID {
		t.Fatalf("provider filter: %+v %v %v", c, ok, err)
	}
	if c, ok, _, err := ResolveLeaseClaimForProviderScopeWithExact("same-slug", "aws", "scope-b"); err != nil || !ok || c.LeaseID != claims[3].LeaseID {
		t.Fatalf("scope filter: %+v %v %v", c, ok, err)
	}
}

func TestClaimLookupPrefersLiveClaimOverReleasedReceipt(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	released := &FixedCreateIntent{State: "released"}
	for _, c := range []leaseClaim{
		{LeaseID: "cbx_000000000001", Slug: "reused", FixedCreateIntent: released},
		{LeaseID: "cbx_000000000002", Slug: "reused"},
		{LeaseID: "cbx_000000000003", Slug: "receipts", FixedCreateIntent: released},
		{LeaseID: "cbx_000000000004", Slug: "receipts", FixedCreateIntent: released},
		{LeaseID: "cbx_000000000005", Slug: "only-receipt", FixedCreateIntent: released},
	} {
		writeClaimsListFixture(t, c.LeaseID+".json", c)
	}
	any := func(leaseClaim) bool { return true }
	if got, ok, err := findUniqueLeaseClaim(t.Context(), "reused", any); err != nil || !ok || got.LeaseID != "cbx_000000000002" {
		t.Fatalf("live claim should win over released receipt: %q %v %v", got.LeaseID, ok, err)
	}
	if _, ok, err := findUniqueLeaseClaim(t.Context(), "receipts", any); err == nil || ok {
		t.Fatalf("two released receipts must stay ambiguous: ok=%v err=%v", ok, err)
	}
	if got, ok, err := findUniqueLeaseClaim(t.Context(), "only-receipt", any); err != nil || !ok || got.LeaseID != "cbx_000000000005" {
		t.Fatalf("a lone receipt still resolves: %q %v %v", got.LeaseID, ok, err)
	}
}
