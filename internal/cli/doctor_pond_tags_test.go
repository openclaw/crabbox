package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestDoctorPondRecordedTags(t *testing.T) {
	const first = "tag:cbx-pond-alice-alpha"
	const second = "tag:cbx-pond-bob-alpha"
	for _, tc := range []struct {
		name, owner   string
		tags, policy  []string
		legacy        bool
		want, missing string
	}{
		{name: "original identity", owner: "alice@example.test", tags: []string{first}, policy: []string{first}, want: "ok"},
		{name: "changed identity", owner: "bob@example.test", tags: []string{first}, policy: []string{first}, want: "ok"},
		{name: "wrong identity row", owner: "bob@example.test", tags: []string{first}, policy: []string{second}, want: "failed", missing: first},
		{name: "missing identity", owner: "", tags: []string{first}, policy: []string{first}, want: "ok"},
		{name: "missing member row", owner: "alice@example.test", tags: []string{first}, policy: []string{second}, want: "failed", missing: first},
		{name: "multiple member tags", owner: "alice@example.test", tags: []string{first, second}, policy: []string{first}, want: "failed", missing: second},
		{name: "all member tags", owner: "alice@example.test", tags: []string{first, second}, policy: []string{first, second}, want: "ok"},
		{name: "mixed legacy", owner: "bob@example.test", tags: []string{first}, legacy: true, policy: []string{first}, want: "failed", missing: second},
		{name: "mixed legacy complete", owner: "bob@example.test", tags: []string{first}, legacy: true, policy: []string{first, second}, want: "ok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var claims []leaseClaim
			for i, tag := range tc.tags {
				claims = append(claims, leaseClaim{LeaseID: []string{"isb_web", "isb_db"}[i], Provider: "islo", Pond: "alpha", RepoRoot: "/fixture", TailscaleTags: []string{"tag:crabbox", tag}})
			}
			if tc.legacy {
				claims = append(claims, leaseClaim{LeaseID: "cbx_legacy", Provider: "hetzner", Pond: "alpha", RepoRoot: "/fixture"})
			}
			withTempClaims(t, claims)
			for _, claim := range claims {
				if len(claim.TailscaleTags) > 0 {
					if err := UpdateLeaseClaimTailscaleSettings(claim.LeaseID, "fixture-host", claim.TailscaleTags, "", "", false); err != nil {
						t.Fatal(err)
					}
				}
			}
			t.Setenv("CRABBOX_OWNER", tc.owner)
			t.Setenv("TS_API_KEY", "tskey-api-stub")
			owners := map[string][]string{}
			var grants []map[string]any
			for _, tag := range tc.policy {
				owners[tag] = []string{"autogroup:admin"}
				grants = append(grants, map[string]any{"src": []string{tag}, "dst": []string{tag}, "ip": []string{"*"}})
			}
			body, err := json.Marshal(map[string]any{"tagOwners": owners, "grants": grants})
			if err != nil {
				t.Fatal(err)
			}
			prev := doctorTailscaleACLClientFactory
			t.Cleanup(func() { doctorTailscaleACLClientFactory = prev })
			doctorTailscaleACLClientFactory = func(string) doctorTailscaleACLClient { return stubDoctorTailscaleACLClient{policy: string(body)} }
			verdict, message, details := doctorPondSummary(context.Background(), Config{Pond: "alpha"})
			if verdict != tc.want {
				t.Fatalf("verdict=%q want=%q message=%s details=%v", verdict, tc.want, message, details)
			}
			if tc.missing != "" && (!strings.Contains(message, tc.missing) || !strings.Contains(details["tag"], tc.missing)) {
				t.Errorf("missing tag not identified: %s %v", message, details)
			}
			if tc.want == "ok" {
				for _, tag := range tc.tags {
					if !strings.Contains(message, tag) || !strings.Contains(details["tag"], tag) {
						t.Errorf("recorded tag missing: %s %v", message, details)
					}
				}
			}
		})
	}
}
