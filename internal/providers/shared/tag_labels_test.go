package shared

import (
	"reflect"
	"testing"
)

func TestTagLabelReducerOwnershipConflictsAreSticky(t *testing.T) {
	for _, key := range []string{"provider", "lease", "slug", "target"} {
		for _, values := range [][]string{{"first", "second", "first"}, {"second", "first", "second"}} {
			t.Run(key+values[0], func(t *testing.T) {
				r := LeaseTagSchema().Reducer()
				for _, value := range values {
					r.Apply(key, value)
				}
				// A complete newer encoding cannot erase contradictory ownership.
				r.Overlay(key, "first", false)
				got := r.Finish("conflicts")
				if _, exists := got[key]; exists || got["conflicts"] != key {
					t.Fatalf("conflicting identity accepted: %v", got)
				}
			})
		}
	}
}

func TestTagLabelReducerMergePolicies(t *testing.T) {
	tests := []struct {
		name, key string
		values    []string
		want      string
	}{
		{"provider case", "provider", []string{"Example", "EXAMPLE"}, "example"},
		{"target case", "target", []string{"Linux", "LINUX"}, "linux"},
		{"running wins", "state", []string{"running", "ready", "provisioning"}, "running"},
		{"ready beats unknown", "state", []string{"stopped", "READY", "mystery"}, "ready"},
		{"equal ranks use latest", "state", []string{"ready", "ACTIVE"}, "active"},
		{"latest expiry", "expires_at", []string{"200", "100", "invalid"}, "200"},
		{"latest touch", "last_touched_at", []string{"100", "200", "199"}, "200"},
		{"numeric tie keeps representation", "expires_at", []string{"100", "0100"}, "0100"},
		{"invalid numeric tie", "expires_at", []string{"0", "invalid"}, "invalid"},
		{"overflow compatibility", "expires_at", []string{"1", "9223372036854775808"}, "1"},
		{"ordinary timestamp last wins", "updated_at", []string{"200", "100"}, "100"},
		{"only explicit fields lowercase", "desktop", []string{"TRUE"}, "TRUE"},
		{"boolean normalization", "keep", []string{"TRUE"}, "true"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := LeaseTagSchema().Reducer()
			for _, value := range tt.values {
				r.Apply(tt.key, value)
			}
			if got := r.Finish("conflicts")[tt.key]; got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTagLabelReducerExtensionsStayOptIn(t *testing.T) {
	tests := []struct {
		name   string
		schema TagLabelSchema
		want   map[string]string
	}{
		{"base", LeaseTagSchema(), map[string]string{}},
		{"tailscale", LeaseTagSchema(TailscaleTagFields()...), map[string]string{"tailscale": "true", "tailscale_hostname": "My_Host"}},
		{"key ownership", LeaseTagSchema(TagLabelField{Key: "provider_key_owned", Lowercase: true}), map[string]string{"provider_key_owned": "true"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := tt.schema.Reducer()
			for key, value := range map[string]string{"tailscale": "TRUE", "tailscale_hostname": "My_Host", "provider_key_owned": "TRUE", "foreign": "ignored"} {
				r.Apply(key, value)
			}
			if got := r.Finish("conflicts"); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTagLabelReducerOverlay(t *testing.T) {
	r := LeaseTagSchema(TailscaleTagFields()...).Reducer()
	r.MarkOwned()
	r.Apply("slug", "legacy")
	r.Overlay("slug", "complete-new-value", false)
	r.Apply("expires_at", "200")
	r.Overlay("expires_at", "100", false)
	r.Apply("tailscale_hostname", "old")
	r.Overlay("tailscale_hostname", "", true)
	r.Apply("lease", "cbx_abcdef123456")
	r.Overlay("lease", "", true)
	r.Apply("target", "linux")
	r.Overlay("target", "", true)
	want := map[string]string{
		"crabbox": "true", "created_by": "crabbox", "slug": "complete-new-value",
		"expires_at": "100", "conflicts": "lease,target",
	}
	if got := r.Finish("conflicts"); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestTagValueSetConflictSuppressesFallback(t *testing.T) {
	for _, reject := range []bool{false, true} {
		var values TagValueSet
		values.Record("name", "first")
		values.Record("name", "first")
		if value, present, conflict := values.Get("name"); value != "first" || !present || conflict {
			t.Fatalf("equal duplicates conflict: %q %v %v", value, present, conflict)
		}
		if reject {
			values.Reject("name")
		} else {
			values.Record("name", "second")
		}
		values.Record("name", "first")
		if value, present, conflict := values.Get("name"); value != "" || !present || !conflict {
			t.Fatalf("conflict was resurrected or allows fallback: %q %v %v", value, present, conflict)
		}
		if _, present, conflict := values.Get("absent"); present || conflict {
			t.Fatal("missing value is not an encoding conflict")
		}
	}
}
