package shared

import (
	"reflect"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestSandboxObservationLabels(t *testing.T) {
	o := SandboxObservation{Provider: "fixture", Target: "linux", LeaseID: "lease", Slug: "blue", State: "running",
		CreatedAt: time.Unix(100, 0), UpdatedAt: time.Unix(200, 0)}
	for _, tc := range []struct {
		name  string
		claim *core.LeaseClaim
		want  map[string]string
	}{
		{name: "unclaimed", want: map[string]string{}},
		{name: "recorded policy", claim: &core.LeaseClaim{
			LastUsedAt: time.Unix(300, 0).UTC().Format(time.RFC3339), IdleTimeoutSeconds: 300, Pond: "tests",
			Labels: map[string]string{"ttl_secs": "600", "idle_timeout_secs": "1800", "keep": "true", "profile": "creation",
				"state": "old", "created_at": "1", "expires_at": "999", "private_context": "not-for-output"},
		}, want: map[string]string{"ttl_secs": "600", "idle_timeout": "300", "idle_timeout_secs": "300", "keep": "true", "profile": "creation", "pond": "tests", "last_touched_at": "300"}},
		{name: "unknown legacy policy", claim: &core.LeaseClaim{LastUsedAt: "invalid", Labels: map[string]string{"ttl_secs": "unknown", "keep": "unknown"}}, want: map[string]string{}},
		{name: "nonpositive policy", claim: &core.LeaseClaim{IdleTimeoutSeconds: -1, Labels: map[string]string{"ttl_secs": "0"}}, want: map[string]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var before map[string]string
			if tc.claim != nil {
				before = make(map[string]string)
				for k, v := range tc.claim.Labels {
					before[k] = v
				}
			}
			got := o.Labels(tc.claim)
			want := map[string]string{"provider": "fixture", "target": "linux", "lease": "lease", "slug": "blue", "state": "running", "crabbox": "true", "created_by": "crabbox", "created_at": "100", "updated_at": "200"}
			for k, v := range tc.want {
				want[k] = v
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("labels=%v want %v", got, want)
			}
			if tc.claim != nil && !reflect.DeepEqual(tc.claim.Labels, before) {
				t.Fatal("projection mutated the recorded labels")
			}
		})
	}
	o.CreatedAt, o.UpdatedAt = time.Time{}, time.Unix(-1, 0)
	got := o.Labels(nil)
	if _, ok := got["created_at"]; ok {
		t.Fatal("unknown creation time was invented")
	}
	if _, ok := got["updated_at"]; ok {
		t.Fatal("invalid update time was retained")
	}
}
